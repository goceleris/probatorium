#!/usr/bin/env python3
"""Tests of ansible/files/os_upgrade.py against a fake host (osu_fakehost.py): the grub menu parser on two
generated grub.cfg samples, the survey (and that it writes nothing and starts nothing that changes a host), every
host-changing subcommand in the order the playbook runs them, and the refusals each of them has.

    python3 ansible/tests/test_os_upgrade.py        (python 3 standard library only; no root, no apt, no grub)

workflow_cluster_os_upgrade_test.go runs this file in CI (skipping when python3 is absent).
"""
import json
import os
import shutil
import sys
import tempfile
import time
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
sys.path.insert(0, os.path.join(HERE, "..", "files"))
import osu_fakehost  # noqa: E402
from osu_fakehost import FakeHost, FIXTURES  # noqa: E402
import os_upgrade as osu  # noqa: E402

UUID = "0b1c2d3e-aaaa-bbbb-cccc-000000000001"
ID_31 = "gnulinux-advanced-%s>gnulinux-7.0.0-31-generic-advanced-%s" % (UUID, UUID)
ID_38 = "gnulinux-advanced-%s>gnulinux-7.0.0-38-generic-advanced-%s" % (UUID, UUID)
ID_40 = "gnulinux-advanced-%s>gnulinux-7.0.0-40-generic-advanced-%s" % (UUID, UUID)

# commands a survey may run: nothing that changes a host
MUTATING = ("grub-set-default", "grub-reboot", "update-grub", "update-initramfs", "dkms autoinstall", "apt-mark manual", "apt-get -o DPkg::Lock::Timeout=600 update",
            "apt-get -y")


def cfg_text(name):
    with open(os.path.join(FIXTURES, name)) as f:
        return f.read()


def unpinned_cfg(text):
    """The grub.cfg of a host with GRUB_DEFAULT=0: no saved-entry logic at all."""
    start = text.index('if [ "${next_entry}" ] ; then')
    end = text.index("fi\n", text.index('set default="${saved_entry}"')) + 3
    return text[:start] + 'set default="0"\n' + text[end:]


class Base(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="osu-test-")
        self.addCleanup(shutil.rmtree, self.tmp, True)
        self.h = FakeHost(self.tmp)
        # the host as msr1 is now: running -31, -38 installed and newest, GRUB_DEFAULT=0 so grub boots -38
        self.h.w("/boot/grub/grub.cfg", unpinned_cfg(cfg_text("grub-arm64-efi.cfg")))
        self.h.state["next_grub_cfg"] = os.path.join(FIXTURES, "grub-arm64-efi.cfg")  # what update-grub generates once pinned

    def j(self, r):
        self.assertEqual(r.returncode, 0, r.stderr)
        return json.loads(r.stdout)

    def run_ok(self, *a):
        self.h.save()
        return self.j(self.h.run(*a))


class GrubParser(unittest.TestCase):
    def test_arm64_entries(self):
        e = osu.parse_grub_cfg(cfg_text("grub-arm64-efi.cfg"))
        non_rec = [x for x in e if not x["recovery"]]
        self.assertEqual(sorted({x["kernel"] for x in e if x["kernel"]}), ["7.0.0-31-generic", "7.0.0-38-generic"])
        self.assertEqual(len(non_rec), 4)  # Ubuntu (simple), 2 x advanced, UEFI firmware (no kernel)
        self.assertEqual(osu.entry_id_for(e, "7.0.0-31-generic"), ID_31)
        self.assertEqual(osu.entry_id_for(e, "7.0.0-38-generic"), ID_38)
        self.assertEqual(osu.entry_id_for(e, "7.0.0-99-generic"), "")

    def test_a_recovery_entry_is_never_chosen(self):
        e = osu.parse_grub_cfg(cfg_text("grub-arm64-efi.cfg"))
        self.assertNotIn("recovery", osu.entry_id_for(e, "7.0.0-31-generic"))
        rec = [x for x in e if x["recovery"]]
        self.assertEqual(len(rec), 2)
        self.assertTrue(all(x["kernel"] for x in rec))

    def test_amd64_entries(self):
        e = osu.parse_grub_cfg(cfg_text("grub-amd64.cfg"))
        self.assertTrue(osu.entry_id_for(e, "6.14.0-37-generic").endswith(">gnulinux-6.14.0-37-generic-advanced-9f8e7d6c-1111-2222-3333-000000000002"))

    def test_kernel_of_entry_by_full_id_and_by_title(self):
        e = osu.parse_grub_cfg(cfg_text("grub-arm64-efi.cfg"))
        self.assertEqual(osu.kernel_of_entry(e, ID_38), "7.0.0-38-generic")
        self.assertEqual(osu.kernel_of_entry(e, "Ubuntu, with Linux 7.0.0-31-generic"), "7.0.0-31-generic")
        self.assertEqual(osu.kernel_of_entry(e, "no such entry"), "")

    def test_recordfail_timeout(self):
        self.assertEqual(osu.cfg_recordfail_timeout(cfg_text("grub-arm64-efi.cfg")), "5")
        self.assertEqual(osu.cfg_recordfail_timeout(cfg_text("grub-arm64-efi.cfg").replace("set timeout=5", "set timeout=-1")), "-1")

    def test_kernel_version_order_is_numeric(self):
        self.assertTrue(osu.kver_key("7.0.0-100-generic") > osu.kver_key("7.0.0-38-generic"))
        self.assertEqual(osu.flavor("7.0.0-38-generic-64k"), "generic-64k")

    def test_dkms_status_formats(self):
        for line, want in (("r8127/11.015.00, 7.0.0-31-generic, aarch64: installed", ("r8127", "11.015.00", "7.0.0-31-generic", "installed")),
                           ("r8127, 11.015.00, 7.0.0-31-generic, aarch64: installed", ("r8127", "11.015.00", "7.0.0-31-generic", "installed")),
                           ("r8127/11.015.00, 7.0.0-40-generic, aarch64: built", ("r8127", "11.015.00", "7.0.0-40-generic", "built"))):
            m = osu.DKMS.match(line)
            self.assertIsNotNone(m, line)
            self.assertEqual((m.group(1), m.group(2), m.group(3), m.group(5)), want)


class Survey(Base):
    def test_records_what_the_maintainer_asked_for(self):
        s = self.run_ok("survey")
        self.assertEqual(s["kernel"], "7.0.0-31-generic")
        self.assertEqual(s["os_release"]["VERSION_ID"], "26.04")
        self.assertEqual(s["installed_kernels"], ["7.0.0-31-generic", "7.0.0-38-generic"])
        self.assertEqual(s["newest_same_flavor"], "7.0.0-38-generic")
        b = s["boot"]
        self.assertFalse(b["cfg_uses_saved"])
        self.assertEqual(b["default_kernel"], "7.0.0-38-generic")  # grub would boot -38 after a power-cycle: not the running kernel
        self.assertEqual(b["running_entry_id"], ID_31)
        self.assertEqual(b["settings"]["GRUB_DEFAULT"], "0")
        self.assertTrue(b["efi"])
        self.assertEqual(b["boot_fs"], "ext4")
        self.assertEqual(b["boot_stack"], "")
        # packages
        pk = s["packages"]
        self.assertEqual(pk["simulation"]["kernel_inst"], ["linux-image-7.0.0-40-generic"])
        self.assertEqual(pk["kept_back"], ["linux-generic"])
        self.assertEqual(pk["phased_held_back"], ["phased-pkg"])
        self.assertEqual(pk["third_party_repos"], ["https://pkgs.tailscale.com/stable/ubuntu"])
        self.assertFalse(pk["dpkg_lock_held"])
        self.assertEqual(pk["package_processes"], [])
        self.assertFalse(pk["policy_rc_d_present"])
        self.assertIsNotNone(pk["lists_age_hours"])
        # unattended-upgrades
        u = s["unattended_upgrades"]
        self.assertTrue(u["installed"])
        self.assertEqual(u["remove_unused_kernel_packages"], "true")
        self.assertEqual(u["units"]["unattended-upgrades.service"]["active"], "active")
        # disk, time, dkms
        self.assertEqual(s["disk"]["/boot"]["free_kib"], 1200000)
        self.assertTrue(s["time_sync"]["ntp_synchronized"])
        self.assertEqual(s["dkms"]["installed_for_running"], ["r8127"])
        self.assertEqual(s["firmware"]["linux_firmware"], "20260301.git1")
        self.assertEqual(s["host_toolchain"]["packages"]["build-essential"], "12.12")
        self.assertEqual(s["marker"], {})
        self.assertFalse(b["dropin_present"])
        self.assertEqual(b["next_entry"], "")

    def test_nics_and_the_r8127_report(self):
        s = self.run_ok("survey")
        self.assertEqual([(n["iface"], n["driver"], n["pci_id"], n["speed_mbps"]) for n in s["nics"]],
                         [("enp1s0", "r8127", "10ec:8127", 10000), ("enp49s0", "r8127", "10ec:8127", 10000)])
        r = s["r8127"]
        self.assertTrue(r["present"])
        self.assertEqual(r["loaded_version"], "11.015.00")
        self.assertEqual(r["source"], "dkms-source-without-package")
        self.assertEqual(r["pci_ids"], ["10ec:8127"])
        self.assertEqual(r["bound"], {"enp1s0": "r8127", "enp49s0": "r8127"})
        self.assertEqual(r["firmware_rtl8127_files"], ["rtl8127a-1.fw"])
        self.assertFalse(r["r8169_blacklisted"])
        # the in-tree r8169 of the running kernel does not list 10ec:8127, the one of -38 does
        pk = r["per_kernel"]
        self.assertFalse(pk["7.0.0-31-generic"]["intree_lists_id"]["10ec:8127"])
        self.assertTrue(pk["7.0.0-38-generic"]["intree_lists_id"]["10ec:8127"])
        self.assertEqual(pk["7.0.0-38-generic"]["claims"]["10ec:8127"], ["r8127", "r8169"])
        self.assertTrue(pk["7.0.0-38-generic"]["r8127"]["found"])
        self.assertEqual(pk["7.0.0-38-generic"]["r8127"]["version"], "11.015.00")

    def test_r8127_from_a_package(self):
        self.h.state["src_pkg"] = "r8127-dkms"
        s = self.run_ok("survey")
        self.assertEqual(s["r8127"]["source"], "package:r8127-dkms")

    def test_a_pinned_host(self):
        self.h.w("/boot/grub/grub.cfg", cfg_text("grub-arm64-efi.cfg"))
        self.h.state["grubenv"]["saved_entry"] = ID_31
        s = self.run_ok("survey")
        self.assertTrue(s["boot"]["cfg_uses_saved"])
        self.assertEqual(s["boot"]["default_kernel"], "7.0.0-31-generic")

    def test_busy_dpkg_and_broken_packages_are_seen(self):
        self.h.w("/proc/4242/comm", "unattended-upgr\n")
        self.h.state["audit"] = ["The following packages are only half configured:", " foo  a thing"]
        self.h.state["held"] = ["linux-image-generic", "vim"]
        s = self.run_ok("survey")["packages"]
        self.assertEqual(s["package_processes"], ["unattended-upgr"])
        self.assertEqual(len(s["dpkg_audit"]), 2)
        self.assertEqual(s["held_kernel"], ["linux-image-generic"])

    def test_a_held_dpkg_lock_is_seen(self):
        import fcntl
        f = open(self.h.root + "/var/lib/dpkg/lock-frontend", "w")
        fcntl.lockf(f, fcntl.LOCK_EX)
        try:
            # the lock is taken by THIS process: lockf locks are per process, so ask from a child
            s = self.run_ok("survey")
        finally:
            f.close()
        # a child process is refused the shared lock while the parent holds the exclusive one
        self.assertTrue(s["packages"]["dpkg_lock_held"])

    def test_boot_on_btrfs_and_lvm_is_flagged(self):
        self.h.state["boot_fs"] = "btrfs"
        self.assertEqual(self.run_ok("survey")["boot"]["boot_stack"], "btrfs")
        self.h.state["boot_fs"] = "ext4"
        self.h.state["boot_src"] = "/dev/mapper/vg-root"
        self.assertEqual(self.run_ok("survey")["boot"]["boot_stack"], "lvm-or-mdraid")

    def test_survey_writes_nothing_and_runs_no_changing_command(self):
        self.h.save()
        before = self.h.snapshot()
        self.j(self.h.run("survey"))
        self.j(self.h.run("simulate"))
        for what in ("packages", "policy", "sysctl", "cmdline", "grub"):
            self.assertEqual(self.h.run("dump", what).returncode, 0)
        self.assertEqual(self.h.run("marker").returncode, 0)
        self.assertEqual(before, self.h.snapshot(), "a read-only subcommand changed the tree")
        calls = self.h.calls()
        self.assertGreater(len(calls), 20)
        for c in calls:
            for m in MUTATING:
                self.assertNotIn(m, c, "a read-only run executed: " + c)
            if c.startswith("apt-get"):
                self.assertIn(" -s ", c + " ", c)
            if c.startswith("apt-mark"):
                self.assertTrue(c.startswith("apt-mark showhold"), c)

    def test_the_read_only_guard_refuses_a_changing_command(self):
        osu.READ_ONLY = True
        try:
            for argv in (["apt-get", "update"], ["apt-get", "-y", "full-upgrade"], ["grub-set-default", "x"], ["apt-mark", "manual", "x"],
                         ["dkms", "autoinstall"], ["rm", "-rf", "/"], ["systemctl", "restart", "x"], ["dpkg", "--configure", "-a"]):
                with self.assertRaises(SystemExit, msg=" ".join(argv)):
                    osu.run(argv)
            with self.assertRaises(SystemExit):
                osu.write("/x", "y")
            self.assertTrue(osu.ro_allowed(["apt-get", "-s", "full-upgrade"]))
        finally:
            osu.READ_ONLY = False

    def test_survey_mode_refuses_every_mutating_subcommand_at_the_command_level(self):
        # the subcommands that read are the ones main() puts in READ_ONLY mode
        src = open(os.path.join(HERE, "..", "files", "os_upgrade.py")).read()
        self.assertIn('a.cmd in ("survey", "simulate", "dump", "upgrade-status", "upgrade-log", "marker")', src)


class BootProof(Base):
    """GRUB must be shown to have BOOTED the host and to read the grubenv Linux writes; unknown is not proven."""

    def proof(self):
        self.h.save()
        return self.run_ok("survey")["boot"]["proof"]

    def test_a_grub_booted_host_is_proven(self):
        p = self.proof()
        self.assertTrue(p["ok"], p["problems"])
        self.assertEqual(p["boot_image"], "/boot/vmlinuz-7.0.0-31-generic")
        self.assertEqual(p["efi_loader"], "\\EFI\\ubuntu\\shimaa64.efi")
        self.assertEqual(p["prefix"], "/boot/grub")

    def test_a_cmdline_without_boot_image_is_refused(self):
        self.h.w("/proc/cmdline", "root=/dev/nvme0n1p2 ro console=ttyAMA0\n")
        p = self.proof()
        self.assertFalse(p["ok"])
        self.assertIn("no BOOT_IMAGE=", p["problems"][0])

    def test_a_boot_image_of_another_kernel_is_refused(self):
        self.h.w("/proc/cmdline", "BOOT_IMAGE=/boot/vmlinuz-7.0.0-38-generic ro\n")
        self.assertIn("does not name vmlinuz-7.0.0-31-generic", self.proof()["problems"][0])

    def test_a_firmware_that_booted_something_else_is_refused(self):
        self.h.set_efi_boot("\\EFI\\systemd\\systemd-bootaa64.efi")
        p = self.proof()
        self.assertFalse(p["ok"])
        self.assertIn("not shim or grub", p["problems"][0])

    def test_the_removable_media_path_is_unsure_and_refused(self):
        self.h.set_efi_boot("\\EFI\\BOOT\\BOOTAA64.EFI")
        self.assertFalse(self.proof()["ok"])

    def test_unreadable_efi_variables_are_unsure_and_refused(self):
        shutil.rmtree(self.h.root + "/sys/firmware/efi/efivars")
        p = self.proof()
        self.assertFalse(p["ok"])
        self.assertIn("EFI variables are not readable", p["problems"][0])

    def test_grub_reading_another_filesystem_than_grub_editenv_writes_is_refused(self):
        self.h.w("/boot/efi/EFI/ubuntu/grub.cfg", "search.fs_uuid bbbbbbbb-0000-0000-0000-000000000000 root hd0,gpt1\nset prefix=($root)'/boot/grub'\n")
        p = self.proof()
        self.assertFalse(p["ok"])
        self.assertIn("never reads", p["problems"][0])

    def test_a_prefix_on_the_esp_is_refused(self):
        self.h.w("/boot/efi/EFI/ubuntu/grub.cfg", "search.fs_uuid %s root hd0,gpt2\nset prefix=($root)'/EFI/ubuntu'\n" % self.h.BOOT_UUID)
        self.assertIn("not /boot/grub", self.proof()["problems"][0])

    def test_a_missing_stub_is_unsure_and_refused(self):
        os.remove(self.h.root + "/boot/efi/EFI/ubuntu/grub.cfg")
        self.assertIn("no readable search/prefix", self.proof()["problems"][0])

    def test_a_missing_or_short_grubenv_is_refused(self):
        with open(self.h.root + "/boot/grub/grubenv", "w") as f:
            f.write("# short\n")
        self.assertIn("not 1024", self.proof()["problems"][0])
        os.remove(self.h.root + "/boot/grub/grubenv")
        self.assertIn("does not exist", self.proof()["problems"][0])

    def test_a_non_efi_host_is_proven_by_boot_image_and_grubenv_alone(self):
        shutil.rmtree(self.h.root + "/sys/firmware/efi")
        self.assertTrue(self.proof()["ok"])


class Simulate(Base):
    def test_removals_are_listed(self):
        self.h.state["sim"] += ["Remv linux-image-7.0.0-25-generic [7.0.0-25.25]", "Remv foo [1]"]
        s = self.run_ok("simulate")
        self.assertEqual(s["remv"], ["linux-image-7.0.0-25-generic", "foo"])
        self.assertEqual([i["name"] for i in s["inst"]], ["linux-image-7.0.0-40-generic", "libc6"])
        self.assertEqual(s["inst"][1]["old"], "2.43-0ubuntu1")

    def test_a_failing_simulation_is_reported(self):
        self.h.state["sim_rc"] = 100
        s = self.run_ok("simulate")
        self.assertEqual(s["rc"], 100)


class PinGrub(Base):
    def pin(self):
        return self.h.run("pin-grub", "--running", "7.0.0-31-generic", "--run-id", "42")

    def test_the_running_kernel_becomes_the_saved_default_before_anything_is_installed(self):
        r = self.j((self.h.save(), self.pin())[1])
        self.assertTrue(r["ok"])
        self.assertEqual(r["saved_entry"], ID_31)
        self.assertEqual(self.h.load()["grubenv"]["saved_entry"], ID_31)
        dropin = open(self.h.root + "/etc/default/grub.d/99-celeris-os-upgrade.cfg").read()
        self.assertIn("GRUB_DEFAULT=saved", dropin)
        self.assertIn("GRUB_RECORDFAIL_TIMEOUT=5", dropin)
        self.assertTrue(os.path.exists(self.h.root + "/var/lib/celeris-os-upgrade/42/etc-default-grub.bak"))
        # /etc/default/grub itself is untouched
        self.assertIn("GRUB_DEFAULT=0", open(self.h.root + "/etc/default/grub").read())
        self.assertEqual(sorted(r["marked_manual"]), ["linux-headers-7.0.0-31-generic", "linux-image-7.0.0-31-generic", "linux-modules-7.0.0-31-generic"])
        calls = self.h.calls()
        self.assertTrue(any(c.startswith("apt-mark manual linux-") for c in calls))
        # no package was touched
        self.assertFalse(any(c.startswith("apt-get") for c in calls))
        # the order: update-grub, then set-default
        self.assertLess(calls.index("update-grub "), calls.index("grub-set-default " + ID_31))

    def test_idempotent(self):
        self.h.save()
        self.j(self.pin())
        self.h.load()
        self.j(self.pin())
        self.assertEqual(open(self.h.root + "/etc/default/grub.d/99-celeris-os-upgrade.cfg").read().count("GRUB_DEFAULT=saved"), 1)

    def test_refuses_savedefault_true(self):
        self.h.w("/etc/default/grub", "GRUB_DEFAULT=0\nGRUB_SAVEDEFAULT=true\n")
        self.h.save()
        r = self.pin()
        self.assertEqual(r.returncode, 1)
        self.assertIn("GRUB_SAVEDEFAULT=true", r.stdout + r.stderr)
        self.assertFalse(any(c.startswith("grub-set-default") for c in self.h.calls()))

    def test_refuses_when_another_file_overrides_grub_default(self):
        self.h.state["next_grub_cfg"] = None  # update-grub regenerates nothing: the cfg stays GRUB_DEFAULT=0
        self.h.save()
        r = self.pin()
        self.assertEqual(r.returncode, 1)
        self.assertIn("does not use the saved entry", r.stdout + r.stderr)

    def test_refuses_when_the_running_kernel_has_no_entry(self):
        self.h.save()
        r = self.h.run("pin-grub", "--running", "7.0.0-12-generic", "--run-id", "42")
        self.assertEqual(r.returncode, 1)
        self.assertIn("no grub menu entry for the running kernel", r.stdout + r.stderr)

    def test_refuses_a_menu_that_waits_for_ever_after_a_failed_boot(self):
        bad = os.path.join(self.tmp, "bad.cfg")
        open(bad, "w").write(cfg_text("grub-arm64-efi.cfg").replace("set timeout=5", "set timeout=-1"))
        self.h.state["next_grub_cfg"] = bad
        self.h.save()
        r = self.pin()
        self.assertEqual(r.returncode, 1)
        self.assertIn("recordfail timeout -1", r.stdout + r.stderr)

    def test_a_failing_update_grub_fails_before_set_default(self):
        self.h.state["update_grub_rc"] = 1
        self.h.save()
        self.assertEqual(self.pin().returncode, 1)
        self.assertFalse(any(c.startswith("grub-set-default") for c in self.h.calls()))


class Upgrade(Base):
    def wait_done(self, timeout=20):
        end = time.time() + timeout
        while time.time() < end:
            st = self.j(self.h.run("upgrade-status", "--run-id", "42"))
            if st["state"] == "done":
                return st
            time.sleep(0.1)
        self.fail("the detached upgrade did not finish")

    def test_detached_run_with_policy_rc_d_and_the_environment(self):
        self.h.save()
        self.assertEqual(self.j(self.h.run("upgrade-status", "--run-id", "42"))["state"], "none")
        self.j(self.h.run("upgrade-start", "--run-id", "42"))
        st = self.wait_done()
        self.assertEqual(st["rc"], 0)
        log = self.h.run("upgrade-log", "--run-id", "42").stdout
        self.assertIn("policy-rc.d present: True", log)
        self.assertIn("env: noninteractive l 1", log)
        self.assertIn("--force-confold", log)
        self.assertIn("full-upgrade", log)
        self.assertFalse(os.path.exists(self.h.root + "/usr/sbin/policy-rc.d"), "the policy-rc.d outlived the run")
        self.assertNotIn("autoremove", " ".join(self.h.calls()))
        self.assertEqual(self.h.load()["packages"]["libc6"]["version"], "2.43-0ubuntu2")

    def test_the_detached_run_survives_the_deletion_of_the_script_copy_ansible_ran(self):
        # ansible.builtin.script runs a temporary copy and deletes it when the task returns
        tmpdir = tempfile.mkdtemp(prefix="osu-copy-")
        copy = os.path.join(tmpdir, "os_upgrade.py")
        shutil.copy(osu_fakehost.SCRIPT, copy)
        self.h.state["upgrade_sleep"] = 1.0
        self.h.save()
        import subprocess
        r = subprocess.run([sys.executable, copy, "upgrade-start", "--run-id", "42"], capture_output=True, text=True, env=self.h.env(), cwd=tmpdir)
        self.assertEqual(r.returncode, 0, r.stderr)
        shutil.rmtree(tmpdir)
        st = self.wait_done()
        self.assertEqual(st["rc"], 0)
        self.assertTrue(os.path.exists(self.h.root + "/var/lib/celeris-os-upgrade/42/os_upgrade.py"))

    def test_a_died_run_is_reported_and_its_policy_rc_d_cleaned_up(self):
        import signal
        self.h.state["upgrade_sleep"] = 30
        self.h.save()
        started = self.j(self.h.run("upgrade-start", "--run-id", "42"))
        time.sleep(0.5)
        self.assertEqual(self.j(self.h.run("upgrade-status", "--run-id", "42"))["state"], "running")
        self.assertTrue(os.path.exists(self.h.root + "/usr/sbin/policy-rc.d"))
        os.kill(started["pid"], signal.SIGKILL)
        end = time.time() + 10
        st = {}
        while time.time() < end:
            st = self.j(self.h.run("upgrade-status", "--run-id", "42"))
            if st["state"] == "died":
                break
            time.sleep(0.2)
        self.assertEqual(st["state"], "died")
        self.assertEqual(self.j(self.h.run("upgrade-cleanup"))["removed"], True)
        self.assertFalse(os.path.exists(self.h.root + "/usr/sbin/policy-rc.d"))
        # a policy-rc.d that is not ours is never removed
        self.h.w("/usr/sbin/policy-rc.d", "#!/bin/sh\nexit 0\n", 0o755)
        self.assertEqual(self.j(self.h.run("upgrade-cleanup"))["removed"], False)
        self.assertTrue(os.path.exists(self.h.root + "/usr/sbin/policy-rc.d"))
        subprocess_kill = [c for c in self.h.calls() if c.startswith("apt-get")]
        self.assertTrue(subprocess_kill)

    def test_status_can_wait_on_the_host(self):
        self.h.save()
        self.j(self.h.run("upgrade-start", "--run-id", "42"))
        t0 = time.time()
        st = self.j(self.h.run("upgrade-status", "--run-id", "42", "--wait", "20"))
        self.assertEqual(st["state"], "done")
        self.assertLess(time.time() - t0, 15)

    def test_a_failing_apt_is_reported_and_policy_rc_d_removed(self):
        self.h.state["upgrade_rc"] = 100
        self.h.save()
        self.j(self.h.run("upgrade-start", "--run-id", "42"))
        self.assertEqual(self.wait_done()["rc"], 100)
        self.assertFalse(os.path.exists(self.h.root + "/usr/sbin/policy-rc.d"))

    def test_an_existing_policy_rc_d_is_not_touched(self):
        self.h.w("/usr/sbin/policy-rc.d", "#!/bin/sh\nexit 0\n", 0o755)
        self.h.save()
        r = self.h.run("upgrade-start", "--run-id", "42")
        self.assertEqual(r.returncode, 1)
        self.assertEqual(open(self.h.root + "/usr/sbin/policy-rc.d").read(), "#!/bin/sh\nexit 0\n")
        self.assertFalse(any(c.startswith("apt-get") for c in self.h.calls()))

    def test_apt_update_runs_apt_get_update(self):
        self.h.save()
        r = self.h.run("apt-update")
        self.assertEqual(r.returncode, 0)
        self.assertIn("apt-get -o DPkg::Lock::Timeout=600 update", self.h.calls())


class DkmsCheck(Base):
    def after_upgrade(self):
        """-40 installed (vmlinuz, initrd, headers, a grub entry) but with no DKMS build yet."""
        for k in ("7.0.0-40-generic",):
            self.h.w("/boot/vmlinuz-" + k)
            self.h.w("/boot/initrd.img-" + k)
            self.h.w("/lib/modules/%s/build/Makefile" % k)
            self.h.w("/lib/modules/%s/modules.alias" % k, "alias pci:v000010ECd00008127sv*sd*bc*sc*i* r8127\n")
            self.h.state["modinfo"][k + "/r8169"] = {"filename": "/x/r8169.ko"}
        # a menu that has -40 as well
        text = cfg_text("grub-arm64-efi.cfg")
        fortieth = text.replace("7.0.0-38-generic", "7.0.0-40-generic")
        # keep -31, add -40: take the -38 entries renamed
        self.h.w("/boot/grub/grub.cfg", text.replace("7.0.0-38-generic", "7.0.0-40-generic"))
        del fortieth
        self.h.state["grubenv"]["saved_entry"] = ID_31
        self.h.state["dkms_status"] = ["r8127/11.015.00, 7.0.0-31-generic, aarch64: installed"]

    def check(self, build=True):
        self.h.save()
        args = ["dkms-check"] + (["--build"] if build else []) + ["--running", "7.0.0-31-generic", "--target", "7.0.0-40-generic"]
        return self.j(self.h.run(*args))

    def test_missing_module_is_built_for_the_new_kernel(self):
        self.after_upgrade()
        d = self.check()
        self.assertTrue(d["built_now"])
        self.assertEqual(d["build_rc"], 0)
        self.assertEqual(d["missing_modules"], [])
        self.assertTrue(d["r8127"]["found"])
        self.assertTrue(d["r8127_aliases"])
        self.assertTrue(d["vmlinuz_present"] and d["initrd_present"] and d["headers_present"])
        self.assertEqual(d["saved_resolves_to"], "7.0.0-31-generic")
        self.assertTrue(d["target_entry_id"].endswith("gnulinux-7.0.0-40-generic-advanced-" + UUID))
        self.assertIn("dkms autoinstall -k 7.0.0-40-generic", self.h.calls())

    def test_a_failed_build_leaves_the_module_missing_and_keeps_the_log(self):
        self.after_upgrade()
        self.h.state["dkms_build_ok"] = False
        self.h.w("/var/lib/dkms/r8127/11.015.00/build/make.log", "error: implicit declaration of function\n")
        d = self.check()
        self.assertEqual(d["build_rc"], 10)
        self.assertEqual(d["missing_modules"], ["r8127"])
        self.assertFalse(d["r8127"]["found"])
        self.assertIn("implicit declaration", json.dumps(d["make_logs"]))

    def test_without_build_nothing_is_built(self):
        self.after_upgrade()
        d = self.check(build=False)
        self.assertFalse(d["built_now"])
        self.assertEqual(d["missing_modules"], ["r8127"])
        self.assertFalse(any(c.startswith("dkms autoinstall") for c in self.h.calls()))

    def test_the_in_tree_driver_claiming_the_pci_id_is_reported(self):
        self.after_upgrade()
        self.h.w("/lib/modules/7.0.0-40-generic/modules.alias",
                 "alias pci:v000010ECd00008127sv*sd*bc*sc*i* r8169\nalias pci:v000010ECd00008127sv*sd*bc*sc*i* r8127\n")
        d = self.check()
        self.assertTrue(d["intree_lists_id"])
        self.assertFalse(d["intree_lists_id_running"])
        self.assertFalse(d["r8169_blacklisted"])
        self.h.w("/etc/modprobe.d/celeris-r8127.conf", "blacklist r8169\n")
        self.assertTrue(self.check()["r8169_blacklisted"])

    def test_secure_boot_and_signer_are_reported(self):
        self.after_upgrade()
        self.h.state["sb"] = "enabled"
        self.h.w("/sys/firmware/efi/efivars/SecureBoot-8be4df61-93ca-11d2-aa0d-00e098032b8c")
        open(self.h.root + "/sys/firmware/efi/efivars/SecureBoot-8be4df61-93ca-11d2-aa0d-00e098032b8c", "wb").write(b"\x06\x00\x00\x00\x01")
        d = self.check()
        self.assertEqual(d["secure_boot"], "enabled")
        self.assertEqual(d["r8127"]["signer"], "")

    def test_a_driver_missing_for_the_new_kernel_is_reported(self):
        self.after_upgrade()
        self.h.state["dkms_build_ok"] = False
        d = self.check()
        self.assertEqual(d["nic_drivers_missing"], ["r8127"])

    def test_a_menu_without_the_new_kernel(self):
        self.after_upgrade()
        self.h.w("/boot/grub/grub.cfg", cfg_text("grub-arm64-efi.cfg"))
        self.assertEqual(self.check()["target_entry_id"], "")

    def test_a_default_that_is_not_the_old_kernel(self):
        self.after_upgrade()
        self.h.state["grubenv"]["saved_entry"] = ID_40
        self.assertNotEqual(self.check()["saved_resolves_to"], "7.0.0-31-generic")

    def test_blacklist_r8169(self):
        self.after_upgrade()
        self.h.save()
        r = self.h.run("blacklist-r8169", "--run-id", "42", "--target", "7.0.0-40-generic")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(open(self.h.root + "/etc/modprobe.d/celeris-r8127.conf").read().splitlines()[-1], "blacklist r8169")
        self.assertIn("update-initramfs -u -k 7.0.0-40-generic", self.h.calls())


class OneShotAndPermanent(DkmsCheck):
    def arm(self):
        self.h.save()
        return self.h.run("arm-oneshot", "--running", "7.0.0-31-generic", "--target", "7.0.0-40-generic")

    def test_the_old_kernel_stays_the_default_and_only_the_next_boot_is_the_new_one(self):
        self.after_upgrade()
        self.h.state["next_grub_cfg"] = self.h.root + "/boot/grub/grub.cfg"
        r = self.j(self.arm())
        env = self.h.load()["grubenv"]
        self.assertEqual(env["saved_entry"], ID_31)
        self.assertEqual(env["next_entry"], ID_40)
        self.assertEqual((r["saved_entry"], r["next_entry"]), (ID_31, ID_40))
        calls = self.h.calls()
        self.assertLess(calls.index("grub-set-default " + ID_31), calls.index("grub-reboot " + ID_40))

    def test_no_entry_for_the_new_kernel_means_no_one_shot(self):
        self.after_upgrade()
        self.h.w("/boot/grub/grub.cfg", cfg_text("grub-arm64-efi.cfg"))
        self.h.state["next_grub_cfg"] = self.h.root + "/boot/grub/grub.cfg"
        r = self.arm()
        self.assertEqual(r.returncode, 1)
        self.assertFalse(any(c.startswith("grub-reboot") for c in self.h.calls()))

    def test_make_permanent_refuses_while_the_one_shot_is_still_armed(self):
        self.after_upgrade()
        self.h.state["next_grub_cfg"] = self.h.root + "/boot/grub/grub.cfg"
        self.j(self.arm())
        self.h.load()
        r = self.h.run("make-permanent", "--running", "7.0.0-31-generic", "--target", "7.0.0-40-generic", "--run-id", "42")
        self.assertEqual(r.returncode, 1)
        self.assertFalse(os.path.exists(self.h.root + "/var/lib/celeris-os-upgrade/last-upgrade.json"))

    def test_make_permanent_sets_the_new_default_and_writes_the_marker(self):
        self.after_upgrade()
        self.h.state["grubenv"]["next_entry"] = ""  # the one-shot was consumed by the boot
        self.h.save()
        r = self.j(self.h.run("make-permanent", "--running", "7.0.0-31-generic", "--target", "7.0.0-40-generic", "--run-id", "42"))
        self.assertTrue(r["permanent"])
        self.assertEqual(self.h.load()["grubenv"]["saved_entry"], ID_40)
        m = self.j(self.h.run("marker"))
        self.assertEqual((m["kernel_before"], m["kernel_after"], m["run_id"], m["permanent"]), ("7.0.0-31-generic", "7.0.0-40-generic", "42", True))
        self.assertEqual(self.run_ok("survey")["marker"]["kernel_after"], "7.0.0-40-generic")


class Toolcheck(Base):
    def test_ok_and_failing(self):
        self.h.save()
        t = self.j(self.h.run("toolcheck"))
        self.assertTrue(t["cc_ok"])
        self.assertTrue(t["https_ok"])
        self.h.state["cc_rc"] = 1
        self.h.state["curl_rc"] = {"github.com": 60, "nodejs.org": 7}
        self.h.save()
        t = self.j(self.h.run("toolcheck"))
        self.assertFalse(t["cc_ok"])
        self.assertFalse(t["https_ok"])
        self.assertEqual(len(t["warnings"]), 1)


if __name__ == "__main__":
    unittest.main(verbosity=1)
