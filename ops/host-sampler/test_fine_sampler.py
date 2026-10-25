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
import unittest

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
        with open(os.path.join(self.out, name)) as f:
            return f.read().splitlines()


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

    def test_rotation_keeps_two_generations_each_with_a_header(self):
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
        sizes = [os.path.getsize(os.path.join(self.out, n)) for n in ("fine.log", "fine.log.1")]
        self.assertTrue(all(0 < z <= 65536 for z in sizes), sizes)
        self.assertTrue(s.series.rotations >= 1)
        for n in ("fine.log", "fine.log.1"):
            self.assertTrue(self.lines(n)[0].startswith("# fine-sampler format=1"), n)
        self.assertFalse(os.path.exists(os.path.join(self.out, "fine.log.2")))


class EventsTest(Base):
    def ticks(self, fp, s, clock, n, step, **per_tick):
        for _ in range(n):
            clock.advance(step)
            for k, (cpu, idx, inc) in per_tick.items():
                fp.cpu[cpu][idx] += inc
            fp.flush()
            s.tick()

    def test_the_starvation_signature_is_logged_once_a_second(self):
        fp, clock, s = self.sampler(ncpu=2)
        s.tick()
        # CPU 1 spends the whole second in softirq (5 jiffies of 5 per 50 ms), CPU 0 is busy in user
        self.ticks(fp, s, clock, 60, 0.05, a=(1, 6, 5), b=(0, 0, 5))
        s.close()
        ev = [x for x in self.lines("fine-events.log") if " starve " in x]
        self.assertTrue(2 <= len(ev) <= 3, ev)  # rate limited: ~1 per second over 3 s
        self.assertTrue(all("cpu=1 " in x for x in ev), ev)
        self.assertIn("softirq_pct=100.0 usrsys_pct=0.0", ev[0])

    def test_a_busy_cpu_and_a_mixed_cpu_are_not_starved(self):
        fp, clock, s = self.sampler(ncpu=2)
        s.tick()
        # CPU 0: user busy; CPU 1: 80 % softirq, 20 % user: heavy softirq but the thread runs
        self.ticks(fp, s, clock, 60, 0.05, a=(0, 0, 5), b=(1, 6, 4), c=(1, 0, 1))
        s.close()
        self.assertEqual([x for x in self.lines("fine-events.log") if " starve " in x], [])

    def test_90_percent_softirq_is_not_the_signature(self):
        fp, clock, s = self.sampler(ncpu=1)
        s.tick()
        # 90 % softirq, 10 % idle, no user time: heavy, but under the 95 % of the signature
        self.ticks(fp, s, clock, 5, 1.0, a=(0, 6, 90), b=(0, 3, 10))
        s.close()
        self.assertEqual([x for x in self.lines("fine-events.log") if " starve " in x], [])

    def test_96_percent_softirq_with_4_percent_user_is_not_the_signature(self):
        fp, clock, s = self.sampler(ncpu=1)
        s.tick()
        # the thread got 4 % of the CPU: above the 3 % of the signature even though softirq is over 95 %
        self.ticks(fp, s, clock, 5, 1.0, a=(0, 6, 96), b=(0, 0, 4))
        s.close()
        self.assertEqual([x for x in self.lines("fine-events.log") if " starve " in x], [])

    def test_96_percent_softirq_with_2_percent_user_is(self):
        fp, clock, s = self.sampler(ncpu=1)
        s.tick()
        self.ticks(fp, s, clock, 3, 1.0, a=(0, 6, 97), b=(0, 0, 2), c=(0, 3, 1))
        s.close()
        self.assertEqual(len([x for x in self.lines("fine-events.log") if " starve " in x]), 3)

    def test_a_signature_shorter_than_the_window_is_not_logged(self):
        fp, clock, s = self.sampler(ncpu=1)
        s.tick()
        self.ticks(fp, s, clock, 8, 0.05, a=(0, 6, 5))  # 0.4 s of softirq
        self.ticks(fp, s, clock, 40, 0.05, b=(0, 0, 5))  # then user
        s.close()
        self.assertEqual([x for x in self.lines("fine-events.log") if " starve " in x], [])

    def test_the_signature_works_at_one_hz(self):
        fp, clock, s = self.sampler(ncpu=1)
        s.tick()
        self.ticks(fp, s, clock, 3, 1.0, a=(0, 6, 100))
        s.close()
        self.assertEqual(len([x for x in self.lines("fine-events.log") if " starve " in x]), 3)

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

    def test_the_event_log_is_capped_not_rotated(self):
        fp, clock, s = self.sampler(ncpu=1, SAMPLER_FINE_EVENTS_MAXBYTES="4096")
        s.tick()
        for _ in range(400):
            self.ticks(fp, s, clock, 1, 3.5)  # a gap each time
        s.close()
        self.assertLessEqual(os.path.getsize(os.path.join(self.out, "fine-events.log")), 4096 + 100)
        self.assertEqual(self.lines("fine-events.log")[-1], "# event log full: later events are not recorded")
        self.assertFalse(os.path.exists(os.path.join(self.out, "fine-events.log.1")))


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

    def test_the_process_runs_at_a_lower_priority_than_it_was_started_at(self):
        FakeProc(self.proc, ncpu=1)
        probe = ("import os,sys,importlib.machinery,importlib.util as u;"
                 "l=importlib.machinery.SourceFileLoader('f',%r);m=u.module_from_spec(u.spec_from_loader('f',l));l.exec_module(m);"
                 "b=os.nice(0);m.os.nice(m.Config({'SAMPLER_FINE_INTERVAL':'1','SAMPLER_NICE':'0'}).nice);a=os.nice(0);"
                 "m.os.nice(m.Config({'SAMPLER_FINE_INTERVAL':'1'}).nice);c=os.nice(0);print(b,a,c)") % SCRIPT
        r = subprocess.run([sys.executable, "-I", "-c", probe], capture_output=True, text=True, timeout=30)
        self.assertEqual(r.returncode, 0, r.stderr)
        b, a, c = (int(x) for x in r.stdout.split())
        self.assertEqual(a, b)  # SAMPLER_NICE=0 changes nothing
        self.assertEqual(c, min(19, b + 10))  # the default adds 10


if __name__ == "__main__":
    unittest.main(verbosity=1)
