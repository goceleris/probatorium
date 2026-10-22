#!/usr/bin/env python3
"""The three REAL hosts as regression fixtures: the survey.json and preflight-snapshot.json that the first real survey
(cluster-os-upgrade run 39814880117, 2026-10-22, mode=survey, refresh_lists=true) recorded on msa2-server, msr1 and
msa2-client, under ansible/tests/fixtures/real-survey-39814880117/<host>/.

Why: a fake host proves what its author thought of. The first real survey showed the row "dpkg clean, nothing held, apt idle,
no policy-rc.d" as FAIL on all three hosts: the tool judged the idle daemons `unattended-upgrade-shutdown --wait-for-signal` and
`packagekitd` (kernel comm names `unattended-upgr`, `packagekitd`) to be a package run, so every upgrade and rehearsal would have been
refused for ever. These tests replay the recorded values through the summary's checklist (every row) and through the judgement
(every refusal predicate, in every mode) and assert the mark and the verdict per host.

The fixtures keep the structure and every judged value. They were sanitized once: the filesystem UUIDs (the boot filesystem's, and
the root filesystem's that grub's menu entry ids carry) and the boot id were replaced by readable tokens, equal where they were equal
(GRUB's stub filesystem is the /boot/grub filesystem). A survey holds no IP address, MAC address, tailscale address or serial number;
test_no_fixture_holds_an_address_or_an_identifier fails if one ever gets in.

    python3 ansible/tests/test_os_upgrade_real_hosts.py     (jq for the checklist, ansible-playbook for the judgement; each skips without)
"""
import copy
import glob
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import test_os_upgrade_judge as tj  # noqa: E402

REAL = os.path.join(HERE, "fixtures", "real-survey-39814880117")
HOSTS = ("msa2-server", "msr1", "msa2-client")
SUMMARY = os.path.join(HERE, "..", "..", ".github", "scripts", "os-upgrade-summary.sh")
JQ = shutil.which("jq")

if os.environ.get("CI") == "true" and (JQ is None or tj.ANSIBLE is None):
    raise SystemExit("CI=true: this suite needs jq and ansible-playbook and must not skip itself")

IPV4 = re.compile(r"(?<![\w.\-])(?:\d{1,3}\.){3}\d{1,3}(?![\w\-]|\.\d)")
IPV6 = re.compile(r"(?i)(?<![0-9a-f:])(?:[0-9a-f]{1,4}:){7}[0-9a-f]{1,4}(?![0-9a-f:])"
                  r"|(?<![0-9a-f:])(?:[0-9a-f]{1,4}:){0,6}[0-9a-f]{0,4}::(?:[0-9a-f]{1,4}:){0,6}[0-9a-f]{0,4}(?![0-9a-f:])")
MAC = re.compile(r"(?i)\b(?:[0-9a-f]{2}[:-]){5}[0-9a-f]{2}\b")
UUID = re.compile(r"(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b")
SERIAL = re.compile(r"(?i)serial|machine-id|\.ts\.net|\bfd7a:")


def load(host, name="survey.json"):
    with open(os.path.join(REAL, host, name)) as f:
        return json.load(f)


# the marks the maintainer must read on the real hosts. The apt-idle row is PASS: the first survey said FAIL (the tool defect)
COMMON = {
    "GRUB booted this host and reads the grubenv Linux writes": "PASS",
    "no one-shot pending (next_entry)": "PASS",
    "upgrade pin vs the running kernel": "NOTE",
    "kernel a GRUB that ignores the grubenv would boot (first menu entry)": "FAIL",
    "boot filesystem of /boot/grub": "PASS",
    "GRUB_DEFAULT and the recordfail timeout": "PASS",
    "apt would remove nothing protected": "PASS",
    "dpkg clean, nothing held, apt idle, no policy-rc.d": "PASS",
    "the way in is intact today (sshd -t, ssh/tailscaled enabled, netplan generate)": "PASS",
    "disk free": "PASS",
    "clock synchronized": "PASS",
    "a new kernel is on offer": "NOTE",
    "the kernel version parses (X.Y.Z-N-flavor)": "PASS",
    "DKMS modules installed for the running kernel": "PASS",
    "one-shot rehearsal done on the running kernel": "NOTE",
    "kernels armed earlier and never verified": "PASS",
    "run state": "PASS",
    "access-path packages and third-party moves the full-upgrade brings": "NOTE",
    "unattended-upgrades and phasing": "NOTE",
}
R8127 = {
    "r8127: where the module comes from": "PASS",
    "r8127: firmware blob rtl8127a-1.fw": "PASS",
    "r8127: Secure Boot and the signer of the running module": "NOTE",
    "r8127: the r8169 decision (does the newest kernel's in-tree driver list 10ec:8127?)": "PASS",
}
EXPECTED_MARKS = {"msa2-server": COMMON, "msa2-client": COMMON, "msr1": dict(COMMON, **R8127)}


class Hygiene(unittest.TestCase):
    def test_the_patterns_catch_what_they_are_for(self):
        for bad, pat in (("192.168.50.65", IPV4), ("100.64.1.2", IPV4), ('"x": "10.0.0.1"', IPV4), ("fd7a:115c:a1e0::1", IPV6), ("2001:db8::1", IPV6),
                         ("fe80::1ff:fe23:4567:890a", IPV6), ("2001:0db8:0000:0000:0000:ff00:0042:8329", IPV6), ("::1", IPV6), ("aa:bb:cc:dd:ee:ff", MAC), ("AA-BB-CC-DD-EE-FF", MAC),
                         ("48ead342-85d5-4ca1-b186-7bd3d3d5b6a5", UUID), ("dev.tail1234.ts.net", SERIAL), ("Serial Number: 123", SERIAL)):
            self.assertTrue(pat.search(bad), bad)

    def test_the_patterns_leave_versions_and_pci_slots_alone(self):
        for ok in ("1.2.15.3-1ubuntu1.7", "12.12ubuntu2.26.04.2", "0000:31:00.0", "7.0.0-38-generic", "1:26.04.27", "Python 3.14.4", "11.015.00-NAPI", "boot 2026-09-09 10:33:41",
                   "gnulinux-advanced-ROOTFS-MSR1>gnulinux-7.0.0-38-generic-advanced-ROOTFS-MSR1", "BOOTFS-MSR1", "20260319.git217ca6e4.1ubuntu", "version 1.102.3"):
            for pat in (IPV4, IPV6, MAC, UUID, SERIAL):
                self.assertFalse(pat.search(ok), "%s matched %r" % (pat.pattern, ok))

    def test_no_fixture_holds_an_address_or_an_identifier(self):
        files = glob.glob(os.path.join(REAL, "*", "*.json"))
        self.assertEqual(len(files), 6, files)
        for f in files:
            text = open(f).read()
            for name, pat in (("an IPv4 address", IPV4), ("an IPv6 address", IPV6), ("a MAC address", MAC), ("a UUID", UUID), ("a serial / machine id / tailnet name", SERIAL)):
                m = pat.search(text)
                self.assertIsNone(m, "%s holds %s: %r" % (os.path.relpath(f, HERE), name, m and m.group(0)))

    def test_the_fixtures_are_the_real_records(self):
        """Not rewritten to make a test pass: the judged values of the real run are all there."""
        for h in HOSTS:
            s = load(h)
            self.assertEqual((s["host"], s["kernel"], s["newest_same_flavor"], s["boot"]["entry0_kernel"]), (h, "7.0.0-31-generic", "7.0.0-38-generic", "7.0.0-38-generic"))
            self.assertEqual(s["packages"]["package_processes"], ["packagekitd", "unattended-upgr"])  # the value that read as busy
            self.assertIs(s["packages"]["dpkg_lock_held"], False)
            self.assertEqual(s["packages"]["dpkg_audit"], [])
            pf = s["boot"]["proof"]
            self.assertEqual(pf["stub_uuid"], pf["boot_uuid"])
            self.assertTrue(pf["ok"])
        self.assertEqual(load("msr1")["packages"]["simulation"]["inst"].__len__(), 33)
        self.assertEqual(load("msa2-server")["packages"]["simulation"]["inst"].__len__(), 84)
        self.assertEqual(load("msa2-client")["packages"]["simulation"]["inst"].__len__(), 77)


@unittest.skipIf(JQ is None, "jq not installed")
class ChecklistReplay(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.out = tempfile.mkdtemp(prefix="osu-real-")
        for h in HOSTS:
            shutil.copytree(os.path.join(REAL, h), os.path.join(cls.out, h))
        r = subprocess.run(["bash", SUMMARY, cls.out, "survey", "-", "0", "replay of the real survey", "1"], capture_output=True, text=True, timeout=120)
        assert r.returncode == 0, r.stderr
        cls.md = r.stdout
        cls.marks = {}
        for line in cls.md.splitlines():
            m = re.match(r"^\| (msa2-server|msr1|msa2-client) \| (.+?) \| (.*) \| \*\*(\w+)\*\* \| .* \|$", line)
            if m:
                cls.marks.setdefault(m.group(1), {})[m.group(2)] = (m.group(4), m.group(3))

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.out, ignore_errors=True)

    def test_every_row_has_the_expected_mark_on_every_host(self):
        for h in HOSTS:
            got = {k: v[0] for k, v in self.marks[h].items()}
            self.assertEqual(got, EXPECTED_MARKS[h], h)

    def test_the_apt_idle_row_is_pass_on_all_three_hosts(self):
        for h in HOSTS:
            mark, value = self.marks[h]["dpkg clean, nothing held, apt idle, no policy-rc.d"]
            self.assertEqual(mark, "PASS", h)
            self.assertIn("package runs none", value)

    def test_the_first_entry_row_is_fail_on_all_three_hosts(self):
        for h in HOSTS:
            self.assertEqual(self.marks[h]["kernel a GRUB that ignores the grubenv would boot (first menu entry)"][0], "FAIL", h)

    def test_the_r8169_decision_is_pass_on_msr1(self):
        self.assertEqual(self.marks["msr1"]["r8127: the r8169 decision (does the newest kernel's in-tree driver list 10ec:8127?)"][0], "PASS")
        self.assertNotIn("r8127: where the module comes from", self.marks["msa2-server"])

    def test_the_survey_section_does_not_show_the_idle_daemons_as_a_package_run(self):
        self.assertNotIn("unattended-upgr`", self.md)
        self.assertNotIn("packagekitd`", self.md)
        self.assertEqual(self.md.count("package runs `none`"), 3)

    def test_a_real_run_in_the_same_survey_is_marked_busy(self):
        d = tempfile.mkdtemp(prefix="osu-real-busy-")
        try:
            for h in HOSTS:
                shutil.copytree(os.path.join(REAL, h), os.path.join(d, h))
            for h, mut in (("msa2-server", {"package_processes": ["apt-get", "packagekitd", "unattended-upgr", "unattended-upgrade"]}),
                           ("msr1", {"locks_held": ["/var/lib/apt/lists/lock"]}),  # each predicate alone: the names, the lock list, the lock flag
                           ("msa2-client", {"dpkg_lock_held": True})):
                p = os.path.join(d, h, "survey.json")
                s = json.load(open(p))
                s["packages"].update(mut)
                json.dump(s, open(p, "w"))
            r = subprocess.run(["bash", SUMMARY, d, "survey", "-", "0", "x", "1"], capture_output=True, text=True, timeout=120)
            rows = {}
            for line in r.stdout.splitlines():
                m = re.match(r"^\| (\S+) \| dpkg clean, nothing held, apt idle, no policy-rc.d \| (.*) \| \*\*(\w+)\*\* \|", line)
                if m:
                    rows[m.group(1)] = (m.group(3), m.group(2))
            self.assertEqual({h: v[0] for h, v in rows.items()}, {h: "FAIL" for h in HOSTS})
            self.assertIn("package runs apt-get,unattended-upgrade;", rows["msa2-server"][1])
            self.assertIn("locks held /var/lib/apt/lists/lock", rows["msr1"][1])
            self.assertIn("locks held yes", rows["msa2-client"][1])
        finally:
            shutil.rmtree(d, ignore_errors=True)


REHEARSE_FIRST_ENTRY = "the first menu entry of grub is 7.0.0-38-generic, not the running kernel 7.0.0-31-generic"
NO_REHEARSAL = "no successful one-shot rehearsal of this host on the running kernel 7.0.0-31-generic (record: none)"
NOTHING_TO_REPAIR = "nothing to repair: dpkg --audit is clean"
NOTHING_TO_RELEASE = "nothing to release: no hold put by this tool is in place"
NOTHING_TO_ABORT = "nothing to abort: no kernel newer than the running one has a package that is not cleanly installed"
PIN_MISSING = "grub saved default (7.0.0-38-generic) is not the running kernel 7.0.0-31-generic or the upgrade pin is missing"


@unittest.skipIf(tj.ANSIBLE is None, "ansible-playbook not installed")
class JudgementReplay(unittest.TestCase):
    """The judgement of vars/os_upgrade_judge.yml on the real records: every refusal predicate of every mode, host by host."""

    @classmethod
    def setUpClass(cls):
        survey, sim, dk = tj.build_baseline()
        surveys = {h: load(h) for h in HOSTS}
        cls.cases = {}

        def case(name, host, mode, confirm, mutate=None, **kw):
            c = tj.base_case(survey, sim, dk)
            c.update(os_host=host, os_mode=mode, os_confirm=confirm, pre_target=load(host, "preflight-snapshot.json"), pre_survey=copy.deepcopy(surveys[host]),
                     os_surveys={h: {"kernel": surveys[h]["kernel"], "marker": surveys[h]["marker"]} for h in HOSTS}, sim=copy.deepcopy(surveys[host]["packages"]["simulation"]))
            c.update(kw)
            if mutate:
                mutate(c)
            cls.cases[name] = c

        for h in HOSTS:
            case("upgrade-" + h, h, "upgrade", "UPGRADE-" + h)
            case("rehearse-" + h, h, "rehearse", "REHEARSE-" + h)
            case("rehearse-console-" + h, h, "rehearse", "REHEARSE-%s-CONSOLE" % h)
            case("blacklist-" + h, h, "upgrade", "UPGRADE-" + h, blacklist_r8169=True)
            for action in ("configure", "release-holds", "abort-new-kernel"):
                case("repair-%s-%s" % (action, h), h, "repair", "REPAIR-" + h, repair_action=action)
            case("disarm-" + h, h, "disarm", "DISARM-" + h)
        # a refusal predicate fires on a real record when the real value is what trips it
        case("upgrade-msr1-after-a-real-run-started", "msr1", "upgrade", "UPGRADE-msr1",
             lambda c: c["pre_survey"]["packages"].update(package_processes=["apt-get", "packagekitd", "unattended-upgr"]))
        case("upgrade-msr1-a-held-lock", "msr1", "upgrade", "UPGRADE-msr1",
             lambda c: c["pre_survey"]["packages"].update(locks_held=["/var/cache/apt/archives/lock"], dpkg_lock_held=True))
        # the servers upgraded and verified (their record names the kernel they run): the client's order refusals go
        def servers_done(c):
            for h in ("msa2-server", "msr1"):
                c["os_surveys"][h] = {"kernel": "7.0.0-38-generic", "marker": {"permanent": True, "kernel_after": "7.0.0-38-generic"}}
        case("upgrade-msa2-client-after-both-servers", "msa2-client", "upgrade", "UPGRADE-msa2-client", servers_done)
        cls.out = tj.judge_all(cls.cases)

    def refusals(self, name):
        r = self.out[name]["refusals"]
        return [x for x in r.split(" ;; ") if x]

    def maint(self, name):
        return [x for x in self.out[name]["maint"].split(" ;; ") if x]

    def test_an_upgrade_is_refused_only_for_the_missing_rehearsal_on_the_two_servers(self):
        for h in ("msa2-server", "msr1"):
            r = self.refusals("upgrade-" + h)
            self.assertEqual(len(r), 1, (h, r))
            self.assertTrue(r[0].startswith(NO_REHEARSAL), (h, r[0]))
            self.assertNotIn("busy", self.out["upgrade-" + h]["refusals"])

    def test_the_client_upgrade_is_refused_for_the_rehearsal_and_the_order_while_the_servers_are_not_upgraded(self):
        r = self.refusals("upgrade-msa2-client")
        self.assertEqual(len(r), 3, r)
        self.assertTrue(r[0].startswith(NO_REHEARSAL))
        self.assertTrue(r[1].startswith("order: msa2-server has not been upgraded and verified yet"))
        self.assertTrue(r[2].startswith("order: msr1 has not been upgraded and verified yet"))

    def test_a_rehearsal_is_refused_on_all_three_hosts_without_a_console(self):
        for h in HOSTS:
            r = self.refusals("rehearse-" + h)
            self.assertEqual(len(r), 1, (h, r))
            self.assertTrue(r[0].startswith(REHEARSE_FIRST_ENTRY), (h, r[0]))

    def test_a_rehearsal_with_the_console_confirmation_has_no_refusal_on_any_host_in_any_order(self):
        # the order rule is for an upgrade: all three hosts can be rehearsed in ONE rack session, the client included, while neither server is upgraded
        for h in HOSTS:
            self.assertEqual(self.refusals("rehearse-console-" + h), [], h)

    def test_the_client_is_upgradable_in_order_once_the_servers_are_done(self):
        r = self.refusals("upgrade-msa2-client-after-both-servers")
        self.assertEqual(len(r), 1, r)
        self.assertTrue(r[0].startswith(NO_REHEARSAL))

    def test_the_blacklist_is_refused_where_a_nic_is_bound_to_r8169_and_fine_on_msr1(self):
        self.assertEqual(len(self.refusals("blacklist-msr1")), 1)  # only the rehearsal
        for h in ("msa2-server", "msa2-client"):
            r = self.refusals("blacklist-" + h)
            self.assertEqual(len(r), 2 if h == "msa2-server" else 4, (h, r))
            self.assertTrue(any(x.startswith("blacklist_r8169=true, but a NIC is bound to r8169 (enp3s0)") for x in r), (h, r))

    def test_a_real_run_or_a_held_lock_in_the_real_record_is_busy(self):
        busy = [x for x in self.refusals("upgrade-msr1-after-a-real-run-started") if "busy" in x]
        self.assertEqual(len(busy), 1)
        self.assertTrue(busy[0].startswith("dpkg or apt is busy (apt-get): an unattended upgrade"), busy)
        self.assertIn("dpkg or apt is busy (an apt or dpkg lock is held (/var/cache/apt/archives/lock))", self.out["upgrade-msr1-a-held-lock"]["refusals"])

    def test_every_repair_is_refused_on_a_clean_real_host_for_its_own_reason(self):
        for h in HOSTS:
            self.assertEqual(self.maint("repair-configure-" + h), [NOTHING_TO_REPAIR], h)
            self.assertEqual([x[:len(NOTHING_TO_RELEASE)] for x in self.maint("repair-release-holds-" + h)], [NOTHING_TO_RELEASE], h)
            r = self.maint("repair-abort-new-kernel-" + h)
            self.assertEqual(len(r), 2, (h, r))
            self.assertTrue(r[0].startswith(NOTHING_TO_ABORT), (h, r))
            self.assertTrue(r[1].startswith(PIN_MISSING), (h, r))

    def test_a_disarm_has_no_refusal(self):
        for h in HOSTS:
            self.assertEqual(self.maint("disarm-" + h), [], h)

    def test_the_plan_of_every_host_is_acceptable(self):
        for n, v in self.out.items():
            self.assertEqual(v["plan"], "", n)


if __name__ == "__main__":
    unittest.main(verbosity=1)
