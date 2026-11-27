//go:build linux

package iouring

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// celeris#1430, the guard of the list. The sweep computes when its next look at a
// deadline is due (Worker.dueAt), and a deadline that the sweep reads but that
// the computation does not know is one the loop does not wake for: it is met at
// the next sweep of the period (about a second) instead of when it falls due.
// This test parses the sweeps and fails when a clock they read is not named in
// the arguments of a dueAt call of the same group, unless the group lists it
// here with the reason it needs none; and when a site that arms a deadline (a
// connection accepted or adopted, a send started, a close deferred, a header
// timer armed, an HTTP/2 frame taken) does not lower the next look.

// clockName1430 says which fields are clocks: the connection's and the worker's
// own stamps, deadlines and sample times, by name. The durations of the
// configuration (ReadTimeout and the others) are the computation's inputs, not
// clocks the sweep reads.
var clockName1430 = regexp.MustCompile(`(?i)(deadline|dl$|activity|sampled|since|owedat|look|sweepnext|until)`)

// group1430 is a set of functions of one file that make up one look of the
// sweep. exempt names the clocks the group reads and the next-look computation
// does not take, each with the reason.
type group1430 struct {
	file   string
	funcs  []string
	exempt map[string]string
	// mustCall are selector names (method or function) that one of the funcs must
	// call, inside the argument of a dueAt call.
	mustCall []string
}

var groups1430 = []group1430{
	{
		file:  "worker.go",
		funcs: []string{"checkTimeouts"},
	},
	{
		file:     "worker.go",
		funcs:    []string{"sweepH2Deadlines"},
		mustCall: []string{"NextDeadline"},
	},
	{
		// The stall look of a detached connection: the look is taken when
		// stallLookDue says (lastStallLook plus the floor), and what the look reads
		// of the connection (when the stall began, when the peer's progress was last
		// sampled) is decided at that look, so the next look's time is the one the
		// computation needs.
		file:  "worker.go",
		funcs: []string{"sweepDetachedStall", "detachedStalled", "sampleDetachedAcked", "stallLookDue"},
		exempt: map[string]string{
			"stallSince":   "decided at the look; the computation names the look (lastStallLook plus the floor)",
			"closeSampled": "decided at the look; the computation names the look (lastStallLook plus the floor)",
		},
	},
	{
		// The drain sweep of a transplant (celeris#657) has its own cap on the ring
		// wait (sweepWait, which sweptTimeout applies), and is not a deadline of a
		// connection.
		file:  "sweep.go",
		funcs: []string{"sweep"},
		exempt: map[string]string{
			"sweepNext": "the drain sweep's own cap of the ring wait (sweepWait), not a connection's deadline",
		},
	},
}

// armSites1430 are the functions that start or move a deadline earlier than the
// sweep's last computation could know: each must lower the next look.
var armSites1430 = []struct {
	file, fn, why string
}{
	{"worker.go", "onAcceptedFD", "a connection accepted: its idle and read clock starts"},
	{"transplant.go", "attachAdoptedFD", "a connection adopted: its idle and read clock starts"},
	{"worker.go", "flushSend", "a send started: the write clock starts at the stamp it takes"},
	{"worker.go", "closeConn", "a close deferred behind the SENDs in flight: the drain's bound starts"},
	{"worker.go", "closeStalled", "a stalled stream closed: the drain's bound is spent, the next look reaps it"},
	{"worker.go", "closeRefused", "an HTTP/2 conn closed for output that did not go out: the next look reaps it"},
	{"worker.go", "armHeaderTimerAt", "a header deadline armed: the sweep is the kernel timer's fallback"},
	{"worker.go", "handleRecv", "an HTTP/2 frame taken: a request, header block or write window deadline may have been armed"},
	{"worker.go", "switchToH2Local", "the first frames of an h2c upgrade taken"},
}

func parseFuncs1430(t *testing.T, file string) map[string]*ast.FuncDecl {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	out := map[string]*ast.FuncDecl{}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
			out[fd.Name.Name] = fd
		}
	}
	return out
}

// clockReads1430 returns the names of the clock fields fd reads: the selectors
// whose name says clock, except where they are only assigned to or are the name
// of a method being called.
func clockReads1430(fd *ast.FuncDecl) map[string]bool {
	reads := map[string]bool{}
	var visit func(n ast.Node) bool
	visit = func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if n.Tok == token.ASSIGN || n.Tok == token.DEFINE {
				for _, l := range n.Lhs {
					if se, ok := l.(*ast.SelectorExpr); ok {
						ast.Inspect(se.X, visit) // cs.x = ...: x is written, cs is read
					} else {
						ast.Inspect(l, visit)
					}
				}
				for _, r := range n.Rhs {
					ast.Inspect(r, visit)
				}
				return false
			}
		case *ast.CallExpr:
			// w.method(...): the method's name is not a field read.
			if se, ok := n.Fun.(*ast.SelectorExpr); ok {
				ast.Inspect(se.X, visit)
				for _, a := range n.Args {
					ast.Inspect(a, visit)
				}
				return false
			}
		case *ast.SelectorExpr:
			if clockName1430.MatchString(n.Sel.Name) {
				reads[n.Sel.Name] = true
			}
		}
		return true
	}
	ast.Inspect(fd.Body, visit)
	return reads
}

// dueArgs1430 returns the names (selectors and identifiers) inside the arguments
// of the dueAt calls of fd.
func dueArgs1430(fd *ast.FuncDecl) map[string]bool {
	names := map[string]bool{}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if !calls1430(call, "dueAt") {
			return true
		}
		for _, a := range call.Args {
			ast.Inspect(a, func(m ast.Node) bool {
				switch m := m.(type) {
				case *ast.SelectorExpr:
					names[m.Sel.Name] = true
				case *ast.Ident:
					names[m.Name] = true
				}
				return true
			})
		}
		return false
	})
	return names
}

func calls1430(call *ast.CallExpr, name string) bool {
	switch f := call.Fun.(type) {
	case *ast.SelectorExpr:
		return f.Sel.Name == name
	case *ast.Ident:
		return f.Name == name
	}
	return false
}

// missing1430 returns the clock reads of the group's functions that no dueAt
// argument of the group names and that the group does not exempt, and the
// exemptions that name a clock the group does not read or that the computation
// takes after all.
func missing1430(funcs map[string]*ast.FuncDecl, g group1430) (unknown, stale, noCall []string) {
	reads, due := map[string]bool{}, map[string]bool{}
	for _, name := range g.funcs {
		fd := funcs[name]
		if fd == nil {
			unknown = append(unknown, "function "+name+" not found in "+g.file)
			continue
		}
		for r := range clockReads1430(fd) {
			reads[r] = true
		}
		for d := range dueArgs1430(fd) {
			due[d] = true
		}
	}
	for r := range reads {
		if _, ok := g.exempt[r]; !due[r] && !ok {
			unknown = append(unknown, r)
		}
	}
	for e := range g.exempt {
		switch {
		case !reads[e]:
			stale = append(stale, e+" (not read by the group any more)")
		case due[e]:
			stale = append(stale, e+" (the computation names it: drop the exemption)")
		}
	}
	for _, m := range g.mustCall {
		found := false
		for _, name := range g.funcs {
			if fd := funcs[name]; fd != nil && dueArgs1430(fd)[m] {
				found = true
			}
		}
		if !found {
			noCall = append(noCall, m)
		}
	}
	sort.Strings(unknown)
	sort.Strings(stale)
	return
}

func TestSweepClocksAreInTheNextLookComputation1430(t *testing.T) {
	files := map[string]map[string]*ast.FuncDecl{}
	for _, g := range groups1430 {
		if files[g.file] == nil {
			files[g.file] = parseFuncs1430(t, g.file)
		}
		unknown, stale, noCall := missing1430(files[g.file], g)
		if len(unknown) > 0 {
			t.Errorf("%s %v read these clocks and the next-look computation (a dueAt call with the clock in its argument) does not "+
				"name them, and the group does not exempt them with a reason: %s. A deadline the sweep reads and the computation does "+
				"not know is met at the next sweep of the period, not when it falls due",
				g.file, g.funcs, strings.Join(unknown, ", "))
		}
		if len(stale) > 0 {
			t.Errorf("%s %v: stale exemptions: %s", g.file, g.funcs, strings.Join(stale, ", "))
		}
		if len(noCall) > 0 {
			t.Errorf("%s %v must lower the next look through %s (a dueAt call whose argument calls it)", g.file, g.funcs, strings.Join(noCall, ", "))
		}
	}
}

// TestDeadlineArmSitesLowerTheNextLook1430: each site that starts a deadline the
// last sweep could not have known calls dueAt, directly or through a helper
// that is named here by the call it makes.
func TestDeadlineArmSitesLowerTheNextLook1430(t *testing.T) {
	files := map[string]map[string]*ast.FuncDecl{}
	for _, s := range armSites1430 {
		if files[s.file] == nil {
			files[s.file] = parseFuncs1430(t, s.file)
		}
		fd := files[s.file][s.fn]
		if fd == nil {
			t.Errorf("%s: no function %s (%s)", s.file, s.fn, s.why)
			continue
		}
		found := false
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && (calls1430(call, "dueAt") || calls1430(call, "dueAfterIdle") || calls1430(call, "dueH2")) {
				found = true
			}
			return !found
		})
		if !found {
			t.Errorf("%s %s does not lower the next look (dueAt): %s", s.file, s.fn, s.why)
		}
	}
	// The wait is capped at the next look, and the standing timer too.
	funcs := parseFuncs1430(t, "worker.go")
	if fd := funcs["sweptTimeout"]; fd == nil {
		t.Error("worker.go: no function sweptTimeout")
	} else if !callsAny1430(fd, "dueWait") {
		t.Error("sweptTimeout does not cap the ring wait at the next look (dueWait)")
	}
	gate := parseFuncs1430(t, "sweep_gate_884.go")
	if fd := gate["sweepDue"]; fd == nil {
		t.Error("sweep_gate_884.go: no function sweepDue")
	} else if !mentions1430(fd, "nextDue") {
		t.Error("sweepDue does not ask whether the next look is due (nextDue)")
	}
	if fd := gate["armSweepTick"]; fd == nil {
		t.Error("sweep_gate_884.go: no function armSweepTick")
	} else if !callsAny1430(fd, "dueWait") {
		t.Error("armSweepTick does not arm the standing timer at the next look (dueWait)")
	}
}

func callsAny1430(fd *ast.FuncDecl, name string) bool {
	found := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && calls1430(call, name) {
			found = true
		}
		return !found
	})
	return found
}

func mentions1430(fd *ast.FuncDecl, name string) bool {
	found := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			found = found || n.Sel.Name == name
		case *ast.Ident:
			found = found || n.Name == name
		}
		return !found
	})
	return found
}

// TestStructuralGuardSeesAMissingClock1430 is the guard's own test: on a sweep
// that reads a clock its computation does not name, it reports it; once named
// or exempted it does not; a clock that is only assigned is not a read.
func TestStructuralGuardSeesAMissingClock1430(t *testing.T) {
	const src = `package p
func sweep(w *W, cs *C, now int64) {
	cs.lastActivity = now
	if now > cs.detectDeadline {
		return
	}
	if cs.otherClock > 0 && now-cs.sampledAt > 5 {
		w.dueAt(cs.lastActivity+3, now)
	}
}
`
	f, err := parser.ParseFile(token.NewFileSet(), "p.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	funcs := map[string]*ast.FuncDecl{}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok {
			funcs[fd.Name.Name] = fd
		}
	}
	g := group1430{file: "p.go", funcs: []string{"sweep"}}
	unknown, stale, _ := missing1430(funcs, g)
	if got := strings.Join(unknown, ","); got != "detectDeadline,sampledAt" {
		t.Errorf("unknown = %q, want detectDeadline,sampledAt (lastActivity is named by the dueAt call, otherClock is not a clock)", got)
	}
	if len(stale) != 0 {
		t.Errorf("stale = %v, want none", stale)
	}
	g.exempt = map[string]string{"detectDeadline": "r", "sampledAt": "r", "lastActivity": "r", "neverRead": "r"}
	unknown, stale, _ = missing1430(funcs, g)
	if len(unknown) != 0 {
		t.Errorf("unknown = %v with every read exempted, want none", unknown)
	}
	if got := strings.Join(stale, ";"); !strings.Contains(got, "neverRead") || !strings.Contains(got, "lastActivity") {
		t.Errorf("stale = %q, want neverRead (not read) and lastActivity (named by dueAt)", got)
	}
}
