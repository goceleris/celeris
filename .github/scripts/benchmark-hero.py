#!/usr/bin/env python3
"""Regenerate .github/benchmark.png (the README benchmark hero) from published results.

Every number on the chart is read from one goceleris/docs results cell, so the
picture can be re-derived and checked. Nothing is typed in by hand.

Usage:
  benchmark-hero.py <summary.json> [scenario] > hero.html

  <summary.json>  a published cell, e.g. goceleris/docs
                  results/v1.5.8/20260829/x86_64/summary.json
  [scenario]      defaults to get-simple-1024c

Render the PNG (headless Chrome loads Inter and JetBrains Mono from Google
Fonts, so it needs network):

  chrome --headless=new --disable-gpu --hide-scrollbars \
    --force-device-scale-factor=1.5 --window-size=1280,<H> \
    --virtual-time-budget=8000 --screenshot=benchmark.png file://$PWD/hero.html

<H> is printed to stderr (the chart height in CSS pixels).

What the chart shows, so a reader can judge it: one bar per framework, the best
server configuration of that framework on the row (celeris has nine
configurations, most frameworks one to three); the nine fastest frameworks plus
the best net/http entry at its true rank. Rows of the run are single 90 s
runs, so a gap of a few percent is not a ranking.
"""
import html
import json
import sys

LANG = {"go": "Go", "rust": "Rust", "c": "C", "cpp": "C++", "java": "Java", "node": "Node",
        "python": "Python", "csharp": "C#", "bun": "Bun", "zig": "Zig"}
DISPLAY = {"celeris": "Celeris", "gnet": "gnet", "lithium": "lithium", "h2o": "h2o", "actix-web": "actix",
           "uWebSockets.js": "uWS", "hyper": "hyper", "vertx": "Vert.x", "drogon": "drogon", "ntex": "ntex",
           "netty": "Netty", "axum": "axum", "net/http": "net/http", "fasthttp": "fasthttp", "fiber": "fiber",
           "aspnet": "ASP.NET"}
STD = {"stdhttp-h1", "stdhttp-h2", "stdhttp-hybrid"}


def main():
    path = sys.argv[1]
    scn = sys.argv[2] if len(sys.argv) > 2 else "get-simple-1024c"
    d = json.load(open(path))
    cfg = d["benchmark_config"]
    best = {}  # framework -> (rps, config name, language)
    for e in d["benchmarks"]:
        v = e["saturation_mode_rps"].get(scn)
        if not v:
            continue
        fw = "net/http" if e["name"] in STD else e["framework"]
        if v > best.get(fw, (0,))[0]:
            best[fw] = (v, e["name"], e["language"])
    ranked = sorted(best.items(), key=lambda kv: -kv[1][0])
    n_fw = len(ranked)
    n_srv = sum(1 for e in d["benchmarks"] if e["saturation_mode_rps"].get(scn))
    if ranked[0][0] != "celeris":
        sys.exit("celeris is not first on %s: the headline of this chart would be false" % scn)
    top = ranked[:9]
    std_i = [i for i, (fw, _) in enumerate(ranked) if fw == "net/http"][0]
    rows = [(i + 1,) + r for i, r in enumerate(top)]
    gap = std_i > len(top)
    rows.append((std_i + 1,) + ranked[std_i])
    celeris_rps = ranked[0][1][0]
    second = ranked[1]
    lead = celeris_rps / second[1][0] - 1
    vs_std = celeris_rps / ranked[std_i][1][0]
    arch = d["host_arch_pair"]
    version = d["benchmark_config"].get("git_ref") or ""
    date = cfg["started_at"][:10]
    dur = int(cfg["duration"] / 1e9)
    warm = int(cfg["warmup"] / 1e9)
    conns = scn.rsplit("-", 1)[-1].rstrip("c")

    W, left, bar_x, bar_w = 1280, 80, 280, 900
    top_y, row_h = 150, 40
    H = top_y + row_h * len(rows) + (28 if gap else 0) + 130
    out = []
    a = out.append
    a('<!doctype html><meta charset="utf-8"><link rel="preconnect" href="https://fonts.googleapis.com">'
      '<link href="https://fonts.googleapis.com/css2?family=Inter:wght@500;700;800&family=JetBrains+Mono:wght@400;500&display=swap" rel="stylesheet">'
      '<style>html,body{margin:0;background:#090C12}svg{display:block}</style>')
    a('<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" '
      'aria-label="Bar chart: saturation requests per second on %s">' % (W, H, W, H, html.escape(scn)))
    a('<defs><linearGradient id="bg" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#090C12"/>'
      '<stop offset="1" stop-color="#0D131E"/></linearGradient>'
      '<linearGradient id="hot" x1="0" y1="0" x2="1" y2="0"><stop offset="0" stop-color="#F0603A"/>'
      '<stop offset="1" stop-color="#FF9A52"/></linearGradient>'
      '<radialGradient id="glow" cx="0.3" cy="0" r="0.8"><stop offset="0" stop-color="#FF8A4C" stop-opacity="0.16"/>'
      '<stop offset="1" stop-color="#FF8A4C" stop-opacity="0"/></radialGradient>'
      '<pattern id="grid" width="28" height="28" patternUnits="userSpaceOnUse"><path d="M28 0H0V28" fill="none" '
      'stroke="#ffffff" stroke-opacity="0.025"/></pattern></defs>')
    a('<rect width="%d" height="%d" fill="url(#bg)"/><rect width="%d" height="%d" fill="url(#grid)"/>'
      '<rect width="%d" height="%d" fill="url(#glow)"/>' % (W, H, W, H, W, 360))
    mono = "font-family=\"'JetBrains Mono',ui-monospace,monospace\""
    sans = "font-family=\"Inter,system-ui,sans-serif\""
    a('<text x="%d" y="44" %s font-size="13" letter-spacing="2.4" fill="#FF8A4C">SATURATION THROUGHPUT · GET / · %s CONNECTIONS · %s</text>'
      % (left, mono, conns, html.escape(arch.upper())))
    a('<text x="%d" y="86" %s font-size="38" font-weight="800" fill="#F5F7FB">Celeris leads this row</text>' % (left, sans))
    a('<text x="%d" y="114" %s font-size="17" font-weight="500" fill="#FF9A52">#1 of %d frameworks · +%.0f%% over %s · %.1f× net/http</text>'
      % (left, sans, n_fw, lead * 100, html.escape(DISPLAY.get(second[0], second[0])), vs_std))
    a('<text x="%d" y="136" %s font-size="12.5" fill="#8A94A8">plaintext GET · saturation req/s · best configuration per framework · celeris %s · %s · one %d s run per cell</text>'
      % (left, mono, html.escape(version), html.escape(date), dur))
    a('<line x1="%d" x2="%d" y1="%d" y2="%d" stroke="#ffffff" stroke-opacity="0.07"/>' % (left, W - left, top_y - 6, top_y - 6))
    maxv = celeris_rps
    y = top_y
    prev_rank = 0
    for rank, fw, (v, name, lang) in rows:
        if gap and rank == std_i + 1:
            a('<text x="%d" y="%d" %s font-size="13" fill="#5C667A">… %d more frameworks</text>' % (bar_x, y + 14, mono, rank - len(top) - 1))
            y += 28
        hot = fw == "celeris"
        w = max(4, bar_w * v / maxv)
        a('<text x="%d" y="%d" %s font-size="15" font-weight="700" fill="%s" text-anchor="end">%d</text>'
          % (left + 20, y + 26, sans, "#FF8A4C" if hot else "#5C667A", rank))
        a('<text x="%d" y="%d" %s font-size="17" font-weight="%d" fill="%s">%s</text>'
          % (left + 36, y + 26, sans, 800 if hot else 500, "#FFFFFF" if hot else "#D5DAE5", html.escape(DISPLAY.get(fw, fw))))
        a('<text x="%d" y="%d" %s font-size="12" fill="#6B7589" text-anchor="end">%s</text>'
          % (bar_x - 10, y + 25, mono, html.escape("Go stdlib" if fw == "net/http" else LANG.get(lang, lang))))
        a('<rect x="%d" y="%d" width="%d" height="28" rx="6" fill="#121A27"/>' % (bar_x, y + 5, bar_w))
        a('<rect x="%d" y="%d" width="%.1f" height="28" rx="6" fill="%s"/>' % (bar_x, y + 5, w, "url(#hot)" if hot else "#334057"))
        a('<text x="%.1f" y="%d" %s font-size="15" font-weight="500" fill="%s" text-anchor="end">%s</text>'
          % (bar_x + w - 12 if w > 150 else bar_x + w + 70, y + 25, mono, "#1A0B04" if hot else "#E4E8F1", f"{round(v):,}"))
        y += row_h
    fy = y + 12
    a('<line x1="%d" x2="%d" y1="%d" y2="%d" stroke="#ffffff" stroke-opacity="0.07"/>' % (left, W - left, fy, fy))
    a('<text x="%d" y="%d" %s font-size="14" font-weight="500" fill="#C9D0DE">Best celeris configuration: %s. Single run, so a few percent is not a ranking.</text>'
      % (left, fy + 26, sans, html.escape(ranked[0][1][1])))
    a('<text x="%d" y="%d" %s font-size="12" fill="#6B7589">%d frameworks, %d server configurations on this row · %s · %d s warm-up · goceleris/probatorium cluster</text>'
      % (left, fy + 48, mono, n_fw, n_srv, html.escape(arch), warm))
    a('<text x="%d" y="%d" %s font-size="12" fill="#6B7589">Other rows differ, and not every one is a win: all scenarios are at goceleris.dev</text>'
      % (left, fy + 68, mono))
    a('</svg>')
    sys.stdout.write("".join(out))
    sys.stderr.write("window height: %d\n" % H)


if __name__ == "__main__":
    main()
