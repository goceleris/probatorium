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
        out = run("gate", os.path.join(TD, "m1485-b"))
        for want in ("| 1,000 | 1.90 [1.85..1.90] (1.85, 1.90, 1.90) | 2.45 [2.35..2.55] (2.45, 2.55, 2.35) |",
                     "| 10,000 | 3.05 [3.05..3.20]", "| 10,000 | 3.05 [3.05..3.20] (3.20, 3.05, 3.05) | 2.55 [2.40..2.75] (2.75, 2.55, 2.40) |",
                     "| 50,000 | main | 226.8 [225.7..227.9] | 18.304", "| 1,000 | head | 226.7 [225.0..226.9] | 1.275",
                     "| 0 | head | 10.6 [10.4..10.8] | 0.187"):
            self.assertIn(want, out)
        self.assertIn("head - main | head-aw - head", out)
        self.assertIn("| 1,000 | +0.60, +0.65, +0.45 |", out)  # head higher in every pass (SUMMARY: -0.60, -0.65, -0.45 as main - head)
        self.assertIn("**OVERALL: UNDECIDED at 1,000", out)  # no perf and no energy in the laptop VM

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


def cell(d, tag, cpu, ins=None, w=None, wake=230.0):
    """one synthetic cell: an IDLE line, and optionally perf (instructions, user only) and an energy line (W)"""
    lines = ["READY engine=epoll", "IDLE engine=epoll workers=0 cpus=4 conns=1000 window=20s loop_threads=3 process_cpu_pct=%.3f loop_wakeups_per_s=[%.1f %.1f %.1f] "
             "other_threads_voluntary_per_s=100.0 sched_cpu_pct=1.0 sched_loop_cpu_pct=0.6 sched_other_cpu_pct=0.4 rss_mib=20 win_start_ms=1 win_end_ms=2" % (cpu, wake, wake, wake),
             "SHUTDOWN engine=epoll shutdown=1ms start_err=<nil>"]
    if w is not None:
        lines.append("ENERGY src=powercap-intel-rapl:0-package-0 kind=counter start_uj=1 end_uj=2 max_range_uj=3 window_s=20.000 joules=1 watts=%.4f" % w)
    with open(os.path.join(d, tag + ".srv.txt"), "w") as f:
        f.write("\n".join(lines) + "\n")
    if ins is not None:
        with open(os.path.join(d, tag + ".perf.txt"), "w") as f:
            f.write("20000000000,ns,duration_time,20000000000,100.00,,\n%d,,instructions:u,20000000000,100.00,,\n<not supported>,,instructions:k,0,100.00,,\n" % (ins * 20))


class Decision(unittest.TestCase):
    def verdict(self, main, head, n=1000, base=10.0):
        """main/head: dict(cpu, ins, w) for three passes"""
        with tempfile.TemporaryDirectory() as d:
            for p in (1, 2, 3):
                if base is not None:
                    cell(d, "baseline-n0-pass%d" % p, 0.3, w=base)
                for v, spec in (("main", main), ("head", head)):
                    cell(d, "%s-n%d-pass%d" % (v, n, p), spec["cpu"][p - 1], spec.get("ins", [None] * 3)[p - 1], None if spec.get("w") is None else spec["w"][p - 1])
            return run("gate", d)

    def test_no_loss(self):
        out = self.verdict(dict(cpu=[2.0, 2.0, 2.0], ins=[5e6] * 3, w=[10.5] * 3), dict(cpu=[1.5, 1.6, 1.5], ins=[4e6] * 3, w=[10.4] * 3))
        self.assertIn("**OVERALL: NO LOSS", out)

    def test_accounting_effect(self):
        out = self.verdict(dict(cpu=[1.9, 1.9, 1.9], ins=[5e6] * 3, w=[10.5] * 3), dict(cpu=[2.4, 2.5, 2.4], ins=[5e6] * 3, w=[10.5] * 3))
        self.assertIn("ACCOUNTING EFFECT (head's CPU is higher, instructions measured and energy measured are not)", out)
        self.assertIn("**OVERALL: ACCOUNTING EFFECT at 1,000", out)

    def test_real_loss_at_low_n_is_no_merge(self):
        out = self.verdict(dict(cpu=[1.9] * 3, ins=[5e6] * 3, w=[10.5] * 3), dict(cpu=[2.4] * 3, ins=[6e6] * 3, w=[10.5] * 3))
        self.assertIn("REAL LOSS (head's CPU is higher and instructions higher)", out)
        self.assertIn("NO MERGE: the bot takes the table to Albert", out)
        self.assertIn("W above idle", out)

    def test_user_only_instructions_are_said(self):
        out = self.verdict(dict(cpu=[1.9] * 3, ins=[5e6] * 3, w=[10.5] * 3), dict(cpu=[2.4] * 3, ins=[5e6] * 3, w=[10.5] * 3))
        self.assertIn("instructions counted in USER mode only", out)

    def test_energy_alone_is_a_real_loss(self):
        out = self.verdict(dict(cpu=[1.9] * 3, ins=[5e6] * 3, w=[10.5] * 3), dict(cpu=[2.4] * 3, ins=[5e6] * 3, w=[10.8] * 3))
        self.assertIn("higher and energy higher", out)

    def test_real_loss_only_at_high_n_is_not_a_merge_block(self):
        out = self.verdict(dict(cpu=[19.0] * 3, ins=[5e6] * 3, w=[10.5] * 3), dict(cpu=[22.0] * 3, ins=[6e6] * 3, w=[10.5] * 3), n=10000)
        self.assertIn("**OVERALL: REAL LOSS at 10,000 (not at 100 to 1,000). The bot decides.**", out)

    def test_missing_measurements_are_never_read_as_equal(self):
        out = self.verdict(dict(cpu=[1.9] * 3), dict(cpu=[2.4] * 3), base=None)
        self.assertIn("UNDECIDED", out)
        self.assertIn("none: perf did not count", out)
        self.assertIn("no power or energy source was readable", out)

    def test_heterogeneous_perf_lines_are_summed(self):
        ev = {"duration_time": 2e10, "cpu_core/instructions/u": 3e9, "cpu_atom/instructions/u": 1e9, "cpu_core/instructions/k": 5e8, "cpu_core/cycles/u": None, "instructions:k": None}
        self.assertEqual(reader.perf_sum(ev, "instructions", "u"), 4e9)
        self.assertEqual(reader.perf_sum(ev, "instructions", "k"), 5e8)
        self.assertIsNone(reader.perf_sum(ev, "cycles", "u"))
        self.assertAlmostEqual(reader.per_second(ev, "instructions", "u", 20.0), 2e8)

    def test_head_above_in_only_one_pass_is_not_higher(self):
        out = self.verdict(dict(cpu=[2.0, 2.0, 2.0]), dict(cpu=[2.6, 1.9, 1.9]))
        self.assertIn("NO LOSS (head's CPU is not higher than main's)", out)


if __name__ == "__main__":
    unittest.main()
