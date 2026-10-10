// Command stallrepro: does GC stall a process whose LockOSThread'd goroutines are pinned to a
// slow core? Linux only, standard library only.
//
//	go run main.go                        # -class=little: CPUs with cpu_capacity <= half the largest (max 4)
//	go run main.go -class=big             # the other CPUs (max 8): control
//	go run main.go -cpus=2-5 -secs=30     # explicit CPU list
//	GODEBUG=asyncpreemptoff=1 go run main.go   # async preemption off: the control that cures it
//
// Each loop is LockOSThread()ed, pinned with sched_setaffinity to one CPU, and loops on
// epoll_wait(idle epfd, 1 ms) plus ~20 us of pure-Go work per wake-up (calibrated on the fastest
// core, so a slow core runs Go code longer). Four allocator goroutines churn pointer-bearing garbage
// (256 MiB/s) over a 32 MiB live heap so GC cycles keep running (GOGC=100 unless set). Wake gap =
// time between two epoll_wait returns that are not EINTR. Prints one RESULT line; verdict STALL
// (exit 1) if the longest wake gap exceeds -threshold-ms. On one core type "little"/"big" are the
// lowest/highest CPU ids (negative control). max_tick_gap_ms: longest gap of an unpinned 1 ms ticker goroutine.
package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

type node struct {
	next *node
	_    [56]byte
}

var (
	sink uint64
	stop int32
	t0   = time.Now()
)

//go:noinline
func spin(n int) (x uint64) {
	for x = 88172645463325252; n > 0; n-- {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
	}
	return x
}

// affinity pins the calling thread to cpu (cpu >= 0), or returns the CPUs it may run on.
func affinity(cpu int) (set []int) {
	var m [16]uint64
	call := uintptr(syscall.SYS_SCHED_GETAFFINITY)
	if cpu >= 0 {
		call, m[cpu/64] = syscall.SYS_SCHED_SETAFFINITY, 1<<(uint(cpu)%64)
	}
	if _, _, e := syscall.RawSyscall(call, 0, unsafe.Sizeof(m), uintptr(unsafe.Pointer(&m[0]))); e != 0 {
		panic(e)
	}
	for c := 0; c < 1024; c++ {
		if m[c/64]>>(uint(c)%64)&1 == 1 {
			set = append(set, c)
		}
	}
	return set
}

func capacity(cpu int) (n int) { // 0 when the kernel does not export cpu_capacity (x86)
	b, _ := ioutil.ReadFile(fmt.Sprintf("/sys/devices/system/cpu/cpu%d/cpu_capacity", cpu))
	fmt.Sscan(string(b), &n)
	return n
}

func gcCycles() int64 { // takes only the heap lock, no stop-the-world (unlike ReadMemStats)
	var st debug.GCStats
	debug.ReadGCStats(&st)
	return st.NumGC
}

func main() {
	secs := flag.Int("secs", 30, "measuring time, seconds")
	list := flag.String("cpus", "", "CPU list, one loop on each, e.g. 2-5 (default: by -class)")
	class := flag.String("class", "little", "little|big, from cpu_capacity")
	thr := flag.Float64("threshold-ms", 50, "STALL when the longest wake gap exceeds this")
	flag.Parse()
	all := affinity(-1)
	hi, fast := 0, all[0]
	for _, c := range all {
		if k := capacity(c); k > hi {
			hi, fast = k, c
		}
	}
	var small, large []int // little: cpu_capacity <= half the largest; big: the rest
	for _, c := range all {
		if hi > 0 && capacity(c)*2 <= hi {
			small = append(small, c)
		} else {
			large = append(large, c)
		}
	}
	hetero := len(small) > 0 && len(large) > 0
	if !hetero { // one core type: little = the 4 lowest ids, big = the 8 highest
		small, large = all, all
	}
	cpus := small
	if *class == "big" {
		cpus = large
	}
	if *class == "little" && len(cpus) > 4 {
		cpus = cpus[:4]
	} else if len(cpus) > 8 {
		cpus = cpus[len(cpus)-8:]
	}
	label := *class
	if *list != "" {
		cpus, label = nil, "cpus="+*list
		for _, p := range strings.Split(*list, ",") {
			a, b := 0, -1
			fmt.Sscanf(p, "%d-%d", &a, &b)
			for c := a; c <= a || c <= b; c++ {
				cpus = append(cpus, c)
			}
		}
	}
	calib := make(chan float64)
	go func() { // ns per spin iteration on the fastest core: best of 5
		runtime.LockOSThread()
		affinity(fast)
		best := 1e18
		for i := 0; i < 5; i++ {
			s := time.Now()
			atomic.AddUint64(&sink, spin(2000000))
			if d := float64(time.Since(s)) / 2e6; d < best {
				best = d
			}
		}
		calib <- best
	}()
	nsPerIter := <-calib
	workIters := int(20000 / nsPerIter)
	last, maxGap := make([]int64, len(cpus)), make([]int64, len(cpus))
	ready := make(chan bool, len(cpus))
	for i, c := range cpus {
		go func(i, c int) {
			runtime.LockOSThread() // never unlocked: the thread dies with the goroutine
			epfd, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
			if err != nil {
				panic(err)
			}
			affinity(c)
			ready <- true
			evs := make([]syscall.EpollEvent, 8)
			atomic.StoreInt64(&last[i], int64(time.Since(t0)))
			for atomic.LoadInt32(&stop) == 0 {
				if _, err := syscall.EpollWait(epfd, evs, 1); err != nil { // EINTR: a signal, keep waiting
					continue
				}
				now := int64(time.Since(t0))
				if g := now - atomic.LoadInt64(&last[i]); g > atomic.LoadInt64(&maxGap[i]) {
					atomic.StoreInt64(&maxGap[i], g)
				}
				atomic.StoreInt64(&last[i], now)
				atomic.AddUint64(&sink, spin(workIters))
			}
		}(i, c)
	}
	for range cpus {
		<-ready
	}
	live := make([]*node, 32<<20/64)
	for i := range live {
		live[i] = new(node)
	}
	gc0 := gcCycles()
	for a := 0; a < 4; a++ { // 4 allocators, 64 MiB/s of garbage each
		go func(lo, hi int) {
			start, did := time.Now(), 0.0
			for j := 1; atomic.LoadInt32(&stop) == 0; j++ {
				allowed := 64 << 20 * time.Since(start).Seconds()
				if allowed-did > 8<<20 {
					did = allowed - 8<<20 // no catch-up burst after a stall
				}
				if did >= allowed {
					time.Sleep(200 * time.Microsecond)
					continue
				}
				var head *node
				for k := 0; k < 1024; k++ {
					head = &node{next: head}
				}
				did += 1024 * 64
				if j%16 == 0 { // churn the live set so the write barrier is busy during mark
					live[lo+(j/16)%(hi-lo)] = &node{next: head.next.next}
				}
			}
		}(a*len(live)/4, (a+1)*len(live)/4)
	}
	var tickMax int64
	go func() {
		for t := time.Now(); atomic.LoadInt32(&stop) == 0; t = time.Now() {
			time.Sleep(time.Millisecond)
			if g := int64(time.Since(t)); g > atomic.LoadInt64(&tickMax) {
				atomic.StoreInt64(&tickMax, g)
			}
		}
	}()
	time.Sleep(time.Duration(*secs) * time.Second)
	end := int64(time.Since(t0))
	atomic.StoreInt32(&stop, 1)
	gcN := gcCycles() - gc0
	var worst int64
	for i := range cpus {
		g := atomic.LoadInt64(&maxGap[i])
		if open := end - atomic.LoadInt64(&last[i]); open > g { // a gap still open at the end counts
			g = open
		}
		if g > worst {
			worst = g
		}
	}
	verdict := "PASS"
	if float64(worst)/1e6 > *thr {
		verdict = "STALL"
	}
	fmt.Printf("RESULT go=%s goarch=%s class=%s hetero=%v cpus=%s gomaxprocs=%d gogc=%q godebug=%q ns_per_iter=%.3f work_iters=%d gc_cycles=%d max_gap_ms=%.1f max_tick_gap_ms=%.1f threshold_ms=%g verdict=%s\n",
		runtime.Version(), runtime.GOARCH, label, hetero, strings.Trim(strings.ReplaceAll(fmt.Sprint(cpus), " ", ","), "[]"), runtime.GOMAXPROCS(0), os.Getenv("GOGC"), os.Getenv("GODEBUG"), nsPerIter, workIters,
		gcN, float64(worst)/1e6, float64(atomic.LoadInt64(&tickMax))/1e6, *thr, verdict)
	if verdict == "STALL" {
		os.Exit(1)
	}
}
