#!/usr/bin/env python3
"""Tests of ansible/files/cpu_power.py against a FAKE sysfs tree (CPU_SYSFS / CPUINFO hooks): record, apply performance and as-found, restore equal to the original, a host already
on performance (zero writes), a host without cpufreq, a refused write (exit 1, state reported), the write-once original, compare (including the scaling limits), summary.
CPU_POWER_SCRIPT points the suite at a mutant (ansible/tests/test_cpu_power_mutations.py). Not tested here: real sysfs, real sudo (the smoke dispatch proves them)."""
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest

sys.dont_write_bytecode = True
HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPT = os.environ.get("CPU_POWER_SCRIPT") or os.path.join(HERE, "..", "files", "cpu_power.py")


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
        shutil.rmtree(self.f.sys + "/cpu3")
        rc, out = self.apply("performance")
        self.assertEqual(rc, 1, out)
        self.assertIn("differ from the original record", out)

    def test_a_host_wide_setting_that_moved_is_a_read_back_error(self):
        w(self.f.sys + "/cpufreq/boost", "0\n")
        rc, out = self.apply("performance")
        self.assertEqual(rc, 1, out)
        self.assertIn("global.cpufreq_boost", out)

    def test_an_unreadable_record_and_a_bad_arm_are_usage_errors(self):
        self.assertEqual(self.f.run("apply", "--arm", "turbo", "--orig", self.orig)[0], 2)
        self.assertEqual(self.f.run("apply", "--arm", "performance", "--orig", self.f.path("nope.json"))[0], 2)

    def record_now(self, name, samples="1"):
        p = self.f.path(name)
        rc, out = self.f.run("record", "--out", p, "--samples", samples, "--gap", "0")
        self.assertEqual(rc, 0, out)
        return p

    def test_performance_then_restore_ends_equal_to_the_original(self):
        self.assertEqual(self.apply("performance")[0], 0)
        self.assertEqual(self.governors(), {"performance"})
        rc, out = self.apply("as-found")
        self.assertEqual(rc, 0, out)
        self.assertEqual(self.governors(), {"powersave"})
        self.assertEqual({self.f.rdf("cpu%d/cpufreq/energy_performance_preference" % c) for c in range(4)}, {"balance_performance"})
        rc, out = self.f.run("compare", "--a", self.orig, "--b", self.record_now("after.json"))
        self.assertEqual(rc, 0, out)
        self.assertIn("EQUAL", out)

    def test_the_restore_writes_back_a_changed_idle_flag_and_a_changed_limit(self):
        """a restore is from the record, not from what this run changed: whatever differs at the end is put back"""
        self.assertEqual(self.apply("performance")[0], 0)
        w(self.f.sys + "/cpu1/cpuidle/state3/disable", "1\n")  # something else disabled C3 meanwhile
        w(self.f.sys + "/cpu2/cpufreq/scaling_min_freq", "800000\n")  # and a limit moved
        w(self.f.sys + "/cpu3/cpufreq/scaling_max_freq", "2000000\n")
        rc, out = self.apply("as-found")
        self.assertEqual(rc, 0, out)
        self.assertEqual(self.disables(3), {"0"})
        self.assertEqual((self.f.rdf("cpu2/cpufreq/scaling_min_freq"), self.f.rdf("cpu3/cpufreq/scaling_max_freq")), ("400000", "3500000"))
        self.assertEqual(self.f.run("compare", "--a", self.orig, "--b", self.record_now("after.json"))[0], 0)

    def test_performance_never_writes_an_idle_state_even_when_one_was_disabled_as_found(self):
        self.f.close()
        self.f = Fake()
        self.addCleanup(self.f.close)
        w(self.f.sys + "/cpu0/cpuidle/state3/disable", "1\n")  # C3 disabled as found on cpu0: left as found
        self.orig = self.record_now("orig.json")
        self.assertEqual(self.apply("performance")[0], 0)
        self.assertEqual({self.f.rdf("cpu0/cpuidle/state%d/disable" % i) for i in (3,)}, {"1"})
        self.assertEqual(self.disables(2), {"0"})

    def test_a_host_already_on_performance_gets_zero_writes_and_a_restore_is_a_no_op(self):
        """msr1 as found on 2026-11-05 (cpufreq, governor performance, no EPP file): nothing is written, in either direction"""
        self.f.close()
        self.f = Fake(ncpu=12, governor="performance", epp=None, driver="scmi")
        self.addCleanup(self.f.close)
        self.orig = self.record_now("orig.json")
        for arm in ("performance", "as-found"):
            rc, out = self.apply(arm)
            self.assertEqual(rc, 0, out)
            self.assertEqual(jload(self.f.path("res-%s.json" % arm))["changed_writes"], 0)
        self.assertEqual(self.governors(), {"performance"})
        self.assertEqual(self.f.run("compare", "--a", self.orig, "--b", self.record_now("after.json"))[0], 0)

    def test_a_host_without_cpufreq_is_recorded_not_an_error_and_apply_changes_nothing(self):
        self.f.close()
        self.f = Fake()
        self.addCleanup(self.f.close)
        for c in range(4):
            shutil.rmtree("%s/cpu%d/cpufreq" % (self.f.sys, c))
        self.orig = self.record_now("orig.json")
        r = jload(self.orig)
        self.assertFalse(r["has_cpufreq"])
        self.assertEqual(r["cpus"]["cpu0"]["governor"], "-")
        self.assertEqual([s["name"] for s in r["cpus"]["cpu0"]["idle"]], ["POLL", "C1", "C2", "C3"])
        rc, out = self.apply("performance")
        self.assertEqual(rc, 0, out)
        self.assertEqual(jload(self.f.path("res-performance.json"))["changed_writes"], 0)
        rc, out = self.f.run("summary", "--rec", self.orig)
        self.assertEqual(rc, 0, out)
        self.assertIn("governor=-=4", out)

    def test_a_host_with_no_cpu_directory_at_all_is_recorded_empty(self):
        shutil.rmtree(self.f.sys)
        os.makedirs(self.f.sys)
        p = self.record_now("empty.json")
        self.assertEqual(jload(p)["cpus"], {})
        rc, out = self.f.run("summary", "--rec", p)
        self.assertEqual(rc, 0, out)
        self.assertIn("no cpufreq", out)
        self.assertEqual(self.f.run("apply", "--arm", "performance", "--orig", p)[0], 0)

    def test_compare_reports_a_left_behind_change_and_ignores_measurements(self):
        self.assertEqual(self.apply("performance")[0], 0)
        rc, out = self.f.run("compare", "--a", self.orig, "--b", self.record_now("mid.json"))
        self.assertEqual(rc, 1, out)
        self.assertIn("cpu0.governor: 'powersave' -> 'performance'", out)
        self.assertIn("cpu3.epp: 'balance_performance' -> 'performance'", out)
        self.assertEqual(self.apply("as-found")[0], 0)
        w(self.f.sys + "/cpu1/cpufreq/scaling_cur_freq", "999999\n")  # a measurement changed: still equal
        w(self.f.sys + "/cpu1/cpuidle/state2/usage", "77777\n")
        self.assertEqual(self.f.run("compare", "--a", self.orig, "--b", self.record_now("end.json"))[0], 0)

    def test_compare_names_every_kind_of_difference(self):
        for rel, val, needle in (("cpu0/cpuidle/state2/disable", "1", "cpu0.idle[C2].disable: 0 -> 1"), ("cpu1/cpufreq/scaling_max_freq", "3000000", "cpu1.max_khz: 3500000 -> 3000000"),
                                 ("cpu2/cpufreq/scaling_min_freq", "800000", "cpu2.min_khz"), ("cpu3/cpufreq/energy_performance_preference", "power", "cpu3.epp"),
                                 ("cpu0/cpufreq/scaling_driver", "acpi-cpufreq", "cpu0.driver"), ("cpufreq/boost", "0", "global.cpufreq_boost"),
                                 ("cpuidle/current_governor", "teo", "global.cpuidle_governor")):
            before = self.f.rdf(rel)
            w(self.f.sys + "/" + rel, val + "\n")
            rc, out = self.f.run("compare", "--a", self.orig, "--b", self.record_now("c.json"))
            self.assertEqual(rc, 1, "%s: %s" % (rel, out))
            self.assertIn(needle, out)
            w(self.f.sys + "/" + rel, before + "\n")
        self.assertEqual(self.f.run("compare", "--a", self.orig, "--b", self.record_now("c.json"))[0], 0)

    def test_summary_states_what_the_host_runs_on(self):
        rc, out = self.f.run("summary", "--rec", self.orig)
        self.assertEqual(rc, 0, out)
        for needle in ("cpus=4", "driver=amd-pstate-epp=4", "governor=powersave=4", "epp=balance_performance=4", "boost=1", "pstate=active", "cpuidle=acpi_idle/menu",
                       "POLL(enabled),C1(enabled),C2(enabled),C3(enabled)"):
            self.assertIn(needle, out)
        self.assertEqual(self.apply("performance")[0], 0)
        rc, out = self.f.run("summary", "--rec", self.record_now("p.json"))
        self.assertIn("governor=performance=4", out)

    def test_apply_refuses_a_governor_the_kernel_does_not_offer(self):
        for c in range(4):
            w("%s/cpu%d/cpufreq/scaling_available_governors" % (self.f.sys, c), "powersave\n")
        rc, out = self.apply("performance")
        self.assertEqual(rc, 1, out)
        self.assertIn("not in scaling_available_governors", out)
        self.assertEqual(self.governors(), {"powersave"})

    def make_run(self, governor="performance", restore=True):
        """a results/cpu-power directory as the step script leaves it, built from this fake host"""
        d = self.f.path("cp-%s-%s" % (governor, restore))
        for label in ("orig", "applied", "final"):
            os.makedirs("%s/%s" % (d, label), exist_ok=True)
        shutil.copy(self.orig, d + "/orig/msa2-server.json")
        if governor == "performance":
            self.assertEqual(self.apply("performance")[0], 0)
            shutil.copy(self.record_now("applied.json"), d + "/applied/msa2-server.json")
            if restore:
                self.assertEqual(self.apply("as-found")[0], 0)
        shutil.copy(self.record_now("final.json"), d + "/final/msa2-server.json")
        status = "RESTORED-EQUAL x" if (governor == "performance" and restore) else ("KEPT-EQUAL x" if governor == "keep" else "NOT-EQUAL x")
        with open(d + "/restore-status.txt", "w") as fh:
            fh.write(status + "\n")
        return d

    def test_gate_passes_a_restored_run_and_names_what_is_wrong_with_others(self):
        d = self.make_run()
        rc, out = self.f.run("gate", "--dir", d, "--hosts", "msa2-server")
        self.assertEqual(rc, 0, out)
        self.assertIn("gate: OK", out)
        self.assertIn("msa2-server applied: cpus=4 driver=amd-pstate-epp=4 governor=performance=4", out)
        self.assertIn("msa2-server final: cpus=4 driver=amd-pstate-epp=4 governor=powersave=4", out)
        rc, out = self.f.run("gate", "--dir", d, "--hosts", "msa2-server,msr1")
        self.assertEqual(rc, 1, out)
        self.assertIn("!= expected", out)
        rc, out = self.f.run("gate", "--dir", self.make_run(restore=False))  # the host was left on performance: final differs
        self.assertEqual(rc, 1, out)
        self.assertIn("final differs from orig", out)
        self.assertIn("restore-status is not RESTORED-EQUAL", out)
        with open(d + "/restore-status.txt", "w") as fh:
            fh.write("NOT-EQUAL x\n")
        rc, out = self.f.run("gate", "--dir", d)  # the records say equal but the status text does not: still a failure
        self.assertEqual(rc, 1, out)
        os.remove(d + "/restore-status.txt")
        self.assertEqual(self.f.run("gate", "--dir", d)[0], 1)
        shutil.copy(d + "/orig/msa2-server.json", d + "/applied/msa2-server.json")  # an applied record still on powersave
        with open(d + "/restore-status.txt", "w") as fh:
            fh.write("RESTORED-EQUAL x\n")
        rc, out = self.f.run("gate", "--dir", d)
        self.assertEqual(rc, 1, out)
        self.assertIn("applied governor is powersave, not performance", out)

    def test_gate_keep(self):
        d = self.make_run(governor="keep")
        rc, out = self.f.run("gate", "--dir", d, "--governor", "keep")
        self.assertEqual(rc, 0, out)
        self.assertEqual(self.f.run("gate", "--dir", d)[0], 1)  # a keep run is not a performance run
        self.assertEqual(self.f.run("gate", "--dir", d, "--governor", "turbo")[0], 2)

    def test_gate_fails_an_applied_record_whose_idle_flags_moved(self):
        d = self.make_run()
        rec = jload(d + "/applied/msa2-server.json")
        rec["cpus"]["cpu1"]["idle"][3]["disable"] = 1
        with open(d + "/applied/msa2-server.json", "w") as fh:
            json.dump(rec, fh)
        rc, out = self.f.run("gate", "--dir", d)
        self.assertEqual(rc, 1, out)
        self.assertIn("idle flags of the applied record differ", out)


if __name__ == "__main__":
    unittest.main(verbosity=1)
