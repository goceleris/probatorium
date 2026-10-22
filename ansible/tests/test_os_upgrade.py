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

    def test_a_policy_rc_d_on_the_host_is_seen(self):
        self.h.w("/usr/sbin/policy-rc.d", "#!/bin/sh\nexit 0\n", 0o755)
        p = self.run_ok("survey")["packages"]
        self.assertTrue(p["policy_rc_d_present"])
        self.assertFalse(p["policy_rc_d_ours"])

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

    def test_the_survey_checks_the_way_in_before_anything_is_changed(self):
        self.assertEqual(self.run_ok("survey")["access_problems"], [])
        self.h.state["sshd_rc"] = 255
        self.h.state["netplan_rc"] = 1
        probs = " | ".join(self.run_ok("survey")["access_problems"])
        self.assertIn("sshd -t fails", probs)
        self.assertIn("netplan generate fails", probs)

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

    def test_a_failed_pin_removes_its_drop_in(self):
        self.h.state["update_grub_rc"] = 1
        self.h.save()
        self.assertEqual(self.pin().returncode, 1)
        self.assertFalse(os.path.exists(self.h.root + "/etc/default/grub.d/99-celeris-os-upgrade.cfg"), "a failed pin left GRUB_DEFAULT=saved behind")

    def test_a_pin_that_fails_after_the_drop_in_removes_it_and_regenerates(self):
        bad = os.path.join(self.tmp, "bad2.cfg")
        with open(bad, "w") as f:
            f.write(cfg_text("grub-arm64-efi.cfg").replace("set timeout=5", "set timeout=-1"))
        self.h.state["next_grub_cfg"] = bad
        self.h.save()
        self.assertEqual(self.pin().returncode, 1)
        self.assertFalse(os.path.exists(self.h.root + "/etc/default/grub.d/99-celeris-os-upgrade.cfg"))
        self.assertGreaterEqual(len([c for c in self.h.calls() if c == "update-grub "]), 2)  # the pin's and the undo's

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
        self.assertEqual(self.j(self.h.run("upgrade-cleanup", "--run-id", "42"))["removed"], True)
        self.assertFalse(os.path.exists(self.h.root + "/usr/sbin/policy-rc.d"))
        # a policy-rc.d that is not ours is never removed
        self.h.w("/usr/sbin/policy-rc.d", "#!/bin/sh\nexit 0\n", 0o755)
        self.assertEqual(self.j(self.h.run("upgrade-cleanup", "--run-id", "42"))["removed"], False)
        self.assertTrue(os.path.exists(self.h.root + "/usr/sbin/policy-rc.d"))
        subprocess_kill = [c for c in self.h.calls() if c.startswith("apt-get")]
        self.assertTrue(subprocess_kill)

    def test_cleanup_never_removes_the_policy_while_apt_is_alive(self):
        # a lost poll must not let the services restart under a running apt
        self.h.state["upgrade_sleep"] = 20
        self.h.save()
        self.j(self.h.run("upgrade-start", "--run-id", "42"))
        time.sleep(0.5)
        c = self.j(self.h.run("upgrade-cleanup", "--run-id", "42"))
        self.assertEqual(c["removed"], False)
        self.assertIn("still alive", c["reason"])
        self.assertTrue(os.path.exists(self.h.root + "/usr/sbin/policy-rc.d"))
        # when the run is over it is the run itself that removes it
        pid = int(open(self.h.root + "/var/log/celeris-os-upgrade/42/apt-upgrade.pid").read())
        import signal
        os.kill(pid, signal.SIGKILL)
        time.sleep(0.5)
        c = self.j(self.h.run("upgrade-cleanup", "--run-id", "42"))
        self.assertEqual(c["removed"], True)

    def test_a_run_whose_pid_is_not_recorded_yet_is_running_not_died(self):
        # between the creation of the log and the pid file
        self.h.save()
        os.makedirs(self.h.root + "/var/log/celeris-os-upgrade/42", exist_ok=True)
        open(self.h.root + "/var/log/celeris-os-upgrade/42/apt-upgrade.log", "w").close()
        self.assertEqual(self.j(self.h.run("upgrade-status", "--run-id", "42"))["state"], "running")

    def test_cleanup_waits_for_a_dpkg_that_outlives_its_wrapper(self):
        # the wrapper died (the OOM killer) and apt-get, reparented to init, still runs
        self.h.w("/usr/sbin/policy-rc.d", osu.POLICY, 0o755)
        self.h.w("/proc/4343/comm", "dpkg\n")
        self.h.save()
        c = self.j(self.h.run("upgrade-cleanup", "--run-id", "42"))
        self.assertEqual(c["removed"], False)
        self.assertIn("dpkg or apt is still running", c["reason"])
        os.remove(self.h.root + "/proc/4343/comm")
        self.assertEqual(self.j(self.h.run("upgrade-cleanup", "--run-id", "42"))["removed"], True)

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

    def test_a_module_without_pci_aliases_is_reported(self):
        self.after_upgrade()
        self.h.state["dkms_build_ok"] = True
        d = self.check()
        self.assertTrue(d["r8127_aliases"])
        self.h.load()  # the build of the first check registered the module
        self.h.state["modinfo"]["7.0.0-40-generic/r8127"]["no_alias"] = True
        self.assertFalse(self.check(build=False)["r8127_aliases"])

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
        return self.h.run("arm-oneshot", "--running", "7.0.0-31-generic", "--target", "7.0.0-40-generic", "--run-id", "42")

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

    def test_an_arming_that_reports_success_and_writes_nothing_is_not_trusted(self):
        self.after_upgrade()
        self.h.state["reboot_noop"] = True
        self.h.state["next_grub_cfg"] = self.h.root + "/boot/grub/grub.cfg"
        r = self.arm()
        self.assertEqual(r.returncode, 1)
        self.assertIn("grub-editenv:", r.stdout + r.stderr)

    def test_an_arming_that_does_not_verify_is_disarmed(self):
        self.after_upgrade()
        self.h.state["reboot_sets_wrong"] = True
        self.h.state["next_grub_cfg"] = self.h.root + "/boot/grub/grub.cfg"
        r = self.arm()
        self.assertEqual(r.returncode, 1)
        self.assertIn("one-shot disarmed: True", r.stdout + r.stderr)
        self.assertEqual(self.h.load()["grubenv"]["next_entry"], "")

    def test_arming_into_a_running_package_run_is_refused(self):
        self.after_upgrade()
        self.h.w("/proc/4242/comm", "unattended-upgr\n")
        self.h.state["next_grub_cfg"] = self.h.root + "/boot/grub/grub.cfg"
        r = self.arm()
        self.assertEqual(r.returncode, 1)
        self.assertIn("dpkg or apt is busy", r.stdout + r.stderr)
        self.assertFalse(any(c.startswith(("grub-reboot", "update-grub")) for c in self.h.calls()))

    def test_disarm_clears_a_pending_one_shot_and_nothing_else(self):
        self.after_upgrade()
        self.h.state["grubenv"]["next_entry"] = ID_40
        self.h.save()
        d = self.j(self.h.run("disarm"))
        self.assertEqual((d["ok"], d["was_armed"], d["next_entry_before"], d["next_entry_after"], d["saved_entry"]), (True, True, ID_40, "", ID_31))
        d = self.j(self.h.run("disarm"))
        self.assertEqual((d["ok"], d["was_armed"]), (True, False))
        self.assertEqual(self.h.load()["grubenv"]["saved_entry"], ID_31)

    def test_make_permanent_refuses_while_the_one_shot_is_still_armed(self):
        self.after_upgrade()
        self.h.state["next_grub_cfg"] = self.h.root + "/boot/grub/grub.cfg"
        self.j(self.arm())
        self.h.load()
        self.h.w("/proc/sys/kernel/osrelease", "7.0.0-40-generic\n")  # the host booted the new kernel and GRUB left the one-shot pending
        r = self.h.run("make-permanent", "--running", "7.0.0-31-generic", "--target", "7.0.0-40-generic", "--run-id", "42")
        self.assertEqual(r.returncode, 1)
        self.assertIn("a one-shot is still pending", r.stdout + r.stderr)
        self.assertEqual(self.h.load()["grubenv"]["saved_entry"], ID_31, "make-permanent changed the saved default before refusing")
        self.assertFalse(os.path.exists(self.h.root + "/var/lib/celeris-os-upgrade/last-upgrade.json"))

    def test_make_permanent_sets_the_new_default_and_writes_the_marker(self):
        self.after_upgrade()
        self.h.state["grubenv"]["next_entry"] = ""  # the one-shot was consumed by the boot
        self.h.w("/proc/sys/kernel/osrelease", "7.0.0-40-generic\n")  # ... and the host runs the new kernel
        self.h.save()
        r = self.j(self.h.run("make-permanent", "--running", "7.0.0-31-generic", "--target", "7.0.0-40-generic", "--run-id", "42"))
        self.assertTrue(r["permanent"])
        self.assertEqual(self.h.load()["grubenv"]["saved_entry"], ID_40)
        m = self.j(self.h.run("marker"))
        self.assertEqual((m["kernel_before"], m["kernel_after"], m["run_id"], m["permanent"]), ("7.0.0-31-generic", "7.0.0-40-generic", "42", True))
        self.assertEqual(self.run_ok("survey")["marker"]["kernel_after"], "7.0.0-40-generic")


class MakePermanentGuards(DkmsCheck):
    def test_an_unbooted_kernel_is_never_made_the_default(self):
        self.after_upgrade()
        self.h.save()
        r = self.h.run("make-permanent", "--running", "7.0.0-31-generic", "--target", "7.0.0-40-generic", "--run-id", "42")
        self.assertEqual(r.returncode, 1)
        self.assertIn("refusing to make an unbooted kernel the default", r.stdout + r.stderr)
        self.assertEqual(self.h.load()["grubenv"]["saved_entry"], ID_31)


class AccessAndInitrd(DkmsCheck):
    def test_the_way_in_is_checked_on_the_upgraded_userland(self):
        self.after_upgrade()
        self.assertEqual(self.check()["access_problems"], [])
        self.h.state["sshd_rc"] = 255
        self.h.state["tailscaled_rc"] = 1
        self.h.state["netplan_rc"] = 1
        self.h.state["services"]["is-enabled ssh"] = "disabled"
        probs = " | ".join(self.check()["access_problems"])
        for want in ("sshd -t fails", "neither ssh.service nor ssh.socket is enabled", "tailscaled does not run", "netplan generate fails"):
            self.assertIn(want, probs)

    def test_an_older_target_is_flagged(self):
        self.after_upgrade()
        self.h.save()
        d = self.j(self.h.run("dkms-check", "--running", "7.0.0-38-generic", "--target", "7.0.0-31-generic"))
        self.assertTrue(d["target_older_than_running"])
        self.assertFalse(self.check()["target_older_than_running"])

    def test_a_blacklist_is_only_good_when_the_initramfs_was_built_after_it(self):
        self.after_upgrade()
        d = self.check()
        self.assertFalse(d["r8169_blacklisted"])
        self.assertFalse(d["r8169_blacklist_in_initrd"])
        self.h.w("/etc/modprobe.d/celeris-r8127.conf", "blacklist r8169\n")
        d = self.check()
        self.assertTrue(d["r8169_blacklisted"])
        os.utime(self.h.root + "/boot/initrd.img-7.0.0-40-generic", (1, 1))  # an initramfs older than the blacklist
        self.assertFalse(self.check()["r8169_blacklist_in_initrd"])
        os.utime(self.h.root + "/boot/initrd.img-7.0.0-40-generic", None)
        self.assertTrue(self.check()["r8169_blacklist_in_initrd"])

    def test_running_signer_is_reported(self):
        self.after_upgrade()
        self.h.state["modinfo"]["7.0.0-31-generic/r8127"]["signer"] = "Albert MOK"
        self.assertEqual(self.check()["running_r8127_signer"], "Albert MOK")

    def test_apt_sources_never_publish_a_credential(self):
        self.h.w("/etc/apt/sources.list.d/private.list", "deb https://user:secret@repo.example.com/apt stable main\n")
        s = self.run_ok("survey")
        uris = [r["uri"] for r in s["packages"]["repos"]]
        self.assertIn("https://repo.example.com/apt", uris)
        self.assertNotIn("secret", json.dumps(s))


class Repair(Base):
    def broken(self):
        self.h.state["upgrade_kernels"] = ["7.0.0-40-generic"]
        for pk, st in (("linux-image-7.0.0-40-generic", "installed"), ("linux-headers-7.0.0-40-generic", "half-configured"), ("linux-modules-7.0.0-40-generic", "installed")):
            self.h.state["packages"][pk] = {"version": "7.0.0-40", "status": st}
        self.h.state["packages"]["linux-image-7.0.0-25-generic"] = {"version": "7.0.0-25", "status": "installed"}  # OLDER than the running kernel
        self.h.state["audit"] = ["linux-headers-7.0.0-40-generic  half-configured"]
        self.h.state["dkms_status"] = ["r8127/11.015.00, 7.0.0-31-generic, aarch64: installed"]

    def wait(self):
        end = time.time() + 20
        while time.time() < end:
            st = self.j(self.h.run("upgrade-status", "--run-id", "9"))
            if st["state"] == "done":
                return st
            time.sleep(0.1)
        self.fail("the repair did not finish")

    def test_survey_lists_the_kernel_packages_newer_than_the_running_kernel(self):
        self.broken()
        s = self.run_ok("survey")
        self.assertNotIn("linux-image-7.0.0-25-generic", s["packages"]["new_kernel_pkgs"])  # older than the running kernel: never a candidate
        self.assertEqual(s["packages"]["new_kernel_pkgs"], ["linux-headers-7.0.0-40-generic", "linux-image-7.0.0-38-generic", "linux-image-7.0.0-40-generic", "linux-modules-7.0.0-40-generic"])
        # only the version a failed upgrade left half-installed is ours to abort; the -38 that was installed and working is not
        self.assertEqual(s["packages"]["broken_kernel_pkgs"], ["linux-headers-7.0.0-40-generic", "linux-image-7.0.0-40-generic", "linux-modules-7.0.0-40-generic"])
        self.assertEqual(len(s["packages"]["dpkg_audit"]), 1)
        self.assertFalse(s["packages"]["policy_rc_d_ours"])

    def test_configure_runs_dpkg_configure_detached_and_removes_the_policy(self):
        self.broken()
        self.h.state["configure_ok"] = True
        self.h.save()
        self.j(self.h.run("repair-start", "--run-id", "9", "--action", "configure"))
        self.assertEqual(self.wait()["rc"], 0)
        self.assertFalse(os.path.exists(self.h.root + "/usr/sbin/policy-rc.d"))
        self.assertEqual(self.h.load()["audit"], [])
        self.assertIn("dpkg --force-confold --configure -a", self.h.calls())
        self.assertFalse(any(c.startswith("apt-get") and "remove" in c for c in self.h.calls()))

    def test_a_configure_that_keeps_failing_is_reported(self):
        self.broken()
        self.h.save()
        self.j(self.h.run("repair-start", "--run-id", "9", "--action", "configure"))
        self.assertEqual(self.wait()["rc"], 1)
        self.assertEqual(len(self.h.load()["audit"]), 1)

    def test_abort_purges_only_the_kernels_newer_than_the_running_one(self):
        self.broken()
        self.h.state["configure_ok"] = True
        self.h.save()
        self.j(self.h.run("repair-start", "--run-id", "9", "--action", "abort-new-kernel"))
        self.assertEqual(self.wait()["rc"], 0)
        pk = self.h.load()["packages"]
        self.assertNotIn("linux-image-7.0.0-40-generic", pk)
        self.assertNotIn("linux-headers-7.0.0-40-generic", pk)
        self.assertIn("linux-image-7.0.0-31-generic", pk)  # the running kernel stays
        self.assertIn("linux-image-7.0.0-38-generic", pk)  # an older one stays
        self.assertIn("linux-image-7.0.0-25-generic", pk)  # the older one stays
        removes = [c for c in self.h.calls() if c.startswith("apt-get") and "remove" in c and " -s " not in c]
        self.assertEqual(len(removes), 1)
        self.assertNotIn("7.0.0-31", removes[0])
        self.assertNotIn("7.0.0-38", removes[0])


class RepairCascade(Repair):
    def test_the_abort_simulates_first_and_the_kernel_metapackages_may_follow(self):
        self.broken()
        self.h.state["remove_cascade"] = ["linux-generic", "linux-image-generic"]
        self.h.save()
        self.j(self.h.run("repair-start", "--run-id", "9", "--action", "abort-new-kernel"))
        self.assertEqual(self.wait()["rc"], 0)
        calls = self.h.calls()
        sim = [i for i, c in enumerate(calls) if c.startswith("apt-get -s") and "remove" in c]
        real = [i for i, c in enumerate(calls) if c.startswith("apt-get -y") and "remove" in c]
        self.assertTrue(sim and real and sim[0] < real[0], calls)
        log = self.h.run("upgrade-log", "--run-id", "9").stdout
        self.assertIn("KERNEL METAPACKAGES REMOVED WITH IT: linux-generic, linux-image-generic", log)

    def test_an_abort_that_would_remove_anything_else_removes_nothing(self):
        self.broken()
        self.h.state["remove_cascade"] = ["tailscale"]
        self.h.save()
        self.j(self.h.run("repair-start", "--run-id", "9", "--action", "abort-new-kernel"))
        self.assertEqual(self.wait()["rc"], 3)
        self.assertIn("tailscale", self.h.run("upgrade-log", "--run-id", "9").stdout)
        self.assertFalse([c for c in self.h.calls() if c.startswith("apt-get -y") and "remove" in c])
        self.assertIn("linux-image-7.0.0-40-generic", self.h.load()["packages"])
        self.assertFalse(os.path.exists(self.h.root + "/usr/sbin/policy-rc.d"))


class ArmedRecords(DkmsCheck):
    def test_arming_leaves_a_record_and_a_verified_boot_removes_it(self):
        self.after_upgrade()
        self.h.state["next_grub_cfg"] = self.h.root + "/boot/grub/grub.cfg"
        self.j(self.arm_for_record())
        rec = json.loads(open(self.h.root + "/var/lib/celeris-os-upgrade/armed-7.0.0-40-generic.json").read())
        self.assertEqual((rec["kernel"], rec["running"], rec["run_id"]), ("7.0.0-40-generic", "7.0.0-31-generic", "42"))
        self.assertEqual([r["kernel"] for r in self.run_ok("survey")["armed_kernels"]], ["7.0.0-40-generic"])
        d = self.check()
        self.assertEqual([r["kernel"] for r in d["target_armed_before"]], ["7.0.0-40-generic"])
        # the boot consumed the one-shot and the host runs the kernel: verified, then made permanent
        st = self.h.load()
        st["grubenv"]["next_entry"] = ""
        self.h.save()
        self.h.w("/proc/sys/kernel/osrelease", "7.0.0-40-generic\n")
        self.j(self.h.run("make-permanent", "--running", "7.0.0-31-generic", "--target", "7.0.0-40-generic", "--run-id", "42"))
        self.assertFalse(os.path.exists(self.h.root + "/var/lib/celeris-os-upgrade/armed-7.0.0-40-generic.json"))

    def arm_for_record(self):
        self.h.save()
        return self.h.run("arm-oneshot", "--running", "7.0.0-31-generic", "--target", "7.0.0-40-generic", "--run-id", "42")

    def test_an_armed_kernel_that_installs_cleanly_is_still_a_candidate_for_the_abort(self):
        # the dead end after a NIC-less boot: the kernel is installed cleanly, the host runs the old one, nothing is half-installed
        self.after_upgrade()
        for pk in ("linux-image-7.0.0-40-generic", "linux-modules-7.0.0-40-generic"):
            self.h.state["packages"][pk] = {"version": "7.0.0-40", "status": "installed"}
        self.h.state["next_grub_cfg"] = self.h.root + "/boot/grub/grub.cfg"
        self.assertEqual(self.run_ok("survey")["packages"]["broken_kernel_pkgs"], [])
        self.j(self.arm_for_record())
        self.h.load()
        self.h.state["grubenv"]["next_entry"] = ""  # consumed by the NIC-less boot, then a power-cycle back to the old kernel
        self.h.state["configure_ok"] = True
        self.h.save()
        s = self.run_ok("survey")
        self.assertEqual(s["packages"]["broken_kernel_pkgs"], ["linux-image-7.0.0-40-generic", "linux-modules-7.0.0-40-generic"])
        self.j(self.h.run("repair-start", "--run-id", "9", "--action", "abort-new-kernel"))
        end = time.time() + 20
        while time.time() < end and self.j(self.h.run("upgrade-status", "--run-id", "9"))["state"] != "done":
            time.sleep(0.1)
        self.assertNotIn("linux-image-7.0.0-40-generic", self.h.load()["packages"])
        self.assertFalse(os.path.exists(self.h.root + "/var/lib/celeris-os-upgrade/armed-7.0.0-40-generic.json"), "the abort left the suspicion behind")


class Rehearsal(Base):
    def pinned(self):
        self.h.save()
        self.j(self.h.run("pin-grub", "--running", "7.0.0-31-generic", "--run-id", "5"))
        self.h.load()

    def test_it_arms_the_running_kernel_and_records_nothing_until_the_boot_proved_it(self):
        self.pinned()
        r = self.j(self.h.run("arm-rehearsal", "--running", "7.0.0-31-generic", "--target", "7.0.0-31-generic"))
        # -38 is newer, so the top-level entry boots -38: the running kernel's own Advanced entry is the safe one
        self.assertEqual((r["via"], r["next_entry"], r["saved_entry"]), ("advanced entry", ID_31, ID_31))
        self.assertEqual(self.h.load()["grubenv"]["next_entry"], ID_31)
        # GRUB did not consume it: no record
        bad = self.h.run("record-rehearsal", "--kernel", "7.0.0-31-generic", "--run-id", "5")
        self.assertEqual(bad.returncode, 1)
        self.assertEqual(self.run_ok("survey")["rehearsal"], {})
        # GRUB consumed it and the same kernel runs: the record
        self.h.state["grubenv"]["next_entry"] = ""
        self.h.save()
        rec = self.j(self.h.run("record-rehearsal", "--kernel", "7.0.0-31-generic", "--run-id", "5"))
        self.assertEqual(self.run_ok("survey")["rehearsal"]["kernel"], "7.0.0-31-generic")
        self.assertTrue(rec["ok"])

    def test_it_uses_the_top_level_entry_when_the_running_kernel_is_the_newest(self):
        os.remove(self.h.root + "/boot/vmlinuz-7.0.0-38-generic")
        self.h.state["packages"].pop("linux-image-7.0.0-38-generic")
        self.h.state["grub_template"] = os.path.join(FIXTURES, "grub-arm64-efi.cfg")
        self.h.state["next_grub_cfg"] = None
        self.pinned()
        r = self.j(self.h.run("arm-rehearsal", "--running", "7.0.0-31-generic", "--target", "7.0.0-31-generic"))
        self.assertEqual(r["via"], "top-level entry")
        self.assertTrue(r["next_entry"].startswith("gnulinux-simple-"))
        self.assertNotEqual(r["next_entry"], r["saved_entry"])

    def test_it_refuses_when_the_running_kernel_is_not_the_saved_default(self):
        self.h.save()
        r = self.h.run("arm-rehearsal", "--running", "7.0.0-31-generic", "--target", "7.0.0-31-generic")
        self.assertEqual(r.returncode, 1)
        self.assertIn("pin it first", r.stdout + r.stderr)
        self.assertFalse(any(c.startswith("grub-reboot") for c in self.h.calls()))

    def test_it_refuses_into_a_running_package_run_and_disarms_what_it_cannot_verify(self):
        self.pinned()
        self.h.w("/proc/4242/comm", "dpkg\n")
        self.assertEqual(self.h.run("arm-rehearsal", "--running", "7.0.0-31-generic", "--target", "7.0.0-31-generic").returncode, 1)
        os.remove(self.h.root + "/proc/4242/comm")
        self.h.state["reboot_sets_wrong"] = True
        self.h.save()
        r = self.h.run("arm-rehearsal", "--running", "7.0.0-31-generic", "--target", "7.0.0-31-generic")
        self.assertEqual(r.returncode, 1)
        self.assertIn("one-shot disarmed: True", r.stdout + r.stderr)
        self.assertEqual(self.h.load()["grubenv"]["next_entry"], "")

    def test_a_record_for_another_kernel_than_the_running_one_is_refused(self):
        self.h.save()
        self.assertEqual(self.h.run("record-rehearsal", "--kernel", "7.0.0-38-generic", "--run-id", "5").returncode, 1)


class HoldAccessPackages(Base):
    def wait(self, run_id="42"):
        end = time.time() + 20
        while time.time() < end:
            st = self.j(self.h.run("upgrade-status", "--run-id", run_id))
            if st["state"] == "done":
                return st
            time.sleep(0.1)
        self.fail("the upgrade did not finish")

    def test_the_access_path_is_held_for_the_length_of_the_run_and_released_after(self):
        self.h.state["held"] = ["vim", "shim-signed"]  # holds somebody else put, one of them on an access-path package
        self.h.save()
        out = self.j(self.h.run("upgrade-start", "--run-id", "42", "--hold"))
        self.assertEqual(out["access_packages_held"], ["grub-common", "grub-efi-arm64-bin", "openssh-server", "tailscale"])
        self.assertEqual(self.wait()["rc"], 0)
        log = self.h.run("upgrade-log", "--run-id", "42").stdout
        self.assertIn("apt-mark hold grub-common grub-efi-arm64-bin openssh-server tailscale", log)
        self.assertIn("held: grub-common grub-efi-arm64-bin openssh-server shim-signed tailscale vim", log)  # held while apt ran
        self.assertEqual(self.h.load()["held"], ["shim-signed", "vim"])  # released after; the two that were held before are not ours

    def test_without_the_flag_nothing_is_held(self):
        self.h.save()
        out = self.j(self.h.run("upgrade-start", "--run-id", "42"))
        self.assertEqual(out["access_packages_held"], [])
        self.wait()
        self.assertNotIn("apt-mark hold", self.h.run("upgrade-log", "--run-id", "42").stdout)

    def test_a_run_that_died_with_the_holds_on_is_released_by_the_cleanup(self):
        import signal
        self.h.state["upgrade_sleep"] = 30
        self.h.save()
        started = self.j(self.h.run("upgrade-start", "--run-id", "42", "--hold"))
        time.sleep(0.7)
        self.assertIn("tailscale", self.h.load()["held"])
        os.kill(started["pid"], signal.SIGKILL)
        time.sleep(0.7)
        c = self.j(self.h.run("upgrade-cleanup", "--run-id", "42"))
        self.assertIn("tailscale", c["unheld"])
        self.assertEqual(self.h.load()["held"], [])


class AbortKeepsTheSuspicionUntilTheKernelIsGone(Repair):
    def arm_record(self, kernel, run_id="77"):
        self.h.w("/var/lib/celeris-os-upgrade/armed-%s.json" % kernel, json.dumps({"kernel": kernel, "run_id": run_id, "running": "7.0.0-31-generic"}))

    def record(self, kernel):
        return os.path.exists(self.h.root + "/var/lib/celeris-os-upgrade/armed-%s.json" % kernel)

    def test_a_failed_purge_keeps_the_armed_record_so_the_kernel_is_not_armed_again_blindly(self):
        self.broken()
        self.arm_record("7.0.0-40-generic")
        self.h.state["remove_rc"] = 100
        self.h.state["configure_ok"] = True
        self.h.save()
        self.j(self.h.run("repair-start", "--run-id", "9", "--action", "abort-new-kernel"))
        self.assertNotEqual(self.wait()["rc"], 0)
        self.assertIn("linux-image-7.0.0-40-generic", self.h.load()["packages"])  # still installed
        self.assertTrue(self.record("7.0.0-40-generic"), "a failed purge forgot the kernel that stranded the host")

    def test_a_purge_that_exits_zero_but_leaves_the_version_installed_keeps_the_record(self):
        self.broken()
        self.arm_record("7.0.0-40-generic")
        self.h.state["configure_ok"] = True
        self.h.state["remove_noop"] = True  # apt reports success and dpkg still lists the packages
        self.h.save()
        self.j(self.h.run("repair-start", "--run-id", "9", "--action", "abort-new-kernel"))
        self.wait()
        self.assertIn("linux-image-7.0.0-40-generic", self.h.load()["packages"])
        self.assertTrue(self.record("7.0.0-40-generic"))

    def test_a_successful_purge_forgets_only_the_purged_versions(self):
        self.broken()
        self.arm_record("7.0.0-40-generic")
        self.arm_record("7.0.0-45-generic", "78")  # another kernel's record: not among the purged packages
        self.h.state["configure_ok"] = True
        self.h.save()
        self.j(self.h.run("repair-start", "--run-id", "9", "--action", "abort-new-kernel"))
        self.assertEqual(self.wait()["rc"], 0)
        self.assertFalse(self.record("7.0.0-40-generic"))
        self.assertTrue(self.record("7.0.0-45-generic"), "the abort forgot a kernel it did not remove")

    def test_configure_never_forgets_a_record(self):
        self.broken()
        self.arm_record("7.0.0-40-generic")
        self.h.state["configure_ok"] = True
        self.h.save()
        self.j(self.h.run("repair-start", "--run-id", "9", "--action", "configure"))
        self.assertEqual(self.wait()["rc"], 0)
        self.assertTrue(self.record("7.0.0-40-generic"))

    def test_the_record_of_a_kernel_with_another_flavor_is_matched_by_its_version(self):
        self.assertEqual(osu.kver_prefix("7.0.0-40-generic"), "7.0.0-40")
        self.assertEqual(osu.kver_prefix("7.0.0-40-generic-64k"), "7.0.0-40")
        self.assertEqual(osu.kver_prefix("garbage"), "")


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
