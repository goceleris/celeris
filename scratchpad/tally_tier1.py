#!/usr/bin/env python3
"""Tally Tier 1 (celeris#583) logs: per engine/arm/cell from the post-settle
TIER1-CELL lines, per-connection strata from TIER1-CONN lines (inq_before
bucketed at the 32 KiB drain cap), and run exit codes."""
import glob
import os
import re
import sys
from collections import defaultdict

CAP = 32768
root = sys.argv[1]
kv = re.compile(r'(\w+)=([^ ]+)')


def parse(line):
    d = {}
    for k, v in kv.findall(line):
        d[k] = v
    return d


cells = defaultdict(lambda: defaultdict(int))
cellruns = defaultdict(int)
exits = defaultdict(lambda: defaultdict(int))
conns = defaultdict(lambda: defaultdict(int))
inqrange = defaultdict(lambda: [None, None])
uninformative = defaultdict(int)
sumfields = ['conns', 'joined', 'unjoinedProbes', 'probeTimeouts', 'stoppedOK', 'pauseSeen', 'blockedNoPause',
             'writeBlocked', 'writeErrs', 'inqBeforePos', 'inqAfterPos', 'outqDataPos', 'EOF', 'RST', 'timeout',
             'other', 'soErrEPIPE', 'soErrECONNRESET', 'soErr0', 'closeFrameRx', 'fullRx', 'truncTail',
             'handlerReadErr', 'handlerEchoErr', 'closeNowTimeout', 'harnessFail', 'abortOnClose', 'abortOnData',
             'drainedSum', 'inqBeforeSum']

for path in sorted(glob.glob(os.path.join(root, '*_run*.log'))):
    base = os.path.basename(path)
    m = re.match(r'(.+)_(on|off)_run(\d+)\.log', base)
    if not m:
        continue
    eng, arm, run = m.group(1), m.group(2), int(m.group(3))
    txt = open(path, errors='replace').read()
    ex = re.search(r'RUN-EXIT=(\d+)', txt)
    exits[(eng, arm)][ex.group(1) if ex else 'none'] += 1
    for line in txt.splitlines():
        if 'TIER1-CELL ' in line:
            d = parse(line)
            key = (d['engine'], d['arm'], d['cell'])
            cellruns[key] += 1
            for f in sumfields:
                if f in d:
                    cells[key][f] += int(d[f])
            if d.get('informative') == 'false':
                uninformative[key] += 1
            lo, hi = int(d['inqBeforeMin']), int(d['inqBeforeMax'])
            r = inqrange[key]
            r[0] = lo if r[0] is None else min(r[0], lo)
            r[1] = hi if r[1] is None else max(r[1], hi)
            cells[key]['workers_min'] = min(cells[key].get('workers_min', 99), int(d['workers']))
        elif 'TIER1-CONN ' in line:
            d = parse(line)
            if d.get('probe') != 'true':
                continue
            inqb = int(d['inq_before'])
            bucket = 'inq0' if inqb == 0 else ('inq<=cap' if inqb <= CAP else 'inq>cap')
            key = (d['engine'], d['arm'], d['cell'], bucket)
            c = conns[key]
            c['n'] += 1
            c['peer_' + d['peer']] += 1
            c['soErr_' + d['soErr']] += 1
            c['inqAfterPos'] += int(int(d['inq_after']) > 0)
            c['outqData'] += int(int(d['outq']) > 1)
            c['closeRx'] += int(d['closeRx'] == 'true')
            c['fullRx'] += int(d['bytesRx'] == d['expectedRx'])
            c['drained'] += int(d['drained'])

print('== run exit codes ==')
for k in sorted(exits):
    print(k, dict(exits[k]))

print('\n== per engine/arm/cell (sums over runs; from post-settle TIER1-CELL lines) ==')
hdr = ['runs', 'workers_min', 'conns', 'joined', 'pauseSeen', 'blockedNoPause', 'writeBlocked', 'inqBeforePos',
       'inqAfterPos', 'outqDataPos', 'EOF', 'RST', 'timeout', 'soErrEPIPE', 'closeFrameRx', 'fullRx',
       'abortOnClose', 'abortOnData', 'handlerReadErr', 'harnessFail', 'probeTimeouts', 'unjoinedProbes']
for key in sorted(cells):
    c = cells[key]
    n = cellruns[key]
    j = c['joined'] or 1
    print('%-20s %-9s %-6s' % key,
          ' '.join('%s=%s' % (h, (n if h == 'runs' else c.get(h, 0))) for h in hdr),
          'P(inq_before>0)=%.3f' % (c['inqBeforePos'] / j),
          'P(inq_after>0)=%.3f' % (c['inqAfterPos'] / j),
          'P(outq>1)=%.3f' % (c['outqDataPos'] / j),
          'inqBefore[min,max]=%s' % inqrange[key],
          'uninformativeRuns=%d' % uninformative[key])

print('\n== per-connection strata by inq_before vs the 32 KiB cap (joined closes only) ==')
for key in sorted(conns):
    c = conns[key]
    n = c['n']
    print('%-20s %-9s %-6s %-8s' % key,
          'n=%d EOF=%d RST=%d timeout=%d soErrEPIPE=%d soErr0=%d inqAfterPos=%d outqData=%d closeFrameRx=%d fullRx=%d drainedAvg=%d' % (
              n, c['peer_EOF'], c['peer_ECONNRESET'], c['peer_timeout'], c['soErr_EPIPE'], c['soErr_0'],
              c['inqAfterPos'], c['outqData'], c['closeRx'], c['fullRx'], c['drained'] // max(n, 1)))
