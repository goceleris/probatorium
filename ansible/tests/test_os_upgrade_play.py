#!/usr/bin/env python3
"""Runs the REAL ansible/os-upgrade.yml (not --check) end to end on local connections, with four things replaced
and nothing else: the reboot task (osu_fake_reboot.py stands in for the machine restarting and for what grub would
boot), the host's commands (the stubs of osu_fakehost.py: dpkg, apt-get, dkms, grub-*), sudo (a stand-in), and the
polling delays. It checks the control flow that only a real run shows: what refuses before a host is touched, that
the old kernel is pinned BEFORE apt runs, that a DKMS failure means no one-shot and no reboot, that a host which
fell back or came back wrong is NOT made permanent, what a person is told, and the evidence left behind.

    python3 ansible/tests/test_os_upgrade_play.py     (Linux and ansible-playbook; skips with exit 0 without:
                                                       reboot_snapshot.sh reads /proc with GNU tools)

The grub, apt and dkms commands are STUBS and the kernel never boots: this proves the playbook's ordering and
verdicts, not that a real GRUB honours grub-reboot or that r8127 builds for a real kernel.
"""
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import textwrap
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
ANSIBLE_DIR = os.path.abspath(os.path.join(HERE, ".."))
sys.path.insert(0, HERE)
from osu_fakehost import FakeHost, FIXTURES  # noqa: E402

PLAYBOOK = shutil.which("ansible-playbook")
UUID = "0b1c2d3e-aaaa-bbbb-cccc-000000000001"
ID_31 = "gnulinux-advanced-%s>gnulinux-7.0.0-31-generic-advanced-%s" % (UUID, UUID)
ID_40 = "gnulinux-advanced-%s>gnulinux-7.0.0-40-generic-advanced-%s" % (UUID, UUID)

INVENTORY = """all:
  vars:
    ansible_connection: local
    ansible_user: mini
    ansible_python_interpreter: "{{ ansible_playbook_python }}"
    manifest_path: /tmp/celeris-bench-manifest.json
    results_root: /tmp/celeris-results
  children:
    bench_targets:
      hosts:
        msa2-server:
          lan_ip: 192.168.50.65
        msr1:
          lan_ip: 192.168.50.199
          nic_irq_placement: {iface: enp1s0, vector: enp1s0-8, cpu: 1}
    loadgen:
      hosts:
        msa2-client:
          lan_ip: 192.168.50.195
    cluster:
      children:
        bench_targets:
        loadgen:
"""


def patch_playbook(src, work):
    s = open(src).read()
    m = re.search(r"    - name: Reboot \| issue the reboot.*?register: reboot_result\n", s, re.S)
    assert m, "reboot task not found"
    s = s.replace(m.group(0), """    - name: Reboot | (test) the restart
      ansible.builtin.script:
        cmd: "%s/fake-reboot.sh"
      register: reboot_result
""" % work)
    s = s.replace("{{ playbook_dir }}/files/os_upgrade.py", work + "/osu-wrapper.sh")
    s = s.replace("{{ playbook_dir }}/files/reboot_snapshot.sh", work + "/snapshot-wrapper.sh")
    old = "{{ manifest_path | quote }} {{ results_root | quote }} - - -"
    assert old in s
    s = s.replace(old, old + " {{ inventory_hostname }}")
    for a, b in (("retries: 36", "retries: 2"), ("retries: 24", "retries: 2"), ("delay: 5", "delay: 0")):
        assert a in s, a
        s = s.replace(a, b)
    return s


@unittest.skipIf(PLAYBOOK is None or not sys.platform.startswith("linux"), "needs ansible-playbook on Linux")
class Play(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.work = tempfile.mkdtemp(prefix="osu-play-")
        cls.pb = os.path.join(cls.work, "pb")
        os.makedirs(cls.pb)
        for d in ("files", "tasks", "vars"):
            shutil.copytree(os.path.join(ANSIBLE_DIR, d), os.path.join(cls.pb, d))
        with open(os.path.join(cls.pb, "os-upgrade.yml"), "w") as f:
            f.write(patch_playbook(os.path.join(ANSIBLE_DIR, "os-upgrade.yml"), cls.work))
        with open(os.path.join(cls.pb, "inventory.yml"), "w") as f:
            f.write(INVENTORY)
        cls.h = FakeHost(os.path.join(cls.work, "host"))
        w = cls.work
        with open(w + "/osu-wrapper.sh", "w") as f:
            f.write('#!/bin/bash\nPATH=%s:$PATH OSU_ROOT=%s STUB_STATE=%s STUB_LOG=%s exec %s %s "$@"\n' % (
                cls.h.bin, cls.h.root, cls.h.state_dir, cls.h.log, sys.executable, os.path.join(ANSIBLE_DIR, "files", "os_upgrade.py")))
        with open(w + "/snapshot-wrapper.sh", "w") as f:
            f.write('''#!/bin/bash
# the preflight call carries a sixth argument, the host that asks; FAIL_HOST makes that host's preflight snapshot fail
if [ $# -ge 6 ]; then
	[ "$6" = "${FAIL_HOST:-}" ] && { echo "snapshot failed (test)" >&2; exit 3; }
	set -- "${@:1:5}"
fi
PATH=%s:$PATH STUB_STATE=%s STUB_LOG=%s OSU_ROOT=%s HOST_SNAPSHOT_ROOT=%s exec bash %s "$@"
''' % (cls.h.bin, cls.h.state_dir, cls.h.log, cls.h.root, cls.h.root, os.path.join(ANSIBLE_DIR, "files", "reboot_snapshot.sh")))
        with open(w + "/fake-reboot.sh", "w") as f:
            f.write('#!/bin/bash\nexec %s %s %s %s\n' % (sys.executable, os.path.join(HERE, "osu_fake_reboot.py"), cls.h.root, cls.h.state_dir))
        os.makedirs(w + "/sbin")
        with open(w + "/sbin/sudo", "w") as f:
            f.write('#!/bin/bash\nwhile [ $# -gt 0 ]; do case "$1" in -u | -p | -g | -C | -U) shift 2 ;; -*) shift ;; *) break ;; esac; done\nexec "$@"\n')
        for x in ("osu-wrapper.sh", "snapshot-wrapper.sh", "fake-reboot.sh", "sbin/sudo"):
            os.chmod(os.path.join(w, x), 0o755)

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.work, ignore_errors=True)

    # ---- a host that is ready to be upgraded, fresh for each scenario
    def fresh(self, **state):
        h = self.h
        shutil.rmtree(h.root, ignore_errors=True)
        open(h.log, "w").close()
        h.build("msr1", True)
        h.state["grub_template"] = os.path.join(FIXTURES, "grub-arm64-efi.cfg")
        h.state["next_grub_cfg"] = None
        h.state["upgrade_sleep"] = 0.2
        h.state.update(state)
        h.w("/boot/grub/grub.cfg", "")
        h.save()
        subprocess.run([h.bin + "/update-grub"], env=h.env(), check=True)  # the cfg of a host with GRUB_DEFAULT=0
        h.load()
        h.state["packages"].pop("linux-image-7.0.0-38-generic")
        h.save()
        for p in ("/boot/vmlinuz-7.0.0-38-generic", "/boot/initrd.img-7.0.0-38-generic"):
            os.remove(h.root + p)
        subprocess.run([h.bin + "/update-grub"], env=h.env(), check=True)
        open(h.log, "w").close()
        return h

    def write_extra_vars(self, out, base):
        """extra-vars.json written by the REAL snippet of .github/workflows/cluster-os-upgrade.yml, from the same
        environment variables the workflow gives it: what is tested is what the runner does with free text."""
        with open(os.path.join(ANSIBLE_DIR, "..", ".github", "workflows", "cluster-os-upgrade.yml")) as f:
            wf = f.read()
        m = re.search(r"python3 - <<'PY'\n(.*?)\n\s*PY\n", wf, re.S)
        assert m, "the request writer is not in the workflow"
        code = textwrap.dedent(m.group(1))
        env = dict(os.environ, IN_MODE=base["os_mode"], IN_HOST=base["os_host"], IN_CONFIRM=base.get("os_confirm", ""), IN_REASON=base["os_reason"],
                   IN_ALLOW=str(bool(base.get("allow_results_loss", False))).lower(), IN_REFRESH=str(bool(base.get("refresh_lists", False))).lower(),
                   IN_BLACKLIST=str(bool(base.get("blacklist_r8169", False))).lower(), RUN_ID=base["os_run_id"], OS_OUT=out)
        subprocess.run([sys.executable, "-c", code], env=env, check=True)
        return os.path.join(out, "extra-vars.json")

    # free text that is a Jinja template: it must stay text
    MARK = "/tmp/osu-injection-mark"
    PAYLOAD = "{{ lookup('pipe', 'touch /tmp/osu-injection-mark') }}"

    def play(self, name, extra, env=None):
        out = os.path.join(self.work, "out." + name)
        shutil.rmtree(out, ignore_errors=True)
        os.makedirs(out)
        if os.path.exists(self.MARK):
            os.remove(self.MARK)
        base = {"os_reason": "platform upgrade %s before the final evidence" % self.PAYLOAD, "os_run_id": "4242", "os_out_dir": out}
        base.update(extra)
        e = dict(os.environ, PATH=os.path.join(self.work, "sbin") + os.pathsep + os.environ["PATH"], ANSIBLE_LOCALHOST_WARNING="False",
                 ANSIBLE_INVENTORY_UNPARSED_WARNING="False", ANSIBLE_DEPRECATION_WARNINGS="False", ANSIBLE_ROLES_PATH="/nonexistent",
                 STATE=self.h.state_dir)
        if env:
            e.update(env)
        ev = self.write_extra_vars(out, base)
        r = subprocess.run([PLAYBOOK, "-i", "inventory.yml", "os-upgrade.yml", "-e", "@" + ev, "-e", "upgrade_poll_retries=200", "-e", "os_out_dir=" + out],
                           cwd=self.pb, capture_output=True, text=True, env=e, timeout=900)
        self.log = r.stdout + r.stderr
        self.rc = r.returncode
        self.out = out
        self.assertFalse(os.path.exists(self.MARK), "free text was run as a Jinja template on the runner")
        self.h.load()
        return r

    def j(self, host, name):
        with open(os.path.join(self.out, host, name)) as f:
            return json.load(f)

    def phase(self, host="msr1"):
        with open(os.path.join(self.out, host, "phase.txt")) as f:
            return f.read().strip()

    def calls(self):
        return self.h.calls()

    def summary(self, mode, host, rc=None):
        """The step summary the workflow renders from the evidence of the last play()."""
        if shutil.which("jq") is None:
            self.skipTest("jq not installed")
        script = os.path.join(ANSIBLE_DIR, "..", ".github", "scripts", "os-upgrade-summary.sh")
        r = subprocess.run(["bash", script, self.out, mode, host, str(self.rc if rc is None else rc), "platform upgrade", "4242"],
                           capture_output=True, text=True, timeout=60)
        self.assertEqual(r.returncode, 0, r.stderr)
        return r.stdout

    def idx(self, prefix, calls=None):
        calls = calls or self.calls()
        for i, c in enumerate(calls):
            if c.startswith(prefix):
                return i
        self.fail("no call starting %r in %s" % (prefix, calls))

    def no_call(self, prefix):
        self.assertFalse([c for c in self.calls() if c.startswith(prefix)], "unexpected call " + prefix)

    UP = {"os_mode": "upgrade", "os_host": "msr1", "os_confirm": "UPGRADE-msr1"}

    # ---------------------------------------------------------------------------------------- the good path
    def test_a_healthy_upgrade_of_msr1(self):
        self.fresh()
        self.play("ok", self.UP)
        self.assertEqual(self.rc, 0, self.log[-3000:])
        self.assertEqual(self.phase(), "permanent")
        c = self.calls()
        # the order that makes a power-cycle at any moment boot the old kernel, and the new one only after verifying
        order = [self.idx("grub-set-default " + ID_31), self.idx("apt-get -o DPkg::Lock::Timeout=600 update"), self.idx("apt-get -y"), self.idx("dkms autoinstall"),
                 self.idx("grub-reboot " + ID_40), self.idx("FAKE-REBOOT"), self.idx("grub-set-default " + ID_40)]
        self.assertEqual(order, sorted(order), order)
        self.assertEqual(len(set(order)), len(order))
        # the first grub-set-default is before ANY package change
        self.assertLess(self.idx("grub-set-default"), self.idx("apt-get -y"))
        self.assertLess(self.idx("apt-mark manual"), self.idx("apt-get -y"))
        self.assertEqual(self.h.state["grubenv"]["saved_entry"], ID_40)
        self.assertEqual(self.h.state["grubenv"]["next_entry"], "")
        self.no_call("apt-get autoremove")
        r = self.j("msr1", "result.json")
        self.assertEqual((r["status"], r["kernel_before"], r["kernel_after"], r["default_kernel"]), ("permanent", "7.0.0-31-generic", "7.0.0-40-generic", "7.0.0-40-generic"))
        self.assertTrue(r["marker"]["permanent"])
        self.assertIn(self.PAYLOAD, r["reason"])
        self.assertIn("No power-cycle needed", r["power_cycle"])
        for f in ("preflight-snapshot.json", "pre-snapshot.json", "post-snapshot.json", "pre-survey.json", "post-survey.json", "survey.json", "dkms.json",
                  "simulate.json", "pin.json", "arm.json", "toolcheck.json", "dpkg-before.txt", "dpkg-after.txt", "apt-policy-before.txt",
                  "apt-update.log", "apt-upgrade.log", "sysctl-before.txt", "sysctl-after.txt", "grub-before.txt", "grub-after.txt", "phase.txt", "result.json"):
            self.assertTrue(os.path.getsize(os.path.join(self.out, "msr1", f)) > 0, f)
        self.assertIn("policy-rc.d present: True", open(os.path.join(self.out, "msr1", "apt-upgrade.log")).read())
        self.assertTrue(os.path.exists(self.h.root + "/var/lib/celeris-os-upgrade/last-upgrade.json"))
        md = self.summary("upgrade", "msr1")
        for want in ("UPGRADED, VERIFIED, NEW KERNEL IS NOW THE DEFAULT", "Power-cycle: **not needed**", "linux-image-7.0.0-40-generic", "| kernel | `7.0.0-31-generic` | `7.0.0-40-generic` |",
                     "| enp1s0 | r8127 11.015.00 | r8127 11.015.00 |", "package-diff.txt", "marked manual"):
            self.assertIn(want, md)
        self.assertTrue(os.path.getsize(os.path.join(self.out, "msr1", "package-diff.txt")) > 0)
        for other in ("msa2-server", "msa2-client"):
            self.assertTrue(os.path.exists(os.path.join(self.out, other, "survey.json")))
            self.assertFalse(os.path.exists(os.path.join(self.out, other, "pre-snapshot.json")), "a peer was changed or recorded as a target")

    # ---------------------------------------------------------------------------------------- refusals before any change
    def refused(self, name, extra, text, env=None):
        self.play(name, extra, env)
        self.assertNotEqual(self.rc, 0, self.log[-1500:])
        self.assertIn(text, self.log)
        for pre in ("grub-set-default", "grub-reboot", "update-grub", "apt-get -o DPkg::Lock::Timeout=600 update", "apt-get -y", "apt-mark manual", "dkms autoinstall", "FAKE-REBOOT"):
            self.no_call(pre)
        self.assertEqual(self.h.state["grubenv"]["saved_entry"], "")

    def test_results_in_results_root_refuse_and_allow_results_loss_overrides(self):
        self.fresh()
        os.makedirs(self.h.root + "/tmp/celeris-results/old")
        self.refused("results", self.UP, "refused, nothing was changed: results_root /tmp/celeris-results holds 1 entr(ies)")
        with open(os.path.join(self.out, "msr1", "refusal.txt")) as f:
            self.assertIn("holds 1 entr", f.read())
        md = self.summary("upgrade", "msr1")
        self.assertIn("### Refused: msr1", md)
        self.assertIn("NOT COMPLETE", md)
        self.assertIn("Nothing was rebooted (preflight-ok)", md)
        self.play("results_ok", dict(self.UP, allow_results_loss=True))
        self.assertEqual(self.rc, 0, self.log[-2500:])

    def test_a_manifest_anywhere_refuses(self):
        self.fresh()
        self.h.w("/tmp/celeris-bench-manifest.json", "{}")
        self.refused("manifest", self.UP, "celeris-bench-manifest.json exists on")

    def test_a_peer_that_fails_its_preflight_stops_the_target(self):
        self.fresh()
        self.refused("peer", self.UP, "snapshot failed (test)", env={"FAIL_HOST": "msa2-client"})

    def test_disk_and_package_state_refuse(self):
        self.fresh()
        self.h.state["df"]["/boot"]["free"] = 1000
        self.h.save()
        self.refused("disk", self.UP, "not enough free disk on /boot")
        self.fresh()
        self.h.state["held"] = ["linux-image-generic"]
        self.h.save()
        self.refused("held", self.UP, "held kernel, boot or DKMS packages: linux-image-generic")

    def test_a_boot_that_cannot_be_one_shot_refuses(self):
        self.fresh(boot_fs="btrfs")
        self.refused("btrfs", self.UP, "/boot/grub is on btrfs")

    def test_template_syntax_in_free_text_is_data(self):
        self.fresh()
        # a survey takes no confirmation: a template there is refused, not rendered
        self.play("inj_survey_confirm", {"os_mode": "survey", "os_host": "-", "os_confirm": self.PAYLOAD})
        self.assertNotEqual(self.rc, 0)
        self.assertEqual(self.calls(), [])
        # an upgrade with a template as the confirmation: refused, no host touched
        self.play("inj_upgrade_confirm", dict(self.UP, os_confirm=self.PAYLOAD))
        self.assertNotEqual(self.rc, 0)
        self.assertEqual(self.calls(), [])
        # a template as the reason (the default of play()) in a survey: taken literally, the survey works
        self.play("inj_survey_reason", {"os_mode": "survey", "os_host": "-", "os_confirm": ""})
        self.assertEqual(self.rc, 0, self.log[-1500:])

    def test_a_wrong_request_touches_nothing(self):
        self.fresh()
        for name, extra, _ in (
                ("badconfirm", dict(self.UP, os_confirm="UPGRADE-msa2-server"), 0),
                ("noconfirm", {"os_mode": "upgrade", "os_host": "msr1", "os_confirm": ""}, 0),
                ("allhosts", {"os_mode": "upgrade", "os_host": "all", "os_confirm": "UPGRADE-all"}, 0),
                ("multiline", dict(self.UP, os_reason="two\nlines"), 0),
                ("surveyhost", {"os_mode": "survey", "os_host": "msr1", "os_confirm": ""}, 0),
                ("badmode", {"os_mode": "dist-upgrade", "os_host": "msr1", "os_confirm": "UPGRADE-msr1"}, 0)):
            self.play(name, extra)
            self.assertNotEqual(self.rc, 0, name)
            self.assertFalse(os.path.exists(os.path.join(self.out, "msr1", "preflight-snapshot.json")), name + ": a host was touched")
            self.assertEqual(self.calls(), [], name)

    def test_the_load_generator_goes_last(self):
        self.fresh()
        self.refused("client_first", {"os_mode": "upgrade", "os_host": "msa2-client", "os_confirm": "UPGRADE-msa2-client"}, "order: msa2-server has not been upgraded")
        # every host's record says: verified at the kernel it runs (the three share one fake tree)
        self.fresh()
        self.h.w("/var/lib/celeris-os-upgrade/last-upgrade.json",
                 json.dumps({"permanent": True, "kernel_after": "7.0.0-31-generic", "run_id": "1"}))
        self.play("client_ok", {"os_mode": "upgrade", "os_host": "msa2-client", "os_confirm": "UPGRADE-msa2-client"})
        self.assertEqual(self.rc, 0, self.log[-2500:])
        self.assertEqual(self.phase("msa2-client"), "permanent")

    # ---------------------------------------------------------------------------------------- stops before the reboot
    def test_apt_plan_that_removes_the_kernel_stops_before_any_package_change(self):
        self.fresh()
        self.h.state["sim"] = self.h.state["sim"] + ["Remv linux-image-7.0.0-31-generic [7.0.0-31.31]"]
        self.h.save()
        self.play("remove", self.UP)
        self.assertNotEqual(self.rc, 0)
        self.assertIn("would REMOVE protected package(s): linux-image-7.0.0-31-generic", self.log)
        self.no_call("apt-get -y")
        self.no_call("grub-reboot")
        self.no_call("FAKE-REBOOT")
        self.assertEqual(self.h.state["grubenv"]["saved_entry"], ID_31)  # the (harmless) pin was done
        self.assertEqual(self.phase(), "pinned")

    def test_a_failing_apt_stops_before_the_reboot(self):
        self.fresh(upgrade_rc=100)
        self.play("aptfail", self.UP)
        self.assertNotEqual(self.rc, 0)
        self.assertIn("apt-get full-upgrade did not succeed", self.log)
        self.assertIn("exit 100", self.log)
        self.no_call("grub-reboot")
        self.no_call("FAKE-REBOOT")
        self.assertEqual(self.h.state["grubenv"]["saved_entry"], ID_31)
        self.assertEqual(self.phase(), "upgrading")

    def test_r8127_not_buildable_for_the_new_kernel_refuses_to_reboot(self):
        self.fresh(dkms_build_ok=False)
        self.play("nodkms", self.UP)
        self.assertNotEqual(self.rc, 0)
        self.assertIn("REFUSED TO REBOOT, nothing was rebooted", self.log)
        self.assertIn("r8127 is NOT built for 7.0.0-40-generic", self.log)
        self.no_call("grub-reboot")
        self.no_call("FAKE-REBOOT")
        self.no_call("grub-set-default " + ID_40)
        self.assertEqual(self.h.state["grubenv"]["saved_entry"], ID_31)
        self.assertEqual(self.h.state["grubenv"]["next_entry"], "")
        self.assertEqual(self.phase(), "upgraded")
        r = self.j("msr1", "result.json")
        self.assertEqual(r["status"], "refused-reboot")
        self.assertEqual(r["default_kernel"], "7.0.0-31-generic")
        self.assertIn("No power-cycle needed: nothing was rebooted", r["power_cycle"])
        with open(os.path.join(self.out, "msr1", "refusal.txt")) as f:
            self.assertIn("r8127 is NOT built", f.read())
        self.assertTrue(os.path.getsize(os.path.join(self.out, "msr1", "dkms.json")) > 0)
        md = self.summary("upgrade", "msr1")
        self.assertIn("Nothing was rebooted (upgraded)", md)
        self.assertIn("r8127 built false", md)
        self.assertNotIn("What the next run re-applies", md)
        self.assertIn("### Refused: msr1", md)

    def test_the_in_tree_driver_racing_r8127_refuses_unless_blacklisted_on_request(self):
        self.fresh(new_kernel_alias_races=True)
        self.play("race", self.UP)
        self.assertNotEqual(self.rc, 0)
        self.assertIn("in-tree r8169 of 7.0.0-40-generic lists the same PCI id as r8127 (the running kernel does not) and r8169 is not blacklisted", self.log)
        self.no_call("grub-reboot")
        self.no_call("FAKE-REBOOT")
        self.assertEqual(self.h.state["grubenv"]["saved_entry"], ID_31)
        self.fresh(new_kernel_alias_races=True)
        self.play("race_ok", dict(self.UP, blacklist_r8169=True))
        self.assertEqual(self.rc, 0, self.log[-2500:])
        self.assertLess(self.idx("update-initramfs -u -k 7.0.0-40-generic"), self.idx("grub-reboot"))
        self.assertIn("blacklist r8169", open(self.h.root + "/etc/modprobe.d/celeris-r8127.conf").read())

    # ---------------------------------------------------------------------------------------- after the reboot
    def test_a_boot_that_fell_back_is_not_verified_and_not_made_permanent(self):
        self.fresh()
        self.play("fallback", self.UP, env={"BOOT_MODE": "ignore-next"})
        self.assertNotEqual(self.rc, 0)
        self.assertIn("the host booted the OLD kernel 7.0.0-31-generic again", self.log)
        self.assertEqual(self.h.state["grubenv"]["saved_entry"], ID_31)
        self.no_call("grub-set-default " + ID_40)
        self.assertFalse(os.path.exists(self.h.root + "/var/lib/celeris-os-upgrade/last-upgrade.json"))
        self.assertEqual(self.phase(), "returned")
        r = self.j("msr1", "result.json")
        self.assertEqual(r["status"], "failed-verification")
        self.assertIn("No power-cycle needed: the host answered but failed verification", r["power_cycle"])
        md = self.summary("upgrade", "msr1")
        self.assertIn("Power-cycle: **not needed**. msr1 answered after the reboot but failed verification", md)
        self.assertIn("the one-shot entry for 7.0.0-40-generic fell back", md)

    def test_a_host_that_never_answers_tells_the_person_to_power_cycle(self):
        self.fresh()
        self.play("hang", self.UP, env={"BOOT_MODE": "hang"})
        self.assertNotEqual(self.rc, 0)
        self.assertEqual(self.phase(), "rebooting")
        r = self.j("msr1", "result.json")
        self.assertEqual(r["status"], "rebooting")
        self.assertIn("Power-cycle msr1 PHYSICALLY", r["power_cycle"])
        self.assertIn('"rebooting"', r["power_cycle"])
        self.assertIn("OLD kernel 7.0.0-31-generic", r["power_cycle"])
        md = self.summary("upgrade", "msr1")
        self.assertIn("Power-cycle: **YES, if and only if msr1 does not answer.**", md)
        self.assertIn("Ask the maintainer to power-cycle msr1 physically", md)
        # the old kernel is what a power-cycle boots
        self.assertEqual(self.h.state["grubenv"]["saved_entry"], ID_31)
        self.assertEqual(self.h.state["grubenv"]["next_entry"], ID_40)

    def test_a_nic_that_did_not_come_back_is_not_verified(self):
        self.fresh()
        self.play("nonic", self.UP, env={"BOOT_MODE": "no-nic"})
        self.assertNotEqual(self.rc, 0)
        self.assertIn("NIC enp1s0 (r8127) is gone", self.log)
        self.no_call("grub-set-default " + ID_40)
        self.assertEqual(self.h.state["grubenv"]["saved_entry"], ID_31)

    def test_no_answer_over_the_fabric_after_the_reboot_is_not_verified(self):
        self.fresh()
        self.play("noping", self.UP, env={"BOOT_MODE": "no-ping"})
        self.assertNotEqual(self.rc, 0)
        self.assertIn("no answer from 192.168.50.195 over the fabric", self.log)
        self.no_call("grub-set-default " + ID_40)
        self.assertEqual(self.h.state["grubenv"]["saved_entry"], ID_31)
        # the host now RUNS the unverified new kernel while grub's default is still the old one: a second dispatch must
        # not pin the running (unverified) kernel as the fallback
        before = len(self.calls())
        self.play("noping_again", self.UP)
        self.assertNotEqual(self.rc, 0)
        self.assertIn("an earlier OS upgrade left grub saved default 7.0.0-31-generic while the host runs 7.0.0-40-generic", self.log)
        self.assertEqual([c for c in self.calls()[before:] if c.startswith(("grub-set-default", "grub-reboot", "apt-get -y", "apt-get -o", "apt-mark manual", "update-grub", "FAKE-REBOOT"))], [])
        self.assertEqual(self.h.state["grubenv"]["saved_entry"], ID_31)

    # ---------------------------------------------------------------------------------------- survey mode
    def test_survey_changes_nothing_and_records_every_host(self):
        self.fresh()
        self.h.w("/tmp/celeris-bench-manifest.json", "{}")  # a running bench does not stop a read-only survey
        before = self.h.snapshot()
        self.play("survey", {"os_mode": "survey", "os_host": "-", "os_confirm": ""})
        self.assertEqual(self.rc, 0, self.log[-2500:])
        for host in ("msa2-server", "msr1", "msa2-client"):
            s = self.j(host, "survey.json")
            self.assertEqual(s["kernel"], "7.0.0-31-generic")
            self.assertEqual(s["r8127"]["pci_ids"], ["10ec:8127"])
            self.assertTrue(os.path.getsize(os.path.join(self.out, host, "preflight-snapshot.json")) > 0)
            self.assertFalse(os.path.exists(os.path.join(self.out, host, "pre-snapshot.json")))
            self.assertFalse(os.path.exists(os.path.join(self.out, host, "refusal.txt")))
        with open(os.path.join(self.out, "toolchains.json"), "w") as f:
            json.dump([{"name": "dotnet sdk", "kind": "pinned", "pin": "10.0.400", "where": "x", "upstream": "10.0.402", "upstream_source": "y", "upstream_ahead": "true"}], f)
        md = self.summary("survey", "-")
        for want in ("SURVEYED", "| msr1 |", "| msa2-server |", "| msa2-client |", "r8127 (out-of-tree) against the in-tree r8169", "dotnet sdk", "10.0.402",
                     "in-tree driver as an option on `7.0.0-31-generic`: NO"):
            self.assertIn(want, md)
        self.assertEqual(before, self.h.snapshot(), "the survey changed the host")
        for c in self.calls():
            for bad in ("grub-set-default", "grub-reboot", "update-grub", "dkms autoinstall", "apt-mark manual", "FAKE-REBOOT", "apt-get -o DPkg::Lock::Timeout=600 update", "update-initramfs"):
                self.assertFalse(c.startswith(bad), c)
            if c.startswith("apt-get"):
                self.assertIn(" -s ", c)

    def test_survey_shows_whether_the_in_tree_driver_is_an_option(self):
        self.fresh()
        k = "7.0.0-38-generic"
        for p in ("/boot/vmlinuz-" + k, "/boot/initrd.img-" + k):
            self.h.w(p)
        self.h.w("/lib/modules/%s/modules.alias" % k, "alias pci:v000010ECd00008127sv*sd*bc*sc*i* r8169\nalias pci:v000010ECd00008127sv*sd*bc*sc*i* r8127\n")
        self.h.state["packages"]["linux-image-" + k] = {"version": k, "status": "installed"}
        self.h.state["modinfo"][k + "/r8169"] = {"filename": "/x"}
        self.h.save()
        self.play("survey_intree", {"os_mode": "survey", "os_host": "-", "os_confirm": ""})
        self.assertEqual(self.rc, 0, self.log[-2500:])
        self.assertTrue(self.j("msr1", "survey.json")["r8127"]["per_kernel"][k]["intree_lists_id"]["10ec:8127"])
        md = self.summary("survey", "-")
        self.assertIn("in-tree driver as an option on `7.0.0-38-generic`: the in-tree r8169 of that kernel LISTS the PCI id", md)

    def test_survey_can_refresh_the_apt_lists_when_asked(self):
        self.fresh()
        self.play("refresh", {"os_mode": "survey", "os_host": "-", "os_confirm": "", "refresh_lists": True})
        self.assertEqual(self.rc, 0, self.log[-2500:])
        self.assertTrue([c for c in self.calls() if c == "apt-get -o DPkg::Lock::Timeout=600 update"])
        self.no_call("grub-set-default")


if __name__ == "__main__":
    if PLAYBOOK is None or not sys.platform.startswith("linux"):
        print("SKIP: needs ansible-playbook on Linux")
        sys.exit(0)
    unittest.main(verbosity=1)
