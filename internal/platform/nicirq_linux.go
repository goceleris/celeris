//go:build linux

package platform

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// NIC interrupt report (celeris#1036 item 1). On the one big.LITTLE host it was
// seen on, a bonded port whose TX-completion interrupt was serviced by a CPU
// that ran no engine loop was seen with ~200 ms whole-port TX silences, which
// were not seen after the interrupt was moved (one observation). The report
// only logs (a Warn); it never changes the loops.
//
// DetectNICIRQs answers "which CPUs service the interrupts of the NICs this
// listener's traffic crosses", once, when the engine has started its loops. It
// is the other half of the read nicrx_linux.go does for the rx-port rule and
// shares its helpers (listenInterfaces, lowerDevices, deviceIRQs, effectiveCPU,
// parseInterrupts and their bounds); what it adds is the traffic test below
// and the report. The CPUs the rx-port rule frees on purpose are left out of
// the Warn by [CPUPlan.WithNICIRQs], not here: this file reports every vector
// that carries traffic.
//
//	listen address -> interfaces -> physical devices -> IRQ numbers -> CPU per IRQ
//	 (net, pure Go)   (lower_* links)  (msi_irqs, irq,    (effective_affinity_list,
//	                                    /proc/interrupts)  smp_affinity_list)
//
// Every failure (a missing file, a permission error, a format the parser does
// not know, a container that shows no host interface) skips the item and, at
// worst, yields an empty result: the caller then logs nothing about
// interrupts. Nothing here panics or returns an error to Listen.

// Limits of the traffic test. They are choices fitted to msr1.
const (
	// nicMinSharePercent is the share of the interrupts of all the vectors of
	// the listener's physical NICs, counted since boot, that one vector must
	// have taken to count as carrying traffic (celeris#1036). nicMinInterrupts
	// is the fewest interrupts it must have taken in all, whatever the share.
	// Both must hold: the share keeps a vector that fired a few times out of a
	// busy NIC, the floor keeps the tiny counts of an almost idle host, where
	// any vector would otherwise hold a large share of nothing. See
	// [DetectNICIRQs] for the measured margin.
	nicMinSharePercent = 5
	nicMinInterrupts   = 1000
	// nicMaxSkipped is the number of skipped vectors that keep a line of detail
	// (the count of all of them is kept).
	nicMaxSkipped = 8
)

// NICVector is one interrupt vector of a NIC the listener is reachable through.
type NICVector struct {
	// IRQ is the Linux IRQ number.
	IRQ int
	// CPU is the CPU that services it.
	CPU int
	// Name is the action name /proc/interrupts shows ("enp1s0-8"), or
	// "irq<N>" when that file did not list the vector.
	Name string
	// Dev is the physical interface the vector belongs to.
	Dev string
	// Count is the vector's interrupts since boot, summed over the CPUs. Every
	// vector in a result passed the traffic test (see [NICIRQs.Total]), so
	// Count is at least 1000 and at least 5% of Total.
	Count uint64
}

// NICIRQs is what DetectNICIRQs found.
type NICIRQs struct {
	// Ifaces are the interfaces the listen address resolved to.
	Ifaces []string
	// Devs are the physical devices behind them.
	Devs []string
	// Vectors are the vectors whose servicing CPU is known, in ascending IRQ
	// order per device.
	Vectors []NICVector
	// Unknown counts the vectors whose servicing CPU could not be told (a
	// multi-CPU mask or no readable file).
	Unknown int
	// Idle counts the vectors left out because /proc/interrupts shows no
	// interrupt for them (absent or all zero): a vector that never fires cannot
	// be the one that matters. Only the first nicMaxVectors (4096) vectors found are judged;
	// the ones beyond that cap are in neither Idle nor Negligible (their
	// interrupts are still in Total), and a device lists at most
	// nicMaxIRQsPerDev (1024) vectors.
	Idle int
	// Negligible counts the vectors left out because they took interrupts but
	// not a meaningful share: fewer than 1000 since boot, or under 5% of Total.
	// A vector that fired a handful of times is not checked against the loops
	// either. The count
	// also holds the evenly busy vectors of a NIC balanced over more than about
	// 20 of them, each under 5%: it is "under the limits", not "quiet".
	Negligible int
	// Skipped describes the first few Negligible vectors (at most eight),
	// for the log. Idle vectors are only counted: a NIC has dozens of them.
	Skipped []NICSkip
	// Total is the interrupts since boot of all the vectors found for Devs,
	// whether or not their CPU is known: the denominator of the 5% test.
	Total uint64
	// Reason says why Vectors is empty (or short); empty when it is not.
	Reason string
	// Took is how long the detection took.
	Took time.Duration
}

// NICSkip is a vector left out of a result because it took too few interrupts.
type NICSkip struct {
	IRQ  int
	Name string
	Dev  string
	// CPU is the CPU that services the vector, or -1 when that is not known.
	CPU int
	// Count is its interrupts since boot.
	Count uint64
	// Why says which limit it is under.
	Why string
}

// DetectNICIRQs finds the CPUs that service the interrupt vectors of the NICs
// a listener bound to addr ("host:port", as the engine's concrete address) is
// reachable through. It reads /sys/class/net, /proc/interrupts and
// /proc/irq/N once and never returns an error: what it cannot read it skips,
// and Reason says why the result is empty.
//
// A vector is reported only if it carries traffic. With total the sum of the
// interrupts since boot of every vector of every physical device behind the
// listener (whether or not the vector's CPU is known, and whatever its count),
// a vector with n interrupts qualifies when
//
//	n >= nicMinInterrupts (1000)  and  n*100 >= total*nicMinSharePercent (5)
//
// compared exactly in integers. The test reads one /proc/interrupts: nothing is
// sampled over time and nothing sleeps. Vectors that fail it are counted in
// Idle (no interrupt at all, or not listed) and Negligible (the others), and
// the first few of the latter are described in Skipped. If /proc/interrupts
// cannot be read or parsed, or the vectors of one device cannot be listed, no
// vector can be judged and the result is empty with a Reason: the caller has
// nothing to warn about.
//
// The limits come from msr1, the one host measured (a bond of two ports with 32
// vectors each, 96 snapshots over 111 minutes): four vectors carried the
// traffic with about 15% to 35% of the total each (1.7e9 to 3.9e9 interrupts),
// and the other 60 had none or 3 interrupts (IRQ 156 and 188, 3e-8 % of the
// total). Any share from 3e-8 % up to 14% and any floor from 4 to 1.7e9 separate
// the two groups; 5% and 1000 sit in the gap, a factor 3 under the quietest
// busy vector and a factor 333 over the loudest idle one, and are a choice, not
// a measurement.
//
// Known blind spots. The counts are since boot and the kernel counts per CPU
// in 32 bits. Some of the blind spots leave a vector unwarned about, but two of
// them can WARN about a vector that does not matter now, so they do not all fail
// on the quiet side:
//
//   - Fresh boot (no Warn): a vector that becomes busy after the boot has not
//     reached 1000 interrupts yet. A server started at boot will most likely
//     see every vector under the floor and get no Warn, while a restart later
//     would get one.
//   - Busy long ago (can Warn about a vector that is quiet now): a vector that
//     was busy before an idle stretch still qualifies, because the counts are
//     since boot and nothing is sampled over time.
//   - Counter wrap, the vector that matters (no Warn): when the counter of the
//     vector that carries the traffic wraps (IRQ 167 on msr1 is at about 2.6e9
//     of 4.29e9, far from it) it reads small, can fall under 5% and is not
//     warned about, silently (the Info line counts it in nic_irq_skipped and a
//     Debug line names it). The exception: a vector the rx-port rule stepped
//     aside over (RxFree.NoLoop, its floor of 1000 is its test) is warned about
//     whatever its share, by [CPUPlan.WithNICIRQs]; where the rule does not run,
//     or under 1000, it stays unwarned.
//   - Counter wrap, a busy vector on a big CPU (can Warn about a vector that
//     does not matter): the denominator shrinks and can lift another vector's
//     share over 5%. With a big-CPU vector at 50,000 after the wrap and a
//     little-CPU vector at 5,000, the latter reads 9.1%.
//   - A NIC balanced over more than about 20 evenly busy vectors puts each
//     under 5%: no Warn. The Reason then reads "none reaches the share/floor",
//     although the vectors are busy.
func DetectNICIRQs(addr string) NICIRQs {
	t0 := time.Now()
	out := detectNICIRQs(addr, defaultNICSource())
	out.Took = time.Since(t0)
	return out
}

func detectNICIRQs(addr string, src nicSource) (out NICIRQs) {
	// The parsers index with checks, but a panic out of Listen over a procfs
	// format nobody anticipated would be worse than the missing warning.
	defer func() {
		if r := recover(); r != nil {
			out = NICIRQs{Reason: fmt.Sprintf("internal error: %v", r)}
		}
	}()
	if src.fs == nil || src.ifaceAddrs == nil {
		return NICIRQs{Reason: "no source"}
	}
	ifaces, reason := listenInterfaces(addr, src)
	if len(ifaces) == 0 {
		return NICIRQs{Reason: reason}
	}
	out.Ifaces = ifaces
	devs := lowerDevices(ifaces, src)
	if len(devs) == 0 {
		out.Reason = "no physical device behind " + strings.Join(ifaces, ",") + " (virtual interfaces only)"
		return out
	}
	out.Devs = devs

	// /proc/interrupts: the counts that say which vectors carry traffic, the
	// names, and the only source for a platform device. Without a table nothing
	// can be told from nothing: no vector is reported and nothing is warned about.
	var table map[int]irqRow
	var rows []irqRow
	if b, err := src.fs.readFile(src.procDir + "/interrupts"); err == nil {
		rows = parseInterrupts(b)
	}
	if len(rows) == 0 {
		out.Reason = "no usable " + src.procDir + "/interrupts: cannot tell which vectors carry traffic"
		return out
	}
	table = make(map[int]irqRow, len(rows))
	for _, r := range rows {
		table[r.irq] = r
	}

	// Pass 1: every vector of every device, once each. Their totals make the
	// denominator of the share test, so it is complete before any vector is
	// judged: it holds the vectors whose CPU turns out unknown and the ones
	// the test drops, and not a single vector of another interface.
	type cand struct {
		irq    int
		dev    string
		row    irqRow
		listed bool
	}
	var cands []cand
	seen := map[int]bool{}
	for _, dev := range devs {
		irqs, ok := deviceIRQs(dev, src, rows)
		if !ok {
			// A device whose vectors cannot be read is traffic nobody can
			// count: the shares of the others would be too large.
			out.Reason = "cannot read the interrupt vectors of " + dev
			return out
		}
		for _, irq := range irqs {
			if seen[irq] {
				continue
			}
			seen[irq] = true
			row, listed := table[irq]
			if listed {
				out.Total = satAdd(out.Total, row.total)
			}
			if len(cands) < nicMaxVectors {
				cands = append(cands, cand{irq, dev, row, listed})
			}
		}
	}

	// Pass 2: a vector counts as carrying traffic when it took at least nicMinInterrupts
	// and at least nicMinSharePercent of Total since boot.
	for _, c := range cands {
		v := NICVector{IRQ: c.irq, Dev: c.dev, Name: "irq" + strconv.Itoa(c.irq)}
		if c.listed && c.row.label != "" {
			v.Name = c.row.label
		}
		if !c.listed || c.row.total == 0 {
			out.Idle++
			continue
		}
		if why := negligible(c.row.total, out.Total); why != "" {
			out.Negligible++
			if len(out.Skipped) < nicMaxSkipped {
				cpu := effectiveCPU(c.irq, src)
				out.Skipped = append(out.Skipped, NICSkip{IRQ: c.irq, Name: v.Name, Dev: c.dev, CPU: cpu, Count: c.row.total, Why: why})
			}
			continue
		}
		v.Count = c.row.total
		cpu := effectiveCPU(c.irq, src)
		if cpu < 0 {
			out.Unknown++
			continue
		}
		v.CPU = cpu
		out.Vectors = append(out.Vectors, v)
	}
	if len(out.Vectors) == 0 {
		switch {
		case out.Unknown > 0:
			out.Reason = fmt.Sprintf("%d interrupt vectors of %s, none with a single servicing CPU", out.Unknown, strings.Join(devs, ","))
		case out.Idle+out.Negligible > 0:
			out.Reason = fmt.Sprintf("%d interrupt vectors of %s, none reaches the share/floor (%d without an interrupt since boot, %d under the limits)",
				out.Idle+out.Negligible, strings.Join(devs, ","), out.Idle, out.Negligible)
		default:
			out.Reason = "no interrupt vectors found for " + strings.Join(devs, ",")
		}
	}
	return out
}

// negligible says why a vector with n interrupts since boot, out of total
// for all the vectors of the listener's NICs, does not carry traffic; empty
// when it does: n >= nicMinInterrupts and n >= nicMinSharePercent of total.
func negligible(n, total uint64) string {
	if n < nicMinInterrupts {
		return fmt.Sprintf("%d interrupts since boot, under the floor of %d", n, nicMinInterrupts)
	}
	if !atLeastPercent(n, total, nicMinSharePercent) {
		return fmt.Sprintf("%.4g%% of the %d interrupts of the NICs' vectors since boot, under %d%%",
			100*float64(n)/float64(total), total, nicMinSharePercent)
	}
	return ""
}
