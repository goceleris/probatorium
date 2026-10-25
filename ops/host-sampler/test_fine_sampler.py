#!/usr/bin/env python3
"""Tests of celeris-host-sampler-fine against a fixture /proc.

Run by `go test` (host_sampler_fine_test.go) and by hand:
    python3 -I ops/host-sampler/test_fine_sampler.py
"""
import importlib.machinery
import importlib.util
import os
import subprocess
import sys
import tempfile
import random
import re
import unittest
import zlib

HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.path.join(HERE, "celeris-host-sampler-fine")


def load():
    loader = importlib.machinery.SourceFileLoader("fine", SCRIPT)
    spec = importlib.util.spec_from_loader("fine", loader)
    mod = importlib.util.module_from_spec(spec)
    loader.exec_module(mod)
    return mod


fine = load()


class FakeProc:
    """A /proc fixture whose counters a test advances."""

    def __init__(self, root, ncpu=2, ksoftirqd=True, psi=True):
        self.root = root
        self.ncpu = ncpu
        self.cpu = {n: [0] * 8 for n in range(ncpu)}  # user nice system idle iowait irq softirq steal
        self.rx = {n: 0 for n in range(ncpu)}
        self.kd = {n: 0 for n in range(ncpu)}
        self.psi_total = 0
        self.run = 1
        self.l1 = 0.25
        self.has_psi = psi
        os.makedirs(os.path.join(root, "pressure"), exist_ok=True)
        if ksoftirqd:
            for n in range(ncpu):
                d = os.path.join(root, str(100 + n))
                os.makedirs(d, exist_ok=True)
                self._w(os.path.join(d, "comm"), "ksoftirqd/%d\n" % n)
        self.flush()

    def _w(self, path, text):
        with open(path, "w") as f:  # in place: the sampler holds the descriptor open
            f.write(text)

    def add_process(self, pid, comm):
        d = os.path.join(self.root, str(pid))
        os.makedirs(d, exist_ok=True)
        self._w(os.path.join(d, "comm"), comm + "\n")

    def remove_process(self, pid):
        d = os.path.join(self.root, str(pid))
        os.remove(os.path.join(d, "comm"))
        os.rmdir(d)

    def flush(self):
        lines = ["cpu  0 0 0 0 0 0 0 0 0 0"]
        for n in range(self.ncpu):
            lines.append("cpu%d %s 0 0" % (n, " ".join(str(x) for x in self.cpu[n])))
        lines += ["intr 1 2 3", "ctxt 5", "procs_running 3"]
        self._w(os.path.join(self.root, "stat"), "\n".join(lines) + "\n")
        hdr = " ".join("CPU%d" % n for n in range(self.ncpu))
        rows = ["          " + hdr,
                "    HI: " + " ".join("0" for _ in range(self.ncpu)),
                "NET_RX: " + " ".join(str(self.rx[n]) for n in range(self.ncpu)),
                " TIMER: " + " ".join("7" for _ in range(self.ncpu))]
        self._w(os.path.join(self.root, "softirqs"), "\n".join(rows) + "\n")
        self._w(os.path.join(self.root, "loadavg"), "%.2f 0.10 0.05 %d/200 4242\n" % (self.l1, self.run))
        if self.has_psi:
            self._w(os.path.join(self.root, "pressure", "cpu"),
                    "some avg10=0.00 avg60=0.00 avg300=0.00 total=%d\n" % self.psi_total)
        for n in range(self.ncpu):
            p = os.path.join(self.root, str(100 + n), "schedstat")
            if os.path.isdir(os.path.dirname(p)):
                self._w(p, "%d 5 6\n" % self.kd[n])


class Clock:
    def __init__(self, t=1000.0, epoch=1761400000.0):
        self.t = t
        self.epoch = epoch

    def mono(self):
        return self.t

    def wall(self):
        return self.epoch + (self.t - 1000.0)

    def advance(self, s):
        self.t += s


class Base(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.proc = os.path.join(self.tmp.name, "proc")
        self.out = os.path.join(self.tmp.name, "out")
        self.run_dir = os.path.join(self.tmp.name, "run")
        os.makedirs(self.proc)
        os.makedirs(self.run_dir)

    def env(self, **kw):
        e = {"SAMPLER_FINE_INTERVAL": "1", "SAMPLER_PROCROOT": self.proc, "SAMPLER_FINE_DIR": self.out,
             "SAMPLER_RUNDIR": self.run_dir, "SAMPLER_TAG": "bench-1"}
        e.update(kw)
        return e

    def sampler(self, ncpu=2, clock=None, **kw):
        fp = FakeProc(self.proc, ncpu=ncpu)
        clock = clock or Clock()
        s = fine.Sampler(fine.Config(self.env(**kw)), host="h1", wall=clock.wall, mono=clock.mono,
                         cpu_time=lambda: 0.0)
        return fp, clock, s

    def stats(self):
        with open(os.path.join(self.out, "fine.stats")) as f:
            return f.read()

    def lines(self, name="fine.log"):
        if name != "fine.log":
            with open(os.path.join(self.out, name)) as f:
                return f.read().splitlines()
        return self.series_text().splitlines()

    def generations(self):
        return sorted(n for n in os.listdir(self.out) if n.startswith("fine.") and n.endswith(".log.z"))

    def series_text(self):
        """every closed generation (decompressed, oldest first) and the open one"""
        out = []
        for n in self.generations():
            with open(os.path.join(self.out, n), "rb") as f:
                out.append(zlib.decompress(f.read()).decode())
        p = os.path.join(self.out, "fine.log")
        if os.path.exists(p):
            with open(p) as f:
                out.append(f.read())
        return "".join(out)


class ConfigTest(Base):
    def test_interval_bounds(self):
        for ok in ("0.05", "0.1", "1", "10", "2.5"):
            self.assertAlmostEqual(fine.Config({"SAMPLER_FINE_INTERVAL": ok}).interval, float(ok))
        for bad in ("", "0", "0.01", "0.049", "10.1", "60", "-1", "abc", "nan", "inf", "1e-3x", " "):
            with self.assertRaises(fine.ConfigError, msg=bad):
                fine.Config({"SAMPLER_FINE_INTERVAL": bad})

    def test_nice_is_never_negative_and_defaults_to_lower_priority(self):
        self.assertEqual(fine.Config({"SAMPLER_FINE_INTERVAL": "1"}).nice, 10)
        for bad in ("-5", "20", "x"):
            with self.assertRaises(fine.ConfigError, msg=bad):
                fine.Config({"SAMPLER_FINE_INTERVAL": "1", "SAMPLER_NICE": bad})

    def test_caps_are_bounded(self):
        for k in ("SAMPLER_FINE_MAXBYTES", "SAMPLER_FINE_EVENTS_MAXBYTES"):
            for bad in ("0", "1", str(1 << 40)):
                with self.assertRaises(fine.ConfigError, msg=k + bad):
                    fine.Config({"SAMPLER_FINE_INTERVAL": "1", k: bad})

    def test_duration_is_positive(self):
        for bad in ("0", "-1", "x"):
            with self.assertRaises(fine.ConfigError):
                fine.Config({"SAMPLER_FINE_INTERVAL": "1", "SAMPLER_DURATION": bad})


class ParseTest(unittest.TestCase):
    def test_stat_skips_the_aggregate_and_stops_after_the_cpu_block(self):
        got = fine.parse_stat(b"cpu  9 9 9 9 9 9 9 9\ncpu0 1 2 3 4 5 6 7 8 9 10\ncpu3 1 1 1 1 1 1 1 1\nintr 1\ncpu9 7 7 7 7 7 7 7 7\n")
        self.assertEqual(sorted(got), [0, 3])
        self.assertEqual(got[0], [1, 2, 3, 4, 5, 6, 7, 8])

    def test_softirq_row_maps_columns_to_cpu_numbers(self):
        b = b"       CPU0       CPU2\n  HI:  1  1\nNET_RX:  10  20\n"
        self.assertEqual(fine.parse_softirq_row(b, "NET_RX"), {0: 10, 2: 20})
        self.assertEqual(fine.parse_softirq_row(b, "NET_TX"), {})

    def test_loadavg_psi_schedstat(self):
        self.assertEqual(fine.parse_loadavg(b"1.50 1 1 7/300 99\n"), (1.5, 7))
        self.assertEqual(fine.parse_loadavg(b"garbage"), (0.0, 0))
        self.assertEqual(fine.parse_psi_total(b"some avg10=0.00 avg60=0.00 avg300=0.00 total=1234\nfull avg10=0 total=9\n"), 1234)
        self.assertIsNone(fine.parse_psi_total(b""))
        self.assertEqual(fine.parse_schedstat_run_ns(b"5000000 7 8\n"), 5000000)
        self.assertIsNone(fine.parse_schedstat_run_ns(b""))


class SeriesTest(Base):
    def test_header_and_a_busy_cpu_line_with_idle_cpus_elided(self):
        fp, clock, s = self.sampler(ncpu=3)
        s.tick()  # baseline: nothing written
        clock.advance(1.0)
        fp.cpu[1][0] += 30  # user
        fp.cpu[1][2] += 20  # system
        fp.cpu[1][5] += 1  # irq
        fp.cpu[1][6] += 4  # softirq
        fp.cpu[1][7] += 2  # steal
        fp.rx[1] += 9
        fp.kd[1] += 12_500_000  # ns
        fp.psi_total += 1500
        fp.run = 4
        fp.l1 = 1.5
        fp.flush()
        s.tick()
        s.close()
        got = self.lines()
        self.assertTrue(got[0].startswith("# fine-sampler format=1 host=h1 tag=bench-1 interval_s=1.0"), got[0])
        self.assertEqual(len(got), 2, got)
        self.assertEqual(got[1], "1761400001.000 1000.0 run=4 l1=1.50 psi=1500 | c1=50,1,4,2,9,12")

    def test_the_baseline_tick_writes_nothing_and_counters_are_deltas(self):
        fp, clock, s = self.sampler()
        fp.cpu[0][0] = 10**6  # a host that has been up for a month
        fp.flush()
        s.tick()
        clock.advance(0.05)
        s.tick()
        s.close()
        self.assertEqual(self.lines()[1], "1761400000.050 50.0 run=1 l1=0.25 psi=0 | ")

    def test_a_counter_that_goes_backwards_is_not_a_negative_delta(self):
        fp, clock, s = self.sampler()
        fp.cpu[0][0] = 100
        fp.flush()
        s.tick()
        clock.advance(1)
        fp.cpu[0][0] = 5  # reset (hotplug)
        fp.flush()
        s.tick()
        s.close()
        self.assertNotIn("-", self.lines()[1].split("|")[1])

    def test_a_kernel_without_psi_or_ksoftirqd_threads(self):
        FakeProc(self.proc, ncpu=1, ksoftirqd=False, psi=False)
        clock = Clock()
        s = fine.Sampler(fine.Config(self.env()), wall=clock.wall, mono=clock.mono)
        s.tick()
        clock.advance(1)
        s.tick()
        s.close()
        self.assertIn("psi=0", self.lines()[1])
        self.assertEqual(s.ksoftirqd, {})

    def test_closed_generations_are_compressed_and_each_has_a_header(self):
        fp = FakeProc(self.proc)
        clock = Clock()
        s = fine.Sampler(fine.Config(self.env(SAMPLER_FINE_MAXBYTES="65536")), wall=clock.wall, mono=clock.mono)
        s.tick()
        for _ in range(3000):
            clock.advance(0.05)
            fp.cpu[0][0] += 3
            fp.flush()
            s.tick()
        s.close()
        gens = self.generations()
        self.assertTrue(len(gens) >= 3, gens)
        self.assertFalse(os.path.exists(os.path.join(self.out, "fine.log")), "the open generation is compressed at the end")
        self.assertFalse(os.path.exists(os.path.join(self.out, "fine.log.1")))
        for n in gens:
            raw = zlib.decompress(open(os.path.join(self.out, n), "rb").read()).decode()
            self.assertTrue(raw.startswith("# fine-sampler format=1"), n)
            self.assertTrue(len(raw) <= 65536 + 200, (n, len(raw)))
        text = self.series_text()
        self.assertEqual(text.count("\n"), 3000 + len(gens), "every tick, plus one header per generation")
        st = self.stats()
        self.assertIn("generations=%d " % len(gens), st)
        self.assertIn("generations_dropped=0", st)


class RetentionTest(Base):
    """A 65 h bench at a coarse clock: the detail of the early hours may go, the per-minute record may not."""

    def run_long(self, hours, **env):
        fp = FakeProc(self.proc, ncpu=8)
        clock = Clock(epoch=1761400020.0)  # 20 s into a minute
        s = fine.Sampler(fine.Config(self.env(SAMPLER_FINE_INTERVAL="10", **env)), wall=clock.wall, mono=clock.mono)
        rnd = random.Random(7)
        s.tick()
        for i in range(int(hours * 360)):
            clock.advance(10)
            for c in range(8):
                if i == 100 and c == 3:  # a stall in the first hour: CPU 3 is all softirq for this tick
                    fp.cpu[3][6] += 1000
                    continue
                fp.cpu[c][0] += rnd.randint(0, 600)
                fp.cpu[c][2] += rnd.randint(0, 300)
                fp.cpu[c][3] += rnd.randint(0, 100)
                fp.rx[c] += rnd.randint(0, 99999)
                fp.kd[c] += rnd.randint(0, 900_000_000)
            fp.flush()
            s.tick()
        s.close()
        return s

    def test_the_first_hour_survives_a_65_hour_run_in_the_minute_log(self):
        s = self.run_long(65, SAMPLER_FINE_MAXBYTES="65536", SAMPLER_FINE_TOTAL_MAXBYTES=str(1 << 20))
        self.assertGreaterEqual(s.series.dropped, 1, "the test must drive the detail past its cap")
        self.assertLessEqual(s.series.closed_bytes, 1 << 20)
        minute = [x for x in self.lines("fine-minute.log") if not x.startswith("#")]
        self.assertGreaterEqual(len(minute), 65 * 60 - 1, "one line per minute of the run")
        first = int(minute[0].split()[0])
        self.assertEqual(first, 1761400020 // 60 * 60)
        self.assertEqual(len({int(x.split()[0]) for x in minute}), len(minute), "no minute twice")
        # the detail of the first hour is gone (dropped, and counted) ...
        self.assertNotIn("fine.00000.log.z", self.generations())
        self.assertNotIn("1761400000", self.series_text()[:200])
        ev = "\n".join(self.lines("fine-events.log"))
        self.assertIn("generation_dropped file=fine.00000.log.z", ev)
        # ... and the stall of the first hour is still in the minute record, at 100 % softirq on CPU 3
        stall = [x for x in minute if int(x.split()[0]) <= 1761400020 + 3600 and " c3=100," in x]
        self.assertEqual(len(stall), 1, stall)
        self.assertIn("generations_dropped=", self.stats())

    def test_nothing_is_dropped_when_the_cap_holds_the_run(self):
        s = self.run_long(6, SAMPLER_FINE_MAXBYTES="65536")
        self.assertEqual(s.series.dropped, 0)
        text = self.series_text()
        data = [x for x in text.splitlines() if not x.startswith("#")]
        self.assertEqual(len(data), 6 * 360)
        self.assertTrue(data[0].startswith("1761400030.000 "), data[0])

    def test_the_minute_log_is_capped_with_a_drop_counter(self):
        s = self.run_long(2, SAMPLER_FINE_MINUTE_MAXBYTES="4096")
        self.assertGreater(s.minutes.out.dropped, 0)
        self.assertRegex(self.lines("fine-minute.log")[-1], r"^# minutes_dropped=\d+$")
        self.assertIn("minutes_dropped=%d" % s.minutes.out.dropped, self.stats())

    def test_a_minute_line_holds_the_peak_softirq_and_the_lowest_user_time(self):
        fp, clock, s = self.sampler(ncpu=2, SAMPLER_FINE_INTERVAL="1", )
        clock.epoch = 1761400020.0 - 0  # 20 s into the minute
        s.tick()
        for sirq, usr in ((10, 80), (96, 2), (20, 70)):
            clock.advance(1)
            fp.cpu[1][6] += sirq
            fp.cpu[1][0] += usr
            fp.cpu[1][3] += 100 - sirq - usr
            fp.flush()
            s.tick()
        clock.advance(60)  # next minute: flushes the first
        fp.flush()
        s.tick()
        s.close()
        first = [x for x in self.lines("fine-minute.log") if not x.startswith("#")][0]
        # peak 96 %, lowest user 2 %, whole-minute softirq (10+96+20)/300 = 42 %
        self.assertTrue(first.startswith("1761400020 n=3 "), first)
        self.assertIn(" c1=96,2,42", first)
        self.assertNotIn("c0=", first, "an idle CPU is not listed")


class SchedulingTest(unittest.TestCase):
    class FakeOS:
        SCHED_IDLE = 5
        SCHED_BATCH = 3

        def __init__(self, refuse_idle=False, refuse_batch=False):
            self.calls = []
            self.refuse_idle, self.refuse_batch = refuse_idle, refuse_batch

        def nice(self, n):
            self.calls.append(("nice", n))
            return n

        def sched_param(self, p):
            return p

        def sched_setscheduler(self, pid, policy, param):
            self.calls.append(("sched", pid, policy, param))
            if (policy == 5 and self.refuse_idle) or (policy == 3 and self.refuse_batch):
                raise PermissionError("refused")

    def test_sched_idle_after_nice_10(self):
        o = self.FakeOS()
        self.assertEqual(fine.lower_priority(10, o), "idle")
        self.assertEqual(o.calls, [("nice", 10), ("sched", 0, 5, 0)])

    def test_batch_at_nice_19_when_idle_is_refused(self):
        o = self.FakeOS(refuse_idle=True)
        self.assertEqual(fine.lower_priority(10, o), "batch+nice19")
        self.assertEqual(o.calls, [("nice", 10), ("sched", 0, 5, 0), ("nice", 9), ("sched", 0, 3, 0)])

    def test_nice_only_when_no_scheduler_call_works(self):
        o = self.FakeOS(refuse_idle=True, refuse_batch=True)
        self.assertEqual(fine.lower_priority(10, o), "nice-only")

        class NoSched:  # a platform without the scheduler calls (macOS): nice alone
            def nice(self, n):
                return n

        self.assertEqual(fine.lower_priority(10, NoSched()), "nice-only")

    def test_the_priority_is_never_raised(self):
        for kw in ({}, {"refuse_idle": True}, {"refuse_idle": True, "refuse_batch": True}):
            for nice in (0, 10, 19):
                o = self.FakeOS(**kw)
                fine.lower_priority(nice, o)
                self.assertTrue(all(c[1] >= 0 for c in o.calls if c[0] == "nice"), o.calls)
                self.assertTrue(all(c[2] in (3, 5) for c in o.calls if c[0] == "sched"), o.calls)


class EventsTest(Base):
    def ticks(self, fp, s, clock, n, step, **per_tick):
        for _ in range(n):
            clock.advance(step)
            for k, (cpu, idx, inc) in per_tick.items():
                fp.cpu[cpu][idx] += inc
            fp.flush()
            s.tick()

    def starve_lines(self):
        return [x for x in self.lines("fine-events.log") if " starve_" in x]

    def test_the_starvation_signature_is_one_episode_not_a_line_a_second(self):
        fp, clock, s = self.sampler(ncpu=2)
        s.tick()
        # CPU 1 spends 3 s in softirq (5 jiffies of 5 per 50 ms), CPU 0 is busy in user
        self.ticks(fp, s, clock, 60, 0.05, a=(1, 6, 5), b=(0, 0, 5))
        # then CPU 1 runs user code again
        self.ticks(fp, s, clock, 60, 0.05, a=(1, 0, 5), b=(0, 0, 5))
        s.close()
        ev = self.starve_lines()
        self.assertEqual(len(ev), 2, ev)
        self.assertIn(" starve_begin cpu=1 softirq_pct=100.0 usrsys_pct=0.0", ev[0])
        self.assertRegex(ev[1], r" starve_end cpu=1 duration_s=[23]\.\d\d peak_softirq_pct=100\.0 ticks=\d+$")

    def test_an_episode_still_open_at_the_end_is_closed_and_marked(self):
        fp, clock, s = self.sampler(ncpu=1)
        s.tick()
        self.ticks(fp, s, clock, 5, 1.0, a=(0, 6, 100))
        s.close()
        ev = self.starve_lines()
        self.assertEqual(len(ev), 2, ev)
        self.assertTrue(ev[1].endswith("open_at_exit=1"), ev[1])

    def test_a_cpu_stuck_for_hours_costs_two_lines(self):
        fp, clock, s = self.sampler(ncpu=1)
        s.tick()
        self.ticks(fp, s, clock, 3600, 1.0, a=(0, 6, 100))  # an hour at 1 s
        s.close()
        self.assertEqual(len(self.starve_lines()), 2)
        self.assertLess(os.path.getsize(os.path.join(self.out, "fine-events.log")), 1000)

    def test_a_busy_cpu_and_a_mixed_cpu_are_not_starved(self):
        fp, clock, s = self.sampler(ncpu=2)
        s.tick()
        # CPU 0: user busy; CPU 1: 80 % softirq, 20 % user: heavy softirq but the thread runs
        self.ticks(fp, s, clock, 60, 0.05, a=(0, 0, 5), b=(1, 6, 4), c=(1, 0, 1))
        s.close()
        self.assertEqual(self.starve_lines(), [])

    def test_90_percent_softirq_is_not_the_signature(self):
        fp, clock, s = self.sampler(ncpu=1)
        s.tick()
        # 90 % softirq, 10 % idle, no user time: heavy, but under the 95 % of the signature
        self.ticks(fp, s, clock, 5, 1.0, a=(0, 6, 90), b=(0, 3, 10))
        s.close()
        self.assertEqual(self.starve_lines(), [])

    def test_96_percent_softirq_with_4_percent_user_is_not_the_signature(self):
        fp, clock, s = self.sampler(ncpu=1)
        s.tick()
        # the thread got 4 % of the CPU: above the 3 % of the signature even though softirq is over 95 %
        self.ticks(fp, s, clock, 5, 1.0, a=(0, 6, 96), b=(0, 0, 4))
        s.close()
        self.assertEqual(self.starve_lines(), [])

    def test_96_percent_softirq_with_2_percent_user_is(self):
        fp, clock, s = self.sampler(ncpu=1)
        s.tick()
        self.ticks(fp, s, clock, 3, 1.0, a=(0, 6, 97), b=(0, 0, 2), c=(0, 3, 1))
        s.close()
        self.assertEqual(len(self.starve_lines()), 2)  # begin and end(open at exit)

    def test_a_signature_shorter_than_the_window_is_not_logged(self):
        fp, clock, s = self.sampler(ncpu=1)
        s.tick()
        self.ticks(fp, s, clock, 8, 0.05, a=(0, 6, 5))  # 0.4 s of softirq
        self.ticks(fp, s, clock, 40, 0.05, b=(0, 0, 5))  # then user
        s.close()
        self.assertEqual(self.starve_lines(), [])

    def test_the_signature_works_at_one_hz(self):
        fp, clock, s = self.sampler(ncpu=1)
        s.tick()
        self.ticks(fp, s, clock, 3, 1.0, a=(0, 6, 100))
        s.close()
        self.assertEqual(len(self.starve_lines()), 2)
        self.assertIn(" starve_begin cpu=0 ", self.starve_lines()[0])  # begin and end(open at exit)

    def test_late_ticks_within_a_second_are_one_burst_line(self):
        fp, clock, s = self.sampler(ncpu=1, SAMPLER_FINE_INTERVAL="0.05")
        s.tick()
        for _ in range(30):  # a sampler that is starved for good: every tick 0.4 s late
            self.ticks(fp, s, clock, 1, 0.4)
        s.close()
        ev = [x for x in self.lines("fine-events.log") if " gap" in x]
        gaps = [x for x in ev if " gap " in x]
        bursts = [x for x in ev if " gap_burst " in x]
        self.assertLess(len(ev), 30, ev)
        self.assertEqual(len(gaps) + sum(int(re.search(r"n=(\d+)", b).group(1)) for b in bursts), 30)
        self.assertIn("max_dt_ms=400.0", bursts[0])
        self.assertIn("gap_events=30", self.stats())

    def test_a_late_tick_is_a_gap_event(self):
        fp, clock, s = self.sampler(ncpu=1)
        s.tick()
        self.ticks(fp, s, clock, 2, 1.0)
        self.ticks(fp, s, clock, 1, 3.5)  # the sampler itself was starved for 3.5 s
        self.ticks(fp, s, clock, 1, 1.0)
        s.close()
        gaps = [x for x in self.lines("fine-events.log") if " gap " in x]
        self.assertEqual(len(gaps), 1, gaps)
        self.assertIn("dt_ms=3500.0 expected_ms=1000.0", gaps[0])

    def test_the_event_log_is_capped_and_says_how_much_it_dropped(self):
        fp, clock, s = self.sampler(ncpu=1, SAMPLER_FINE_EVENTS_MAXBYTES="4096")
        s.tick()
        for _ in range(400):
            self.ticks(fp, s, clock, 1, 3.5)  # a gap each time
        s.close()
        self.assertLessEqual(os.path.getsize(os.path.join(self.out, "fine-events.log")), 4096 + 100)
        last = self.lines("fine-events.log")[-1]
        self.assertRegex(last, r"^# events_dropped=\d+$")
        n = int(last.split("=")[1])
        written = len([x for x in self.lines("fine-events.log") if " gap " in x])
        self.assertEqual(written + n, 400, "every gap is either written or counted")
        self.assertIn("events_dropped=%d" % n, self.stats())
        self.assertFalse(os.path.exists(os.path.join(self.out, "fine-events.log.1")))

    def test_system_time_counts_as_the_thread_running(self):
        fp, clock, s = self.sampler(ncpu=1)
        s.tick()
        # 96 % softirq and 4 % SYSTEM time (idx 2): the thread ran: not the signature
        self.ticks(fp, s, clock, 5, 1.0, a=(0, 6, 96), b=(0, 2, 4))
        s.close()
        self.assertEqual(self.starve_lines(), [])

    def test_nice_time_counts_as_the_thread_running(self):
        fp, clock, s = self.sampler(ncpu=1)
        s.tick()
        self.ticks(fp, s, clock, 5, 1.0, a=(0, 6, 96), b=(0, 1, 4))
        s.close()
        self.assertEqual(self.starve_lines(), [])


class RunLoopTest(Base):
    def test_stats_report_the_achieved_rate_and_the_samplers_own_cpu(self):
        fp, clock, s = self.sampler(ncpu=1, SAMPLER_FINE_INTERVAL="0.1", SAMPLER_FINE_MAX_TICKS="20")
        cpu = [0.0]
        s.cpu_time = lambda: cpu[0]
        s.sleep_calls = []

        def sleep(d):
            clock.advance(d)
            cpu[0] += 0.0004

        self.assertEqual(s.run(sleep=sleep), "max_ticks")
        s.exit_reason = "max_ticks"
        s.close()
        st = self.stats()
        self.assertIn("ticks=20 ", st)
        self.assertIn("target_hz=10.000", st)
        self.assertIn("final=1 exit_reason=max_ticks", st)
        self.assertIn("skipped_deadlines=0", st)
        hz = float(st.split("achieved_hz=")[1].split()[0])
        self.assertTrue(8.0 <= hz <= 10.5, st)
        cpu_pct = float(st.split("self_cpu_pct=")[1].split()[0])
        self.assertTrue(0.3 <= cpu_pct <= 0.5, st)  # 0.4 ms per 100 ms tick

    def test_the_stop_sentinel_ends_the_loop_within_one_tick(self):
        fp, clock, s = self.sampler(ncpu=1)
        n = [0]

        def sleep(d):
            clock.advance(d)
            n[0] += 1
            if n[0] == 3:
                open(s.cfg.stopfile, "w").close()

        self.assertEqual(s.run(sleep=sleep), "stop_requested")
        s.close()
        self.assertLessEqual(s.ticks, 3)

    def test_the_deadline_ends_the_loop(self):
        fp, clock, s = self.sampler(ncpu=1, SAMPLER_DURATION="5")

        def sleep(d):
            clock.advance(d)

        self.assertEqual(s.run(sleep=sleep), "deadline")
        s.close()
        self.assertTrue(4 <= s.ticks <= 6, s.ticks)

    def test_missed_deadlines_are_counted_and_the_schedule_resumes(self):
        fp, clock, s = self.sampler(ncpu=1, SAMPLER_FINE_INTERVAL="0.5", SAMPLER_FINE_MAX_TICKS="6")
        first = [True]

        def sleep(d):
            clock.advance(d)

        orig_tick = s.tick

        def slow_tick():
            orig_tick()
            if s.ticks == 2 and first[0]:
                first[0] = False
                clock.advance(5.0)  # the host froze for 5 s

        s.tick = slow_tick
        self.assertEqual(s.run(sleep=sleep), "max_ticks")
        s.close()
        self.assertGreaterEqual(s.skipped, 8)

    def test_a_runner_that_vanishes_ends_the_sampler_after_a_minute(self):
        fp, clock, s = self.sampler(ncpu=1, SAMPLER_EXIT_WITHOUT_RUNNER="1")
        fp.add_process(900, "Runner.Listener")

        def sleep(d):
            clock.advance(d)
            if clock.t > 1010 and os.path.isdir(os.path.join(self.proc, "900")):
                fp.remove_process(900)

        self.assertEqual(s.run(sleep=sleep), "runner_gone")
        s.close()
        self.assertTrue(70 <= clock.t - 1000 <= 125, clock.t)

    def test_a_host_that_never_had_a_listener_is_not_stopped_by_its_absence(self):
        fp, clock, s = self.sampler(ncpu=1, SAMPLER_EXIT_WITHOUT_RUNNER="1", SAMPLER_FINE_MAX_TICKS="200")
        self.assertEqual(s.run(sleep=lambda d: clock.advance(d)), "max_ticks")
        s.close()

    def test_the_runner_check_is_off_by_default(self):
        fp, clock, s = self.sampler(ncpu=1, SAMPLER_FINE_MAX_TICKS="100")
        fp.add_process(900, "Runner.Listener")
        fp.remove_process(900)
        self.assertEqual(s.run(sleep=lambda d: clock.advance(d)), "max_ticks")
        s.close()


class TakeoverTest(Base):
    def test_an_older_instance_is_asked_with_a_file_and_waited_for(self):
        cfg = fine.Config(self.env())
        os.makedirs(os.path.join(self.proc, "4321"))
        with open(cfg.pidfile, "w") as f:
            f.write("4321\n")
        seen = []

        def sleep(d):
            seen.append(os.path.exists(cfg.stopfile))
            if len(seen) == 3:
                os.rmdir(os.path.join(self.proc, "4321"))

        fine.takeover(cfg, lambda pid: os.path.isdir(os.path.join(self.proc, pid)), sleep=sleep)
        self.assertEqual(seen, [True, True, True])
        self.assertFalse(os.path.exists(cfg.stopfile), "the sentinel must be gone so the successor does not stop itself")

    def test_a_stale_sentinel_is_removed_when_nothing_runs(self):
        cfg = fine.Config(self.env())
        open(cfg.stopfile, "w").close()
        fine.takeover(cfg, lambda pid: False)
        self.assertFalse(os.path.exists(cfg.stopfile))

    def test_an_older_instance_that_does_not_go_is_not_escalated_against(self):
        cfg = fine.Config(self.env())
        os.makedirs(os.path.join(self.proc, "4321"))
        with open(cfg.pidfile, "w") as f:
            f.write("4321\n")
        fine.takeover(cfg, lambda pid: True, sleep=lambda d: None, waited_max=2)
        self.assertTrue(os.path.isdir(os.path.join(self.proc, "4321")))


class EndToEndTest(Base):
    def run_script(self, **kw):
        env = {"PATH": os.environ.get("PATH", ""), **self.env(**kw)}
        return subprocess.run([sys.executable, "-I", SCRIPT], env=env, capture_output=True, text=True, timeout=60)

    def test_a_real_process_samples_and_leaves_its_files(self):
        FakeProc(self.proc, ncpu=2)
        r = self.run_script(SAMPLER_FINE_INTERVAL="0.05", SAMPLER_FINE_MAX_TICKS="12")
        self.assertEqual(r.returncode, 0, r.stderr)
        got = self.lines()
        self.assertTrue(got[0].startswith("# fine-sampler format=1"))
        self.assertEqual(len(got), 13, got)  # header + 12 ticks
        st = self.stats()
        self.assertIn("ticks=12 ", st)
        self.assertIn("exit_reason=max_ticks", st)
        self.assertFalse(os.path.exists(os.path.join(self.run_dir, "host-sampler-fine.pid")), "the pidfile outlived the process")

    def test_a_bad_interval_is_refused_and_writes_nothing(self):
        r = self.run_script(SAMPLER_FINE_INTERVAL="0.01")
        self.assertEqual(r.returncode, 2)
        self.assertIn("SAMPLER_FINE_INTERVAL", r.stderr)
        self.assertFalse(os.path.exists(self.out))

    def test_a_missing_interval_is_refused(self):
        env = {"PATH": os.environ.get("PATH", ""), "SAMPLER_RUNDIR": self.run_dir, "SAMPLER_FINE_DIR": self.out}
        r = subprocess.run([sys.executable, "-I", SCRIPT], env=env, capture_output=True, text=True, timeout=60)
        self.assertEqual(r.returncode, 2)

    def test_a_pending_stop_sentinel_from_a_previous_run_does_not_stop_the_new_one(self):
        FakeProc(self.proc, ncpu=1)
        open(os.path.join(self.run_dir, "host-sampler-fine-stop"), "w").close()
        r = self.run_script(SAMPLER_FINE_INTERVAL="0.05", SAMPLER_FINE_MAX_TICKS="3")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("exit_reason=max_ticks", self.stats())

    def test_main_lowers_the_priority_and_says_which_class_took(self):
        FakeProc(self.proc, ncpu=1)
        r = self.run_script(SAMPLER_FINE_INTERVAL="0.05", SAMPLER_FINE_MAX_TICKS="3")
        self.assertEqual(r.returncode, 0, r.stderr)
        st, hdr = self.stats(), self.lines()[0]
        m = re.search(r"sched=(\S+)", st)
        self.assertIsNotNone(m, st)
        self.assertIn("sched=" + m.group(1), hdr)
        self.assertIn("sched=" + m.group(1), open(os.path.join(self.out, "fine-run.txt")).read())
        if sys.platform.startswith("linux"):
            self.assertEqual(m.group(1), "idle", "an unprivileged process may enter SCHED_IDLE")
        else:
            self.assertEqual(m.group(1), "nice-only")

    def test_the_run_file_carries_the_tag_the_stop_matches_on(self):
        FakeProc(self.proc, ncpu=1)
        r = self.run_script(SAMPLER_FINE_INTERVAL="0.05", SAMPLER_FINE_MAX_TICKS="2", SAMPLER_TAG="bench-4242")
        self.assertEqual(r.returncode, 0, r.stderr)
        with open(os.path.join(self.out, "fine-run.txt")) as f:
            self.assertTrue(f.read().startswith("tag=bench-4242 host="))

    def test_files_of_an_earlier_run_do_not_survive_a_new_start(self):
        FakeProc(self.proc, ncpu=1)
        os.makedirs(self.out)
        stale = ["fine.00007.log.z", "fine.00001.log.z", "fine.log", "fine-events.log", "fine-minute.log", "fine.stats",
                 "fine-run.txt", "fine.00002.log.z.tmp"]
        for n in stale:
            with open(os.path.join(self.out, n), "w") as f:
                f.write("tag=bench-OLDRUN\n")
        with open(os.path.join(self.out, "notes.txt"), "w") as f:
            f.write("not ours\n")
        r = self.run_script(SAMPLER_FINE_INTERVAL="0.05", SAMPLER_FINE_MAX_TICKS="2", SAMPLER_TAG="bench-NEW")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(self.generations().count("fine.00007.log.z"), 0)
        for n in os.listdir(self.out):
            if n == "notes.txt":
                continue
            p = os.path.join(self.out, n)
            if n.endswith(".z"):
                self.assertNotIn("OLDRUN", zlib.decompress(open(p, "rb").read()).decode(), n)
            else:
                self.assertNotIn("OLDRUN", open(p).read(), n)
        self.assertTrue(os.path.exists(os.path.join(self.out, "notes.txt")), "only its own files are removed")
        self.assertFalse(os.path.exists(os.path.join(self.out, "fine.00002.log.z.tmp")))


class RobustnessTest2(Base):
    def test_a_tick_that_overran_by_less_than_an_interval_is_not_followed_by_a_burst(self):
        fp = FakeProc(self.proc, ncpu=1)
        clock = Clock()
        s = fine.Sampler(fine.Config(self.env(SAMPLER_FINE_INTERVAL="0.5", SAMPLER_FINE_MAX_TICKS="6")),
                         wall=clock.wall, mono=clock.mono)
        orig = s.tick
        done = [False]

        def slow_tick():
            orig()
            if s.ticks == 2 and not done[0]:
                done[0] = True
                clock.advance(0.6)  # the tick took longer than the interval

        s.tick = slow_tick
        s.run(sleep=lambda d: clock.advance(d))
        s.close()
        dts = [float(x.split()[1]) for x in self.lines()[1:]]
        self.assertTrue(all(d >= 490 for d in dts[2:]), dts)  # no tick follows the late one at once


class RobustnessTest(Base):
    def test_a_huge_intr_line_does_not_cost_a_big_read(self):
        fp, clock, s = self.sampler(ncpu=2)
        self.assertLessEqual(fine.PROC_READ, 65536)
        with open(os.path.join(self.proc, "stat"), "w") as f:
            f.write("cpu  0 0 0 0 0 0 0 0 0 0\ncpu0 1 0 0 9 0 0 0 0\ncpu1 1 0 0 9 0 0 0 0\nintr " + "7 " * 200000 + "\nprocs_running 3\n")
        s.tick()
        clock.advance(1)
        with open(os.path.join(self.proc, "stat"), "w") as f:
            f.write("cpu  0 0 0 0 0 0 0 0 0 0\ncpu0 6 0 0 14 0 0 0 0\ncpu1 1 0 0 9 0 0 0 0\nintr " + "7 " * 200000 + "\nprocs_running 3\n")
        s.tick()
        s.close()
        self.assertIn("c0=5,0,0,0,0,0", self.lines()[1])

    def test_a_clock_step_does_not_end_the_run(self):
        fp = FakeProc(self.proc, ncpu=1)
        clock = Clock()
        s = fine.Sampler(fine.Config(self.env(SAMPLER_DURATION="600", SAMPLER_FINE_MAX_TICKS="10")),
                         wall=clock.wall, mono=clock.mono)

        def sleep(d):
            clock.advance(d)
            clock.epoch += 10**6  # NTP steps the wall clock forward by 11 days

        self.assertEqual(s.run(sleep=sleep), "max_ticks")
        s.close()

    def test_a_late_tick_is_not_followed_by_a_burst_of_catch_up_ticks(self):
        fp = FakeProc(self.proc, ncpu=1)
        clock = Clock()
        s = fine.Sampler(fine.Config(self.env(SAMPLER_FINE_INTERVAL="0.5", SAMPLER_FINE_MAX_TICKS="8")),
                         wall=clock.wall, mono=clock.mono)
        orig = s.tick
        first = [True]

        def slow_tick():
            orig()
            if s.ticks == 2 and first[0]:
                first[0] = False
                clock.advance(2.3)  # the host froze

        s.tick = slow_tick
        s.run(sleep=lambda d: clock.advance(d))
        s.close()
        dts = [float(x.split()[1]) for x in self.lines()[1:]]
        self.assertTrue(all(d >= 450 for d in dts[3:]), dts)  # after the freeze every interval is a whole one
        self.assertGreaterEqual(s.skipped, 4)


if __name__ == "__main__":
    unittest.main(verbosity=1)
