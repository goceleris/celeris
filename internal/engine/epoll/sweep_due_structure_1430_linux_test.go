//go:build linux

package epoll

// celeris#1430, part S: the structural guard.
//
// The loop skips a walk of its connections until the earliest time anything
// could need action. That is only as good as the list of clocks the walk knows:
// a deadline or a clock that checkTimeouts (or what it calls) reads, and that
// the next-due computation does not account for, is a connection that can be
// closed late with nothing to say so. The sweep reads dozens of stamps, and a
// stamp added later is exactly the case that goes unnoticed.
//
// So this test parses, with go/parser and no type information, the functions of
// the sweep (checkTimeouts, the HTTP/2 sweep, the write clock, the closing and
// detached-stall samples, the snapshot of a connection's HTTP/1 deadlines, and
// the transplant sweep of sweep.go), finds every field those functions read that
// is a clock (every int64, atomic.Int64, time.Time or time.Duration field of
// connState, Loop, h1DeadlineSnapshot and conn.H1State), and fails when one is
// not named in sweepClocks1430 below, with the way the next-due computation
// accounts for it. It also fails when a name in the table is read by none of
// the functions (a stale entry), and when the way a clock is said to be
// accounted for is not in the code:
//
//   - "walk": the walk reports the clock's own deadline, as an argument of
//     min(due, ...) inside checkTimeouts;
//   - "rearm": the walk cannot date what the clock will decide (a sampled clock,
//     a state the sweep acts on, one it cannot read); the branch that reads it
//     reports "look again soon" (min(due, rearm));
//   - "hook": the clock is started between two walks by the functions named,
//     and each of them lowers the next-due time (lowerNextDue);
//   - "pin": the clock is set or moved off the loop with no event for it, or is
//     the HTTP/2 layer's own: a loop that holds such a connection is not skipped
//     (sweepDue names what pins it);
//   - "copy": the clock is read only to fill a field of the snapshot, which carries
//     the class;
//   - "own": the clock belongs to a sweep with its own pace (the transplant
//     drain's sweepNext, applied to the wait by sweepWaitMs), or is the sweep's
//     own stamp.
//
// And every branch of the walk (an if in the loop over the connections) that
// reads a "walk" or "rearm" clock must report to due.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"sort"
	"strings"
	"testing"
)

type clockSite1430 struct {
	class string
	hooks []string // class "hook": the functions that start the clock between walks
	via   string   // class "copy": the field the value is copied into, which carries the class
	why   string
}

var sweepClocks1430 = map[string]clockSite1430{
	"detectDeadline":   {class: "walk", why: "accept + ReadHeaderTimeout of a connection whose protocol is not decided; also the h2 owed stamp (noteH2Owed), which the exempt branch reports as rearm"},
	"HeaderDeadlineNs": {class: "copy", via: "hdrDL", why: "the header deadline, copied into the snapshot (hdrDL) by snapshotH1Deadlines"},
	"hdrDL":            {class: "walk", why: "the snapshot of HeaderDeadlineNs"},
	"lastActivity":     {class: "walk", why: "last read + min(IdleTimeout, ReadTimeout)"},
	"writeSince":       {class: "hook", hooks: []string{"armEpollOut", "handleWritable"}, why: "a parked response is looked at at every tick until it drains; its first look is owed one tick after the park"},
	"closeSince":       {class: "hook", hooks: []string{"markClosing"}, why: "a closing drain (and a detached stall) is sampled at every tick; the first look is owed one tick after it starts"},
	"closeSampled":     {class: "rearm", why: "sampled by the closing drain, the write-rate window and the detached stall: looked at again at the next tick"},
	"drainDeadline":    {class: "rearm", why: "the grace of a detached connection's reap"},
	"idleDL":           {class: "rearm", why: "a detached connection's idle deadline as read at this walk (the loop is pinned by it as well)"},
	"IdleDeadlineNs":   {class: "pin", via: "idleDL", why: "set by the middleware's goroutine with no event for the loop: a loop that holds a detached connection is not skipped"},
	"nextDue":          {class: "own", why: "the next-due time itself, stored by the walk"},
	"lastSweep":        {class: "own", why: "the sweep's own stamp: the gate's floor and the stepped-clock rule"},
	"sweepNext":        {class: "own", why: "the transplant drain's own pace (sweepWaitMs caps the wait at it)"},
	"sweepIvl":         {class: "own", why: "the transplant drain's pace"},
}

// sweepFuncs1430 are the functions whose reads are scanned, by file.
var sweepFuncs1430 = map[string][]string{
	"loop.go": {"checkTimeouts", "sweepH2Deadlines", "snapshotH1Deadlines", "sweepWriteClock", "detachedStalled",
		"endDetachedStall", "h2ServerOwes", "noteClosingAcked", "closingDrainBound", "timeoutsDue"},
	"write_rate_885.go": {"parkedBelowRate"},
	"conn.go":           {"noteH2Owed", "h2OwedAt", "clearH2Owed"},
	"sweep.go":          {"sweep", "sweepWaitMs"},
}

// sweepCallees1430 is every same-package function the sweep's roots call, with
// either "scanned" (it is in sweepFuncs1430) or the reason it reads no clock the
// next-due computation has to know.
var sweepCallees1430 = map[string]string{
	"snapshotH1Deadlines": "scanned", "sweepH2Deadlines": "scanned", "sweepWriteClock": "scanned",
	"detachedStalled": "scanned", "endDetachedStall": "scanned", "h2ServerOwes": "scanned",
	"noteClosingAcked": "scanned", "closingDrainBound": "scanned", "timeoutsDue": "scanned",
	"parkedBelowRate": "scanned", "noteH2Owed": "scanned", "h2OwedAt": "scanned", "clearH2Owed": "scanned",
	"closeConn":        "acts on what the walk decided; the connection is gone",
	"closeWhenFlushed": "acts on a connection the HTTP/2 sweep closes",
	"flushH2Conn":      "writes; reads no clock",
	"holdsOutput":      "queue state, no clock",
	"bytesAcked":       "a getsockopt",
	"Sweep":            "H2State.Sweep: the HTTP/2 layer's clocks, pinned (sweepDue)",
	"ExtendIdle":       "H2State: pinned",
	"GoAwayNoWait":     "H2State: pinned",
	"Load":             "atomic load",
	"Unlock":           "mutex",
	"TryLock":          "mutex",
	"Usable":           "tcpack",
	"Now":              "the clock",
	"len":              "builtin",
	"int64":            "conversion",
	"Duration":         "conversion",
	"csWritePending":   "queue state, no clock",
	"sweepHorizon":     "the safety cap, made of the configured timeouts",
	"idleBound":        "min(IdleTimeout, ReadTimeout)",
	"min":              "builtin",
}

func parseSweepFiles1430(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, name := range []string{"loop.go", "write_rate_885.go", "conn.go", "sweep.go", "sweep_due_1430.go", "../../conn/h1.go"} {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files[name] = f
	}
	return fset, files
}

func findFunc1430(files map[string]*ast.File, name string) *ast.FuncDecl {
	for _, f := range files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == name && fd.Body != nil {
				return fd
			}
		}
	}
	return nil
}

func typeName1430(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return typeName1430(x.X) + "." + x.Sel.Name
	}
	return ""
}

// clockFields1430 is every field of the structs the sweep reads that is a
// clock by its type.
func clockFields1430(files map[string]*ast.File) map[string]bool {
	out := map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, sp := range g.Specs {
				ts, ok := sp.(*ast.TypeSpec)
				if !ok {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok || !slices.Contains([]string{"connState", "Loop", "h1DeadlineSnapshot", "H1State"}, ts.Name.Name) {
					continue
				}
				for _, fl := range st.Fields.List {
					switch typeName1430(fl.Type) {
					case "int64", "atomic.Int64", "time.Time", "time.Duration":
						for _, n := range fl.Names {
							out[n.Name] = true
						}
					}
				}
			}
		}
	}
	return out
}

// fieldReads1430 is the names selected, not called, in fn's body.
func fieldReads1430(fn *ast.FuncDecl) map[string]bool {
	called := map[*ast.SelectorExpr]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if se, ok := c.Fun.(*ast.SelectorExpr); ok {
				called[se] = true
			}
		}
		return true
	})
	out := map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if se, ok := n.(*ast.SelectorExpr); ok && !called[se] {
			out[se.Sel.Name] = true
		}
		return true
	})
	return out
}

// calls1430 is the names of the functions and methods fn calls.
func calls1430(fn *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch f := c.Fun.(type) {
		case *ast.Ident:
			out[f.Name] = true
		case *ast.SelectorExpr:
			out[f.Sel.Name] = true
		}
		return true
	})
	return out
}

// dueArgs1430 returns, for every min(due, X) call in body, the field names X
// selects, following the identifiers X uses back to the selectors they were
// assigned from in fn, and the number of min(due, rearm) calls.
func dueArgs1430(fn *ast.FuncDecl) (fields map[string]bool, rearms int) {
	idents := map[string]map[string]bool{}
	collect := func(rhs []ast.Expr, lhs []ast.Expr) {
		sel := map[string]bool{}
		for _, r := range rhs {
			ast.Inspect(r, func(n ast.Node) bool {
				if se, ok := n.(*ast.SelectorExpr); ok {
					sel[se.Sel.Name] = true
				}
				return true
			})
		}
		for _, l := range lhs {
			if id, ok := l.(*ast.Ident); ok {
				if idents[id.Name] == nil {
					idents[id.Name] = map[string]bool{}
				}
				for k := range sel {
					idents[id.Name][k] = true
				}
			}
		}
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if as, ok := n.(*ast.AssignStmt); ok {
			collect(as.Rhs, as.Lhs)
		}
		return true
	})
	fields = map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := c.Fun.(*ast.Ident); !ok || id.Name != "min" || len(c.Args) < 2 {
			return true
		}
		if a, ok := c.Args[0].(*ast.Ident); !ok || a.Name != "due" {
			return true
		}
		if id, ok := c.Args[1].(*ast.Ident); ok && id.Name == "rearm" {
			rearms++
			return true
		}
		ast.Inspect(c.Args[1], func(m ast.Node) bool {
			switch x := m.(type) {
			case *ast.SelectorExpr:
				fields[x.Sel.Name] = true
			case *ast.Ident:
				for k := range idents[x.Name] {
					fields[k] = true
				}
			}
			return true
		})
		return true
	})
	return fields, rearms
}

func TestSweepNextDueKnowsEveryClockTheSweepReads1430(t *testing.T) {
	_, files := parseSweepFiles1430(t)
	clocks := clockFields1430(files)
	if len(clocks) < 10 {
		t.Fatalf("PREMISE: only %d clock fields found in connState, Loop, h1DeadlineSnapshot and H1State: the scan is out of date", len(clocks))
	}

	read := map[string][]string{} // clock -> the functions that read it
	for file, names := range sweepFuncs1430 {
		for _, name := range names {
			fn := findFunc1430(files, name)
			if fn == nil {
				t.Errorf("no func %s in %s: the scan is out of date", name, file)
				continue
			}
			for f := range fieldReads1430(fn) {
				if clocks[f] {
					read[f] = append(read[f], name)
				}
			}
		}
	}

	var names []string
	for f := range read {
		names = append(names, f)
	}
	sort.Strings(names)
	for _, f := range names {
		if _, ok := sweepClocks1430[f]; !ok {
			t.Errorf("the sweep reads the clock %q (in %s) and the next-due computation does not name it: add it to sweepClocks1430 with how checkTimeouts reports it (walk, rearm), who lowers the next-due time when it starts (hook), what keeps the loop awake for it (pin), or why it needs none (own)",
				f, strings.Join(read[f], ", "))
		}
	}
	for f, site := range sweepClocks1430 {
		if len(read[f]) == 0 {
			t.Errorf("sweepClocks1430 names %q (%s) and no scanned function reads it: a stale entry", f, site.class)
		}
	}

	// Each way of accounting for a clock must be in the code.
	ct := findFunc1430(files, "checkTimeouts")
	if ct == nil {
		t.Fatal("no checkTimeouts")
	}
	due, rearms := dueArgs1430(ct)
	sd := findFunc1430(files, "sweepDue")
	if sd == nil {
		t.Fatal("no sweepDue: the loop has no 'is a walk due' test, so every walk is made at every tick")
	}
	pins := fieldReads1430(sd)
	for f, site := range sweepClocks1430 {
		switch site.class {
		case "walk":
			if !due[f] {
				t.Errorf("clock %q is said to be reported by the walk, and no min(due, ...) in checkTimeouts reads it", f)
			}
		case "rearm":
			if rearms == 0 {
				t.Errorf("clock %q is said to be reported as rearm, and checkTimeouts has no min(due, rearm)", f)
			}
		case "hook":
			for _, h := range site.hooks {
				fn := findFunc1430(files, h)
				if fn == nil {
					t.Errorf("clock %q: no func %s", f, h)
					continue
				}
				if !calls1430(fn)["lowerNextDue"] {
					t.Errorf("clock %q starts in %s, which does not call lowerNextDue: the next walk may be a second away when the clock needs its first look", f, h)
				}
			}
		case "pin":
		case "copy":
			if _, ok := sweepClocks1430[site.via]; !ok {
				t.Errorf("clock %q is copied into %q, which is not in the table", f, site.via)
			}
		case "own":
		default:
			t.Errorf("clock %q has the class %q, which is none of walk, rearm, hook, pin, copy, own", f, site.class)
		}
	}
	// What pins a loop: HTTP/2 connections (their clocks are the stream layer's,
	// set off the loop's events), detached ones (the middleware stores their idle
	// deadline), a loop with nothing to walk (nothing to skip), a dirty list.
	for _, want := range []string{"h2Conns", "detachedCount", "liveConns", "dirtyHead"} {
		if !pins[want] {
			t.Errorf("sweepDue does not read %s: a loop in that state must not skip its walks", want)
		}
	}
	if !pins["nextDue"] {
		t.Error("sweepDue does not read nextDue")
	}
	if !fieldReads1430(ct)["nextDue"] {
		t.Error("checkTimeouts does not store nextDue: it has nothing to report")
	}

	// Every branch of the walk that reads a clock reports to due.
	var walk *ast.ForStmt
	ast.Inspect(ct.Body, func(n ast.Node) bool {
		if f, ok := n.(*ast.ForStmt); ok && walk == nil {
			walk = f
		}
		return true
	})
	if walk == nil {
		t.Fatal("checkTimeouts has no loop over the connections")
	}
	for _, st := range walk.Body.List {
		ifs, ok := st.(*ast.IfStmt)
		if !ok {
			continue
		}
		reads := map[string]bool{}
		ast.Inspect(ifs, func(n ast.Node) bool {
			if se, ok := n.(*ast.SelectorExpr); ok && clocks[se.Sel.Name] {
				reads[se.Sel.Name] = true
			}
			return true
		})
		var clocksRead []string
		for f := range reads {
			if c := sweepClocks1430[f].class; c == "walk" || c == "rearm" || c == "hook" {
				clocksRead = append(clocksRead, f)
			}
		}
		if len(clocksRead) == 0 {
			continue
		}
		reports := false
		ast.Inspect(ifs, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "min" && len(c.Args) > 0 {
					if a, ok := c.Args[0].(*ast.Ident); ok && a.Name == "due" {
						reports = true
					}
				}
			}
			return true
		})
		if !reports {
			sort.Strings(clocksRead)
			t.Errorf("a branch of the walk reads %v and does not report to due (min(due, ...)): a connection in that state would be looked at on the 1 s cap alone", clocksRead)
		}
	}
}

// TestSweepCalleesAreAllReviewed1430: a function the sweep starts to call is a
// place a clock can be read that the scan above does not see. Every callee of
// checkTimeouts, sweepH2Deadlines and sweepWriteClock is either scanned or in the
// table with the reason it reads no clock.
func TestSweepCalleesAreAllReviewed1430(t *testing.T) {
	_, files := parseSweepFiles1430(t)
	seen := map[string]bool{}
	for _, root := range []string{"checkTimeouts", "sweepH2Deadlines", "sweepWriteClock", "detachedStalled"} {
		fn := findFunc1430(files, root)
		if fn == nil {
			t.Fatalf("no func %s", root)
		}
		for c := range calls1430(fn) {
			seen[c] = true
		}
	}
	var stale []string
	for c := range sweepCallees1430 {
		if !seen[c] {
			stale = append(stale, c)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("sweepCallees1430 lists %v, which the sweep does not call: stale entries", stale)
	}
	var unknown []string
	for c := range seen {
		if _, ok := sweepCallees1430[c]; !ok {
			unknown = append(unknown, c)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		t.Errorf("the sweep calls %v, which sweepCallees1430 does not list: read what each does with a clock, scan it (sweepFuncs1430) or give the reason it needs none", unknown)
	}
}
