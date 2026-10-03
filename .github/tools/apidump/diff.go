package main

import (
	"fmt"
	"strings"
)

// unifiedDiff returns a unified diff (3 lines of context) from the committed
// golden file to the generated one. A nil side is a missing file.
func unifiedDiff(name string, committed, generated []byte) string {
	a, b := splitLines(committed), splitLines(generated)
	from, to := "a/"+name, "b/"+name
	if committed == nil {
		from = "/dev/null"
	}
	if generated == nil {
		to = "/dev/null"
	}
	ops := diffLines(a, b)
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", from, to)
	const ctx = 3
	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		// A hunk: from ctx lines before this change to ctx lines after the
		// last change that is at most 2*ctx unchanged lines further on.
		start := max(i-ctx, 0)
		end := i
		for j := i; j < len(ops); j++ {
			if ops[j].kind != ' ' {
				end = j
			} else if j-end > 2*ctx {
				break
			}
		}
		stop := min(end+ctx+1, len(ops))
		var aStart, aLen, bStart, bLen int
		aStart, bStart = ops[start].a+1, ops[start].b+1
		for _, op := range ops[start:stop] {
			if op.kind != '+' {
				aLen++
			}
			if op.kind != '-' {
				bLen++
			}
		}
		if aLen == 0 {
			aStart--
		}
		if bLen == 0 {
			bStart--
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", aStart, aLen, bStart, bLen)
		for _, op := range ops[start:stop] {
			out.WriteByte(op.kind)
			out.WriteString(op.line)
			out.WriteByte('\n')
		}
		i = stop
	}
	return out.String()
}

func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// diffOp is one line of an edit script: ' ' kept, '-' only in a, '+' only
// in b. a and b are the 0-based positions the line has (or would have) in
// each side.
type diffOp struct {
	kind byte
	line string
	a, b int
}

// diffLines computes a shortest edit script by longest common subsequence.
// The golden files are a few thousand lines at most, so the quadratic table
// is cheap, and it is only built for a file that differs.
func diffLines(a, b []string) []diffOp {
	n, m := len(a), len(b)
	lcs := make([][]int32, n+1)
	for i := range lcs {
		lcs[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var ops []diffOp
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i], i, j})
			i++
			j++
		case i < n && (j == m || lcs[i+1][j] >= lcs[i][j+1]):
			ops = append(ops, diffOp{'-', a[i], i, j}) // a deletion before the insertion that replaces it
			i++
		default:
			ops = append(ops, diffOp{'+', b[j], i, j})
			j++
		}
	}
	return ops
}
