"""Tests of reader.py. Run: python3 -I tools/idlecell/reader_test.py
1. the laptop-VM raw data of celeris#1485 (m1485, SUMMARY.md sections 2, 4, 6: testdata/) reproduces SUMMARY.md's figures;
2. the decision rule, on synthetic cells with perf and energy: every verdict, and a figure that is missing is never read as equal."""
import os
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import reader  # noqa: E402

TD = os.path.join(HERE, "testdata")


def run(*args):
    return subprocess.run([sys.executable, "-I", os.path.join(HERE, "reader.py"), *args], capture_output=True, text=True, check=True).stdout


class OldData(unittest.TestCase):
    def test_summary_section_2(self):
        out = run("gate", os.path.join(TD, "m1485-b"), "--expect", "0,1000,10000,50000")
        for want in ("| 1,000 | 1.90 [1.85..1.90] (1.85, 1.90, 1.90) | 2.45 [2.35..2.55] (2.45, 2.55, 2.35) |",
                     "| 10,000 | 3.05 [3.05..3.20]", "| 10,000 | 3.05 [3.05..3.20] (3.20, 3.05, 3.05) | 2.55 [2.40..2.75] (2.75, 2.55, 2.40) |",
                     "| 50,000 | main | 226.8 [225.7..227.9] | 18.304", "| 1,000 | head | 226.7 [225.0..226.9] | 1.275",
                     "| 0 | head | 10.6 [10.4..10.8] | 0.187"):
            self.assertIn(want, out)
        self.assertIn("head - main | head-aw - head", out)
        self.assertIn("| 1,000 | +0.60, +0.65, +0.45 |", out)  # head higher in every pass (SUMMARY: -0.60, -0.65, -0.45 as main - head)
        self.assertIn("**OVERALL: UNDECIDED, INCOMPLETE", out)  # the laptop run had no main-twin: no tolerance, so no decision
        self.assertIn("main-twin pass 1,2,3 (no result)", out)
        self.assertNotIn("at every count", out)

    def test_summary_section_6(self):
        out = run("gate", os.path.join(TD, "m1485-bdiag"), "--alias", "main-d=main,head-d=head")
        self.assertIn("| 1,000 | 1.30 [1.30..1.35] (1.30, 1.30, 1.35) | 2.40 [2.35..2.40] (2.40, 2.35, 2.40) | 1.20 [1.15..1.30] (1.30, 1.20, 1.15) | 2.45 [2.35..2.55] (2.55, 2.35, 2.45) |", out)
        self.assertIn("| 100 | main | 227.7 [226.1..228.9] | 1.106 [1.065..1.117] | 16.2 [15.5..16.5]", out)
        self.assertIn("| 1,000 | head-aw | 227.5 [224.8..227.8] | 0.506 [0.494..0.526] | 7.4 [7.3..7.7]", out)  # us per loop timeslice from SCHED2
        self.assertIn("| 1,000 | main-skip | 225.6 [224.1..227.0] | 1.235 [1.183..1.314] | 18.2 [17.4..19.5]", out)

    def test_summary_section_4(self):
        out = run("held", os.path.join(TD, "m1485-d"), "real-before", "real-after")
        self.assertIn("| new connection | 10 | real-before -> real-after | 0.39 -> 0.71 ms | **+0.13 [-0.05..+0.66] ms** | 9 of 10 | p = 0.021 | 1.97 / 3.14 ms |", out)
        self.assertIn("| held connection | 10 | real-before -> real-after | 0.17 -> 0.26 ms | **+0.12 [-0.01..+0.21] ms** | 9 of 10 | p = 0.021 | 0.67 / 0.87 ms |", out)
        twin = run("held", os.path.join(TD, "m1485-d"), "twin-tw1", "twin-tw2")
        self.assertIn("**+0.11 [-0.32..+0.40] ms** | 6 of 10 | p = 0.754", twin)
        self.assertIn("**+0.00 [-0.07..+0.10] ms** | 6 of 10 | p = 0.754", twin)


def cell(d, tag, cpu, ins=None, w=None, wake=230.0, kern=True):
    """one synthetic cell: an IDLE line, and optionally perf (user and kernel instructions, in M/s) and an energy line (W)"""
    lines = ["READY engine=epoll", "IDLE engine=epoll workers=0 cpus=4 conns=1000 window=20s loop_threads=3 process_cpu_pct=%.3f loop_wakeups_per_s=[%.1f %.1f %.1f] "
             "other_threads_voluntary_per_s=100.0 sched_cpu_pct=1.0 sched_loop_cpu_pct=0.6 sched_other_cpu_pct=0.4 rss_mib=20 win_start_ms=1 win_end_ms=2" % (cpu, wake, wake, wake),
             "SHUTDOWN engine=epoll shutdown=1ms start_err=<nil>"]
    if w is not None:
        lines.append("ENERGY src=powercap-intel-rapl:0-package-0 kind=counter start_uj=1 end_uj=2 max_range_uj=3 window_s=20.000 joules=1 watts=%.4f" % w)
    with open(os.path.join(d, tag + ".srv.txt"), "w") as f:
        f.write("\n".join(lines) + "\n")
    if ins is not None:
        with open(os.path.join(d, tag + ".perf.txt"), "w") as f:
            total = ins * 1e6 * 20  # M/s over the 20 s window
            u, k_ = (total * 0.8, total * 0.2) if kern else (total, 0)
            k = "%d,,instructions:k,20000000000,100.00,,\n" % k_ if kern else "<not supported>,,instructions:k,0,100.00,,\n"
            f.write("20000000000,ns,duration_time,20000000000,100.00,,\n%d,,instructions:u,20000000000,100.00,,\n" % u + k)


def three(x):
    return x if isinstance(x, list) else [x] * 3


class Decision(unittest.TestCase):
    def verdict(self, main, head, twin=None, n=1000, base=10.0, expect=None, skip_twin=False, extra_args=()):
        """main/head/twin: dict(cpu=, ins=, w=, kern=) each a number or a list of three (one per pass); twin defaults to main"""
        twin = twin or main
        with tempfile.TemporaryDirectory() as d:
            for p in (1, 2, 3):
                if base is not None:
                    cell(d, "baseline-n0-pass%d" % p, 0.3, w=base)
                for v, spec in (("main", main), ("head", head), ("main-twin", twin)):
                    if v == "main-twin" and skip_twin:
                        continue
                    pick = lambda key: (three(spec[key])[p - 1] if spec.get(key) is not None else None)
                    cell(d, "%s-n%d-pass%d" % (v, n, p), pick("cpu"), pick("ins"), pick("w"), kern=spec.get("kern", True))
            return run("gate", d, "--expect", expect or str(n), *extra_args)

    def test_no_loss_all_measured(self):
        out = self.verdict(dict(cpu=2.0, ins=5.0, w=10.5), dict(cpu=1.5, ins=4.0, w=10.4))
        self.assertIn("**OVERALL: NO LOSS: head is at or below main at every count (1,000)", out)

    def test_rule_i_cpu_only(self):
        out = self.verdict(dict(cpu=2.0), dict(cpu=2.0), base=None)
        self.assertIn("**OVERALL: NO LOSS ON CPU ONLY (instructions and energy not measured)", out)
        self.assertNotIn("**OVERALL: NO LOSS:", out)

    def test_rule_ii_cpu_higher_nothing_measured_is_undecided(self):
        out = self.verdict(dict(cpu=1.9), dict(cpu=2.4), base=None)
        self.assertIn("**OVERALL: UNDECIDED at 1,000", out)
        self.assertIn("NOT MEASURED: instructions, energy", out)
        self.assertNotIn("ACCOUNTING", out)
        self.assertNotIn("REAL LOSS", out)

    def test_cpu_higher_user_only_instructions_no_energy_is_undecided(self):
        out = self.verdict(dict(cpu=1.9, ins=5.0, kern=False), dict(cpu=2.4, ins=5.0, kern=False), base=None)
        self.assertIn("UNDECIDED (head's CPU is higher; NOT MEASURED: kernel-side instructions (user mode only), energy)", out)
        self.assertNotIn("ACCOUNTING", out)

    def test_accounting_effect_only_when_everything_is_measured(self):
        out = self.verdict(dict(cpu=1.9, ins=5.0, w=10.5), dict(cpu=2.4, ins=5.0, w=10.5))
        self.assertIn("**OVERALL: ACCOUNTING EFFECT at 1,000", out)

    def test_rule_iii_real_loss_has_its_numbers_beside_it(self):
        out = self.verdict(dict(cpu=1.9, ins=5.0, w=10.5), dict(cpu=2.4, ins=6.0, w=10.5))
        self.assertIn("N = 1,000: REAL LOSS (instructions higher on head, measured). CPU main 1.90 head 2.40", out)
        self.assertIn("instructions main 5.00 head 6.00 M/s (+20.0 %", out)
        self.assertIn("twin spread", out)
        self.assertIn("NO MERGE: the bot takes the table to Albert", out)

    def test_energy_clear_margin_is_a_real_loss_with_numbers(self):
        out = self.verdict(dict(cpu=1.9, ins=5.0, w=10.50), dict(cpu=1.9, ins=5.0, w=11.20))
        self.assertIn("REAL LOSS (energy higher on head, measured)", out)
        self.assertIn("energy main 10.500 head 11.200 W (+0.700 W", out)

    def test_user_only_instructions_higher_is_a_real_loss_and_says_user_only(self):
        out = self.verdict(dict(cpu=1.9, ins=5.0, kern=False), dict(cpu=1.9, ins=6.0, kern=False), base=None)
        self.assertIn("REAL LOSS (instructions higher on head, measured)", out)
        self.assertIn("instructions (user mode only)", out)

    def test_real_loss_only_at_high_n_does_not_say_no_merge(self):
        out = self.verdict(dict(cpu=19.0, ins=5.0, w=10.5), dict(cpu=22.0, ins=6.0, w=10.5), n=10000)
        self.assertIn("**OVERALL: REAL LOSS at 10,000 (not at 100 to 1,000; numbers above). No merge: the table goes to the bot.**", out)

    def test_one_tick_is_not_higher_whatever_the_float_rounding(self):
        # twin identical to main: spread 0, the floor (one tick, 0.05) applies and a difference of exactly one tick is NOT higher
        for m, h in ((1.90, 1.95), (0.25, 0.30), (2.40, 2.45), (1.05, 1.10), (2.35, 2.40)):
            out = self.verdict(dict(cpu=m), dict(cpu=h), base=None)
            self.assertIn("NO LOSS ON CPU ONLY", out, "main %.2f head %.2f" % (m, h))
            self.assertIn("(0: the documented floor 0.05 point is used)", out)
        out = self.verdict(dict(cpu=1.90), dict(cpu=2.00), base=None)
        self.assertIn("UNDECIDED", out)

    def test_the_tolerance_is_the_twins_spread(self):
        # the twin differs from main by 0.20 in one pass: head + 0.15 is inside the noise, head + 0.30 is outside
        twin = dict(cpu=[2.2, 2.0, 2.0])
        inside = self.verdict(dict(cpu=2.0), dict(cpu=2.15), twin, base=None)
        self.assertIn("NO LOSS ON CPU ONLY", inside)
        self.assertIn("twin spread 0.2 point", inside)
        outside = self.verdict(dict(cpu=2.0), dict(cpu=2.30), twin, base=None)
        self.assertIn("UNDECIDED", outside)

    def test_a_missing_twin_or_count_is_named_and_never_reads_as_no_loss(self):
        out = self.verdict(dict(cpu=2.0), dict(cpu=2.0), base=None, skip_twin=True)
        self.assertIn("**OVERALL: UNDECIDED, INCOMPLETE", out)
        self.assertIn("main-twin pass 1,2,3", out)
        out = self.verdict(dict(cpu=2.0), dict(cpu=2.0), base=None, expect="100,1000")
        self.assertIn("N = 100: NOT COMPARED", out)
        self.assertIn("Counts NOT COMPARED: 100", out)
        self.assertNotIn("at every count", out)

    def test_head_above_in_only_one_pass_is_not_higher(self):
        out = self.verdict(dict(cpu=2.0), dict(cpu=[2.6, 1.9, 1.9]), base=None)
        self.assertIn("NO LOSS ON CPU ONLY", out)

    def test_heterogeneous_perf_lines_are_summed(self):
        ev = {"duration_time": 2e10, "cpu_core/instructions/u": 3e9, "cpu_atom/instructions/u": 1e9, "cpu_core/instructions/k": 5e8, "cpu_core/cycles/u": None, "instructions:k": None}
        self.assertEqual(reader.perf_sum(ev, "instructions", "u"), 4e9)
        self.assertEqual(reader.perf_sum(ev, "instructions", "k"), 5e8)
        self.assertIsNone(reader.perf_sum(ev, "cycles", "u"))
        self.assertAlmostEqual(reader.per_second(ev, "instructions", "u", 20.0), 2e8)

    def test_gt_is_exact_in_ticks(self):
        self.assertFalse(reader.gt(1.95 - 1.90, 0.05))
        self.assertFalse(reader.gt(2.45 - 2.40, 0.05))
        self.assertFalse(reader.gt(0.30 - 0.25, 0.05))
        self.assertTrue(reader.gt(2.00 - 1.90, 0.05))


if __name__ == "__main__":
    unittest.main()
