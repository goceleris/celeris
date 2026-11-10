package resid

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Tag is the marker of a line of the export, beside `[sched]`.
const Tag = "[res]"

// Line formats s as the `[res]` line, cumulative since start, one line:
//
//	[res] <unix> n=<N> unst=<U> part=<P> nowr=<W> early=<E> inproc=<I> loops=<n0,n1,..|-> R_n=<N> R_sum_ns=<ns> R=<lo_ns>:<count>,.. G_n=.. G_sum_ns=.. G=.. S1_.. S2_.. S3_.. B_.. Wt_..
//
// It follows `[sched]`'s shape (key=value fields, one `lo:count` list of the
// non-empty buckets per histogram, `-` for an empty list) so the probatorium
// reader can slice it by window the same way; the caller prints it with the
// standard log prefix (log.Printf("%s", line)), which is what `[sched]` has.
// A histogram's lower bounds are integer nanoseconds (BucketLo), not seconds as
// in `[sched]`, so that Go and Python agree exactly; `X_n` is the sum of the
// counts of X (weighted by the requests a sampled recv carried) and `X_sum_ns`
// the sum of value*weight, so the mean is X_sum_ns/X_n. A pass is the
// difference of the lines at its window's edges.
func (s Snap) Line(unix int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %d n=%d unst=%d part=%d nowr=%d early=%d inproc=%d loops=", Tag, unix, s.N, s.Unst, s.Part, s.NoWr, s.Early, s.InProc)
	if len(s.Loops) == 0 {
		b.WriteByte('-')
	}
	for i, n := range s.Loops {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatUint(n, 10))
	}
	for i := range s.H {
		h := &s.H[i]
		name := HistNames[i]
		fmt.Fprintf(&b, " %s_n=%d %s_sum_ns=%d %s=", name, h.N, name, h.SumNS, name)
		first := true
		for j, c := range h.B {
			if c == 0 {
				continue
			}
			if !first {
				b.WriteByte(',')
			}
			first = false
			b.WriteString(strconv.FormatInt(BucketLo(j), 10))
			b.WriteByte(':')
			b.WriteString(strconv.FormatUint(c, 10))
		}
		if first {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// LineNow is Line at the wall clock's second.
func (s Snap) LineNow() string { return s.Line(time.Now().Unix()) }

// Parse reads a `[res]` line back (an optional log prefix before the tag is
// skipped). A line that does not parse in full is an error.
func Parse(line string) (unix int64, s Snap, err error) {
	i := strings.Index(line, Tag+" ")
	if i < 0 {
		return 0, s, errors.New("resid: no [res] tag")
	}
	f := strings.Fields(line[i+len(Tag)+1:])
	if len(f) == 0 {
		return 0, s, errors.New("resid: empty line")
	}
	if unix, err = strconv.ParseInt(f[0], 10, 64); err != nil {
		return 0, s, fmt.Errorf("resid: unix: %w", err)
	}
	kv := make(map[string]string, len(f))
	for _, p := range f[1:] {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			return 0, s, fmt.Errorf("resid: field %q has no '='", p)
		}
		kv[k] = v
	}
	u := func(k string) (uint64, error) {
		v, ok := kv[k]
		if !ok {
			return 0, fmt.Errorf("resid: missing %s", k)
		}
		return strconv.ParseUint(v, 10, 64)
	}
	for _, d := range []struct {
		k string
		p *uint64
	}{{"n", &s.N}, {"unst", &s.Unst}, {"part", &s.Part}, {"nowr", &s.NoWr}, {"early", &s.Early}, {"inproc", &s.InProc}} {
		if *d.p, err = u(d.k); err != nil {
			return 0, Snap{}, fmt.Errorf("resid: %s: %w", d.k, err)
		}
	}
	lv, ok := kv["loops"]
	if !ok {
		return 0, Snap{}, errors.New("resid: missing loops")
	}
	if lv != "-" {
		for _, p := range strings.Split(lv, ",") {
			n, perr := strconv.ParseUint(p, 10, 64)
			if perr != nil {
				return 0, Snap{}, fmt.Errorf("resid: loops: %w", perr)
			}
			s.Loops = append(s.Loops, n)
		}
	}
	for i := range s.H {
		name := HistNames[i]
		h := &s.H[i]
		if h.N, err = u(name + "_n"); err != nil {
			return 0, Snap{}, fmt.Errorf("resid: %s_n: %w", name, err)
		}
		if h.SumNS, err = u(name + "_sum_ns"); err != nil {
			return 0, Snap{}, fmt.Errorf("resid: %s_sum_ns: %w", name, err)
		}
		hv, ok := kv[name]
		if !ok {
			return 0, Snap{}, fmt.Errorf("resid: missing %s", name)
		}
		var tot uint64
		if hv != "-" {
			for _, p := range strings.Split(hv, ",") {
				lo, cs, ok := strings.Cut(p, ":")
				if !ok {
					return 0, Snap{}, fmt.Errorf("resid: %s bucket %q", name, p)
				}
				l, perr := strconv.ParseInt(lo, 10, 64)
				c, cerr := strconv.ParseUint(cs, 10, 64)
				if perr != nil || cerr != nil {
					return 0, Snap{}, fmt.Errorf("resid: %s bucket %q", name, p)
				}
				j := BucketOf(l)
				if BucketLo(j) != l {
					return 0, Snap{}, fmt.Errorf("resid: %s: %d is not a bucket bound", name, l)
				}
				h.B[j] += c
				tot += c
			}
		}
		if tot != h.N {
			return 0, Snap{}, fmt.Errorf("resid: %s: %d in the buckets, %s_n=%d", name, tot, name, h.N)
		}
	}
	return unix, s, nil
}

// WallNowNS is the wall clock (CLOCK_REALTIME) in ns, the clock of the kernel's
// receive timestamps. It is here, outside the engines, because the engines may
// not read the wall clock (internal/mono's scan).
func WallNowNS() int64 { return time.Now().UnixNano() }

// KernelRxNS finds the SCM_TIMESTAMPNS control message (level, typ) in the
// ancillary data oob of a recvmsg and returns the kernel's receive time in ns
// (CLOCK_REALTIME), or 0 when there is none. It parses in place, without the
// allocation of unix.ParseSocketControlMessage, for the 64-bit linux ABI
// (cmsghdr: len uint64, level int32, type int32, then a timespec of two int64).
func KernelRxNS(oob []byte, level, typ int32) int64 {
	const hdr = 16
	for len(oob) >= hdr {
		l := binary.NativeEndian.Uint64(oob[0:8])
		if l < hdr || l > uint64(len(oob)) {
			return 0
		}
		if int32(binary.NativeEndian.Uint32(oob[8:12])) == level && int32(binary.NativeEndian.Uint32(oob[12:16])) == typ && l >= hdr+16 {
			sec := int64(binary.NativeEndian.Uint64(oob[hdr : hdr+8]))
			nsec := int64(binary.NativeEndian.Uint64(oob[hdr+8 : hdr+16]))
			return sec*1_000_000_000 + nsec
		}
		adv := (l + 7) &^ 7 // CMSG_ALIGN
		if adv >= uint64(len(oob)) {
			return 0
		}
		oob = oob[adv:]
	}
	return 0
}
