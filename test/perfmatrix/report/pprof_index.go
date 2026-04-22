package report

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// PprofCapture is one pprof artifact on disk; retained for API
// compatibility with callers that pre-assembled a slice. WritePprofIndex
// no longer takes this type directly — walking the output directory is
// more robust because the runner's per-cell JSON always lists the
// capture paths accurately.
type PprofCapture struct {
	ScenarioName string
	ServerName   string
	RunIdx       int
	Kind         string // "cpu", "heap", "goroutine", "block", "mutex"
	Path         string // path relative to the run output directory
}

// WritePprofIndex walks outDir looking for .pprof files and emits an
// HTML navigation page at w. The page contains one row per (run,
// scenario, server) triple, with columns for cpu/heap/goroutine. Each
// cell links to a copy-pasteable `go tool pprof` command rather than
// trying to embed a viewer.
func WritePprofIndex(w io.Writer, outDir string) error {
	type key struct {
		Run      string
		Scenario string
		Server   string
	}
	cells := make(map[key]map[string]string) // key -> kind -> path

	walkErr := filepath.WalkDir(outDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// Skip unreadable entries rather than aborting the whole index.
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".pprof") {
			return nil
		}
		rel, relErr := filepath.Rel(outDir, path)
		if relErr != nil {
			rel = path
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) < 3 {
			return nil
		}
		// Layout: <run>/<scenario>/<server>.<kind>.pprof
		run := parts[0]
		scenario := parts[1]
		fileName := parts[len(parts)-1]
		// Parse <server>.<kind>.pprof.
		trimmed := strings.TrimSuffix(fileName, ".pprof")
		dot := strings.LastIndex(trimmed, ".")
		if dot < 0 {
			return nil
		}
		server := trimmed[:dot]
		kind := trimmed[dot+1:]

		k := key{Run: run, Scenario: scenario, Server: server}
		m, ok := cells[k]
		if !ok {
			m = map[string]string{}
			cells[k] = m
		}
		m[kind] = rel
		return nil
	})
	if walkErr != nil {
		return walkErr
	}

	// Stable ordering: run → scenario → server.
	keys := make([]key, 0, len(cells))
	for k := range cells {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.Run != b.Run {
			return a.Run < b.Run
		}
		if a.Scenario != b.Scenario {
			return a.Scenario < b.Scenario
		}
		return a.Server < b.Server
	})

	if _, err := io.WriteString(w, htmlHeader); err != nil {
		return err
	}

	for _, k := range keys {
		m := cells[k]
		row := fmt.Sprintf(
			"<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>\n",
			escapeHTML(k.Run),
			escapeHTML(k.Scenario),
			escapeHTML(k.Server),
			renderPprofCell(m["cpu"]),
			renderPprofCell(m["heap"]),
			renderPprofCell(m["goroutine"]),
		)
		if _, err := io.WriteString(w, row); err != nil {
			return err
		}
	}

	if _, err := io.WriteString(w, htmlFooter); err != nil {
		return err
	}
	return nil
}

func renderPprofCell(relPath string) string {
	if relPath == "" {
		return "—"
	}
	cmd := "go tool pprof -http=:0 " + relPath
	return fmt.Sprintf(
		`<a href="%s">%s</a><br><code>%s</code>`,
		escapeHTML(relPath),
		escapeHTML(filepath.Base(relPath)),
		escapeHTML(cmd),
	)
}

func escapeHTML(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&#39;",
	)
	return r.Replace(s)
}

const htmlHeader = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>perfmatrix pprof index</title>
<style>
body { font-family: system-ui, sans-serif; margin: 2rem; }
table { border-collapse: collapse; width: 100%; }
th, td { border: 1px solid #ddd; padding: .5rem .75rem; text-align: left; vertical-align: top; }
th { background: #f5f5f5; }
code { background: #f0f0f0; padding: 1px 4px; border-radius: 3px; font-size: 0.85em; }
</style>
</head>
<body>
<h1>perfmatrix pprof index</h1>
<p>Each row links to a captured pprof artifact. Use the copy-pasteable command to open it in a browser viewer:</p>
<table>
<thead>
<tr><th>run</th><th>scenario</th><th>server</th><th>cpu</th><th>heap</th><th>goroutine</th></tr>
</thead>
<tbody>
`

const htmlFooter = `</tbody>
</table>
</body>
</html>
`
