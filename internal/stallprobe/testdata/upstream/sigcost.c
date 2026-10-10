/* sigcost: cost of a SIGURG-like signal on one pinned CPU, in plain C (no Go).
 *   cc -O2 -pthread -o sigcost sigcost.c
 *   ./sigcost <target_cpu> [sender_cpu] [n=100000]
 * The handler is installed like Go's: SA_SIGINFO|SA_ONSTACK|SA_RESTART with a sigaltstack.
 * self:    the target thread tgkill()s itself; time from before the syscall until it returns
 *          (the handler has run and sigreturn is done).
 * cross:   a sender thread pinned to sender_cpu tgkill()s the target thread, which spins in user
 *          code on target_cpu; time from before tgkill until the handler's first instruction
 *          stamps CLOCK_MONOTONIC (send->ack), then the sender waits for the ack.
 * Also prints ns per iteration of a fixed compute loop on target_cpu, cpu_capacity and
 * scaling_cur_freq, to separate "the core is slower" from "signals are slower on this core".
 * One line per path: SIGCOST path=.. cpu=.. n=.. min_us=.. p50_us=.. p99_us=.. max_us=..  */
#define _GNU_SOURCE
#include <errno.h>
#include <pthread.h>
#include <sched.h>
#include <signal.h>
#include <stdatomic.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/syscall.h>
#include <sys/utsname.h>
#include <time.h>
#include <unistd.h>

static _Atomic long ack_ns, target_tid;
static _Atomic int stop;

static long now_ns(void) { struct timespec t; clock_gettime(CLOCK_MONOTONIC, &t); return t.tv_sec * 1000000000L + t.tv_nsec; }
static void handler(int sig, siginfo_t *si, void *uc) { (void)sig; (void)si; (void)uc; atomic_store(&ack_ns, now_ns()); }
static void pin(int cpu) {
	cpu_set_t s; CPU_ZERO(&s); CPU_SET(cpu, &s);
	if (sched_setaffinity(0, sizeof s, &s)) { perror("sched_setaffinity"); exit(2); }
}
static int cmp(const void *a, const void *b) { long x = *(const long *)a, y = *(const long *)b; return x < y ? -1 : x > y; }
static void report(const char *path, int cpu, long *v, int n) {
	qsort(v, n, sizeof *v, cmp);
	printf("SIGCOST path=%s cpu=%d n=%d min_us=%.2f p50_us=%.2f p99_us=%.2f max_us=%.2f\n", path, cpu, n,
	       v[0] / 1e3, v[n / 2] / 1e3, v[n * 99 / 100] / 1e3, v[n - 1] / 1e3);
}
static long readnum(const char *fmt, int cpu) {
	char p[128], b[64]; snprintf(p, sizeof p, fmt, cpu);
	FILE *f = fopen(p, "r"); if (!f) return -1;
	long v = fgets(b, sizeof b, f) ? atol(b) : -1; fclose(f); return v;
}
static void altstack(void) {
	stack_t ss = { .ss_sp = malloc(SIGSTKSZ * 4), .ss_size = SIGSTKSZ * 4 };
	sigaltstack(&ss, NULL);
}
static void install(void) {
	struct sigaction sa; memset(&sa, 0, sizeof sa);
	sa.sa_sigaction = handler; sa.sa_flags = SA_SIGINFO | SA_ONSTACK | SA_RESTART; sigfillset(&sa.sa_mask);
	if (sigaction(SIGURG, &sa, NULL)) { perror("sigaction"); exit(2); }
}
static volatile unsigned long long sink;
static void *target(void *arg) { /* spins in user code until told to stop; signals interrupt it */
	pin((int)(long)arg); altstack();
	atomic_store(&target_tid, syscall(SYS_gettid));
	unsigned long long x = 88172645463325252ULL;
	while (!atomic_load(&stop)) { x ^= x << 13; x ^= x >> 7; x ^= x << 17; }
	sink = x; return NULL;
}

int main(int argc, char **argv) {
	if (argc < 2) { fprintf(stderr, "usage: %s target_cpu [sender_cpu] [n]\n", argv[0]); return 2; }
	int tcpu = atoi(argv[1]), scpu = argc > 2 ? atoi(argv[2]) : (tcpu == 0 ? 1 : 0), n = argc > 3 ? atoi(argv[3]) : 100000;
	struct utsname u; uname(&u);
	long *v = malloc(n * sizeof *v);
	install();
	pid_t pid = getpid();
	pin(tcpu); altstack();
	/* compute speed of this core: best of 5 x 10M iterations */
	double best = 1e18; unsigned long long x = 88172645463325252ULL;
	for (int r = 0; r < 5; r++) {
		long t = now_ns();
		for (int i = 0; i < 10000000; i++) { x ^= x << 13; x ^= x >> 7; x ^= x << 17; }
		double d = (now_ns() - t) / 1e7; if (d < best) best = d;
	}
	sink = x;
	printf("SIGCOST_FACTS kernel=%s machine=%s cpu=%d cpu_capacity=%ld scaling_cur_freq_khz=%ld compute_ns_per_iter=%.3f\n",
	       u.release, u.machine, tcpu, readnum("/sys/devices/system/cpu/cpu%d/cpu_capacity", tcpu),
	       readnum("/sys/devices/system/cpu/cpu%d/cpufreq/scaling_cur_freq", tcpu), best);
	pid_t me = (pid_t)syscall(SYS_gettid);
	for (int i = 0; i < n; i++) { /* self path */
		long t = now_ns();
		syscall(SYS_tgkill, pid, me, SIGURG);
		v[i] = now_ns() - t;
	}
	report("self", tcpu, v, n);
	pthread_t th; pthread_create(&th, NULL, target, (void *)(long)tcpu);
	while (!atomic_load(&target_tid)) usleep(1000);
	usleep(10000); pin(scpu);
	for (int i = 0; i < n; i++) {
		atomic_store(&ack_ns, 0);
		long t = now_ns();
		syscall(SYS_tgkill, pid, (pid_t)atomic_load(&target_tid), SIGURG);
		long a; while (!(a = atomic_load(&ack_ns))) ;
		v[i] = a - t;
	}
	atomic_store(&stop, 1); pthread_join(th, NULL);
	report("cross_spin", tcpu, v, n);
	return 0;
}
