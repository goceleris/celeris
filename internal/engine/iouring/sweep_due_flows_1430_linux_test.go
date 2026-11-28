//go:build linux

package iouring

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// celeris#1430, the guard of the list, by flow. sweep_due_sites_1430_linux_test.go
// asks, group by group, that every clock the sweeps read is named by some due*
// call of the group, and that eleven named functions call a due* function. Two
// things get past that. A due* call for one clock satisfies the read of every
// other clock named in the group (one dueIdleFrom(cs.lastActivity) stands in for
// the closing drain's own look), and a new function that starts a clock is not
// among the eleven. This file closes both, for the three clocks that run on the
// worker without a kernel timer of their own, each with its own start sites and
// its own look:
//
//   - the write clock (a send started: WriteTimeout from lastActivity). Started
//     by every statement that sets sending (or zcNotifPending, or puts the conn
//     on the dirty list); looked at by dueWrite, in the sweep's branch for a conn
//     with output.
//   - the closing drain (a close deferred behind the sends in flight: the drain
//     bound from lastActivity, sampled every quarter of it). Started by closeConn
//     (closing = true), and by the two closes that set the clock back; looked at
//     by two dueAt calls in the sweep's branch for a closing conn.
//   - the idle and read clock (ReadTimeout and IdleTimeout from lastActivity),
//     and the budgets that start at accept (detectDeadline, the header timer).
//
// startSites1430 is every place in the package (driver.go excepted: a driver conn
// has no timeout sweep) that sets one of the clock fields. Each must be listed
// here, with the flow it belongs to and what makes the look right: a hook (the
// statement's own block calls the flow's due function), or a reason it needs
// none (it moves the clock later, or runs in the sweep, which computes the look
// again). A function that starts a clock and is not listed fails the test, and so
// does a listed one that no longer sets the field.
//
// What it does not see: the types (a field is matched by name, in every file but
// driver.go), a clock started off the worker's thread (the H2 write window armed
// from a pool goroutine, a detached conn's IdleDeadlineNs: both are left to the
// sweeps' own period, and say so in sweep_gate_884.go), and what a hook's branch
// does on a path that does not reach the call (the SQ-full return of flushSend
// comes before the statement that starts the write clock, so it starts nothing).

// clockStartFields1430 are the connState fields whose assignment starts, moves
// or ends a clock the sweep reads. A bool starts one when it is set to true; the
// others when they are set to anything but 0.
var clockStartFields1430 = map[string]bool{ // true: a bool
	"sending":        true,
	"zcNotifPending": true,
	"dirty":          true,
	"closing":        true,
	"lastActivity":   false,
	"closeSampled":   false,
	"stallSince":     false,
	"detectDeadline": false,
	"h2OwedAt":       false,
	"lastStallLook":  false,
}

// site1430 is one place that sets a clock field.
type site1430 struct {
	fn    string   // the function the statement is in
	field string   // the field it sets
	flow  string   // write, closing, idle, detect, stall
	kind  string   // hook, later, sweep, pinned
	dues  []string // hook: the next-look functions, one of which the statement's own block must call
	args  []string // hook: names the arguments of that call must contain (the flow's own clock)
	why   string
}

var startSites1430 = []site1430{
	// The write clock: a send started.
	{fn: "flushSend", field: "sending", flow: "write", kind: "hook", dues: []string{"dueWrite"},
		why: "a send started: the write clock runs from the stamp it takes"},
	{fn: "flushSend", field: "lastActivity", flow: "write", kind: "hook", dues: []string{"dueWrite"},
		why: "the write clock restarts at the first SEND after a quiet spell (celeris#889)"},
	{fn: "flushSendLink", field: "sending", flow: "write", kind: "hook", dues: []string{"dueWrite"},
		why: "a send linked to a recv: the write clock runs from the last read"},
	{fn: "handleSend", field: "zcNotifPending", flow: "write", kind: "later",
		why: "the notification of a send that flushSend started: the write clock was looked at when that send began and the sweep takes it again"},
	{fn: "completeSend", field: "lastActivity", flow: "write", kind: "later",
		why: "a completed send restamps the clock: it only moves later, and the conn's earlier look recomputes"},
	{fn: "markDirty", field: "dirty", flow: "write", kind: "pinned",
		why: "a conn on the dirty list: baseTimeout is 0 while dirtyHead is set, so the loop's gate sweeps at the floor (checked below)"},

	// The closing drain: a close deferred behind the sends in flight.
	{fn: "closeConn", field: "closing", flow: "closing", kind: "hook", dues: []string{"dueAt"}, args: []string{"lastActivity", "bound"},
		why: "the drain's bound starts here, and its first sample a quarter of it on"},
	{fn: "closeConn", field: "lastActivity", flow: "closing", kind: "hook", dues: []string{"dueAt"}, args: []string{"lastActivity", "bound"},
		why: "the drain's clock is restamped where the drain starts"},
	{fn: "closeStalled", field: "lastActivity", flow: "closing", kind: "hook", dues: []string{"dueAt"}, args: []string{"sweepFloor"},
		why: "the drain's clock is set back past its bound: the next look reaps it"},
	{fn: "closeRefused", field: "lastActivity", flow: "closing", kind: "hook", dues: []string{"dueAt"}, args: []string{"sweepFloor"},
		why: "the drain's clock is set back to run out at the next sweep"},
	{fn: "closeRefused", field: "closeSampled", flow: "closing", kind: "hook", dues: []string{"dueAt"}, args: []string{"sweepFloor"},
		why: "no sample of the peer's progress before the reap: the next look reaps it"},
	{fn: "noteClosingAcked", field: "lastActivity", flow: "closing", kind: "sweep",
		why: "in the sweep: the peer's progress restarts the drain, and the sweep computes the look again"},
	{fn: "noteClosingAcked", field: "closeSampled", flow: "closing", kind: "sweep",
		why: "in the sweep: the sample is taken at the look and the sweep computes the next"},
	{fn: "excuseHold", field: "lastActivity", flow: "all", kind: "later",
		why: "a hold on the loop (a pass that stalled) moves every clock later by the time held"},
	{fn: "excuseHold", field: "stallSince", flow: "stall", kind: "later",
		why: "a hold on the loop moves the stall clock later by the time held"},

	// The idle and read clock, and the budgets that start at accept.
	{fn: "onAcceptedFD", field: "lastActivity", flow: "idle", kind: "hook", dues: []string{"dueIdleFrom"}, args: []string{"now"},
		why: "a connection accepted: its idle and read clock starts"},
	{fn: "attachAdoptedFD", field: "lastActivity", flow: "idle", kind: "hook", dues: []string{"dueIdleFrom"}, args: []string{"lastActivity"},
		why: "a connection adopted: its idle and read clock starts"},
	{fn: "handleRecv", field: "lastActivity", flow: "idle", kind: "later",
		why: "a read restamps the clock: it only moves later, and the conn's earlier look recomputes"},
	{fn: "onAcceptedFD", field: "detectDeadline", flow: "detect", kind: "hook", dues: []string{"armHeaderTimerAt"}, args: []string{"detectDeadline"},
		why: "the detect budget starts at accept: the timer is armed at it, and armHeaderTimerAt lowers the look"},

	// The detached stall, taken at the look the sweep registers (stallLookDue).
	{fn: "completeSend", field: "stallSince", flow: "stall", kind: "later",
		why: "the stall clock of a detached conn restarts when a send completes: a look is the sweep's (stallLookDue)"},
	{fn: "detachedStalled", field: "stallSince", flow: "stall", kind: "sweep", why: "in the sweep: decided at the stall look"},
	{fn: "detachedStalled", field: "closeSampled", flow: "stall", kind: "sweep", why: "in the sweep: decided at the stall look"},
	{fn: "sampleDetachedAcked", field: "stallSince", flow: "stall", kind: "sweep", why: "in the sweep: decided at the stall look"},
	{fn: "sampleDetachedAcked", field: "closeSampled", flow: "stall", kind: "sweep", why: "in the sweep: decided at the stall look"},
	{fn: "stallLookDue", field: "lastStallLook", flow: "stall", kind: "sweep", why: "in the sweep: the stall look's own time"},
	{fn: "checkTimeouts", field: "h2OwedAt", flow: "idle", kind: "sweep", why: "in the sweep: an HTTP/2 conn that owes streams, looked at again by dueIdleFrom(cs.h2OwedAt)"},
}

// found1430 is one statement of the package that sets a clock field.
type found1430 struct {
	file, fn, field string
	block           ast.Node // the innermost block that holds the statement
}

// clockStarts1430 returns the statements of the parsed files that set a clock
// field: for a bool the ones that set it to true, for the others every one that is
// not a plain 0.
func clockStarts1430(files map[string]*ast.File) []found1430 {
	var out []found1430
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, d := range files[name].Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			var stack []ast.Node
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]
					return true
				}
				stack = append(stack, n)
				as, ok := n.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for i, l := range as.Lhs {
					se, ok := l.(*ast.SelectorExpr)
					if !ok {
						continue
					}
					isBool, isClock := clockStartFields1430[se.Sel.Name]
					if !isClock {
						continue
					}
					if len(as.Rhs) == len(as.Lhs) && as.Tok == token.ASSIGN {
						if id, ok := as.Rhs[i].(*ast.Ident); ok && isBool && id.Name == "false" {
							continue // false: not a start (true, or a bool taken from a variable, is)
						}
						if bl, ok := as.Rhs[i].(*ast.BasicLit); ok && bl.Value == "0" {
							continue
						}
					}
					var block ast.Node = fd.Body
					for j := len(stack) - 2; j >= 0; j-- {
						switch b := stack[j].(type) {
						case *ast.BlockStmt:
							block = b
						case *ast.CaseClause:
							block = b
						case *ast.CommClause:
							block = b
						default:
							continue
						}
						break
					}
					out = append(out, found1430{file: name, fn: fd.Name.Name, field: se.Sel.Name, block: block})
				}
				return true
			})
		}
	}
	return out
}

// names1430 are the selector and identifier names inside n.
func names1430(n ast.Node) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(n, func(m ast.Node) bool {
		switch m := m.(type) {
		case *ast.SelectorExpr:
			out[m.Sel.Name] = true
		case *ast.Ident:
			out[m.Name] = true
		}
		return true
	})
	return out
}

func hasAll1430(have map[string]bool, want []string) bool {
	for _, w := range want {
		if !have[w] {
			return false
		}
	}
	return true
}

// callIn1430 reports whether n holds a call to one of names whose arguments
// contain every name in args.
func callIn1430(n ast.Node, names, args []string) bool {
	found := false
	ast.Inspect(n, func(m ast.Node) bool {
		call, ok := m.(*ast.CallExpr)
		if !ok || found {
			return !found
		}
		for _, nm := range names {
			if calls1430(call, nm) {
				have := map[string]bool{}
				for _, a := range call.Args {
					for k := range names1430(a) {
						have[k] = true
					}
				}
				if hasAll1430(have, args) {
					found = true
				}
			}
		}
		return true
	})
	return found
}

// callInBlock1430 is callIn1430 over the statements that are in the block itself:
// a statement that opens a block of its own (an if, a for, a switch) is not
// looked into, because its call is made on another path than the one that
// reaches the statement a hook is for.
func callInBlock1430(block ast.Node, names, args []string) bool {
	var list []ast.Stmt
	switch b := block.(type) {
	case *ast.BlockStmt:
		list = b.List
	case *ast.CaseClause:
		list = b.Body
	case *ast.CommClause:
		list = b.Body
	}
	for _, st := range list {
		switch st.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt, *ast.BlockStmt, *ast.LabeledStmt:
			continue
		}
		if callIn1430(st, names, args) {
			return true
		}
	}
	return false
}

// parsePackage1430 parses the non-test files of the package, the driver conn's
// (driver.go) excepted.
func parsePackage1430(t *testing.T) map[string]*ast.File {
	t.Helper()
	ents, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*ast.File{}
	for _, e := range ents {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") || n == "driver.go" {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), n, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", n, err)
		}
		out[n] = f
	}
	return out
}

// checkStarts1430 compares the statements found with the list. It returns one line
// per problem: a statement that is not listed, a listed one that is not found, and
// a hook whose block does not call its flow's due function.
func checkStarts1430(found []found1430, list []site1430) []string {
	var probs []string
	type key struct{ fn, field string }
	listed := map[key][]site1430{}
	for _, s := range list {
		listed[key{s.fn, s.field}] = append(listed[key{s.fn, s.field}], s)
	}
	seen := map[key]int{}
	for _, f := range found {
		k := key{f.fn, f.field}
		ss := listed[k]
		if len(ss) == 0 {
			probs = append(probs, f.file+" "+f.fn+" sets "+f.field+" and is not in startSites1430: say which clock it starts and what looks at it")
			continue
		}
		seen[k]++
		s := ss[0]
		if s.kind == "hook" && !callInBlock1430(f.block, s.dues, s.args) {
			with := ""
			if len(s.args) > 0 {
				with = " naming " + strings.Join(s.args, ", ")
			}
			probs = append(probs, f.file+" "+f.fn+" sets "+f.field+" ("+s.flow+" clock) and its block does not call "+
				strings.Join(s.dues, " or ")+with+": "+s.why)
		}
	}
	for _, s := range list {
		if seen[key{s.fn, s.field}] == 0 {
			probs = append(probs, "startSites1430 lists "+s.fn+" setting "+s.field+", which no statement does any more")
		}
	}
	return probs
}

func TestEveryStartOfAClockLowersItsOwnLook1430(t *testing.T) {
	files := parsePackage1430(t)
	found := clockStarts1430(files)
	if len(found) < 20 {
		t.Fatalf("the scan found %d statements that set a clock field, want about 30: it no longer sees the package", len(found))
	}
	for _, p := range checkStarts1430(found, startSites1430) {
		t.Error(p)
	}
	// The one premise of a "pinned" entry: a conn on the dirty list makes the
	// loop's wait 0, so the gate sweeps at the floor.
	for _, s := range startSites1430 {
		if s.kind != "pinned" {
			continue
		}
		fd := parseFuncs1430(t, "worker.go")["baseTimeout"]
		if fd == nil || !mentions1430(fd, "dirtyHead") {
			t.Errorf("%s is pinned by the dirty list (baseTimeout 0 while dirtyHead is set), and baseTimeout does not look at dirtyHead", s.fn)
		}
	}
}

// branch1430 is what one flow's own look in the sweep must hold: the branch the
// sweep takes for a conn in that flow's state (an if whose condition names
// cond), and in it a call to one of calls with these names among its arguments.
type branch1430 struct {
	flow  string
	cond  []string
	calls []string
	args  []string
	// outside says the call is looked for outside every branch that names one
	// of these (the idle clock's look is the conn's own, not the closing
	// branch's).
	outside []string
}

var sweepFlows1430 = []branch1430{
	{flow: "closing: the reap", cond: []string{"closing"}, calls: []string{"dueAt"}, args: []string{"lastActivity", "bound"}},
	{flow: "closing: the next sample of the peer's progress", cond: []string{"closing"}, calls: []string{"dueAt"}, args: []string{"closeSampled", "bound"}},
	{flow: "write: a conn with output", cond: []string{"dirty", "sending", "zcNotifPending"}, calls: []string{"dueWrite"}, args: []string{"cs"}},
	{flow: "idle: ReadTimeout and IdleTimeout from the last read", calls: []string{"dueIdleFrom"}, args: []string{"lastActivity"}, outside: []string{"closing"}},
	{flow: "idle: an HTTP/2 conn that owed streams", calls: []string{"dueIdleFrom"}, args: []string{"h2OwedAt"}},
	{flow: "detect: the budget of a conn whose protocol is not known", cond: []string{"detectDeadline"}, calls: []string{"dueAt"}, args: []string{"detectDeadline"}},
	{flow: "header: ReadHeaderTimeout of an HTTP/1 conn", cond: []string{"hdrDL"}, calls: []string{"dueAt"}, args: []string{"hdrDL"}},
	{flow: "detached: the deadline the middleware set", cond: []string{"h1Detached"}, calls: []string{"dueAt"}, args: []string{"idleDL"}},
}

// ifsNaming1430 are the if statements of fd whose condition names every one of
// names.
func ifsNaming1430(fd *ast.FuncDecl, names []string) []*ast.IfStmt {
	var out []*ast.IfStmt
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if is, ok := n.(*ast.IfStmt); ok && hasAll1430(names1430(is.Cond), names) {
			out = append(out, is)
		}
		return true
	})
	return out
}

// checkFlows1430 returns the flows of the list whose look is missing from fd.
func checkFlows1430(fd *ast.FuncDecl, list []branch1430) []string {
	var probs []string
	for _, b := range list {
		ok := false
		switch {
		case len(b.cond) > 0:
			for _, is := range ifsNaming1430(fd, b.cond) {
				if callIn1430(is.Body, b.calls, b.args) {
					ok = true
				}
			}
		case len(b.outside) > 0:
			excluded := ifsNaming1430(fd, b.outside)
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				for _, e := range excluded {
					if n == ast.Node(e.Body) {
						return false
					}
				}
				if call, isCall := n.(*ast.CallExpr); isCall && callIn1430(call, b.calls, b.args) {
					ok = true
				}
				return !ok
			})
		default:
			ok = callIn1430(fd.Body, b.calls, b.args)
		}
		if !ok {
			probs = append(probs, b.flow+": no "+strings.Join(b.calls, " or ")+" naming "+strings.Join(b.args, ", ")+" in its own branch of "+fd.Name.Name)
		}
	}
	return probs
}

func TestSweepHasALookForEachClockInItsOwnBranch1430(t *testing.T) {
	fd := parseFuncs1430(t, "worker.go")["checkTimeouts"]
	if fd == nil {
		t.Fatal("worker.go: no checkTimeouts")
	}
	for _, p := range checkFlows1430(fd, sweepFlows1430) {
		t.Error(p)
	}
	// What each look is made of.
	w := parseFuncs1430(t, "sweep_gate_884.go")
	for _, c := range []struct {
		fn    string
		names []string
	}{
		{"dueWrite", []string{"WriteTimeout", "lastActivity"}},
		{"dueIdleFrom", []string{"ReadTimeout", "IdleTimeout"}},
		{"finishDue", []string{"sweepCeiling"}},
		{"dueTime", []string{"sweepFloor"}},
	} {
		d := w[c.fn]
		if d == nil {
			t.Errorf("sweep_gate_884.go: no function %s", c.fn)
			continue
		}
		if have := names1430(d.Body); !hasAll1430(have, c.names) {
			t.Errorf("%s no longer names %s", c.fn, strings.Join(c.names, " and "))
		}
	}
}

// TestFlowGuardSeesWhatTheGroupGuardMisses1430 is the guard's own test, on source
// it makes up: a closing branch that has lost its own look but whose function
// still has another look naming the same field (what the group guard accepts); a
// new function that starts the closing drain; a hook that calls the wrong look; a
// stale entry.
func TestFlowGuardSeesWhatTheGroupGuardMisses1430(t *testing.T) {
	parse := func(src string) *ast.File {
		f, err := parser.ParseFile(token.NewFileSet(), "p.go", src, 0)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	const sweep = `package p
func checkTimeouts(w *W, cs *C, now int64) {
	w.dueIdleFrom(cs.lastActivity, now)
	if cs.closing {
		w.dueAt(cs.closeSampled+bound/4, now)
	}
	if cs.dirty || cs.sending || cs.zcNotifPending {
		w.dueIdleFrom(cs.lastActivity, now)
	}
}
`
	f := parse(sweep)
	var fd *ast.FuncDecl
	for _, d := range f.Decls {
		fd = d.(*ast.FuncDecl)
	}
	got := strings.Join(checkFlows1430(fd, []branch1430{
		{flow: "closing reap", cond: []string{"closing"}, calls: []string{"dueAt"}, args: []string{"lastActivity", "bound"}},
		{flow: "closing sample", cond: []string{"closing"}, calls: []string{"dueAt"}, args: []string{"closeSampled", "bound"}},
		{flow: "write", cond: []string{"dirty", "sending", "zcNotifPending"}, calls: []string{"dueWrite"}, args: []string{"cs"}},
		{flow: "idle", calls: []string{"dueIdleFrom"}, args: []string{"lastActivity"}, outside: []string{"closing"}},
	}), "\n")
	if !strings.Contains(got, "closing reap") || !strings.Contains(got, "write:") || strings.Contains(got, "closing sample") || strings.Contains(got, "idle:") {
		t.Errorf("checkFlows1430 reported:\n%s\nwant the closing reap and the write look missing, and the closing sample and the idle look found", got)
	}

	const pkg = `package p
func closeConn(w *W, cs *C) {
	if cs.sending {
		cs.closing = true
		cs.lastActivity = w.cachedNow
		w.dueAt(cs.lastActivity+bound+1, w.cachedNow)
	}
}
func flushSend(w *W, cs *C) {
	cs.sending = true
	w.dueIdleFrom(cs.lastActivity, 0)
}
func flushOther(w *W, cs *C) {
	if cs.a {
		cs.sending = true
		w.dueWrite(cs)
	}
	cs.sending = true
}
func newCloser(cs *C, deferred bool) {
	cs.closing = true
	cs.sending = false
	cs.lastActivity = 0
}
func closeFromVariable(cs *C, deferred bool) {
	cs.closing = deferred
}
`
	found := clockStarts1430(map[string]*ast.File{"p.go": parse(pkg)})
	var seen []string
	for _, x := range found {
		seen = append(seen, x.fn+"."+x.field)
	}
	if want := "closeConn.closing closeConn.lastActivity flushSend.sending flushOther.sending flushOther.sending newCloser.closing closeFromVariable.closing"; strings.Join(seen, " ") != want {
		t.Errorf("clockStarts1430 found %q, want %q (sending = false and lastActivity = 0 are not starts)", strings.Join(seen, " "), want)
	}
	probs := strings.Join(checkStarts1430(found, []site1430{
		{fn: "closeConn", field: "closing", flow: "closing", kind: "hook", dues: []string{"dueAt"}, args: []string{"lastActivity", "bound"}},
		{fn: "closeConn", field: "lastActivity", flow: "closing", kind: "hook", dues: []string{"dueAt"}, args: []string{"lastActivity", "bound"}},
		{fn: "flushSend", field: "sending", flow: "write", kind: "hook", dues: []string{"dueWrite"}},
		{fn: "flushOther", field: "sending", flow: "write", kind: "hook", dues: []string{"dueWrite"}},
		{fn: "gone", field: "dirty", flow: "write", kind: "later"},
	}), "\n")
	if n := strings.Count(probs, "flushOther sets sending (write clock) and its block does not call dueWrite"); n != 1 {
		t.Errorf("checkStarts1430 reported the second statement of flushOther (a dueWrite on another path) %d times, want once:\n%s", n, probs)
	}
	for _, want := range []string{
		"newCloser sets closing and is not in startSites1430",
		"closeFromVariable sets closing and is not in startSites1430",
		"flushSend sets sending (write clock) and its block does not call dueWrite",
		"lists gone setting dirty",
	} {
		if !strings.Contains(probs, want) {
			t.Errorf("checkStarts1430 did not report %q; it reported:\n%s", want, probs)
		}
	}
	if strings.Contains(probs, "closeConn") {
		t.Errorf("checkStarts1430 reported a problem for closeConn, whose block calls dueAt with lastActivity and bound:\n%s", probs)
	}
}
