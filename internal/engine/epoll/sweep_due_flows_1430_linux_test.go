//go:build linux

package epoll

// celeris#1430, part S: the structural guard, by flow.
//
// sweep_due_structure_1430_linux_test.go asks that every clock the sweep reads is
// in a table with the way the next-due computation accounts for it. Three things
// get past it, and this file closes them.
//
//   - A new START SITE of a clock that a hook accounts for. The write clock
//     (writeSince) and the closing drain's clock (closeSince) are not dated by the
//     walk: a statement that starts one lowers the next-due time itself
//     (lowerNextDue). The structural test checks that three named functions do
//     it, and not that no other function starts one: a new statement
//     `cs.writeSince = mono.Now()` in another function passed. startSites1430 lists
//     every statement of the package that sets writeSince, closeSince, lastActivity
//     or detectDeadline, with the call its own block makes (hook), or why it needs
//     none, and fails for one that is not listed, one listed that is gone, and a
//     hook whose block does not make its call.
//   - One min(due, ...) standing in for several clocks. The structural test
//     follows an identifier such as dl back to every selector it was assigned from
//     in the function, so min(due, dl+1) named the accept budget, the header
//     deadline and the detached idle deadline at once. Here the identifier is
//     followed through the blocks that enclose the call only, and each flow is
//     checked in its own branch of the walk: the accept budget, the header
//     deadline and the idle clock each date themselves, and the write clock, the
//     closing drain and the detached conn each report "look again soon" inside the
//     branch that reads their clock.
//   - A walk clock that is not made of the timeouts the safety cap is made of. A
//     deadline that an event gives a connection must be at least the event plus a
//     timeout that sweepHorizon takes half of (ReadHeaderTimeout, ReadTimeout,
//     IdleTimeout), or the next walk can come after the deadline; sweepHorizon
//     hard-codes the three. walkTimeouts1430 says what each walk clock is made of,
//     and the test checks that against every statement that sets the clock and
//     every min(due, ...) that dates it, and that sweepHorizon takes each timeout
//     named.
//
// What it does not see: types (fields are matched by name), a clock whose stamp
// comes from another package than conn's H1State (an HTTP/2 deadline: those pin
// the loop, sweepDue), and a clock set through a helper that is not the
// assignment itself (a function that returns the stamp, which the caller stores
// and the scan sees).

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// startFields1430 are the connState fields whose assignment (to anything but 0)
// starts or moves a clock the walk accounts for by a hook or by the safety cap.
var startFields1430 = map[string]bool{
	"writeSince":     false,
	"closeSince":     false,
	"lastActivity":   false,
	"detectDeadline": false,
}

// startSite1430 is one place that sets a clock field.
type startSite1430 struct {
	fn, field string
	kind      string   // hook, event, later, sweep, pinned
	dues      []string // hook: the calls, one of which the statement's own block must make
	why       string
}

var startSites1430 = []startSite1430{
	// The write clock of a parked response.
	{fn: "armEpollOut", field: "writeSince", kind: "hook", dues: []string{"lowerNextDue"},
		why: "a response parked: its first look is owed one tick after the park"},
	{fn: "handleWritable", field: "writeSince", kind: "hook", dues: []string{"lowerNextDue"},
		why: "a clock started on EPOLLOUT for bytes queued without a pass through armEpollOut"},
	{fn: "noteWriteProgress", field: "writeSince", kind: "later",
		why: "restarts a clock that is running (writeSince != 0) when the peer took bytes: only later; the walk rearms for as long as it is set"},
	{fn: "parkedBelowRate", field: "writeSince", kind: "sweep",
		why: "in the sweep: the write-rate window restarts at the look the walk rearms"},

	// The closing drain.
	{fn: "markClosing", field: "closeSince", kind: "hook", dues: []string{"lowerNextDue"},
		why: "a close deferred behind queued bytes: the first sample is owed one tick after"},
	{fn: "noteClosingProgress", field: "closeSince", kind: "later",
		why: "restarts a drain that is running when the peer took bytes: only later; the walk rearms for as long as it is set"},
	{fn: "noteClosingAcked", field: "closeSince", kind: "sweep",
		why: "in the sweep: the peer's acknowledged bytes restart the drain at the look"},
	{fn: "detachedStalled", field: "closeSince", kind: "pinned",
		why: "the stall clock of a detached conn: the loop holds a detached conn, so sweepDue never skips it (checked below)"},

	// The idle and read clock: an event, dated by the walk that follows.
	{fn: "acceptAll", field: "lastActivity", kind: "event",
		why: "a connection accepted: event + the idle bound is the walk's own date, covered by the safety cap"},
	{fn: "attachFD", field: "lastActivity", kind: "event",
		why: "a connection adopted: as an accept"},
	{fn: "drainRead", field: "lastActivity", kind: "event",
		why: "a read: only later; the walk dates event + the idle bound"},
	{fn: "endWriteSlow", field: "lastActivity", kind: "later",
		why: "the write clock's end: only later"},
	{fn: "sweepWriteClock", field: "lastActivity", kind: "sweep",
		why: "in the sweep: moves the idle clock to the write window's end, later"},

	// The accept budget.
	{fn: "acceptAll", field: "detectDeadline", kind: "event",
		why: "accept + ReadHeaderTimeout: an event + a timeout the safety cap takes half of (walkTimeouts1430)"},
	{fn: "noteH2Owed", field: "detectDeadline", kind: "sweep",
		why: "in the sweep: the h2 owed stamp, which the exempt branch reports as rearm"},
}

// walkTimeouts1430 is, for each clock the walk dates exactly, the configuration
// timeouts its deadline is an event plus. sweepHorizon must take each, or a walk
// before the event can come after the deadline.
var walkTimeouts1430 = map[string][]string{
	"detectDeadline": {"ReadHeaderTimeout"},
	"hdrDL":          {"ReadHeaderTimeout"},
	"lastActivity":   {"IdleTimeout", "ReadTimeout"},
}

var timeoutName1430 = regexp.MustCompile(`Timeout(Ns)?$`)

// timeoutNames1430 are the names of the timeouts of the configuration inside the
// names of a set: a selector or identifier that ends in Timeout, without Ns.
func timeoutNames1430(set map[string]bool) []string {
	var out []string
	for n := range set {
		if timeoutName1430.MatchString(n) {
			out = append(out, strings.TrimSuffix(n, "Ns"))
		}
	}
	sort.Strings(out)
	return out
}

func subset1430(a, b []string) []string {
	var extra []string
	for _, x := range a {
		found := false
		for _, y := range b {
			found = found || x == y
		}
		if !found {
			extra = append(extra, x)
		}
	}
	return extra
}

// names1430 are the selector, identifier and called-function names inside n.
func namesIn1430(n ast.Node) map[string]bool {
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

// resolveIn1430 returns the names inside x, with every identifier replaced (and
// kept) by the names inside the expressions assigned to it in the blocks and if
// statements that enclose x (stack holds the nodes from the function's body down
// to the node that holds x). An identifier is not followed into a branch that
// does not enclose the call: min(due, dl+1) in one branch does not take the names
// of the dl of another.
func resolveIn1430(stack []ast.Node, x ast.Node) map[string]bool {
	out := map[string]bool{}
	seen := map[string]bool{}
	var add func(n ast.Node)
	add = func(n ast.Node) {
		for k := range namesIn1430(n) {
			out[k] = true
		}
		ast.Inspect(n, func(m ast.Node) bool {
			id, ok := m.(*ast.Ident)
			if !ok || seen[id.Name] {
				return true
			}
			seen[id.Name] = true
			for i := len(stack) - 1; i >= 0; i-- {
				var stmts []ast.Stmt
				switch s := stack[i].(type) {
				case *ast.IfStmt:
					if s.Init != nil {
						stmts = []ast.Stmt{s.Init}
					}
				case *ast.BlockStmt:
					for _, st := range s.List {
						if i+1 < len(stack) && st.Pos() >= stack[i+1].Pos() {
							break
						}
						stmts = append(stmts, st)
					}
				}
				for _, st := range stmts {
					as, ok := st.(*ast.AssignStmt)
					if !ok {
						continue
					}
					for j, l := range as.Lhs {
						if lid, ok := l.(*ast.Ident); ok && lid.Name == id.Name {
							if len(as.Rhs) == len(as.Lhs) {
								add(as.Rhs[j])
							} else {
								for _, r := range as.Rhs {
									add(r)
								}
							}
						}
					}
				}
			}
			return true
		})
	}
	add(x)
	return out
}

// minDue1430 is one min(due, X) call of fn, with the stack of enclosing nodes.
type minDue1430 struct {
	rearm bool
	names map[string]bool // what X names, identifiers followed (resolveIn1430)
	stack []ast.Node
	call  *ast.CallExpr
}

func minDues1430(fn *ast.FuncDecl) []minDue1430 {
	var out []minDue1430
	var stack []ast.Node
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
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
		m := minDue1430{stack: slicesClone1430(stack), call: c}
		if id, ok := c.Args[1].(*ast.Ident); ok && id.Name == "rearm" {
			m.rearm = true
		} else {
			m.names = resolveIn1430(m.stack, c.Args[1])
		}
		out = append(out, m)
		return true
	})
	return out
}

func slicesClone1430(s []ast.Node) []ast.Node { return append([]ast.Node(nil), s...) }

// within1430 reports whether the call is inside the body of an if statement
// whose init or condition names every one of cond (a flow's own branch).
func within1430(m minDue1430, cond []string) bool {
	for _, n := range m.stack {
		is, ok := n.(*ast.IfStmt)
		if !ok {
			continue
		}
		// The call must be in the if's body, not in its condition or init.
		if m.call.Pos() < is.Body.Pos() || m.call.End() > is.Body.End() {
			continue
		}
		have := map[string]bool{}
		if is.Init != nil {
			for k := range namesIn1430(is.Init) {
				have[k] = true
			}
		}
		for k := range namesIn1430(is.Cond) {
			have[k] = true
		}
		all := true
		for _, c := range cond {
			all = all && have[c]
		}
		if all {
			return true
		}
	}
	return false
}

// flow1430 is what one flow's own branch of the walk must hold.
type flow1430 struct {
	name  string
	cond  []string // the branch: an if whose init or condition names these ("" none: anywhere in the walk)
	rearm bool     // the branch reports "look again soon" (min(due, rearm))
	names []string // otherwise it dates the clock: a min(due, X) whose X names these
}

var walkFlows1430 = []flow1430{
	{name: "write: a response parked", cond: []string{"writeSince"}, rearm: true},
	{name: "closing: a drain running", cond: []string{"closeSince"}, rearm: true},
	{name: "detached: the middleware's conn", cond: []string{"detached"}, rearm: true},
	{name: "accept budget: a connection whose protocol is not decided", cond: []string{"detectDeadline"}, names: []string{"detectDeadline"}},
	{name: "header deadline", cond: []string{"hdrDL"}, names: []string{"hdrDL"}},
	{name: "idle: the last read plus the idle bound", names: []string{"lastActivity", "idleBound"}},
}

// checkFlows1430 returns the flows of the list that fn's walk does not carry out
// in their own branch.
func checkFlows1430(fn *ast.FuncDecl, list []flow1430) []string {
	var probs []string
	mins := minDues1430(fn)
	for _, f := range list {
		ok := false
		for _, m := range mins {
			if len(f.cond) > 0 && !within1430(m, f.cond) {
				continue
			}
			if f.rearm && m.rearm {
				ok = true
			}
			if !f.rearm && !m.rearm && hasAllNames1430(m.names, f.names) {
				ok = true
			}
		}
		if !ok {
			what := "min(due, rearm)"
			if !f.rearm {
				what = "min(due, ...) naming " + strings.Join(f.names, ", ")
			}
			in := "in the walk"
			if len(f.cond) > 0 {
				in = "in the branch that reads " + strings.Join(f.cond, ", ")
			}
			probs = append(probs, f.name+": no "+what+" "+in+" of "+fn.Name.Name)
		}
	}
	return probs
}

func hasAllNames1430(have map[string]bool, want []string) bool {
	for _, w := range want {
		if !have[w] {
			return false
		}
	}
	return true
}

// found1430 is one statement that sets a clock field.
type found1430 struct {
	file, fn, field string
	block           ast.Node
	rhs             []ast.Expr
}

func clockStarts1430(files map[string]*ast.File) []found1430 {
	var out []found1430
	var names []string
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
					if _, isField := startFields1430[se.Sel.Name]; !isField {
						continue
					}
					if len(as.Rhs) == len(as.Lhs) && as.Tok == token.ASSIGN {
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
						default:
							continue
						}
						break
					}
					out = append(out, found1430{file: name, fn: fd.Name.Name, field: se.Sel.Name, block: block, rhs: as.Rhs})
				}
				return true
			})
		}
	}
	return out
}

// callInBlock1430 reports whether one of the statements that are in the block
// itself (not in a block of a statement inside it: that is another path) makes a
// call to one of names.
func callInBlock1430(block ast.Node, names []string) bool {
	var list []ast.Stmt
	switch b := block.(type) {
	case *ast.BlockStmt:
		list = b.List
	case *ast.CaseClause:
		list = b.Body
	}
	for _, st := range list {
		switch st.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt, *ast.BlockStmt, *ast.LabeledStmt:
			continue
		}
		found := false
		ast.Inspect(st, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				switch f := c.Fun.(type) {
				case *ast.SelectorExpr:
					found = found || containsStr1430(names, f.Sel.Name)
				case *ast.Ident:
					found = found || containsStr1430(names, f.Name)
				}
			}
			return !found
		})
		if found {
			return true
		}
	}
	return false
}

func containsStr1430(s []string, x string) bool {
	for _, y := range s {
		if x == y {
			return true
		}
	}
	return false
}

// checkStarts1430 compares the statements found with the list.
func checkStarts1430(found []found1430, list []startSite1430) []string {
	var probs []string
	type key struct{ fn, field string }
	listed := map[key]startSite1430{}
	for _, s := range list {
		listed[key{s.fn, s.field}] = s
	}
	seen := map[key]bool{}
	for _, f := range found {
		k := key{f.fn, f.field}
		s, ok := listed[k]
		if !ok {
			probs = append(probs, f.file+" "+f.fn+" sets "+f.field+" and is not in startSites1430: say which clock it starts and what looks at it")
			continue
		}
		seen[k] = true
		if s.kind == "hook" && !callInBlock1430(f.block, s.dues) {
			probs = append(probs, f.file+" "+f.fn+" sets "+f.field+" and its block does not call "+strings.Join(s.dues, " or ")+": "+s.why)
		}
	}
	for _, s := range list {
		if !seen[key{s.fn, s.field}] {
			probs = append(probs, "startSites1430 lists "+s.fn+" setting "+s.field+", which no statement does any more")
		}
	}
	return probs
}

// packageFiles1430 parses the non-test files of the package, and the
// non-test files of internal/conn that arm an HTTP/1 header deadline.
func packageFiles1430(t *testing.T, dir string) map[string]*ast.File {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*ast.File{}
	for _, e := range ents {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), dir+"/"+n, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", n, err)
		}
		out[dir+"/"+n] = f
	}
	return out
}

func TestEveryStartOfAClockLowersItsOwnLook1430(t *testing.T) {
	files := packageFiles1430(t, ".")
	found := clockStarts1430(files)
	if len(found) < 15 {
		t.Fatalf("the scan found %d statements that set a clock field, want about 20: it no longer sees the package", len(found))
	}
	// The file names carry the directory; the list is by function.
	for i := range found {
		found[i].file = strings.TrimPrefix(found[i].file, "./")
	}
	for _, p := range checkStarts1430(found, startSites1430) {
		t.Error(p)
	}
	// The premise of a "pinned" entry: a loop that holds a detached conn is never
	// skipped.
	_, pf := parseSweepFiles1430(t)
	if sd := findFunc1430(pf, "sweepDue"); sd == nil || !fieldReads1430(sd)["detachedCount"] {
		t.Error("detachedStalled is pinned by detachedCount, and sweepDue does not read it")
	}
}

func TestWalkHasALookForEachClockInItsOwnBranch1430(t *testing.T) {
	_, files := parseSweepFiles1430(t)
	ct := findFunc1430(files, "checkTimeouts")
	if ct == nil {
		t.Fatal("no checkTimeouts")
	}
	for _, p := range checkFlows1430(ct, walkFlows1430) {
		t.Error(p)
	}
}

// TestWalkClocksAreMadeOfTheTimeoutsTheHorizonTakes1430: the proof that a clock
// started between two walks needs no hook (sweep_due_1430.go) holds for deadlines
// that are an event plus a timeout of ReadHeaderTimeout, ReadTimeout or
// IdleTimeout, the timeouts sweepHorizon is half of. A walk clock made of
// another, or of one the horizon forgot, can be due before the next walk.
func TestWalkClocksAreMadeOfTheTimeoutsTheHorizonTakes1430(t *testing.T) {
	_, files := parseSweepFiles1430(t)
	sh := findFunc1430(files, "sweepHorizon")
	ct := findFunc1430(files, "checkTimeouts")
	if sh == nil || ct == nil {
		t.Fatal("no sweepHorizon or checkTimeouts")
	}
	horizon := timeoutNames1430(namesIn1430(sh.Body))
	if len(horizon) < 3 {
		t.Fatalf("sweepHorizon names the timeouts %v, want ReadHeaderTimeout, ReadTimeout and IdleTimeout", horizon)
	}
	// 1. The timeouts each walk clock is made of are in the horizon.
	for clock, ts := range walkTimeouts1430 {
		if extra := subset1430(ts, horizon); len(extra) > 0 {
			t.Errorf("the walk clock %s is made of %v, which sweepHorizon (%v) does not take: a walk before the event can come after the deadline", clock, extra, horizon)
		}
	}
	// 2. Every date the walk reports is made of those timeouts only: what a
	// min(due, X) names, followed through identifiers and the functions of the
	// package it calls (idleBound).
	allowed := map[string]bool{}
	for _, ts := range walkTimeouts1430 {
		for _, x := range ts {
			allowed[x] = true
		}
	}
	for _, m := range minDues1430(ct) {
		if m.rearm {
			continue
		}
		set := map[string]bool{}
		for k := range m.names {
			set[k] = true
			if fn := findFunc1430(files, k); fn != nil {
				for c := range namesIn1430(fn.Body) {
					set[c] = true
				}
			}
		}
		for _, x := range timeoutNames1430(set) {
			if !allowed[x] {
				t.Errorf("checkTimeouts reports a date made of %s (min(due, ...) at offset %d), which no entry of walkTimeouts1430 has: sweepHorizon takes %v", x, m.call.Pos(), horizon)
			}
		}
	}
	// 3. Every statement that sets one of the clocks is made of the timeouts the
	// table says (the accept budget), and so is every arm of the header deadline
	// (internal/conn).
	for _, f := range clockStarts1430(packageFiles1430(t, ".")) {
		if f.field != "detectDeadline" {
			continue
		}
		set := map[string]bool{}
		for _, r := range f.rhs {
			for k := range namesIn1430(r) {
				set[k] = true
			}
		}
		if extra := subset1430(timeoutNames1430(set), walkTimeouts1430["detectDeadline"]); len(extra) > 0 {
			t.Errorf("%s sets detectDeadline from %v, which is not in walkTimeouts1430[detectDeadline] %v", f.fn, extra, walkTimeouts1430["detectDeadline"])
		}
	}
	for name, f := range packageFiles1430(t, "../../conn") {
		ast.Inspect(f, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			se, ok := c.Fun.(*ast.SelectorExpr)
			if !ok || se.Sel.Name != "Store" || len(c.Args) != 1 {
				return true
			}
			recv, ok := se.X.(*ast.SelectorExpr)
			if !ok || recv.Sel.Name != "HeaderDeadlineNs" {
				return true
			}
			if bl, ok := c.Args[0].(*ast.BasicLit); ok && bl.Value == "0" {
				return true
			}
			if extra := subset1430(timeoutNames1430(namesIn1430(c.Args[0])), walkTimeouts1430["hdrDL"]); len(extra) > 0 {
				t.Errorf("%s stores a header deadline made of %v, which is not in walkTimeouts1430[hdrDL] %v", name, extra, walkTimeouts1430["hdrDL"])
			}
			return true
		})
	}
}

// TestFlowGuardSeesWhatTheTableGuardMisses1430 is the guard's own test, on source
// it makes up.
func TestFlowGuardSeesWhatTheTableGuardMisses1430(t *testing.T) {
	parse := func(src string) *ast.File {
		f, err := parser.ParseFile(token.NewFileSet(), "p.go", src, 0)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	// A walk in which one min(due, dl+1) is all there is for the accept budget and
	// the header deadline (the identifier dl is assigned from both), the closing
	// branch has lost its rearm, and the write branch dates its clock from
	// WriteTimeout.
	const walk = `package p
func checkTimeouts(l *L) {
	due := now
	rearm := now
	idle := l.idleBound()
	for {
		if !cs.detected {
			if dl := cs.detectDeadline; dl > 0 {
				due = min(due, dl+1)
			}
		}
		if dl := snap.hdrDL; dl > 0 {
		}
		if cs.closeSince != 0 {
			bound := l.closingDrainBound()
		}
		if cs.writeSince != 0 || cs.dirty {
			due = min(due, rearm)
			due = min(due, cs.lastActivity+int64(l.cfg.WriteTimeout)+1)
		}
		last := cs.lastActivity
		due = min(due, last+idle+1)
	}
}
`
	var fn *ast.FuncDecl
	for _, d := range parse(walk).Decls {
		fn = d.(*ast.FuncDecl)
	}
	got := strings.Join(checkFlows1430(fn, walkFlows1430), "\n")
	for _, want := range []string{"closing: a drain running", "header deadline", "detached: the middleware's conn"} {
		if !strings.Contains(got, want) {
			t.Errorf("checkFlows1430 did not report the flow %q; it reported:\n%s", want, got)
		}
	}
	for _, not := range []string{"write: a response parked", "accept budget", "idle: the last read"} {
		if strings.Contains(got, not) {
			t.Errorf("checkFlows1430 reported the flow %q, which the walk carries out:\n%s", not, got)
		}
	}
	var wt []string
	for _, m := range minDues1430(fn) {
		if !m.rearm {
			wt = append(wt, strings.Join(timeoutNames1430(m.names), ","))
		}
	}
	if strings.Join(wt, ";") != ";WriteTimeout;" {
		t.Errorf("the timeouts the dates of the walk name are %q, want only the WriteTimeout one (idleBound is a call, not followed here)", strings.Join(wt, ";"))
	}

	const pkg = `package p
func armEpollOut(l *L, cs *C) {
	if cs.writeSince == 0 {
		cs.writeSince = mono.Now()
		l.lowerNextDue(cs.writeSince)
	}
}
func handleWritable(l *L, cs *C) {
	if cs.writeSince == 0 {
		cs.writeSince = mono.Now()
	}
	cs.writeSince = 0
}
func newParker(cs *C) {
	cs.writeSince = mono.Now()
}
func other(l *L, cs *C) {
	if cs.a {
		l.lowerNextDue(1)
	}
	cs.closeSince = mono.Now()
}
`
	found := clockStarts1430(map[string]*ast.File{"p.go": parse(pkg)})
	probs := strings.Join(checkStarts1430(found, []startSite1430{
		{fn: "armEpollOut", field: "writeSince", kind: "hook", dues: []string{"lowerNextDue"}},
		{fn: "handleWritable", field: "writeSince", kind: "hook", dues: []string{"lowerNextDue"}},
		{fn: "other", field: "closeSince", kind: "hook", dues: []string{"lowerNextDue"}},
		{fn: "gone", field: "closeSince", kind: "later"},
	}), "\n")
	for _, want := range []string{
		"newParker sets writeSince and is not in startSites1430",
		"handleWritable sets writeSince and its block does not call lowerNextDue",
		"other sets closeSince and its block does not call lowerNextDue",
		"lists gone setting closeSince",
	} {
		if !strings.Contains(probs, want) {
			t.Errorf("checkStarts1430 did not report %q; it reported:\n%s", want, probs)
		}
	}
	if strings.Contains(probs, "armEpollOut") {
		t.Errorf("checkStarts1430 reported armEpollOut, whose block calls lowerNextDue:\n%s", probs)
	}
}
