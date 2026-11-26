"""The tie rule of a published cell composed from two dispatches (probatorium tools/mergedocs).

A composed cell holds the candidate dispatch (celeris, its v1.5.11 twin, the drift anchors) and the
frameworks dispatch of one architecture, and carries composition.json next to summary.json. Celeris
counts as ahead of a framework from the OTHER dispatch only when its margin exceeds the tie margin of
that scenario (the largest drift any anchor showed on it, floored at 5 %); against a framework of its
own dispatch the 5 % band holds. A cell without composition.json is a single dispatch: the callers
keep their own 5 % band on the best rival, so existing numbers do not move.
"""
import json
import os

FLOOR = 0.05


def load(summary_path):
    """composition.json beside summary.json, or None (single-dispatch cell, or unreadable)."""
    p = os.path.join(os.path.dirname(summary_path), "composition.json")
    if not os.path.exists(p):
        return None
    with open(p) as f:
        c = json.load(f)
    if c.get("schema") != "composition/1" or "tie_margin_by_scenario" not in c or "columns" not in c:
        raise SystemExit("%s: not a composition/1 file" % p)
    return c


def single_dispatch():
    """The rule of a cell without composition.json: every column is one dispatch, the 5 % band holds."""
    return {"schema": "composition/1", "tie_floor": FLOOR, "tie_margin_by_scenario": {}, "columns": {}}


def margin(comp, scn, name):
    """The margin celeris must exceed over column `name` on scenario `scn`."""
    floor = comp.get("tie_floor") or FLOOR
    if comp["columns"].get(name) == "candidate":
        return floor
    m = comp["tie_margin_by_scenario"]
    if scn in m:
        return max(floor, m[scn])
    return max([floor] + list(m.values()))  # a scenario without a margin takes the widest


def standing(comp, scn, celeris, rivals):
    """'win', 'tie' or 'loss' for celeris' best rate against rivals [(rate, column name)] of a
    composed cell: a win clears every rival by that rival's margin, a loss means some rival clears
    celeris by its margin."""
    win = True
    for v, name in rivals:
        m = margin(comp, scn, name)
        if v > celeris * (1 + m):
            return "loss"
        if not celeris > v * (1 + m):
            win = False
    return "win" if win else "tie"
