#!/usr/bin/env python3
"""Numbers quoted in the README's Performance section, derived from the published results in
goceleris/docs (results/<version>/<date>/<arch>/summary.json).

Usage: analyze-published-results.py <docs-clone-dir> [version [date]]   (default v1.5.8 20260829)

Method, so the counts can be judged: per scenario, the best celeris configuration is compared with the best
configuration of any other Go *framework* (fiber, gin, echo, chi, iris, hertz; gnet, fasthttp and nbio are
libraries and net/http is the stdlib, both excluded as in the docs site). Within 5% counts as a tie. Rows
where no other Go framework ran (the six WebSocket/SSE rows) are walkovers and are not counted. The HTTP/2 rows
and churn-close are left out of the second tally because loadgen v1.4.14 fixed the clients behind them. One
90 s run per cell, so the 5% band is a convention, not a confidence interval."""
import json, statistics, sys
root = sys.argv[1]
VERSION = sys.argv[2] if len(sys.argv) > 2 else "v1.5.8"
DATE = sys.argv[3] if len(sys.argv) > 3 else "20260829"
LIBS = {"gnet-h1","fasthttp-h1","nbio-h1","gorilla_ws","hyper","hyper-h2","h2o","lithium","uws","bunraw"}
STD = {"stdhttp-h1","stdhttp-h2","stdhttp-hybrid"}
for arch in ("x86_64","arm64"):
    d = json.load(open(f"{root}/results/{VERSION}/{DATE}/{arch}/summary.json"))
    bc = d["benchmark_config"]
    print(f"=== {arch}: {d['host_arch_pair']} run {bc['started_at']}..{bc['finished_at']} runs={bc['runs']} duration={bc['duration']/1e9:.0f}s warmup={bc['warmup']/1e9:.0f}s loadgen={bc.get('loadgen_version')} servers={len(d['benchmarks'])}")
    B = {e["name"]: e for e in d["benchmarks"]}
    scn = sorted({s for e in d["benchmarks"] for s in e["saturation_mode_rps"]})
    cel = {n:e for n,e in B.items() if e["framework"]=="celeris"}
    # best per framework on the hero row
    row = "get-simple-1024c"
    best = {}
    for n,e in B.items():
        v = e["saturation_mode_rps"].get(row)
        if v: best[e["framework"]] = max(best.get(e["framework"],(0,""))[0], v), n
    byfw = {}
    for n,e in B.items():
        v = e["saturation_mode_rps"].get(row)
        if v and v > byfw.get(e["framework"],(0,""))[0]: byfw[e["framework"]] = (v,n)
    rank = sorted(byfw.items(), key=lambda kv:-kv[1][0])
    print(f"hero row {row}: {len(rank)} frameworks with data")
    for i,(fw,(v,n)) in enumerate(rank[:12],1): print(f"  {i:2d} {fw:16s} {v:12.0f} {n}")
    nh = byfw.get("net/http") or max((B[n]["saturation_mode_rps"].get(row,0),n) for n in STD if n in B)
    print("  stdlib best:", nh, "rank", [fw for fw,_ in rank].index(next(f for f,(v,n) in byfw.items() if n==nh[1]))+1 if isinstance(nh,tuple) else "")
    print("  celeris configs:", sorted(((round(e['saturation_mode_rps'].get(row,0)),n) for n,e in cel.items()), reverse=True))
    # io_uring vs epoll per row (h1-async pair), ratio
    ratios=[]
    for s in scn:
        i = B.get("celeris-iouring-h1-async",{}).get("saturation_mode_rps",{}).get(s)
        e = B.get("celeris-epoll-h1-async",{}).get("saturation_mode_rps",{}).get(s)
        if i and e: ratios.append((i/e,s))
    r = sorted(ratios)
    print(f"iouring-h1-async / epoll-h1-async over {len(r)} rows: median {statistics.median(x for x,_ in r):.3f} min {r[0]} max {r[-1]}")
    # frameworks vs libs: per-row best celeris vs best other Go framework (the docs rule), tally
    def lang(n): return B[n]["language"]
    wins=ties=loss=walk=0
    detail=[]
    for s in scn:
        bc_=max((e["saturation_mode_rps"].get(s,0) for n,e in cel.items()), default=0)
        rivals=[(e["saturation_mode_rps"].get(s,0),n) for n,e in B.items() if e["language"]=="go" and e["framework"]!="celeris" and n not in LIBS and n not in STD]
        rv,rn = max(rivals, default=(0,""))
        if bc_<=0: continue
        if rv<=0: walk+=1; detail.append((s,"walkover")); continue
        ratio=bc_/rv
        k = "win" if ratio>1.05 else ("loss" if ratio<0.95 else "tie")
        detail.append((s,k,round(ratio,3),rn))
        wins+= k=="win"; ties+= k=="tie"; loss+= k=="loss"
    EXCL={"get-json-h2","post-4k-h2","churn-close"}  # rows measured by loadgen v1.4.13 whose client was fixed in v1.4.14 (h2 client, -close)
    t={"win":0,"tie":0,"loss":0}
    for x in detail:
        if x[0] in EXCL or x[1]=="walkover": continue
        t[x[1]]+=1
    print("same, excluding h2c rows, churn-close and walkovers:", t, "rows", sum(t.values()))
    drv=[x for x in detail if x[0].startswith("driver-")]
    print("driver rows:", {k:sum(1 for x in drv if x[1]==k) for k in ("win","tie","loss")}, "of", len(drv))
    print(f"Go-framework comparison (best celeris config vs best other Go framework, +-5% = tie, single run): win {wins} tie {ties} loss {loss} walkover {walk} rows {len(scn)}")
    for x in detail: print("   ",x)

# ---- spread over every published cell of the version: how much the same quantities move between runs.
import glob
print("\n=== every published cell of", VERSION)
for path in sorted(glob.glob(f"{root}/results/{VERSION}/*/*/summary.json")):
    d = json.load(open(path))
    B = d["benchmarks"]; cel = {n: e for n, e in B.items()} if False else None
    Bn = {e["name"]: e for e in B}
    celc = [e for e in B if e["framework"] == "celeris"]
    scn = sorted({s for e in B for s in e["saturation_mode_rps"]})
    best = {}
    for e in B:
        v = e["saturation_mode_rps"].get("get-simple-1024c")
        if v: best[e["framework"]] = max(best.get(e["framework"], 0), v)
    ratio = best["celeris"] / best["gnet"] if "gnet" in best else None
    t = {"win": 0, "tie": 0, "loss": 0}; drv = {"win": 0, "tie": 0, "loss": 0}
    for s in scn:
        if s in {"get-json-h2", "post-4k-h2", "churn-close"}: continue
        bc_ = max((e["saturation_mode_rps"].get(s, 0) for e in celc), default=0)
        rv = max((e["saturation_mode_rps"].get(s, 0) for e in B if e["language"] == "go" and e["framework"] != "celeris"
                  and e["name"] not in LIBS and e["name"] not in STD), default=0)
        if bc_ <= 0 or rv <= 0: continue
        k = "win" if bc_ / rv > 1.05 else ("loss" if bc_ / rv < 0.95 else "tie")
        t[k] += 1
        if s.startswith("driver-"): drv[k] += 1
    cfg = d["benchmark_config"]
    print(f"{path.split(VERSION + '/')[1]:34s} start {cfg['started_at'][:10]}  hero celeris/gnet {ratio:.3f}  rows {t}  driver rows {drv}")
