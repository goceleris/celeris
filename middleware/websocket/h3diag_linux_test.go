//go:build linux

package websocket

// Lane B-2b H3 DIAGNOSTIC (throwaway branch tmp/b2b-h3-diag; never a PR). CELERIS_B2B_NODRAIN=1
// turns the oracle client's drain between write slices off (the pre-rework client never read while
// it waited), so the lost-window-update give-ups (H3) come back WITH the new timeline. Every
// connection also prints one H3SEG line: both sockets' segment statistics at the end of the flood,
// and each subtest prints the kernel's TcpExt deltas (H3NETSTAT).

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
)

var h3NoDrain = os.Getenv("CELERIS_B2B_NODRAIN") == "1"

func h3Netstat() map[string]int64 {
	out := map[string]int64{}
	b, err := os.ReadFile("/proc/net/netstat")
	if err != nil {
		return out
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	for i := 0; i+1 < len(lines); i += 2 {
		h, v := strings.Fields(lines[i]), strings.Fields(lines[i+1])
		if len(h) != len(v) || len(h) == 0 || h[0] != "TcpExt:" {
			continue
		}
		for j := 1; j < len(h); j++ {
			n, _ := strconv.ParseInt(v[j], 10, 64)
			out[h[j]] = n
		}
	}
	return out
}

func h3NetstatDelta(t *testing.T, kind string, before map[string]int64) {
	after := h3Netstat()
	keys := []string{"TCPRcvQDrop", "PruneCalled", "RcvPruned", "OfoPruned", "TCPRcvCollapsed", "BeyondWindow",
		"TCPWantZeroWindowAdv", "TCPToZeroWindowAdv", "TCPFromZeroWindowAdv", "TCPZeroWindowDrop", "TCPWinProbe",
		"TCPACKSkippedSeq", "TCPTimeouts", "TCPOFOQueue", "TCPOFODrop", "TCPBacklogDrop", "TCPRcvCoalesce", "TCPAutoCorking"}
	var parts []string
	for _, k := range keys {
		if _, ok := after[k]; ok {
			parts = append(parts, fmt.Sprintf("%s=%d", k, after[k]-before[k]))
		}
	}
	t.Logf("H3NETSTAT kind=%s %s", kind, strings.Join(parts, " "))
}

// h3Seg prints both sockets' segment statistics for one connection.
func (cl *wsoCli) h3Seg(t *testing.T, c net.Conn, kind, when string) {
	ti, inq, outq := wsoCliSockInfo(c)
	v := cl.g.srvView(cl.port)
	var b strings.Builder
	fmt.Fprintf(&b, "H3SEG kind=%s when=%s port=%d", kind, when, cl.port)
	if ti != nil {
		fmt.Fprintf(&b, " cli{bytes_received=%d data_segs_in=%d avgIn=%.0f rcv_ssthresh=%d rcv_space=%d rcv_wnd=%d snd_wnd=%d inq=%d outq=%d rcv_mss=%d}",
			ti.Bytes_received, ti.Data_segs_in, float64(ti.Bytes_received)/float64(max(1, int(ti.Data_segs_in))), ti.Rcv_ssthresh, ti.Rcv_space, ti.Rcv_wnd, ti.Snd_wnd, inq, outq, ti.Rcv_mss)
	}
	if v.ti != nil {
		fmt.Fprintf(&b, " srv{bytes_sent=%d data_segs_out=%d avgOut=%.0f snd_mss=%d total_retrans=%d bytes_retrans=%d snd_cwnd=%d rwnd_limited_ms=%d sndbuf_limited_ms=%d pauses=%d resumes=%d frames=%d}",
			v.ti.Bytes_sent, v.ti.Data_segs_out, float64(v.ti.Bytes_sent)/float64(max(1, int(v.ti.Data_segs_out))), v.ti.Snd_mss, v.ti.Total_retrans, v.ti.Bytes_retrans, v.ti.Snd_cwnd,
			v.ti.Rwnd_limited/1000, v.ti.Sndbuf_limited/1000, v.pauses, v.resumes, v.frames)
	}
	t.Log(b.String())
}
