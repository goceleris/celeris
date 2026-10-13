/* sigurg_lab: a plain-C micro-reproduction of Go's suspendG / doSigPreempt SIGURG protocol, no Go runtime.
 *   cc -O2 -pthread -o sigurg_lab sigurg_lab.c
 *
 *   sigurg_lab cell  T S D_US SECS [key=value ...]   one measurement cell (see below)
 *   sigurg_lab facts T S [N]                         per-CPU facts + signal path timing (self and cross)
 *   sigurg_lab exec  CPULIST cmd [args ...]          sched_setaffinity(whole process) then execvp (a taskset)
 *
 * Model (what is Go's, what is ours).
 *   Target thread: pinned to CPU T, spins in USER code incrementing a volatile counter. The SIGURG handler is
 *   installed the way Go installs its own (SA_SIGINFO|SA_ONSTACK|SA_RESTART, every signal blocked in sa_mask,
 *   a sigaltstack). The handler is doSigPreempt: it measures user progress since the previous handler entry
 *   (counter delta and interrupted-PC change), decides whether this signal "preempts", then ACKNOWLEDGES the
 *   way signal_unix.go:353-354 does: gen++ (preemptGen.Add(1)) and AFTER it signalPending=0, and returns
 *   (rt_sigreturn).
 *   Sender thread: pinned to CPU S, runs the suspendG protocol once per request: CAS signalPending 0->1, then
 *   tgkill(SIGURG); poll until gen changes (the ack); wait until >= D us after its PREVIOUS SEND; resend (again
 *   only if the CAS 0->1 succeeds); repeat until the request completes. After a completed request it idles
 *   idle_us (busy wait), then starts the next one.
 *   "Unsafe point" analogue: in Go the first signal often lands where isAsyncSafePoint refuses (it is still
 *   acknowledged). Without that every request would complete on send #1 and nothing could be learned about
 *   re-entry. The first K handler invocations of each request (K=1 by default) are therefore refused: they ack
 *   but cannot complete the request.
 *   Completion (complete=user|handler):
 *     user    (matrix default, Go's behaviour): a later handler that saw user progress only ARMS the request (Go's
 *             doSigPreempt only redirects the thread to asyncPreempt). The request ends when the TARGET LOOP, back in
 *             USER mode, notices the armed request and sets done (asyncPreempt running). The sender keeps resending
 *             on every ack until then, so a signal storm can keep the thread out of user mode and the request open.
 *     handler (smoke controls): the handler itself completes the request, the sender stops at the ack.
 *   RE-ENTRY = a handler invocation with counter delta == 0 AND the same interrupted PC as the previous handler:
 *   no user instruction was observed to retire between the two handlers. It is consistent with the kernel calling
 *   the handler again directly from rt_sigreturn (rhysh's mechanism) but does not prove it: an interrupt taken
 *   right after the return to user mode looks identical. Both mean zero progress toward the preemption request.
 *   `zero_delta` counts delta == 0 with any PC. K=0 is the literal "first handler with progress completes" rule.
 *
 * Differences from Go that matter when reading results:
 *   - Go's suspendG consumes a generation on a rate-limited iteration and then does not resend (CL 842725 is
 *     about exactly that). This sender instead waits until D after the previous send and resends always. It
 *     equals go1.27.2 when the ack arrives after D (always true on an A520 for D <= ack latency); resends_waited /
 *     resends says how often D was the binding constraint.
 *   - Go's handler (sigtrampgo, sighandler, pushCall, the post-ack tail of sigtrampgo) is longer than this one.
 *     postack_ns / preack_ns add a busy wait after / before the ack to emulate a longer Go tail. Off by default.
 *   - There is no synchronous preemption fallback here (Go's stackguard0 = stackPreempt), so a request that
 *     cannot complete times out (tmo_ms) and is counted.
 *   - Go never resends after a failed signalPending CAS (suspendG already recorded the new generation); by default this
 *     sender retries until the CAS succeeds. cas_fail_first counts first-try CAS failures; cas=go follows Go's rule
 *     (no retry: the request then waits for completion or times out, so its timeouts mean "Go would have fallen back").
 *   - Go's suspendG calls osyield every 5 us after its first 10 us; yield=go does the same in the sender's spin loops
 *     (default off: the sender spins with the arch pause hint).
 *
 * cell keys (all optional): k=1 mode=ack|noack complete=handler|user cas=retry|go yield=none|go idle_us=2000 tmo_ms=1000
 *   preack_ns=0 postack_ns=0 out=PATH label=NAME
 *   mode=noack: the handler never acknowledges and never completes: a negative control, every request must time out.
 *   out=PATH: one text line per request: lat_ns sends handlers reentry zero_delta maxconsec timeout.
 * Output lines (stdout): LABCELL k=v ...   LABFACTS k=v ...   Histograms are  idx:count,idx:count  with idx 0 =
 * under 1 us and idx b >= 1 = [1000*2^((b-1)/4), 1000*2^(b/4)) ns.
 */
#define _GNU_SOURCE
#include <errno.h>
#include <pthread.h>
#include <sched.h>
#include <signal.h>
#include <stdatomic.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/syscall.h>
#include <sys/utsname.h>
#include <time.h>
#include <ucontext.h>
#include <unistd.h>

#define CL __attribute__((aligned(64)))
#define NH 128
#define RING 64

static inline int64_t now_ns(void) { struct timespec t; clock_gettime(CLOCK_MONOTONIC, &t); return (int64_t)t.tv_sec * 1000000000LL + t.tv_nsec; }
static inline void relax(void) {
#if defined(__aarch64__)
	__asm__ volatile("yield" ::: "memory");
#elif defined(__x86_64__)
	__asm__ volatile("pause" ::: "memory");
#else
	__asm__ volatile("" ::: "memory");
#endif
}
static void spin_ns(int64_t ns) { int64_t e = now_ns() + ns; while (now_ns() < e) relax(); }
static void die(const char *m) { perror(m); exit(2); }
static void pin(int cpu) {
	cpu_set_t s; CPU_ZERO(&s); CPU_SET(cpu, &s);
	if (sched_setaffinity(0, sizeof s, &s)) die("sched_setaffinity");
}
static void altstack(void) {
	stack_t ss; memset(&ss, 0, sizeof ss); ss.ss_size = 1 << 18; ss.ss_sp = malloc(ss.ss_size);
	if (!ss.ss_sp || sigaltstack(&ss, NULL)) die("sigaltstack");
}
static void install(void (*h)(int, siginfo_t *, void *)) {
	struct sigaction sa; memset(&sa, 0, sizeof sa);
	sa.sa_sigaction = h; sa.sa_flags = SA_SIGINFO | SA_ONSTACK | SA_RESTART; sigfillset(&sa.sa_mask);
	if (sigaction(SIGURG, &sa, NULL)) die("sigaction");
}
static uintptr_t ucpc(void *ctx) {
	ucontext_t *uc = (ucontext_t *)ctx;
#if defined(__aarch64__)
	return (uintptr_t)uc->uc_mcontext.pc;
#elif defined(__x86_64__)
	return (uintptr_t)uc->uc_mcontext.gregs[REG_RIP];
#else
	(void)uc; return 0;
#endif
}
static int cmp64(const void *a, const void *b) { int64_t x = *(const int64_t *)a, y = *(const int64_t *)b; return x < y ? -1 : x > y; }
static int hidx(int64_t ns) { /* idx 0: < 1 us; idx 1+4*o+q covers [1000*2^(o+q/4), 1000*2^(o+(q+1)/4)) ns */
	if (ns < 1000) return 0;
	int o = 63 - __builtin_clzll((uint64_t)(ns / 1000));
	double r = (double)ns / (1000.0 * (double)(1ULL << o));
	int q = r >= 1.681792830507429 ? 3 : r >= 1.414213562373095 ? 2 : r >= 1.189207115002721 ? 1 : 0;
	int b = 1 + 4 * o + q; return b >= NH ? NH - 1 : b;
}
static void hadd(uint64_t *h, int64_t ns) { h[hidx(ns)]++; }
static void hprint(const char *name, const uint64_t *h) {
	int first = 1; printf(" %s=", name);
	for (int i = 0; i < NH; i++) if (h[i]) { printf("%s%d:%llu", first ? "" : ",", i, (unsigned long long)h[i]); first = 0; }
	if (first) printf("-");
}
static long readnum(const char *fmt, int cpu) {
	char p[160], b[64]; snprintf(p, sizeof p, fmt, cpu);
	FILE *f = fopen(p, "r"); if (!f) return -1;
	long v = fgets(b, sizeof b, f) ? atol(b) : -1; fclose(f); return v;
}
static void readstr(const char *fmt, int cpu, char *out, size_t n) {
	char p[160]; snprintf(p, sizeof p, fmt, cpu); snprintf(out, n, "-");
	FILE *f = fopen(p, "r"); if (!f) return;
	if (fgets(out, (int)n, f)) { out[strcspn(out, "\n ")] = 0; if (!out[0]) snprintf(out, n, "-"); } else snprintf(out, n, "-");
	fclose(f);
}

/* ------------------------------------------------------------------ shared state of a cell */
static struct { int tcpu, scpu; int64_t d_ns; int secs, k, noack, usercomp, casgo, yieldgo; int64_t idle_ns, tmo_ns, preack_ns, postack_ns; } cfg;
static CL _Atomic int sig_pending;                 /* m.signalPending */
static CL _Atomic uint64_t gen;                    /* m.preemptGen */
static CL _Atomic uint64_t req_seq, done_seq, armed_seq;
static CL _Atomic uint64_t req_handlers, req_maxconsec;
static CL _Atomic uint64_t a_handlers, a_reentry, a_zdelta;   /* handler totals, readable by the sender */
static CL volatile uint64_t counter;               /* the target's user-space progress */
static CL _Atomic int stopf;
static _Atomic int target_tid;
/* handler private (the target thread only; read by main after the join) */
static uint64_t prev_c, prev_pc, h_n, h_re, h_zd, consec, maxconsec, prog_h[48], ring[RING], ring_i;

static void handler(int sig, siginfo_t *si, void *ctx) {
	(void)sig; (void)si;
	uint64_t c = counter, pc = ucpc(ctx), delta = c - prev_c;
	int reentry = delta == 0 && pc == prev_pc;
	prev_c = c; prev_pc = pc; h_n++;
	prog_h[delta == 0 ? 0 : 1 + (63 - __builtin_clzll(delta)) % 47]++;
	ring[ring_i++ % RING] = delta;
	if (delta == 0) h_zd++;
	if (reentry) { h_re++; consec++; if (consec > maxconsec) maxconsec = consec; } else consec = 0;
	if (consec > atomic_load_explicit(&req_maxconsec, memory_order_relaxed)) atomic_store_explicit(&req_maxconsec, consec, memory_order_relaxed);
	atomic_store_explicit(&a_handlers, h_n, memory_order_relaxed);
	atomic_store_explicit(&a_reentry, h_re, memory_order_relaxed);
	atomic_store_explicit(&a_zdelta, h_zd, memory_order_relaxed);
	if (cfg.noack) return;                          /* negative control: no completion, no ack, signalPending stays 1 */
	if (cfg.preack_ns) spin_ns(cfg.preack_ns);
	uint64_t idx = atomic_fetch_add(&req_handlers, 1);
	if (idx >= (uint64_t)cfg.k && !reentry) atomic_store(cfg.usercomp ? &armed_seq : &done_seq, atomic_load(&req_seq));  /* "this handler saw user progress and the point is safe" */
	atomic_fetch_add(&gen, 1);                      /* preemptGen.Add(1) */
	atomic_store(&sig_pending, 0);                  /* signalPending.Store(0), after the generation bump */
	if (cfg.postack_ns) spin_ns(cfg.postack_ns);    /* emulates a longer post-ack tail (off by default) */
}

static double ns_per_iter(void) {
	double best = 1e18;
	for (int r = 0; r < 3; r++) {
		int64_t t = now_ns();
		for (int i = 0; i < 2000000; i++) counter++;
		double d = (double)(now_ns() - t) / 2e6; if (d < best) best = d;
	}
	return best;
}
static _Atomic double target_nspi;
static void *target_main(void *arg) {
	(void)arg; pin(cfg.tcpu); altstack();
	atomic_store(&target_nspi, ns_per_iter());
	atomic_store(&target_tid, (int)syscall(SYS_gettid));
	while (!atomic_load_explicit(&stopf, memory_order_relaxed)) {
		for (int i = 0; i < 16; i++) counter++;
		if (cfg.usercomp) {                     /* complete=user: asyncPreempt runs here, in USER mode, after a handler armed the request */
			uint64_t a = atomic_load_explicit(&armed_seq, memory_order_relaxed);
			if (a != atomic_load_explicit(&done_seq, memory_order_relaxed)) atomic_store(&done_seq, a);
		}
	}
	return NULL;
}

typedef struct { int64_t lat; uint64_t sends, handlers, reentry, zdelta, maxconsec; int timeout; } rec_t;

static uint64_t ack_h[NH], gap_h[NH], a2r_h[NH];
static uint64_t resends, resends_waited, cas_fail_first;

/* One request = one suspendG call. Returns 1 on completion, 0 on timeout. */
static int run_request(pid_t pid, uint64_t seq, rec_t *r) {
	pid_t tid = (pid_t)atomic_load(&target_tid);
	atomic_store(&req_handlers, 0); atomic_store(&req_maxconsec, 0); atomic_store(&req_seq, seq);
	uint64_t h0 = atomic_load(&a_handlers), z0 = atomic_load(&a_reentry), zd0 = atomic_load(&a_zdelta);
	int64_t t_start = now_ns(), t_first = 0, t_send = 0, t_ack = 0, next_yield = t_start + 10000; uint64_t sends = 0; int ok = 1, nosend = 0;
#define DONE() (atomic_load(&done_seq) == seq)
#define TMO() (now_ns() - t_start > cfg.tmo_ns)
#define STEP() do { if (cfg.yieldgo) { int64_t y = now_ns(); if (y >= next_yield) { sched_yield(); next_yield = now_ns() + 5000; } else relax(); } else relax(); } while (0)
	for (;;) {
		if (sends > 0) {                           /* resend not before D after the previous send */
			int64_t due = t_send + cfg.d_ns;
			resends++; if (t_ack < due) resends_waited++;
			while (now_ns() < due) { if (DONE()) goto fin; if (TMO()) { ok = 0; goto fin; } STEP(); }
		}
		for (unsigned n = 0;; n++) {               /* signalPending.CompareAndSwap(0, 1) */
			int e = 0;
			if (atomic_compare_exchange_strong(&sig_pending, &e, 1)) break;
			if (n == 0) { cas_fail_first++; if (cfg.casgo) nosend = 1; }
			if (nosend) { while (!DONE()) { if (TMO()) { ok = 0; break; } STEP(); } goto fin; }   /* cas=go: Go does not resend after a failed CAS */
			if (DONE()) goto fin;
			if ((n & 63) == 0 && TMO()) { ok = 0; goto fin; }
			STEP();
		}
		uint64_t g = atomic_load(&gen);
		int64_t t = now_ns();
		if (sends > 0) { hadd(gap_h, t - t_send); hadd(a2r_h, t - t_ack); } else t_first = t;
		t_send = t;
		syscall(SYS_tgkill, pid, tid, SIGURG); sends++;
		for (unsigned n = 0; atomic_load(&gen) == g; n++) {   /* wait for the ack */
			if ((n & 63) == 0 && TMO()) { ok = 0; goto fin; }
			STEP();
		}
		t_ack = now_ns(); hadd(ack_h, t_ack - t_send);
		if (DONE()) goto fin;
	}
fin:
	r->lat = ok ? now_ns() - t_first : cfg.tmo_ns;
	if (!ok && sends == 0) r->lat = cfg.tmo_ns;
	r->sends = sends; r->timeout = !ok;
	if (!ok) {                                     /* let the target settle, then clear a stuck signalPending */
		int64_t q = now_ns(), t0 = q; uint64_t g = atomic_load(&gen);
		while (now_ns() - q < 2000000 && now_ns() - t0 < 500000000) { if (atomic_load(&gen) != g) { g = atomic_load(&gen); q = now_ns(); } relax(); }
		atomic_store(&sig_pending, 0);
	}
	r->handlers = atomic_load(&a_handlers) - h0; r->reentry = atomic_load(&a_reentry) - z0; r->zdelta = atomic_load(&a_zdelta) - zd0;
	r->maxconsec = atomic_load(&req_maxconsec);
	return ok;
}

static void kv_int(const char *a, const char *key, int64_t *dst, int64_t mult) {
	size_t n = strlen(key);
	if (!strncmp(a, key, n) && a[n] == '=') *dst = (int64_t)atoll(a + n + 1) * mult;
}

static int cmd_cell(int argc, char **argv) {
	if (argc < 6) { fprintf(stderr, "usage: sigurg_lab cell T S D_US SECS [key=value ...]\n"); return 2; }
	cfg.tcpu = atoi(argv[2]); cfg.scpu = atoi(argv[3]); cfg.d_ns = atoll(argv[4]) * 1000; cfg.secs = atoi(argv[5]);
	cfg.k = 1; cfg.idle_ns = 2000000; cfg.tmo_ns = 1000000000;
	const char *out = NULL, *label = "-"; int64_t v;
	for (int i = 6; i < argc; i++) {
		v = cfg.k; kv_int(argv[i], "k", &v, 1); cfg.k = (int)v;
		kv_int(argv[i], "idle_us", &cfg.idle_ns, 1000); kv_int(argv[i], "tmo_ms", &cfg.tmo_ns, 1000000);
		kv_int(argv[i], "preack_ns", &cfg.preack_ns, 1); kv_int(argv[i], "postack_ns", &cfg.postack_ns, 1);
		if (!strncmp(argv[i], "mode=", 5)) cfg.noack = !strcmp(argv[i] + 5, "noack");
		if (!strncmp(argv[i], "complete=", 9)) cfg.usercomp = !strcmp(argv[i] + 9, "user");
		if (!strncmp(argv[i], "cas=", 4)) cfg.casgo = !strcmp(argv[i] + 4, "go");
		if (!strncmp(argv[i], "yield=", 6)) cfg.yieldgo = !strcmp(argv[i] + 6, "go");
		if (!strncmp(argv[i], "out=", 4)) out = argv[i] + 4;
		if (!strncmp(argv[i], "label=", 6)) label = argv[i] + 6;
	}
	alarm((unsigned)(cfg.secs + 40));                  /* never hang: SIGALRM kills the process */
	install(handler);
	pid_t pid = getpid();
	pthread_t th; if (pthread_create(&th, NULL, target_main, NULL)) die("pthread_create");
	while (!atomic_load(&target_tid)) usleep(1000);
	usleep(20000); pin(cfg.scpu);
	size_t cap = 1 << 14, n = 0; rec_t *recs = malloc(cap * sizeof *recs);
	int64_t t_begin = now_ns(), end = t_begin + (int64_t)cfg.secs * 1000000000LL; uint64_t seq = 0, completes = 0, timeouts = 0, sends_total = 0, sends_max = 0;
	while (now_ns() < end) {
		if (n == cap) { cap *= 2; recs = realloc(recs, cap * sizeof *recs); if (!recs) die("realloc"); }
		rec_t *r = &recs[n++];
		if (run_request(pid, ++seq, r)) completes++; else timeouts++;
		sends_total += r->sends; if (r->sends > sends_max) sends_max = r->sends;
		int64_t e = now_ns() + cfg.idle_ns; while (now_ns() < e) relax();
	}
	double secs_run = (double)(now_ns() - t_begin) / 1e9;
	atomic_store(&stopf, 1); pthread_join(th, NULL);
	if (out) {
		FILE *f = fopen(out, "w"); if (!f) die("fopen out");
		for (size_t i = 0; i < n; i++)
			fprintf(f, "%lld %llu %llu %llu %llu %llu %d\n", (long long)recs[i].lat, (unsigned long long)recs[i].sends, (unsigned long long)recs[i].handlers,
			        (unsigned long long)recs[i].reentry, (unsigned long long)recs[i].zdelta, (unsigned long long)recs[i].maxconsec, recs[i].timeout);
		fclose(f);
	}
	printf("LABCELL label=%s t=%d s=%d d_us=%lld k=%d mode=%s complete=%s cas=%s yield=%s idle_us=%lld tmo_ms=%lld preack_ns=%lld postack_ns=%lld secs=%d secs_run=%.2f requests=%zu completes=%llu timeouts=%llu sends=%llu sends_max=%llu resends=%llu resends_waited=%llu cas_fail_first=%llu handlers=%llu reentry=%llu zero_delta=%llu maxconsec=%llu gen=%llu ns_per_iter=%.3f",
	       label, cfg.tcpu, cfg.scpu, (long long)(cfg.d_ns / 1000), cfg.k, cfg.noack ? "noack" : "ack", cfg.usercomp ? "user" : "handler", cfg.casgo ? "go" : "retry", cfg.yieldgo ? "go" : "none", (long long)(cfg.idle_ns / 1000), (long long)(cfg.tmo_ns / 1000000),
	       (long long)cfg.preack_ns, (long long)cfg.postack_ns, cfg.secs, secs_run, n, (unsigned long long)completes, (unsigned long long)timeouts, (unsigned long long)sends_total,
	       (unsigned long long)sends_max, (unsigned long long)resends, (unsigned long long)resends_waited, (unsigned long long)cas_fail_first, (unsigned long long)h_n, (unsigned long long)h_re, (unsigned long long)h_zd,
	       (unsigned long long)maxconsec, (unsigned long long)atomic_load(&gen), atomic_load(&target_nspi));
	hprint("ack_hist", ack_h); hprint("gap_hist", gap_h); hprint("a2r_hist", a2r_h);
	{ int first = 1; printf(" prog_log2_hist="); for (int i = 0; i < 48; i++) if (prog_h[i]) { printf("%s%d:%llu", first ? "" : ",", i, (unsigned long long)prog_h[i]); first = 0; } if (first) printf("-"); }
	{ printf(" ring_tail="); uint64_t m = ring_i < RING ? ring_i : RING; if (m > 16) m = 16; for (uint64_t i = 0; i < m; i++) printf("%s%llu", i ? "," : "", (unsigned long long)ring[(ring_i - m + i) % RING]); if (!m) printf("-"); }
	printf("\n");
	free(recs);
	return 0;
}

/* ------------------------------------------------------------------ facts */
#define FMAX 200001
static _Atomic int64_t f_hs[FMAX], f_he[FMAX], f_user[FMAX];
static _Atomic uint64_t fgen;
static void fhandler(int sig, siginfo_t *si, void *ctx) {
	(void)sig; (void)si; (void)ctx;
	uint64_t ng = atomic_load(&fgen) + 1;
	int64_t t0 = now_ns();
	if (ng < FMAX) { atomic_store(&f_hs[ng], t0); atomic_store(&f_he[ng], now_ns()); }
	atomic_store(&fgen, ng);
}
static void *ftarget(void *arg) {
	pin((int)(long)arg); altstack();
	atomic_store(&target_tid, (int)syscall(SYS_gettid));
	uint64_t last = 0;
	while (!atomic_load_explicit(&stopf, memory_order_relaxed)) {
		uint64_t g = atomic_load_explicit(&fgen, memory_order_relaxed);
		if (g != last) { if (g < FMAX) atomic_store(&f_user[g], now_ns()); last = g; }
	}
	return NULL;
}
static void pct(const char *name, int64_t *v, int n) {
	if (n <= 0) { printf(" %s_p50_us=- %s_p99_us=-", name, name); return; }
	qsort(v, n, sizeof *v, cmp64);
	printf(" %s_p50_us=%.2f %s_p99_us=%.2f", name, v[n / 2] / 1e3, name, v[(int64_t)n * 99 / 100] / 1e3);
}
/* self: the thread tgkill()s itself. pre = syscall entry -> end of handler body (the ack point), post = ack point -> syscall returns. */
static void facts_self(int cpu, const char *tag, int n) {
	pin(cpu); altstack(); install(fhandler);
	pid_t pid = getpid(), me = (pid_t)syscall(SYS_gettid);
	int64_t *pre = malloc(n * sizeof *pre), *post = malloc(n * sizeof *post), *tot = malloc(n * sizeof *tot);
	for (int i = 0; i < n; i++) {
		uint64_t ng = atomic_load(&fgen) + 1; int64_t t0 = now_ns();
		syscall(SYS_tgkill, pid, me, SIGURG);
		int64_t t1 = now_ns();
		if (ng >= FMAX) { fprintf(stderr, "facts: too many samples\n"); exit(2); }
		pre[i] = atomic_load(&f_he[ng]) - t0; post[i] = t1 - atomic_load(&f_he[ng]); tot[i] = t1 - t0;
	}
	printf(" %s_n=%d", tag, n);
	char nm[64];
	snprintf(nm, sizeof nm, "%s_pre", tag); pct(nm, pre, n);
	snprintf(nm, sizeof nm, "%s_post", tag); pct(nm, post, n);
	snprintf(nm, sizeof nm, "%s_total", tag); pct(nm, tot, n);
	free(pre); free(post); free(tot);
}
static int cmd_facts(int argc, char **argv) {
	if (argc < 4) { fprintf(stderr, "usage: sigurg_lab facts T S [N]\n"); return 2; }
	int t = atoi(argv[2]), s = atoi(argv[3]), n = argc > 4 ? atoi(argv[4]) : 10000;
	if (n > 100000) n = 100000;
	alarm(120);
	struct utsname u; uname(&u);
	char midr_t[32], midr_s[32];
	readstr("/sys/devices/system/cpu/cpu%d/regs/identification/midr_el1", t, midr_t, sizeof midr_t);
	readstr("/sys/devices/system/cpu/cpu%d/regs/identification/midr_el1", s, midr_s, sizeof midr_s);
	pin(t); double nspi = ns_per_iter();
	printf("LABFACTS kernel=%s machine=%s t=%d s=%d t_cap=%ld s_cap=%ld t_khz=%ld s_khz=%ld t_midr=%s s_midr=%s t_ns_per_iter=%.3f",
	       u.release, u.machine, t, s, readnum("/sys/devices/system/cpu/cpu%d/cpu_capacity", t), readnum("/sys/devices/system/cpu/cpu%d/cpu_capacity", s),
	       readnum("/sys/devices/system/cpu/cpu%d/cpufreq/scaling_cur_freq", t), readnum("/sys/devices/system/cpu/cpu%d/cpufreq/scaling_cur_freq", s), midr_t, midr_s, nspi);
	facts_self(t, "self_t", n);
	facts_self(s, "self_s", n);
	/* cross: S sends to a target T that spins in user code; handler stamps start/end, the user loop stamps the first
	 * time it sees the new generation: end -> user is the post-ack return-to-user path of a signal that hit user code */
	atomic_store(&fgen, 0); install(fhandler);
	pthread_t th; if (pthread_create(&th, NULL, ftarget, (void *)(long)t)) die("pthread_create");
	while (!atomic_load(&target_tid)) usleep(1000);
	usleep(20000); pin(s);
	pid_t pid = getpid(); int m = n < 20000 ? n : 20000, got = 0;
	int64_t *s2h = malloc(m * sizeof(int64_t)), *h2e = malloc(m * sizeof(int64_t)), *e2u = malloc(m * sizeof(int64_t)), *s2u = malloc(m * sizeof(int64_t));
	for (int i = 0; i < m; i++) {
		uint64_t ng = (uint64_t)i + 1; int64_t t0 = now_ns(), lim = t0 + 200000000;
		syscall(SYS_tgkill, pid, (pid_t)atomic_load(&target_tid), SIGURG);
		while (atomic_load(&f_user[ng]) == 0 && now_ns() < lim) relax();
		if (atomic_load(&f_user[ng]) == 0) break;
		int64_t hs = atomic_load(&f_hs[ng]), he = atomic_load(&f_he[ng]), us = atomic_load(&f_user[ng]);
		s2h[got] = hs - t0; h2e[got] = he - hs; e2u[got] = us - he; s2u[got] = us - t0; got++;
		spin_ns(100000);
	}
	atomic_store(&stopf, 1); pthread_join(th, NULL);
	printf(" cross_n=%d", got);
	pct("cross_send_to_hstart", s2h, got); pct("cross_hbody", h2e, got); pct("cross_ack_to_user", e2u, got); pct("cross_send_to_user", s2u, got);
	printf("\n");
	return 0;
}

static int cmd_exec(int argc, char **argv) {
	if (argc < 4) { fprintf(stderr, "usage: sigurg_lab exec CPULIST cmd [args ...]\n"); return 2; }
	cpu_set_t s; CPU_ZERO(&s);
	char *l = strdup(argv[2]);
	for (char *p = strtok(l, ","); p; p = strtok(NULL, ",")) {
		int a, b; if (sscanf(p, "%d-%d", &a, &b) != 2) { b = a = atoi(p); }
		for (int c = a; c <= b && c < CPU_SETSIZE; c++) CPU_SET(c, &s);
	}
	if (sched_setaffinity(0, sizeof s, &s)) die("sched_setaffinity");
	execvp(argv[3], argv + 3);
	perror("execvp"); return 127;
}

int main(int argc, char **argv) {
	if (argc >= 2 && !strcmp(argv[1], "cell")) return cmd_cell(argc, argv);
	if (argc >= 2 && !strcmp(argv[1], "facts")) return cmd_facts(argc, argv);
	if (argc >= 2 && !strcmp(argv[1], "exec")) return cmd_exec(argc, argv);
	fprintf(stderr, "usage: sigurg_lab cell|facts|exec ...\n");
	return 2;
}
