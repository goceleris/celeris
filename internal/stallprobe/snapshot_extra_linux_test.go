//go:build linux

package stallprobe

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"
)

// Additions to the stall-time snapshot after run 37975655016, which showed in
// 19 of 19 stalls exactly one loop thread on a Cortex-A520 on a CPU for the
// whole 500 ms window (0-3 sched-ins) and exactly one unpinned thread on a
// fast CPU doing the same, and could not say where either was spending the
// time or which loop owned the stalled connection:
//
//   - a CPU profile of the gap between the two captures (CELERIS_PROBE_PROF=1,
//     default OFF: SIGPROF to every thread can reshape the state it measures, so
//     rate runs must not turn it on; a mechanism run does): the Go stacks the threads were sampled in, as base64 of the
//     gzip pprof protobuf in the log ("pprof-b64" lines; decode with
//     scripts/decode_pprof.sh, then go tool pprof -top). A thread that is in the
//     kernel the whole time has no user samples: that absence is the answer.
//   - which loop owns the stalled connection (epoll engines): the server-side
//     socket's inode from /proc/net/tcp, the fd with that inode in /proc/self/fd,
//     the epoll instance whose /proc/self/fdinfo lists that fd, and the loop
//     thread whose epoll_pwait argument is that epoll fd (a loop that is not
//     blocked in epoll_pwait shows "running" and has no argument: the owner is
//     then the loop no sleeping loop claims).
//   - opt-in (CELERIS_PROBE_GDUMP=1): a goroutine dump after the second capture,
//     timed. Taking it stops the world; if it takes seconds, some thread could not
//     be preempted (celeris#945 saw 2067 ms), which is itself the finding.

type profCapture struct {
	buf bytes.Buffer
	on  bool
	err string
}

func (p *profCapture) start() {
	if !pcfg.Prof {
		return
	}
	p.buf.Reset()
	if err := pprof.StartCPUProfile(&p.buf); err != nil {
		p.err = err.Error()
		return
	}
	p.on = true
}

// stop ends the profile and returns it as log lines.
func (p *profCapture) stop() string {
	if !pcfg.Prof {
		return ""
	}
	if !p.on {
		return "cpu profile: not taken (" + orDash(p.err) + ")"
	}
	t0 := time.Now()
	pprof.StopCPUProfile()
	p.on = false
	enc := base64.StdEncoding.EncodeToString(p.buf.Bytes())
	var sb strings.Builder
	fmt.Fprintf(&sb, "cpu profile of the gap between the two captures: %d bytes gzip pprof, StopCPUProfile took %.1f ms; decode: d1-read/scripts/decode_pprof.sh LOG LINE_OF_THIS_LINE OUT.pb.gz\n", p.buf.Len(), float64(time.Since(t0))/1e6)
	for len(enc) > 0 {
		n := min(len(enc), 160)
		fmt.Fprintf(&sb, "pprof-b64 %s\n", enc[:n])
		enc = enc[n:]
	}
	return strings.TrimRight(sb.String(), "\n")
}

// goroutineDump is the opt-in timed dump.
func goroutineDump() string {
	if !pcfg.GDump {
		return ""
	}
	type res struct {
		s  string
		ms float64
	}
	ch := make(chan res, 1)
	t0 := time.Now()
	go func() {
		var b bytes.Buffer
		_ = pprof.Lookup("goroutine").WriteTo(&b, 2)
		s := b.String()
		if len(s) > 24000 {
			s = s[:24000] + "\n... (cut)"
		}
		ch <- res{s, float64(time.Since(t0)) / 1e6}
	}()
	select {
	case r := <-ch:
		return fmt.Sprintf("goroutine dump (debug=2) took %.1f ms:\n%s", r.ms, r.s)
	case <-time.After(3 * time.Second):
		return "goroutine dump (debug=2) STILL RUNNING after 3 s (the stop-the-world is waiting for a thread that cannot be preempted)"
	}
}

type sockInode struct {
	Local, Remote int
	State         string
	Inode         string
}

func readSockInodes() []sockInode {
	var out []sockInode
	for _, file := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		for i, l := range strings.Split(string(b), "\n") {
			f := strings.Fields(l)
			if i == 0 || len(f) < 10 {
				continue
			}
			out = append(out, sockInode{Local: hexPort(f[1]), Remote: hexPort(f[2]), State: tcpStates[f[3]], Inode: f[9]})
		}
	}
	return out
}

// connOwner says which epoll loop the stalled connection (server side) is
// registered with, when the engine is an epoll engine.
func connOwner(cport, sport int, tasks []snapTask, loops []loopThread) string {
	var inode string
	for _, s := range readSockInodes() {
		if s.Local == sport && s.Remote == cport && s.State == "ESTAB" {
			inode = s.Inode
		}
	}
	if inode == "" {
		return "conn owner: server-side socket not found in /proc/net/tcp"
	}
	fdOf, epfds := "", []string{}
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return "conn owner: /proc/self/fd not readable"
	}
	for _, e := range ents {
		link, err := os.Readlink("/proc/self/fd/" + e.Name())
		if err != nil {
			continue
		}
		switch {
		case link == "socket:["+inode+"]":
			fdOf = e.Name()
		case link == "anon_inode:[eventpoll]":
			epfds = append(epfds, e.Name())
		}
	}
	if fdOf == "" {
		return "conn owner: socket inode " + inode + " has no fd in this process"
	}
	owner := ""
	for _, ep := range epfds {
		info := readTrim("/proc/self/fdinfo/" + ep)
		for _, l := range strings.Split(info, "\n") {
			f := strings.Fields(l)
			if len(f) >= 2 && f[0] == "tfd:" && f[1] == fdOf {
				owner = ep
			}
		}
	}
	if owner == "" {
		return fmt.Sprintf("conn owner: server-side fd %s (socket inode %s) is in none of the %d epoll instances (an io_uring or std engine, or the conn is not yet accepted)", fdOf, inode, len(epfds))
	}
	// loop thread whose blocking epoll call has this epoll fd as its first argument
	claimed := map[string]int{}
	for _, t := range tasks {
		f := strings.Fields(t.Syscall)
		if len(f) >= 2 && (f[0] == "22" || f[0] == "232" || f[0] == "281") { // epoll_pwait arm64, epoll_wait amd64, epoll_pwait amd64
			if n, err := strconv.ParseInt(strings.TrimPrefix(f[1], "0x"), 16, 64); err == nil {
				claimed[strconv.FormatInt(n, 10)] = t.Tid
			}
		}
	}
	isLoop := map[int]loopThread{}
	for _, l := range loops {
		isLoop[l.Tid] = l
	}
	if tid, ok := claimed[owner]; ok {
		if l, ok := isLoop[tid]; ok {
			return fmt.Sprintf("conn owner: server-side fd %s (socket inode %s) is in epoll fd %s = loop thread %d (pin %d), which is blocked in epoll_pwait", fdOf, inode, owner, tid, l.From)
		}
		return fmt.Sprintf("conn owner: server-side fd %s in epoll fd %s, blocked in epoll_pwait on thread %d (not a known loop)", fdOf, owner, tid)
	}
	var unclaimed []string
	for _, l := range loops {
		for _, t := range tasks {
			if t.Tid == l.Tid && !strings.HasPrefix(t.Syscall, "22 ") && !strings.HasPrefix(t.Syscall, "232 ") && !strings.HasPrefix(t.Syscall, "281 ") {
				unclaimed = append(unclaimed, fmt.Sprintf("%d(pin %d, %s, sys=%s)", t.Tid, l.From, t.State, syscallWord(t.Syscall)))
			}
		}
	}
	return fmt.Sprintf("conn owner: server-side fd %s (socket inode %s) is in epoll fd %s, which no loop is blocked on; the owner is one of the loops not blocked in epoll_pwait: %s", fdOf, inode, owner, strings.Join(unclaimed, "; "))
}
