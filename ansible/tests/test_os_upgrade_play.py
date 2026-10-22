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
import time
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
    ghosts:
      hosts:
        ghost:
          ansible_connection: ssh
          ansible_host: 127.0.0.1
          ansible_port: 1
          ansible_user: nobody
          ansible_ssh_common_args: "-o ConnectTimeout=2 -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null"
"""


def patch_playbook(src, work):
    s = open(src).read()
    m = re.search(r"^( *)- name: Reboot \| issue the reboot.*?register: reboot_result\n", s, re.S | re.M)
    assert m, "reboot task not found"
    i = m.group(1)
    s = s.replace(m.group(0), (i + "- name: Reboot | (test) the restart\n" + i + "  ansible.builtin.script:\n" + i + "    cmd: \"%s/fake-reboot.sh\"\n" + i + "  register: reboot_result\n") % work)
    s = s.replace("{{ playbook_dir }}/files/os_upgrade.py", work + "/osu-wrapper.sh")
    s = s.replace("{{ playbook_dir }}/files/reboot_snapshot.sh", work + "/snapshot-wrapper.sh")
    old = "{{ manifest_path | quote }} {{ results_root | quote }} - - -"
    assert old in s
    s = s.replace(old, old + " {{ inventory_hostname }}")
    for a, b in (("retries: 36", "retries: 2"), ("retries: 24", "retries: 2"), ("delay: 5", "delay: 0")):
        assert a in s, a
        s = s.replace(a, b)
    return s


if os.environ.get("CI") == "true" and (PLAYBOOK is None or not sys.platform.startswith("linux")):
    raise SystemExit("CI=true: this suite needs ansible-playbook on Linux and must not skip itself (a broken ansible install would read as green)")


@unittest.skipIf(PLAYBOOK is None or not sys.platform.startswith("linux"), "needs ansible-playbook on Linux")
class Play(unittest.TestCase):
    rid = 0

    @classmethod
    def setUpClass(cls):
        cls.work = tempfile.mkdtemp(prefix="osu-play-")
        cls.pb = os.path.join(cls.work, "pb")
        os.makedirs(cls.pb)
        for d in ("files", "tasks", "vars"):
            shutil.copytree(os.path.join(ANSIBLE_DIR, d), os.path.join(cls.pb, d))
        with open(os.path.join(cls.pb, "os-upgrade.yml"), "w") as f:
            f.write(patch_playbook(os.path.join(ANSIBLE_DIR, "os-upgrade.yml"), cls.work))
        wait_file = os.path.join(cls.pb, "tasks", "os_upgrade_wait_apt.yml")
        with open(wait_file) as f:
            wt = f.read()
        assert "--wait 30" in wt
        with open(wait_file, "w") as f:
            f.write(wt.replace("--wait 30", "--wait 1"))  # a poll waits 1 s on the host, not 30: the same logic, a faster test
        with open(os.path.join(cls.pb, "inventory.yml"), "w") as f:
            f.write(INVENTORY)
        # variants whose arm / reboot task is delegated to a host that refuses the connection: UNREACHABLE (not FAILED) is the
        # class ansible does not rescue, and no stub produces it on a local connection
        for name, old, new in (("unreach_arm", "        register: arm_raw\n        ignore_unreachable: true\n", "        register: arm_raw\n        ignore_unreachable: true\n        delegate_to: ghost\n"),
                               ("unreach_reboot", "        register: reboot_result\n", "        register: reboot_result\n        delegate_to: ghost\n")):
            d = os.path.join(cls.work, "pb_" + name)
            shutil.copytree(cls.pb, d)
            with open(os.path.join(d, "os-upgrade.yml")) as f:
                pbtext = f.read()
            assert old in pbtext, name
            with open(os.path.join(d, "os-upgrade.yml"), "w") as f:
                f.write(pbtext.replace(old, new, 1))
        cls.h = FakeHost(os.path.join(cls.work, "host"))
        w = cls.work
        with open(w + "/osu-wrapper.sh", "w") as f:
            f.write('#!/bin/bash\n[ -e %s/down ] && { echo "ssh: connect to host: Connection timed out" >&2; exit 255; }\necho "OSU $*" >> %s\nPATH=%s:$PATH OSU_ROOT=%s STUB_STATE=%s STUB_LOG=%s exec %s %s "$@"\n' % (
                cls.h.state_dir, cls.h.log, cls.h.bin, cls.h.root, cls.h.state_dir, cls.h.log, sys.executable, os.path.join(ANSIBLE_DIR, "files", "os_upgrade.py")))
        with open(w + "/snapshot-wrapper.sh", "w") as f:
            f.write('''#!/bin/bash
# the preflight call carries a sixth argument, the host that asks; FAIL_HOST makes that host's preflight snapshot fail
if [ $# -ge 6 ]; then
	[ "$6" = "${FAIL_HOST:-}" ] && { echo "snapshot failed (test)" >&2; exit 3; }
	set -- "${@:1:5}"
fi
root=%s
# BOOT_MODE=stale-first: the first snapshot after the restart still shows the old boot
if [ -s %s/stale_count ] && [ "$(cat %s/stale_count)" -gt 0 ]; then
	echo $(( $(cat %s/stale_count) - 1 )) > %s/stale_count
	root=%s/stale_root
fi
PATH=%s:$PATH STUB_STATE=%s STUB_LOG=%s OSU_ROOT=$root HOST_SNAPSHOT_ROOT=$root exec bash %s "$@"
''' % (cls.h.root, cls.h.state_dir, cls.h.state_dir, cls.h.state_dir, cls.h.state_dir, cls.h.state_dir, cls.h.bin, cls.h.state_dir, cls.h.log, os.path.join(ANSIBLE_DIR, "files", "reboot_snapshot.sh")))
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
    def fresh(self, rehearsed=True, **state):
        h = self.h
        for stale in ("down", "stale_count"):
            if os.path.exists(h.state_dir + "/" + stale):
                os.remove(h.state_dir + "/" + stale)
        shutil.rmtree(h.root, ignore_errors=True)
        open(h.log, "w").close()
        h.build("msr1", True)
        h.state["grub_template"] = os.path.join(FIXTURES, "grub-arm64-efi.cfg")
        h.state["next_grub_cfg"] = None
        h.state["upgrade_sleep"] = 0.2
        h.state.update(state)
        h.w("/boot/grub/grub.cfg", "")
        if rehearsed:  # an upgrade needs a one-shot rehearsal on the running kernel
            h.w("/var/lib/celeris-os-upgrade/rehearsal.json", json.dumps({"kernel": "7.0.0-31-generic", "run_id": "1", "ok": True, "utc": "2026-10-22T00:00:00Z"}))
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
                   IN_BLACKLIST=str(bool(base.get("blacklist_r8169", False))).lower(), IN_REPAIR_ACTION=base.get("repair_action", "configure"), IN_RETRY_KERNEL=base.get("retry_kernel", ""), IN_HOLD=base.get("hold_access_packages", "auto"), RUN_ID=base["os_run_id"], OS_OUT=out)
        subprocess.run([sys.executable, "-c", code], env=env, check=True)
        return os.path.join(out, "extra-vars.json")

    # free text that is a Jinja template: it must stay text
    MARK = "/tmp/osu-injection-mark"
    PAYLOAD = "{{ lookup('pipe', 'touch /tmp/osu-injection-mark') }}"

    def play(self, name, extra, env=None, pb=None, xvars=()):
        out = os.path.join(self.work, "out." + name)
        shutil.rmtree(out, ignore_errors=True)
        os.makedirs(out)
        if os.path.exists(self.MARK):
            os.remove(self.MARK)
        Play.rid += 1  # a workflow run id is unique: the host keeps a record per run
        base = {"os_reason": "platform upgrade %s before the final evidence" % self.PAYLOAD, "os_run_id": str(4000 + Play.rid), "os_out_dir": out}
        base.update(extra)
        e = dict(os.environ, PATH=os.path.join(self.work, "sbin") + os.pathsep + os.environ["PATH"], ANSIBLE_LOCALHOST_WARNING="False",
                 ANSIBLE_INVENTORY_UNPARSED_WARNING="False", ANSIBLE_DEPRECATION_WARNINGS="False", ANSIBLE_ROLES_PATH="/nonexistent",
                 STATE=self.h.state_dir)
        if env:
            e.update(env)
        ev = self.write_extra_vars(out, base)
        r = subprocess.run([PLAYBOOK, "-i", "inventory.yml", "os-upgrade.yml", "-e", "@" + ev, "-e", "upgrade_poll_retries=200", "-e", "os_out_dir=" + out] + [x for v in xvars for x in ("-e", v)],
                           cwd=pb or self.pb, capture_output=True, text=True, env=e, timeout=900)
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
    RH = {"os_mode": "rehearse", "os_host": "msr1", "os_confirm": "REHEARSE-msr1"}

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
                ("lowercase", dict(self.UP, os_confirm="upgrade-msr1"), 0),
                ("lowercase_disarm", {"os_mode": "disarm", "os_host": "msr1", "os_confirm": "disarm-msr1"}, 0),
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
        self.assertIn("in-tree r8169 of 7.0.0-40-generic lists the same PCI id as r8127 and r8169 is not blacklisted in the new kernel initramfs as well", self.log)
        self.no_call("grub-reboot")
        self.no_call("FAKE-REBOOT")
        self.assertEqual(self.h.state["grubenv"]["saved_entry"], ID_31)
        self.fresh(new_kernel_alias_races=True)
        self.play("race_ok", dict(self.UP, blacklist_r8169=True))
        self.assertEqual(self.rc, 0, self.log[-2500:])
        self.assertLess(self.idx("update-initramfs -u -k 7.0.0-40-generic"), self.idx("grub-reboot"))
        self.assertIn("blacklist r8169", open(self.h.root + "/etc/modprobe.d/celeris-r8127.conf").read())

    # ---------------------------------------------------------------------------------------- a failed dkms build and its repair
    R = {"os_mode": "repair", "os_host": "msr1", "os_confirm": "REPAIR-msr1"}

    def test_a_dkms_failure_that_fails_apt_has_a_sanctioned_repair_and_a_sanctioned_abort(self):
        # the most likely failure on msr1: the linux-headers postinst fails because r8127 does not build, so apt exits non-zero
        self.fresh(upgrade_rc=100, upgrade_audit=["linux-headers-7.0.0-40-generic  half-configured"], configure_ok=False)
        self.play("sm_upgrade", self.UP)
        self.assertNotEqual(self.rc, 0)
        self.assertEqual(self.phase(), "upgrading")
        self.assertIn("mode=repair is the sanctioned way", self.log)
        self.no_call("grub-reboot")
        self.assertEqual(self.h.state["grubenv"]["saved_entry"], ID_31)
        # every further dispatch of the upgrade refuses on the broken dpkg state
        self.play("sm_again", self.UP)
        self.assertNotEqual(self.rc, 0)
        self.assertIn("dpkg --audit reports half-configured or broken packages", self.log)
        # a repair that cannot configure (the module still does not build) is red and says so
        self.play("sm_configure_fails", dict(self.R, repair_action="configure"))
        self.assertNotEqual(self.rc, 0)
        r = self.j("msr1", "result.json")
        self.assertEqual((r["status"], r["repair_action"]), ("repair-failed", "configure"))
        self.assertIn("repair_action=abort-new-kernel", self.log)
        self.assertEqual(len(r["dpkg_audit_after"]), 1)
        # the abort: the half-installed new kernel goes, the host stays on the old one
        before = len(self.calls())
        self.h.state["remove_cascade"] = ["linux-generic"]  # apt removes the kernel metapackage with the image it depends on
        self.h.save()
        self.play("sm_abort", dict(self.R, repair_action="abort-new-kernel"))
        self.assertEqual(self.rc, 0, self.log[-2500:])
        r = self.j("msr1", "result.json")
        self.assertEqual((r["status"], r["kernel_running"], r["saved_entry"], r["dpkg_audit_after"]), ("repaired", "7.0.0-31-generic", ID_31, []))
        self.assertNotIn("linux-image-7.0.0-40-generic", self.h.state["packages"])
        self.assertIn("linux-image-7.0.0-31-generic", self.h.state["packages"])
        self.assertEqual([c for c in self.calls()[before:] if c.startswith(("grub-set-default", "grub-reboot", "FAKE-REBOOT"))], [])  # a repair never touches grub
        self.assertEqual(self.phase(), "repaired")
        self.assertEqual(r["kernel_metapackages_removed"], "linux-generic")
        md = self.summary("repair", "msr1")
        self.assertIn("REPAIRED", md)
        self.assertIn("KERNEL METAPACKAGES WERE REMOVED", md)

    def test_a_repair_that_configures_lets_the_upgrade_run_to_the_end(self):
        self.fresh(upgrade_rc=100, upgrade_audit=["linux-headers-7.0.0-40-generic  half-configured"], configure_ok=True)
        self.play("smc_upgrade", self.UP)
        self.assertNotEqual(self.rc, 0)
        self.play("smc_repair", dict(self.R, repair_action="configure"))
        self.assertEqual(self.rc, 0, self.log[-2500:])
        self.assertEqual(self.j("msr1", "result.json")["status"], "repaired")
        # the cause is gone (say, a newer r8127 source): the same upgrade now completes
        self.h.load()
        self.h.state.update(upgrade_rc=0, upgrade_audit=None)
        self.h.save()
        self.play("smc_upgrade_again", self.UP)
        self.assertEqual(self.rc, 0, self.log[-2500:])
        self.assertEqual(self.phase(), "permanent")

    def test_repair_refuses_with_a_one_shot_pending_or_beside_a_bench_or_when_there_is_nothing_to_repair(self):
        self.fresh()
        self.play("rp_nothing", dict(self.R, repair_action="configure"))
        self.assertNotEqual(self.rc, 0)
        self.assertIn("nothing to repair: dpkg --audit is clean", self.log)
        self.no_call("dpkg --force-confold")
        self.fresh(audit=["x half-configured"])
        self.h.state["grubenv"].update(saved_entry=ID_31, next_entry=ID_40)
        self.h.save()
        self.play("rp_armed", dict(self.R, repair_action="configure"))
        self.assertIn("dispatch mode=disarm first", self.log)
        self.fresh(audit=["x half-configured"])
        self.h.w("/tmp/celeris-bench-manifest.json", "{}")
        self.play("rp_bench", dict(self.R, repair_action="configure"))
        self.assertNotEqual(self.rc, 0)
        self.no_call("dpkg --force-confold")

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
        self.assertIn("No power-cycle needed while the host answers", r["power_cycle"])
        md = self.summary("upgrade", "msr1")
        self.assertIn("not needed while msr1 answers", md)
        self.assertIn("the one-shot entry for 7.0.0-40-generic fell back", md)

    def test_a_host_that_never_answers_tells_the_person_to_power_cycle(self):
        self.fresh()
        self.play("hang", self.UP, env={"BOOT_MODE": "hang"})
        self.assertNotEqual(self.rc, 0)
        self.assertEqual(self.phase(), "rebooting")
        r = self.j("msr1", "result.json")
        self.assertEqual(r["status"], "failed-unknown-armed")
        self.assertEqual(r["disarm"], {})
        self.assertIn("STILL ARMED", r["power_cycle"])
        self.assertIn("NEW kernel, once", r["power_cycle"])
        self.assertIn("restores the choice of KERNEL only", r["power_cycle"])
        self.assertIn("OLD kernel 7.0.0-31-generic", r["power_cycle"])
        md = self.summary("upgrade", "msr1")
        self.assertIn("Power-cycle: **YES if msr1 stays silent, and what it boots depends on why.**", md)
        self.assertIn("**STILL ARMED**", md)
        self.assertIn("A power-cycle restores the choice of kernel only", md)
        self.assertIn("Ask the maintainer to power-cycle msr1 physically", md)
        # the old kernel is what a power-cycle boots
        self.assertEqual(self.h.state["grubenv"]["saved_entry"], ID_31)
        self.assertEqual(self.h.state["grubenv"]["next_entry"], ID_40)

    def test_a_reboot_that_was_never_issued_disarms_the_one_shot(self):
        self.fresh()
        self.play("notissued", self.UP, env={"BOOT_MODE": "not-issued"})
        self.assertNotEqual(self.rc, 0)
        self.assertIn("(failed-before-boot-disarmed)", self.log)
        self.assertEqual(self.h.state["grubenv"]["next_entry"], "")
        self.assertEqual(self.h.state["grubenv"]["saved_entry"], ID_31)
        self.assertIn("grub-editenv - unset next_entry", self.calls())
        self.assertEqual(self.phase(), "rebooting")
        r = self.j("msr1", "result.json")
        self.assertEqual(r["status"], "failed-before-boot-disarmed")
        self.assertTrue(r["disarm"]["was_armed"])
        self.assertIn("No power-cycle needed", r["power_cycle"])

    def test_an_arming_that_does_not_verify_leaves_nothing_armed(self):
        self.fresh(reboot_sets_wrong=True)
        self.play("armwrong", self.UP)
        self.assertNotEqual(self.rc, 0)
        self.assertIn("one-shot disarmed: True", self.log)
        self.assertEqual(self.h.state["grubenv"]["next_entry"], "")
        self.no_call("FAKE-REBOOT")
        self.assertEqual(self.phase(), "arming")
        # an arming that failed BEFORE anything was armed must not read as "the new kernel has probably booted"
        r = self.j("msr1", "result.json")
        self.assertEqual(r["status"], "failed-before-arming")
        self.assertNotIn("probably booted", json.dumps(r))
        self.assertIn("No power-cycle needed: nothing was rebooted", r["power_cycle"])
        self.assertIn("not needed**. The arming did not complete", self.summary("upgrade", "msr1"))

    def test_the_disarm_mode_clears_only_the_one_shot(self):
        self.fresh()
        self.h.state["grubenv"].update(saved_entry=ID_31, next_entry=ID_40)
        self.h.w("/tmp/celeris-bench-manifest.json", "{}")  # safe beside a bench: it changes nothing a bench reads
        self.h.save()
        D = {"os_mode": "disarm", "os_host": "msr1", "os_confirm": "DISARM-msr1"}
        self.play("disarm_wrongconfirm", dict(D, os_confirm="UPGRADE-msr1"))
        self.assertNotEqual(self.rc, 0)
        self.assertEqual(self.h.state["grubenv"]["next_entry"], ID_40)
        self.play("disarm", D)
        self.assertEqual(self.rc, 0, self.log[-2000:])
        self.assertEqual(self.h.state["grubenv"]["next_entry"], "")
        self.assertEqual(self.h.state["grubenv"]["saved_entry"], ID_31)
        self.assertEqual(self.phase(), "disarmed")
        r = self.j("msr1", "result.json")
        self.assertEqual((r["status"], r["next_entry_after"]), ("disarmed", ""))
        for c in self.calls():
            for bad in ("grub-set-default", "grub-reboot", "update-grub", "apt-get -y", "apt-mark manual", "dkms autoinstall", "FAKE-REBOOT"):
                self.assertFalse(c.startswith(bad), c)
        md = self.summary("disarm", "msr1")
        self.assertIn("DISARMED", md)
        # disarm and repair look at their target only: a peer that fails its preflight does not stop them
        self.h.state["grubenv"].update(next_entry=ID_40)
        self.h.save()
        self.play("disarm_peer_down", D, env={"FAIL_HOST": "msa2-client"})
        self.assertEqual(self.rc, 0, self.log[-1500:])
        self.assertEqual(self.h.state["grubenv"]["next_entry"], "")
        # nothing pending: a no-op, still green
        self.play("disarm_again", D)
        self.assertEqual(self.rc, 0, self.log[-1500:])
        self.assertFalse(self.j("msr1", "disarm.json")["was_armed"])

    def test_a_host_that_answers_before_it_has_booted_is_judged_on_the_new_boot(self):
        self.fresh()
        self.play("stale", self.UP, env={"BOOT_MODE": "stale-first"})
        self.assertEqual(self.rc, 0, self.log[-2500:])
        self.assertEqual(self.phase(), "permanent")

    def test_a_detached_apt_that_dies_is_reported_and_its_policy_rc_d_cleaned_up(self):
        self.fresh(upgrade_dies=True)
        self.play("died", self.UP)
        self.assertNotEqual(self.rc, 0)
        self.assertIn("state died", self.log)
        self.assertFalse(os.path.exists(self.h.root + "/usr/sbin/policy-rc.d"), "the playbook left the policy-rc.d of a dead run behind")
        self.no_call("grub-reboot")
        self.no_call("FAKE-REBOOT")
        self.assertEqual(self.phase(), "upgrading")

    def test_a_host_that_goes_unreachable_while_the_one_shot_is_armed_still_reaches_the_rescue(self):
        self.fresh()
        self.play("unreach_arm", self.UP, pb=os.path.join(self.work, "pb_unreach_arm"))
        self.assertNotEqual(self.rc, 0)
        self.assertIn("UNREACHABLE!", self.log)  # the arm task really was unreachable, not failed
        self.assertIn("Rescue | disarm the one-shot", self.log)  # ... and the rescue ran all the same
        self.assertEqual(self.h.state["grubenv"]["next_entry"], "")
        r = self.j("msr1", "result.json")
        self.assertEqual(r["status"], "failed-before-arming")  # the rescue reached the host and nothing was armed there
        self.assertEqual(self.phase(), "arming")

    def test_an_unreachable_reboot_task_disarms_a_healthy_host(self):
        self.fresh()
        self.play("unreach_reboot", self.UP, pb=os.path.join(self.work, "pb_unreach_reboot"))
        self.assertNotEqual(self.rc, 0)
        self.assertIn("UNREACHABLE!", self.log)
        self.assertIn("Rescue | disarm the one-shot", self.log)
        self.no_call("FAKE-REBOOT")
        self.assertEqual(self.h.state["grubenv"]["next_entry"], "")  # armed by arm-oneshot, cleared by the rescue
        self.assertEqual(self.h.state["grubenv"]["saved_entry"], ID_31)
        r = self.j("msr1", "result.json")
        self.assertEqual(r["status"], "failed-before-boot-disarmed")
        self.assertTrue(r["disarm"]["was_armed"])
        self.assertIn("No power-cycle needed", r["power_cycle"])

    def test_a_reboot_the_module_gave_up_on_but_which_happened_says_the_new_kernel_probably_booted(self):
        self.fresh()
        self.play("gaveup", self.UP, env={"BOOT_MODE": "booted-but-reported-failed"})
        self.assertNotEqual(self.rc, 0)
        r = self.j("msr1", "result.json")
        self.assertEqual(r["status"], "failed-new-kernel-may-have-booted")
        self.assertIn("probably booted", r["problems"][0])
        self.assertEqual(self.h.state["grubenv"]["saved_entry"], ID_31)
        self.no_call("grub-set-default " + ID_40)

    def test_an_apt_that_outlives_the_waits_keeps_its_policy_and_is_not_cleaned_up_under(self):
        self.fresh(upgrade_sleep=12)
        self.play("aptslow", self.UP, xvars=("upgrade_poll_retries=1", "upgrade_poll_rounds=1"))
        self.assertNotEqual(self.rc, 0)
        self.assertIn("STILL BE RUNNING", self.log)
        self.assertEqual([c for c in self.calls() if c.startswith("OSU upgrade-cleanup")], [], "the cleanup ran while apt was still alive")
        self.assertTrue(os.path.exists(self.h.root + "/usr/sbin/policy-rc.d"), "the services could restart under a running apt")
        self.no_call("grub-reboot")
        end = time.time() + 40  # let the detached run finish: it removes its own policy
        while time.time() < end and os.path.exists(self.h.root + "/usr/sbin/policy-rc.d"):
            time.sleep(0.5)
        self.assertFalse(os.path.exists(self.h.root + "/usr/sbin/policy-rc.d"))

    def test_a_wait_round_does_not_end_while_apt_is_still_running(self):
        self.fresh(upgrade_sleep=5)
        self.play("aptwait", self.UP, xvars=("upgrade_poll_rounds=1",))
        self.assertEqual(self.rc, 0, self.log[-2500:])
        self.assertEqual(self.phase(), "permanent")

    # ---------------------------------------------------------------------------------------- the one-shot rehearsal
    def test_a_rehearsal_proves_the_one_shot_with_no_other_kernel_in_play(self):
        self.fresh(rehearsed=False)
        self.play("rehearse", self.RH)
        self.assertEqual(self.rc, 0, self.log[-2500:])
        self.assertEqual(self.phase(), "rehearsed")
        c = self.calls()
        self.assertTrue(any(x.startswith("OSU arm-rehearsal") for x in c))
        self.assertFalse(any(x.startswith(("apt-get -y", "apt-get -o", "dkms autoinstall", "update-initramfs", "grub-set-default " + ID_40)) for x in c))
        self.assertLess(self.idx("grub-set-default"), self.idx("grub-reboot"))  # pinned first
        self.assertLess(self.idx("grub-reboot"), self.idx("FAKE-REBOOT"))
        self.assertEqual(self.h.state["grubenv"]["next_entry"], "")
        self.assertEqual(self.h.state["grubenv"]["saved_entry"], ID_31)
        rec = json.loads(open(self.h.root + "/var/lib/celeris-os-upgrade/rehearsal.json").read())
        self.assertEqual(rec["kernel"], "7.0.0-31-generic")
        r = self.j("msr1", "result.json")
        self.assertEqual((r["status"], r["kernel_before"], r["kernel_after"]), ("rehearsed", "7.0.0-31-generic", "7.0.0-31-generic"))
        self.assertFalse(os.path.exists(self.h.root + "/var/lib/celeris-os-upgrade/last-upgrade.json"), "a rehearsal is not an upgrade: no marker for the order rule")
        self.assertIn("REHEARSED", self.summary("rehearse", "msr1"))
        # ... and now an upgrade is allowed
        self.play("after_rehearsal", self.UP)
        self.assertEqual(self.rc, 0, self.log[-2500:])

    def test_an_upgrade_is_refused_without_a_rehearsal(self):
        self.fresh(rehearsed=False)
        self.refused("norehearsal", self.UP, "no successful one-shot rehearsal of this host on the running kernel")

    def test_a_rehearsal_where_grub_never_consumed_the_one_shot_records_nothing(self):
        self.fresh(rehearsed=False)
        self.play("keepnext", self.RH, env={"BOOT_MODE": "keep-next"})
        self.assertNotEqual(self.rc, 0)
        self.assertIn("a one-shot is still pending", self.log)
        self.assertIn("the fallback is NOT proven", self.log)
        self.assertFalse(os.path.exists(self.h.root + "/var/lib/celeris-os-upgrade/rehearsal.json"))
        self.assertEqual(self.phase(), "returned")

    def test_a_rehearsal_refuses_what_an_upgrade_refuses(self):
        self.fresh(rehearsed=False)
        self.h.state["boot_fs"] = "btrfs"
        self.h.save()
        self.refused("rehearse_btrfs", self.RH, "/boot/grub is on btrfs")

    # ---------------------------------------------------------------------------------------- after a stranded boot
    def stranded(self):
        """msr1's documented failure: the new kernel is armed, the host never answers, a person power-cycles it: the old kernel runs
        again, next_entry was consumed, the new kernel is installed cleanly."""
        self.fresh()
        self.play("strand", self.UP, env={"BOOT_MODE": "hang"})
        self.assertNotEqual(self.rc, 0)
        self.assertEqual(self.j("msr1", "result.json")["status"], "failed-unknown-armed")
        os.remove(self.h.state_dir + "/down")  # the power-cycle
        self.h.load()
        self.h.state["grubenv"]["next_entry"] = ""  # consumed by the NIC-less boot
        self.h.save()

    def test_after_a_stranding_the_next_upgrade_does_not_arm_the_same_kernel_again(self):
        self.stranded()
        self.assertTrue(os.path.exists(self.h.root + "/var/lib/celeris-os-upgrade/armed-7.0.0-40-generic.json"))
        before = len(self.calls())
        self.play("rearm", self.UP)
        self.assertNotEqual(self.rc, 0)
        self.assertIn("was armed as a one-shot by run", self.log)
        self.assertEqual([c for c in self.calls()[before:] if c.startswith(("grub-reboot", "FAKE-REBOOT"))], [], "the stranding kernel was armed a second time")
        self.assertEqual(self.h.state["grubenv"]["next_entry"], "")

    def test_after_a_stranding_a_deliberate_retry_is_possible(self):
        self.stranded()
        self.play("retry", dict(self.UP, retry_kernel="7.0.0-40-generic"))
        self.assertEqual(self.rc, 0, self.log[-2500:])
        self.assertEqual(self.phase(), "permanent")
        self.assertFalse(os.path.exists(self.h.root + "/var/lib/celeris-os-upgrade/armed-7.0.0-40-generic.json"))

    def test_after_a_stranding_the_sanctioned_way_out_removes_the_kernel(self):
        self.stranded()
        self.h.load()
        self.h.state["configure_ok"] = True
        self.h.save()
        self.play("strand_abort", dict(self.R, repair_action="abort-new-kernel"))
        self.assertEqual(self.rc, 0, self.log[-2500:])
        self.assertNotIn("linux-image-7.0.0-40-generic", self.h.state["packages"])
        self.assertFalse(os.path.exists(self.h.root + "/var/lib/celeris-os-upgrade/armed-7.0.0-40-generic.json"))
        self.assertEqual(self.h.state["grubenv"]["saved_entry"], ID_31)

    # ---------------------------------------------------------------------------------------- holding the access path
    def test_the_access_path_is_held_during_the_upgrade_of_an_r8127_host_by_default(self):
        self.fresh()
        self.play("hold_auto", self.UP)
        self.assertEqual(self.rc, 0, self.log[-2500:])
        with open(os.path.join(self.out, "msr1", "apt-upgrade.log")) as f:
            log = f.read()
        self.assertIn("apt-mark hold grub-common grub-efi-arm64-bin openssh-server shim-signed tailscale", log)
        self.assertIn("held: grub-common grub-efi-arm64-bin openssh-server shim-signed tailscale", log)
        self.assertEqual(self.h.state["held"], [], "the holds outlived the run")
        self.assertEqual(self.j("msr1", "result.json")["access_packages_held"], ["grub-common", "grub-efi-arm64-bin", "openssh-server", "shim-signed", "tailscale"])

    def test_the_hold_can_be_switched_off(self):
        self.fresh()
        self.play("hold_off", dict(self.UP, hold_access_packages="off"))
        self.assertEqual(self.rc, 0, self.log[-2500:])
        with open(os.path.join(self.out, "msr1", "apt-upgrade.log")) as f:
            self.assertNotIn("apt-mark hold", f.read())
        self.assertEqual(self.j("msr1", "result.json")["access_packages_held"], [])

    def test_the_hold_can_be_forced_on(self):
        self.fresh()
        self.h.state["upgrade_sleep"] = 0.2
        self.play("hold_on", dict(self.UP, hold_access_packages="on"))
        self.assertEqual(self.rc, 0, self.log[-2500:])
        self.assertTrue(self.j("msr1", "result.json")["access_packages_held"])

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
        self.assertIn("### Checklist: what to read before any host-changing dispatch", md)
        self.assertLess(md.index("### Checklist"), md.index("| host | OS |"), "the checklist is at the top")
        for want in ("GRUB booted this host and reads the grubenv Linux writes", "one-shot rehearsal done on the running kernel", "the r8169 decision",
                     "the way in is intact today"):
            self.assertIn(want, md)
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
        self.assertIn("| **ACTION** | ACTION: dispatch the first upgrade with blacklist_r8169=true", md)  # the checklist says what to decide beforehand

    def test_the_survey_summary_shows_a_pending_one_shot_and_the_pin(self):
        self.fresh()
        self.h.state["grubenv"].update(saved_entry=ID_31, next_entry=ID_40)
        self.h.save()
        self.play("survey_armed", {"os_mode": "survey", "os_host": "-", "os_confirm": ""})
        self.assertEqual(self.rc, 0, self.log[-1500:])
        md = self.summary("survey", "-")
        self.assertIn("NEXT BOOT", md)
        self.assertIn("one-shot: " + ID_40, md)
        self.assertIn("dispatch mode=disarm", md)
        self.assertIn("upgrade drop-in present: false", md)
        self.assertIn("| no one-shot pending (next_entry) |", md)
        self.assertRegex(md, r"no one-shot pending \(next_entry\) \| gnulinux[^|]* \| \*\*FAIL\*\*")
        self.assertIn("kernel packages newer than the running kernel:", md)

    def test_the_checklist_marks_a_host_whose_boot_is_not_proven(self):
        self.fresh()
        self.h.w("/proc/cmdline", "root=/dev/nvme0n1p2 ro\n")
        self.play("survey_noproof", {"os_mode": "survey", "os_host": "-", "os_confirm": ""})
        self.assertEqual(self.rc, 0, self.log[-1500:])
        md = self.summary("survey", "-")
        self.assertRegex(md, r"GRUB booted this host and reads the grubenv Linux writes \| /proc/cmdline has no BOOT_IMAGE=[^|]*\| \*\*FAIL\*\*")

    def test_the_survey_summary_marks_a_broken_way_in(self):
        self.fresh(sshd_rc=255)
        self.play("survey_ssh", {"os_mode": "survey", "os_host": "-", "os_confirm": ""})
        self.assertEqual(self.rc, 0, self.log[-1500:])
        self.assertIn("sshd -t fails", self.j("msr1", "survey.json")["access_problems"][0])
        self.assertRegex(self.summary("survey", "-"), r"the way in is intact today[^\n]*sshd -t fails[^|]*\| \*\*FAIL\*\*")

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
