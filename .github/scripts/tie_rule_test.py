"""Tests of tie_rule.py and of the two README scripts on a cell composed from two dispatches.
Run: python3 .github/scripts/tie_rule_test.py   (or tie-rule-test.sh)"""
import json
import os
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import tie_rule  # noqa: E402

COMP = {
    "schema": "composition/1", "tie_floor": 0.05,
    "tie_margin_by_scenario": {"quiet": 0.05, "noisy": 0.12},
    "columns": {"celeris-a": "candidate", "fiber-h1": "candidate", "gin-h1": "peers", "echo-h1": "peers"},
}


class Standing(unittest.TestCase):
    def test_margins(self):
        self.assertEqual(tie_rule.margin(COMP, "noisy", "fiber-h1"), 0.05)  # same dispatch: floor
        self.assertEqual(tie_rule.margin(COMP, "noisy", "gin-h1"), 0.12)    # other dispatch: the scenario's
        self.assertEqual(tie_rule.margin(COMP, "quiet", "gin-h1"), 0.05)    # a quiet scenario is not decided by a noisy one
        self.assertEqual(tie_rule.margin(COMP, "unseen", "gin-h1"), 0.12)   # no margin: the widest
        self.assertEqual(tie_rule.margin(COMP, "quiet", "mystery"), 0.05)

    def test_standing(self):
        s = lambda scn, c, rivals: tie_rule.standing(COMP, scn, c, rivals)
        self.assertEqual(s("noisy", 111, [(100, "gin-h1")]), "tie")
        self.assertEqual(s("noisy", 113, [(100, "gin-h1")]), "win")
        self.assertEqual(s("quiet", 106, [(100, "gin-h1")]), "win")
        self.assertEqual(s("noisy", 106, [(100, "fiber-h1")]), "win")   # own dispatch: 5 %
        self.assertEqual(s("noisy", 108, [(100, "fiber-h1"), (100, "gin-h1")]), "tie")
        self.assertEqual(s("noisy", 100, [(113, "gin-h1")]), "loss")
        self.assertEqual(s("noisy", 100, [(110, "gin-h1")]), "tie")


def cell(scn_rates, arch):
    """A minimal published summary.json; scn_rates: {name: (framework, language, {scenario: rps})}."""
    return {
        "schema_version": "5.24", "host_arch_pair": "linux/" + ("amd64" if arch == "x86_64" else "arm64"),
        "benchmark_config": {"started_at": "2026-12-01T00:00:00Z", "finished_at": "2026-12-02T10:00:00Z", "runs": 1,
                             "duration": 90e9, "warmup": 20e9, "loadgen_version": "v1.4.15", "git_ref": "v1.6.0"},
        "benchmarks": [{"name": n, "framework": fw, "language": lang, "category": "celeris" if fw == "celeris" else "framework",
                        "saturation_mode_rps": rates} for n, (fw, lang, rates) in scn_rates.items()],
    }


def write_cell(root, arch, benchmarks, comp):
    d = os.path.join(root, "results", "v1.6.0", "20261201", arch)
    os.makedirs(d)
    with open(os.path.join(d, "summary.json"), "w") as f:
        json.dump(cell(benchmarks, arch), f)
    if comp:
        with open(os.path.join(d, "composition.json"), "w") as f:
            json.dump(comp, f)
    return os.path.join(d, "summary.json")


# celeris is 111 vs gin (other dispatch) and 106 vs fiber (own dispatch) on "noisy"; 106 vs gin on "quiet"
BENCH = {
    "celeris-iouring-h1-async": ("celeris", "go", {"noisy": 111, "quiet": 106, "get-simple-1024c": 500}),
    "celeris-epoll-h1-async": ("celeris", "go", {"noisy": 105, "quiet": 101, "get-simple-1024c": 450}),
    "fiber-h1": ("fiber", "go", {"noisy": 100, "quiet": 100, "get-simple-1024c": 100}),
    "gin-h1": ("gin", "go", {"noisy": 100, "quiet": 100, "get-simple-1024c": 100}),
    "stdhttp-h1": ("net/http", "go", {"noisy": 50, "quiet": 50, "get-simple-1024c": 50}),
}


def run(script, *args):
    return subprocess.run([sys.executable, os.path.join(HERE, script), *args], capture_output=True, text=True)


def tally(out, mode="native"):
    for line in out.splitlines():
        if line.startswith(mode + " same, excluding"):
            return json.loads(line.split("walkovers: ")[1].split(" rows")[0].replace("'", '"'))
    raise AssertionError(out)


class Scripts(unittest.TestCase):
    def analyze(self, comp):
        with tempfile.TemporaryDirectory() as root:
            for arch in ("x86_64", "arm64"):
                write_cell(root, arch, BENCH, comp)
            r = run("analyze-published-results.py", root, "v1.6.0", "20261201")
            self.assertEqual(r.returncode, 0, r.stderr)
            return tally(r.stdout)

    def test_single_dispatch_is_unchanged(self):
        # no composition.json: the old 5 % band on the best rival: noisy 111/100 win, quiet 106/100 win
        t = self.analyze(None)
        self.assertEqual((t["win"], t["tie"], t["loss"]), (3, 0, 0))

    def test_composed_cell_applies_the_scenario_margin(self):
        t = self.analyze(dict(COMP, tie_margin_by_scenario={"quiet": 0.05, "noisy": 0.12, "get-simple-1024c": 0.05}))
        # noisy: +11 % over gin from the other dispatch (margin 12 %) is a tie; quiet and the hero row win
        self.assertEqual((t["win"], t["tie"], t["loss"]), (2, 1, 0))

    def hero(self, comp, scn):
        with tempfile.TemporaryDirectory() as root:
            path = write_cell(root, "x86_64", BENCH, comp)
            return run("benchmark-hero.py", path, scn)

    def test_hero_single_dispatch(self):
        self.assertEqual(self.hero(None, "noisy").returncode, 0)

    def test_hero_refuses_a_headline_inside_the_tie_margin(self):
        r = self.hero(COMP, "noisy")
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("not clearly ahead", r.stderr)

    def test_hero_accepts_a_clear_lead(self):
        r = self.hero(COMP, "quiet")  # 106 vs gin (other dispatch, 5 %) and fiber (own dispatch, 5 %)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("Celeris leads this row", r.stdout)

    def test_a_non_composition_file_is_refused(self):
        with tempfile.TemporaryDirectory() as root:
            path = write_cell(root, "x86_64", BENCH, {"schema": "other"})
            r = run("benchmark-hero.py", path, "quiet")
            self.assertNotEqual(r.returncode, 0)


if __name__ == "__main__":
    unittest.main()
