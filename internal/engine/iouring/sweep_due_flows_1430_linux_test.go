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
// WHAT THIS DOES NOT CATCH. This is a tripwire: it makes the next person who adds
// a clock, or moves a place that starts one, stop and look at the next-look rule.
// It does not check meaning, and the behavioural tests (a deadline of each kind
// set, the close observed on time: sweep_due_1430_linux_test.go) are the proof for
// the sites that exist today.
//
//   - A start site whose look is wrong but has the right shape: the argument of a
//     hook call is held to a small grammar (a sum of names, no product, a
//     constant of at most 2, a division by a constant) and not to a value.
//   - A "later", "sweep", "pinned" or "event" site is believed: only its right-hand
//     side is checked, for a subtraction or a product (a clock set back).
//   - Fields are matched by name, not type. driverConn's fields are skipped by
//     the declared type of the receiver or parameter, so a driver conn held in
//     a variable with another declaration shape is not skipped and fails the
//     test (a false failure, not a gap).
//   - A clock started off the worker's thread (the H2 write window armed from a
//     pool goroutine, a detached conn's IdleDeadlineNs: left to the sweeps' own
//     period, as sweep_gate_884.go says), a clock set in a composite literal,
//     and the header deadlines armed in internal/conn are not parsed.
//   - The stall flow (stallSince, lastStallLook) has start sites listed and no
//     sweep branch checked; the flow column is documentation.
//   - A read of a clock under a name that is neither a field of cs nor matches the
//     clock-name pattern, by a function that is not in the sweep groups.
//   - Path sensitivity beyond one block: a hook call must follow its start in
//     the same block with no return or branch statement between them, and the
//     one listed exception (flushSend's SQ-full return) is a reason, not a check.
//
// startSites1430 is every place in the package that sets a field the sweeps read
// (derived: derivedClockFields1430, below). Each must be listed here, with the
// flow it belongs to and what makes the look right: a hook (the statement's own
// block calls the flow's due function, after the statement, with no return or
// branch between), or a reason it needs none (it moves the clock later, or runs
// in the sweep, which computes the look again). A function that starts a clock
// and is not listed fails the test, and so does a listed one that no longer sets
// the field. A new field that the sweeps read fails until its start sites are
// listed, or until it is named in notClock1430 with the reason it is no deadline.

// notClock1430 are the fields of cs, and the names matching the clock pattern,
// that the sweep groups read and that start no deadline: state, identity or
// configuration. Everything else the sweeps read is a clock, and its start sites
// are scanned.
var notClock1430 = map[string]string{
	"asyncInBuf":      "bytes held for an async handler: state, no deadline",
	"asyncInMu":       "a mutex",
	"asyncRun":        "state of the async dispatch",
	"closeAcked":      "the peer's acknowledged bytes at the last sample: a value the look reads, not a time",
	"closeAckedKnown": "whether closeAcked is known: state",
	"detected":        "whether the protocol is decided: state (its deadline is detectDeadline)",
	"fd":              "the descriptor",
	"fixedFile":       "whether the conn uses a fixed file: state",
	"h2State":         "the HTTP/2 state; its deadlines are looked at by dueH2 (NextDeadline)",
	"hdrDL":           "the snapshot's copy of HeaderDeadlineNs, armed in internal/conn and looked at by armHeaderTimerAt",
	"heldSends":       "send completions held for a dispatch goroutine: state",
	"idleDL":          "the snapshot's copy of IdleDeadlineNs, set by the middleware off the loop (the sweeps' own period)",
	"recvArmed":       "whether a recv is armed: state",
	"remainderOut":    "a partial send's remainder is outstanding: state (the write clock is lastActivity)",
	"sweepKick":       "a request for a sweep: state, not a deadline",
	"sweepNext":       "the transplant drain sweep's own pace (sweepWait caps the ring wait at it)",
	"transplantHold":  "a conn held for a hand-off: state, rescued by the sweep (rescueHold)",
	"writeFn":         "the write function of the conn: state",
}

// csReads1430 are the names selected from an identifier called cs in fd.
func csReads1430(fd *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if se, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := se.X.(*ast.Ident); ok && id.Name == "cs" {
				out[se.Sel.Name] = true
			}
		}
		return true
	})
	return out
}

// sweepReaders1430 are the functions, besides the groups of
// sweep_due_sites_1430_linux_test.go, whose reads of cs are the sweep's.
var sweepReaders1430 = []string{"noteClosingAcked", "closingDrainBound"}

// derivedClockFields1430 is every field the sweeps read: the selectors of cs, and
// the names that match the clock pattern, in the functions of the sweep groups,
// less notClock1430.
func derivedClockFields1430(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	files := map[string]map[string]*ast.FuncDecl{}
	add := func(file, name string) {
		if files[file] == nil {
			files[file] = parseFuncs1430(t, file)
		}
		fd := files[file][name]
		if fd == nil {
			t.Fatalf("%s: no function %s (the sweep's reads)", file, name)
		}
		for r := range clockReads1430(fd) {
			out[r] = true
		}
		for r := range csReads1430(fd) {
			out[r] = true
		}
	}
	for _, g := range groups1430 {
		for _, fn := range g.funcs {
			add(g.file, fn)
		}
	}
	for _, fn := range sweepReaders1430 {
		add("worker.go", fn)
	}
	for k := range notClock1430 {
		delete(out, k)
	}
	return out
}

// site1430 is one place that sets a clock field.
type site1430 struct {
	fn    string   // the function the statement is in
	field string   // the field it sets
	flow  string   // write, closing, idle, detect, stall
	kind  string   // hook, later, sweep, pinned
	dues  []string // hook: the next-look functions, one of which the statement's own block must call
	args  []string // hook: names the arguments of that call must contain (the flow's own clock)
	// looks, when set, are several calls the block must make, each naming these
	// (args is then ignored): the closing drain asks for its reap and its first
	// sample.
	looks [][]string
	// skips: a return or branch between the start and the call is by design.
	skips bool
	why   string
}

var startSites1430 = []site1430{
	// The write clock: a send started.
	{fn: "flushSend", field: "sending", flow: "write", kind: "hook", dues: []string{"dueWrite"},
		why: "a send started: the write clock runs from the stamp it takes"},
	{fn: "flushSend", field: "lastActivity", flow: "write", kind: "hook", dues: []string{"dueWrite"}, skips: true,
		why: "the write clock restarts at the first SEND after a quiet spell (celeris#889); the SQ-full return between the stamp and the send swaps the buffers back and the caller puts the conn on the dirty list, which makes the loop's wait 0"},
	{fn: "flushSendLink", field: "sending", flow: "write", kind: "hook", dues: []string{"dueWrite"},
		why: "a send linked to a recv: the write clock runs from the last read"},
	{fn: "handleSend", field: "zcNotifPending", flow: "write", kind: "later",
		why: "the notification of a send that flushSend started: the write clock was looked at when that send began and the sweep takes it again"},
	{fn: "completeSend", field: "lastActivity", flow: "write", kind: "later",
		why: "a completed send restamps the clock: it only moves later, and the conn's earlier look recomputes"},
	{fn: "markDirty", field: "dirty", flow: "write", kind: "pinned",
		why: "a conn on the dirty list: baseTimeout is 0 while dirtyHead is set, so the loop's gate sweeps at the floor (checked below)"},

	// The closing drain: a close deferred behind the sends in flight.
	{fn: "closeConn", field: "closing", flow: "closing", kind: "hook", dues: []string{"dueAt"},
		looks: [][]string{{"lastActivity", "bound"}, {"cachedNow", "bound"}},
		why:   "the drain's bound starts here, and its first sample a quarter of it on"},
	{fn: "closeConn", field: "lastActivity", flow: "closing", kind: "hook", dues: []string{"dueAt"},
		looks: [][]string{{"lastActivity", "bound"}, {"cachedNow", "bound"}},
		why:   "the drain's clock is restamped where the drain starts"},
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
	stmt            ast.Stmt
	tok             token.Token
	rhs             ast.Expr
}

// driverVars1430 are the names declared in fd (receiver, parameters, results)
// with the type driverConn or *driverConn: its fields have the names of a
// connState's and no timeout sweep.
func driverVars1430(fd *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	lists := []*ast.FieldList{fd.Recv, fd.Type.Params, fd.Type.Results}
	for _, l := range lists {
		if l == nil {
			continue
		}
		for _, f := range l.List {
			t := f.Type
			if st, ok := t.(*ast.StarExpr); ok {
				t = st.X
			}
			if id, ok := t.(*ast.Ident); ok && id.Name == "driverConn" {
				for _, n := range f.Names {
					out[n.Name] = true
				}
			}
		}
	}
	// A local taken from something named for the driver conns
	// (dc, ok := w.driverConns[fd]).
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || as.Tok != token.DEFINE {
			return true
		}
		for _, r := range as.Rhs {
			for name := range names1430(r) {
				if strings.Contains(strings.ToLower(name), "driverconn") {
					for _, l := range as.Lhs {
						if id, ok := l.(*ast.Ident); ok {
							out[id.Name] = true
						}
					}
				}
			}
		}
		return true
	})
	return out
}

// clockStarts1430 returns the statements of the parsed files that set one of
// fields: every assignment whose right-hand side is not a plain 0 or false.
func clockStarts1430(files map[string]*ast.File, fields map[string]bool) []found1430 {
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
			drv := driverVars1430(fd)
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
					if !ok || !fields[se.Sel.Name] {
						continue
					}
					if id, ok := se.X.(*ast.Ident); ok && drv[id.Name] {
						continue
					}
					var rhs ast.Expr
					if len(as.Rhs) == len(as.Lhs) {
						rhs = as.Rhs[i]
						if as.Tok == token.ASSIGN {
							if id, ok := rhs.(*ast.Ident); ok && id.Name == "false" {
								continue // false: not a start (true, or a bool taken from a variable, is)
							}
							if bl, ok := rhs.(*ast.BasicLit); ok && bl.Value == "0" {
								continue
							}
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
					out = append(out, found1430{file: name, fn: fd.Name.Name, field: se.Sel.Name, block: block, stmt: as, tok: as.Tok, rhs: rhs})
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
// contain every name in args (for a due* call, the first argument only: the time
// of the look).
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
				for i, a := range call.Args {
					if i > 0 && strings.HasPrefix(nm, "due") {
						break // the time of a look is the first argument; the second is the clock it is read at
					}
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

// blockList1430 are the statements of a block, a case clause or a communication
// clause.
func blockList1430(block ast.Node) []ast.Stmt {
	switch b := block.(type) {
	case *ast.BlockStmt:
		return b.List
	case *ast.CaseClause:
		return b.Body
	case *ast.CommClause:
		return b.Body
	}
	return nil
}

// leaves1430 reports whether st holds a return or a branch statement (break,
// continue, goto), outside a function literal.
func leaves1430(st ast.Stmt) bool {
	found := false
	ast.Inspect(st, func(n ast.Node) bool {
		switch n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ReturnStmt, *ast.BranchStmt:
			found = true
		}
		return !found
	})
	return found
}

// callAfter1430 looks, among the statements of block that are not blocks of their
// own, for one after the statement start that calls one of names with every name
// of args in its arguments. It returns the call, and ok false when there is none,
// or when a statement between the start and the call returns or branches (the
// call is then not reached on every path from the start); skips lets such a
// statement be.
func callAfter1430(block ast.Node, start ast.Stmt, names, args []string, skips bool) (call *ast.CallExpr, ok bool) {
	list := blockList1430(block)
	i := -1
	for k, st := range list {
		if st == start {
			i = k
		}
	}
	if i < 0 {
		return nil, false
	}
	for k := i + 1; k < len(list); k++ {
		st := list[k]
		switch st.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt, *ast.BlockStmt, *ast.LabeledStmt:
			if !skips && leaves1430(st) {
				return nil, false
			}
			continue
		}
		var found *ast.CallExpr
		ast.Inspect(st, func(n ast.Node) bool {
			c, isCall := n.(*ast.CallExpr)
			if isCall && found == nil && callIn1430(c, names, args) {
				found = c
			}
			return found == nil
		})
		if found != nil {
			return found, true
		}
		if !skips && leaves1430(st) {
			return nil, false
		}
	}
	return nil, false
}

// lookOK1430 checks the time a look is asked for, the first argument of a call to
// a next-look function, against a small grammar: names (identifiers, selectors, an
// int64 conversion of them), sums, a division by a constant, and a constant of at
// most 2 in a sum. A product, a shift, a constant above 2 and the time package are
// not: they put a look further away than the deadline it is for. It says what is
// wrong, or "".
func lookOK1430(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return ""
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok && id.Name == "time" {
			return "the time package (" + id.Name + "." + x.Sel.Name + ")"
		}
		return lookOK1430(x.X)
	case *ast.ParenExpr:
		return lookOK1430(x.X)
	case *ast.CallExpr:
		if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "int64" && len(x.Args) == 1 {
			return lookOK1430(x.Args[0])
		}
		return "a call"
	case *ast.BasicLit:
		return ""
	case *ast.BinaryExpr:
		switch x.Op {
		case token.ADD, token.SUB:
			for _, side := range []ast.Expr{x.X, x.Y} {
				if bl, ok := side.(*ast.BasicLit); ok && x.Op == token.ADD && (len(bl.Value) > 1 || bl.Value > "2") {
					return "a constant " + bl.Value + " added"
				}
			}
			if r := lookOK1430(x.X); r != "" {
				return r
			}
			return lookOK1430(x.Y)
		case token.QUO:
			if _, ok := x.Y.(*ast.BasicLit); !ok {
				return "a division by a variable"
			}
			return lookOK1430(x.X)
		}
		return "the operator " + x.Op.String()
	}
	return "an expression the grammar does not know"
}

// exprText1430 prints an identifier or selector chain, "" for anything else.
func exprText1430(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		if b := exprText1430(x.X); b != "" {
			return b + "." + x.Sel.Name
		}
	}
	return ""
}

// parsePackage1430 parses the non-test files of the package.
func parsePackage1430(t *testing.T) map[string]*ast.File {
	t.Helper()
	ents, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*ast.File{}
	for _, e := range ents {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
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
// per problem: a statement that is not listed, a listed one that is not found, a
// hook whose block does not call its flow's due function after the statement and
// without a return or branch in between (or whose look is outside the grammar),
// and a site that is not a hook and sets its clock back.
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
		if s.kind != "hook" {
			if f.rhs != nil && backdates1430(f.rhs) {
				probs = append(probs, f.file+" "+f.fn+" sets "+f.field+" to a value that is earlier than now or scaled (a subtraction or a product), and its entry says it ("+s.kind+") only moves the clock later: "+s.why)
			}
			continue
		}
		looks := s.looks
		if looks == nil {
			looks = [][]string{s.args}
		}
		for _, args := range looks {
			call, ok := callAfter1430(f.block, f.stmt, s.dues, args, s.skips)
			with := ""
			if len(args) > 0 {
				with = " naming " + strings.Join(args, ", ")
			}
			if !ok {
				probs = append(probs, f.file+" "+f.fn+" sets "+f.field+" ("+s.flow+" clock) and its block does not call "+
					strings.Join(s.dues, " or ")+with+" after it, with no return or branch in between: "+s.why)
				continue
			}
			if len(call.Args) > 0 {
				if r := lookOK1430(call.Args[0]); r != "" {
					probs = append(probs, f.file+" "+f.fn+" asks for its look with "+r+": the look is for the deadline of the clock it starts, no later")
				}
			}
		}
	}
	for _, s := range list {
		if seen[key{s.fn, s.field}] == 0 {
			probs = append(probs, "startSites1430 lists "+s.fn+" setting "+s.field+", which no statement does any more")
		}
	}
	return probs
}

// backdates1430 reports whether e subtracts or multiplies.
func backdates1430(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if b, ok := n.(*ast.BinaryExpr); ok && (b.Op == token.SUB || b.Op == token.MUL) {
			found = true
		}
		return !found
	})
	return found
}

func TestEveryStartOfAClockLowersItsOwnLook1430(t *testing.T) {
	files := parsePackage1430(t)
	fields := derivedClockFields1430(t)
	found := clockStarts1430(files, fields)
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
	// direct says the call is one of the statements of the loop over the conns
	// itself, not inside an if of it: the look is the conn's own, taken whatever
	// state the conn is in.
	direct bool
}

var sweepFlows1430 = []branch1430{
	{flow: "closing: the reap", cond: []string{"closing"}, calls: []string{"dueAt"}, args: []string{"lastActivity", "bound"}},
	{flow: "closing: the next sample of the peer's progress", cond: []string{"closing"}, calls: []string{"dueAt"}, args: []string{"closeSampled", "bound"}},
	{flow: "write: a conn with output", cond: []string{"dirty", "sending", "zcNotifPending"}, calls: []string{"dueWrite"}, args: []string{"cs"}},
	{flow: "idle: ReadTimeout and IdleTimeout from the last read", calls: []string{"dueIdleFrom"}, args: []string{"lastActivity"}, direct: true},
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
		case b.direct:
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				fs, isFor := n.(*ast.ForStmt)
				if !isFor || ok {
					return !ok
				}
				for _, st := range fs.Body.List {
					if es, isExpr := st.(*ast.ExprStmt); isExpr && callIn1430(es, b.calls, b.args) {
						ok = true
					}
				}
				return false
			})
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
	// Every look the sweep asks for is for the deadline it names, no later.
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok && (calls1430(c, "dueAt") || calls1430(c, "dueIdleFrom")) && len(c.Args) > 0 {
			if r := lookOK1430(c.Args[0]); r != "" {
				t.Errorf("checkTimeouts asks for a look with %s: the look is for the deadline of the clock it reads, no later", r)
			}
		}
		return true
	})
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
		w.dueAt(w.cachedNow+bound/4, w.cachedNow)
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
func flushEarlyReturn(w *W, cs *C) bool {
	cs.sending = true
	if cs.noZC {
		return false
	}
	w.dueWrite(cs)
	return false
}
func flushBefore(w *W, cs *C) {
	w.dueWrite(cs)
	cs.sending = true
}
func lateLook(w *W, cs *C) {
	cs.closing = true
	w.dueAt(cs.lastActivity+bound*100+1, w.cachedNow)
}
func newCloser(cs *C, deferred bool) {
	cs.closing = true
	cs.sending = false
	cs.lastActivity = 0
}
func closeFromVariable(cs *C, deferred bool) {
	cs.closing = deferred
}
func backdater(cs *C, now int64) {
	cs.lastActivity = now - 5
}
func driverSide(dc *driverConn) {
	dc.closing = true
}
func armPing(cs *C) {
	cs.pingDeadline = now + 1
}
`
	fields := map[string]bool{"closing": true, "sending": true, "lastActivity": true, "pingDeadline": true}
	found := clockStarts1430(map[string]*ast.File{"p.go": parse(pkg)}, fields)
	var seen []string
	for _, x := range found {
		seen = append(seen, x.fn+"."+x.field)
	}
	want := "closeConn.closing closeConn.lastActivity flushSend.sending flushOther.sending flushOther.sending flushEarlyReturn.sending flushBefore.sending lateLook.closing newCloser.closing closeFromVariable.closing backdater.lastActivity armPing.pingDeadline"
	if strings.Join(seen, " ") != want {
		t.Errorf("clockStarts1430 found %q, want %q (sending = false and lastActivity = 0 are not starts, and a driverConn's fields are not scanned)", strings.Join(seen, " "), want)
	}
	closeLooks := [][]string{{"lastActivity", "bound"}, {"cachedNow", "bound"}}
	probs := strings.Join(checkStarts1430(found, []site1430{
		{fn: "closeConn", field: "closing", flow: "closing", kind: "hook", dues: []string{"dueAt"}, looks: closeLooks},
		{fn: "closeConn", field: "lastActivity", flow: "closing", kind: "hook", dues: []string{"dueAt"}, looks: closeLooks},
		{fn: "flushSend", field: "sending", flow: "write", kind: "hook", dues: []string{"dueWrite"}},
		{fn: "flushOther", field: "sending", flow: "write", kind: "hook", dues: []string{"dueWrite"}},
		{fn: "flushEarlyReturn", field: "sending", flow: "write", kind: "hook", dues: []string{"dueWrite"}},
		{fn: "flushBefore", field: "sending", flow: "write", kind: "hook", dues: []string{"dueWrite"}},
		{fn: "lateLook", field: "closing", flow: "closing", kind: "hook", dues: []string{"dueAt"}, args: []string{"lastActivity"}},
		{fn: "backdater", field: "lastActivity", flow: "idle", kind: "later"},
		{fn: "gone", field: "dirty", flow: "write", kind: "later"},
	}), "\n")
	if n := strings.Count(probs, "flushOther sets sending (write clock) and its block does not call dueWrite"); n != 1 {
		t.Errorf("checkStarts1430 reported the second statement of flushOther (a dueWrite on another path) %d times, want once:\n%s", n, probs)
	}
	for _, want := range []string{
		"newCloser sets closing and is not in startSites1430",
		"closeFromVariable sets closing and is not in startSites1430",
		"armPing sets pingDeadline and is not in startSites1430",
		"flushSend sets sending (write clock) and its block does not call dueWrite after it",
		"flushEarlyReturn sets sending (write clock) and its block does not call dueWrite after it, with no return or branch in between",
		"flushBefore sets sending (write clock) and its block does not call dueWrite after it",
		"lateLook asks for its look with the operator *",
		"backdater sets lastActivity to a value that is earlier than now or scaled",
		"lists gone setting dirty",
	} {
		if !strings.Contains(probs, want) {
			t.Errorf("checkStarts1430 did not report %q; it reported:\n%s", want, probs)
		}
	}
	if strings.Contains(probs, "closeConn") || strings.Contains(probs, "driverSide") {
		t.Errorf("checkStarts1430 reported a problem for closeConn (both looks, in order) or driverSide (a driverConn):\n%s", probs)
	}
}
