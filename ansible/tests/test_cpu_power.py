#!/usr/bin/env python3
"""Tests of ansible/files/cpu_power.py against a FAKE sysfs tree (CPU_SYSFS / CPUINFO hooks): record, apply of the three arms, restore equal to the original,
a refused write (exit 1, state reported), the write-once original, compare, the stop rule, the log mode. Not tested here: real sysfs, real sudo (the GOVAB proof run)."""
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest

sys.dont_write_bytecode = True
HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.path.join(HERE, "..", "files", "cpu_power.py")


def jload(p):
    with open(p) as f:
        return json.load(f)


def jlines(p):
    with open(p) as f:
        return f.read().splitlines()


def w(path, text):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        f.write(text)


class Fake:
    def __init__(self, ncpu=4, governor="powersave", epp="balance_performance", driver="amd-pstate-epp", states=("POLL", "C1", "C2", "C3")):
        self.root = tempfile.mkdtemp(prefix="cpupower-")
        self.sys = os.path.join(self.root, "cpu")
        self.info = os.path.join(self.root, "cpuinfo")
        self.ncpu = ncpu
        for c in range(ncpu):
            d = "%s/cpu%d" % (self.sys, c)
            w(d + "/cpufreq/scaling_governor", governor + "\n")
            w(d + "/cpufreq/scaling_driver", driver + "\n")
            w(d + "/cpufreq/scaling_available_governors", "performance powersave\n")
            if epp:
                w(d + "/cpufreq/energy_performance_preference", epp + "\n")
                w(d + "/cpufreq/energy_performance_available_preferences", "default performance balance_performance balance_power power\n")
            w(d + "/cpufreq/scaling_min_freq", "400000\n")
            w(d + "/cpufreq/scaling_max_freq", "3500000\n")
            w(d + "/cpufreq/scaling_cur_freq", "%d\n" % (1500000 + 100000 * c))
            for i, n in enumerate(states):
                s = "%s/cpuidle/state%d" % (d, i)
                w(s + "/name", n + "\n")
                w(s + "/disable", "0\n")
                w(s + "/latency", "%d\n" % (i * 40))
                w(s + "/residency", "%d\n" % (i * 100))
                w(s + "/usage", "%d\n" % (1000 * i))
                w(s + "/time", "%d\n" % (5000 * i))
        w(self.sys + "/cpufreq/boost", "1\n")
        w(self.sys + "/amd_pstate/status", "active\n")
        w(self.sys + "/cpuidle/current_driver", "acpi_idle\n")
        w(self.sys + "/cpuidle/current_governor", "menu\n")
        w(self.info, "".join("processor\t: %d\ncpu MHz\t\t: %d.000\n\n" % (c, 1500 + 100 * c) for c in range(ncpu)))
        self.env = dict(os.environ, CPU_SYSFS=self.sys, CPUINFO=self.info)

    def run(self, *args):
        p = subprocess.run([sys.executable, "-I", "-B", SCRIPT] + list(args), env=self.env, capture_output=True, text=True)
        return p.returncode, p.stdout + p.stderr

    def path(self, name):
        return os.path.join(self.root, name)

    def rdf(self, rel):
        with open(os.path.join(self.sys, rel)) as f:
            return f.read().strip()

    def close(self):
        shutil.rmtree(self.root, ignore_errors=True)


class CpuPowerTest(unittest.TestCase):
    def setUp(self):
        self.f = Fake()
        self.addCleanup(self.f.close)
        self.orig = self.f.path("orig.json")
        rc, out = self.f.run("record", "--out", self.orig, "--tag", "t", "--label", "orig", "--samples", "3", "--gap", "0")
        self.assertEqual(rc, 0, out)

    def apply(self, arm, name=None):
        return self.f.run("apply", "--arm", arm, "--orig", self.orig, "--out", self.f.path(name or "res-%s.json" % arm))

    def governors(self):
        return {self.f.rdf("cpu%d/cpufreq/scaling_governor" % c) for c in range(self.f.ncpu)}

    def disables(self, state):
        return {self.f.rdf("cpu%d/cpuidle/state%d/disable" % (c, state)) for c in range(self.f.ncpu)}

    def test_record_has_every_field_and_three_frequency_samples(self):
        r = jload((self.orig))
        self.assertEqual(len(r["cpus"]), 4)
        e = r["cpus"]["cpu2"]
        self.assertEqual((e["driver"], e["governor"], e["epp"]), ("amd-pstate-epp", "powersave", "balance_performance"))
        self.assertEqual([s["name"] for s in e["idle"]], ["POLL", "C1", "C2", "C3"])
        self.assertEqual({s["disable"] for s in e["idle"]}, {0})
        self.assertEqual((e["idle"][3]["latency"], e["idle"][3]["residency"], e["idle"][3]["usage"]), (120, 300, 3000))
        self.assertEqual(r["global"], {"cpufreq_boost": "1", "intel_pstate_no_turbo": None, "intel_pstate_status": None, "amd_pstate_status": "active",
                                       "cpuidle_driver": "acpi_idle", "cpuidle_governor": "menu"})
        self.assertEqual(len(r["freq_samples"]), 3)
        self.assertEqual(r["freq_samples"][0]["scaling_cur_khz"], [1500000, 1600000, 1700000, 1800000])
        self.assertEqual(r["freq_samples"][0]["cpuinfo_mhz"], [1500.0, 1600.0, 1700.0, 1800.0])

    def test_the_original_record_is_write_once(self):
        rc, out = self.f.run("record", "--out", self.orig, "--once")
        self.assertEqual(rc, 1, out)
        self.assertIn("write-once", out)
        self.assertEqual(jload((self.orig))["label"], "orig")

    def test_performance_sets_the_governor_and_epp_and_keeps_every_idle_state(self):
        rc, out = self.apply("performance")
        self.assertEqual(rc, 0, out)
        self.assertEqual(self.governors(), {"performance"})
        self.assertEqual({self.f.rdf("cpu%d/cpufreq/energy_performance_preference" % c) for c in range(4)}, {"performance"})
        for st in range(4):
            self.assertEqual(self.disables(st), {"0"})
        self.assertTrue(jload((self.f.path("res-performance.json")))["ok"])

    def test_c2off_disables_only_C2_and_C3_by_name(self):
        rc, out = self.apply("performance-c2off")
        self.assertEqual(rc, 0, out)
        self.assertEqual((self.disables(0), self.disables(1), self.disables(2), self.disables(3)), ({"0"}, {"0"}, {"1"}, {"1"}))
        self.assertEqual(self.governors(), {"performance"})

    def test_state_names_not_indices_decide_which_state_is_disabled(self):
        self.f.close()
        self.f = Fake(states=("POLL", "C3", "C1", "C2"))
        self.addCleanup(self.f.close)
        self.orig = self.f.path("orig.json")
        self.assertEqual(self.f.run("record", "--out", self.orig, "--samples", "1")[0], 0)
        self.assertEqual(self.apply("performance-c2off")[0], 0)
        self.assertEqual((self.disables(0), self.disables(1), self.disables(2), self.disables(3)), ({"0"}, {"1"}, {"0"}, {"1"}))

    def test_a_host_without_C2_or_C3_fails_the_c2off_arm(self):
        self.f.close()
        self.f = Fake(states=("POLL", "C1"))
        self.addCleanup(self.f.close)
        self.orig = self.f.path("orig.json")
        self.assertEqual(self.f.run("record", "--out", self.orig, "--samples", "1")[0], 0)
        rc, out = self.apply("performance-c2off")
        self.assertEqual(rc, 1, out)
        self.assertIn("nothing to disable", out)

    def test_every_transition_is_derived_from_the_original_so_PC_to_P_to_asfound_restores(self):
        for arm in ("performance", "performance-c2off", "performance", "as-found"):
            rc, out = self.apply(arm)
            self.assertEqual(rc, 0, "%s: %s" % (arm, out))
        self.assertEqual(self.governors(), {"powersave"})
        self.assertEqual({self.f.rdf("cpu%d/cpufreq/energy_performance_preference" % c) for c in range(4)}, {"balance_performance"})
        for st in range(4):
            self.assertEqual(self.disables(st), {"0"})
        after = self.f.path("after.json")
        self.assertEqual(self.f.run("record", "--out", after, "--samples", "1")[0], 0)
        rc, out = self.f.run("compare", "--a", self.orig, "--b", after)
        self.assertEqual(rc, 0, out)
        self.assertIn("EQUAL", out)

    def test_as_found_from_c2off_reenables_the_deep_states(self):
        self.assertEqual(self.apply("performance-c2off")[0], 0)
        self.assertEqual(self.apply("as-found")[0], 0)
        self.assertEqual((self.disables(2), self.disables(3)), ({"0"}, {"0"}))

    def test_compare_reports_a_left_behind_change_and_ignores_measurements(self):
        self.assertEqual(self.apply("performance-c2off")[0], 0)
        mid = self.f.path("mid.json")
        self.f.run("record", "--out", mid, "--samples", "1")
        rc, out = self.f.run("compare", "--a", self.orig, "--b", mid)
        self.assertEqual(rc, 1, out)
        self.assertIn("cpu0.governor: 'powersave' -> 'performance'", out)
        self.assertIn("cpu3.idle[C3].disable: 0 -> 1", out)
        self.assertEqual(self.apply("as-found")[0], 0)
        w(self.f.sys + "/cpu1/cpufreq/scaling_cur_freq", "999999\n")  # a measurement changed: still equal
        w(self.f.sys + "/cpu1/cpuidle/state2/usage", "77777\n")
        end = self.f.path("end.json")
        self.f.run("record", "--out", end, "--samples", "1")
        self.assertEqual(self.f.run("compare", "--a", self.orig, "--b", end)[0], 0)

    @unittest.skipIf(os.geteuid() == 0, "root ignores file modes")
    def test_a_refused_governor_write_fails_the_arm_and_the_result_says_which_cpu(self):
        os.chmod(self.f.sys + "/cpu2/cpufreq/scaling_governor", 0o444)
        rc, out = self.apply("performance")
        self.assertEqual(rc, 1, out)
        self.assertIn("cpu2", out)
        res = jload((self.f.path("res-performance.json")))
        self.assertFalse(res["ok"])
        self.assertTrue(any("cpu2" in e for e in res["errors"]))
        os.chmod(self.f.sys + "/cpu2/cpufreq/scaling_governor", 0o644)
        self.assertEqual(self.apply("as-found")[0], 0)  # the restore still works and ends equal

    @unittest.skipIf(os.geteuid() == 0, "root ignores file modes")
    def test_a_refused_idle_write_fails_the_c2off_arm(self):
        os.chmod(self.f.sys + "/cpu1/cpuidle/state3/disable", 0o444)
        rc, out = self.apply("performance-c2off")
        self.assertEqual(rc, 1, out)
        self.assertIn("cpu1", out)

    @unittest.skipIf(os.geteuid() == 0, "root ignores file modes")
    def test_a_refused_epp_write_is_a_warning_but_a_refused_epp_restore_is_an_error(self):
        os.chmod(self.f.sys + "/cpu0/cpufreq/energy_performance_preference", 0o444)
        rc, out = self.apply("performance")  # the write is refused: the EPP stays as found, the governor took: warning only
        self.assertEqual(rc, 0, out)
        self.assertIn("warning", out)
        os.chmod(self.f.sys + "/cpu0/cpufreq/energy_performance_preference", 0o644)
        self.assertEqual(self.apply("performance")[0], 0)  # now it takes
        os.chmod(self.f.sys + "/cpu0/cpufreq/energy_performance_preference", 0o444)
        rc, out = self.apply("as-found")  # the EPP cannot be put back: the restore fails loudly
        self.assertEqual(rc, 1, out)
        self.assertIn("cpu0", out)

    def test_a_cpu_that_went_offline_fails_the_arm(self):
        shutil.rmtree(self.f.sys + "/cpu3/cpufreq")
        rc, out = self.apply("performance")
        self.assertEqual(rc, 1, out)
        self.assertIn("differ from the original record", out)

    def test_a_host_wide_setting_that_moved_is_a_read_back_error(self):
        w(self.f.sys + "/cpufreq/boost", "0\n")
        rc, out = self.apply("performance")
        self.assertEqual(rc, 1, out)
        self.assertIn("global.cpufreq_boost", out)

    def test_stop_rule(self):
        rc, out = self.f.run("stop-rule", "--orig", self.orig)
        self.assertEqual(rc, 0, out)
        self.assertEqual(self.apply("performance")[0], 0)
        perf = self.f.path("perf.json")
        self.f.run("record", "--out", perf, "--samples", "1")
        rc, out = self.f.run("stop-rule", "--orig", perf)
        self.assertEqual(rc, 3, out)
        self.assertIn("4 of 4", out)

    def test_the_log_mode_writes_one_json_line_a_tick(self):
        out = self.f.path("cpufreq.log")
        rc, o = self.f.run("log", "--out", out, "--seconds", "0.3", "--interval", "0.1")
        self.assertEqual(rc, 0, o)
        lines = [json.loads(x) for x in jlines(out)]
        self.assertGreaterEqual(len(lines), 2)
        self.assertEqual(lines[0]["governor"], "powersave=4")
        self.assertEqual((lines[0]["khz_min"], lines[0]["khz_max"], lines[0]["mhz_max"]), (1500000, 1800000, 1800.0))

    def test_an_unreadable_record_and_a_bad_arm_are_usage_errors(self):
        self.assertEqual(self.f.run("apply", "--arm", "turbo", "--orig", self.orig)[0], 2)
        self.assertEqual(self.f.run("apply", "--arm", "performance", "--orig", self.f.path("nope.json"))[0], 2)

    def test_a_host_without_cpufreq_records_nothing(self):
        shutil.rmtree(self.f.sys)
        os.makedirs(self.f.sys)
        rc, out = self.f.run("record", "--out", self.f.path("x.json"))
        self.assertEqual(rc, 1, out)


if __name__ == "__main__":
    unittest.main(verbosity=1)
