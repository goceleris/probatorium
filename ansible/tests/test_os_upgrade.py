#!/usr/bin/env python3
"""Tests of ansible/files/os_upgrade.py against a fake host (osu_fakehost.py): the grub menu parser on two
generated grub.cfg samples, the survey (and that it writes nothing and starts nothing that changes a host), every
host-changing subcommand in the order the playbook runs them, and the refusals each of them has.

    python3 ansible/tests/test_os_upgrade.py        (python 3 standard library only; no root, no apt, no grub)

workflow_cluster_os_upgrade_test.go runs this file in CI (skipping when python3 is absent).
"""
import contextlib
import json
import os
import shutil
import subprocess
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

    @contextlib.contextmanager
    def in_fake_root(self):
        """osu.* called IN THIS PROCESS reads /proc and the lock files under osu.ROOT, which is the real machine's unless pointed at the
        fake host: the stub processes of the other shards (OSU_SHARD) and a real unattended upgrade of a CI machine would be seen."""
        old, osu.ROOT = osu.ROOT, self.h.root
        try:
            yield
        finally:
            osu.ROOT = old

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
        self.h.proc(4242, "/usr/bin/python3", "/usr/bin/unattended-upgrade")
        self.h.state["audit"] = ["The following packages are only half configured:", " foo  a thing"]
        self.h.state["held"] = ["linux-image-generic", "vim"]
        s = self.run_ok("survey")["packages"]
        self.assertEqual(s["package_processes"], ["unattended-upgrade"])
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


class PackageRunDetection(Base):
    """Is a package run going on? The real hosts' first survey (run 39814880117) said yes on all three, because the idle Ubuntu daemons
    `unattended-upgrade-shutdown --wait-for-signal` and `packagekitd` were judged by their comm, whose 15 characters make
    "unattended-upgr" of both that daemon and a real unattended-upgrade."""

    IDLE_LISTENER = ("/usr/bin/python3", "/usr/share/unattended-upgrades/unattended-upgrade-shutdown", "--wait-for-signal")

    def pk(self):
        return self.run_ok("survey")["packages"]

    def test_the_idle_listener_and_an_idle_packagekit_are_not_a_package_run(self):
        # the fake host runs both from the start (osu_fakehost.build): this is the state of every real host
        self.assertEqual(open(self.h.root + "/proc/900/comm").read().strip(), "unattended-upgr")  # the premise: comm cannot tell them apart
        self.assertEqual(open(self.h.root + "/proc/901/comm").read().strip(), "packagekitd")
        pk = self.pk()
        self.assertEqual(pk["package_processes"], [])
        self.assertEqual(pk["locks_held"], [])
        self.assertFalse(pk["dpkg_lock_held"])
        with self.in_fake_root():
            self.assertEqual(osu.package_run_busy(), "")

    def test_a_comm_that_only_looks_like_a_package_manager_is_not_enough(self):
        # a kernel thread, a zombie (empty cmdline) and a process that merely has the comm of one
        self.h.w("/proc/4000/comm", "dpkg\n")
        self.h.w("/proc/4000/cmdline", "")
        self.h.proc(4001, "/usr/bin/vim", "/var/log/dpkg.log", comm="dpkg")
        self.h.proc(4002, "/usr/bin/less", "/usr/bin/apt-get")
        self.h.proc(4003, "/usr/bin/dpkg-query", "-W")
        self.assertEqual(self.pk()["package_processes"], [])

    def test_a_real_apt_get_upgrade_is_a_package_run(self):
        self.h.proc(4242, "/usr/bin/apt-get", "upgrade")
        self.assertEqual(self.pk()["package_processes"], ["apt-get"])

    def test_a_real_unattended_upgrade_is_a_package_run_though_its_comm_is_the_listeners(self):
        self.h.proc(4243, "/usr/bin/python3", "/usr/bin/unattended-upgrade", "-d")
        self.assertEqual(open(self.h.root + "/proc/4243/comm").read().strip(), "unattended-upgr")
        self.assertEqual(self.pk()["package_processes"], ["unattended-upgrade"])

    def test_every_program_of_the_list_is_found_whether_run_directly_or_as_a_script(self):
        for i, argv in enumerate((("/usr/bin/apt", "install", "x"), ("/usr/bin/dpkg", "--configure", "-a"), ("/usr/bin/aptitude",),
                                  ("/bin/sh", "/usr/lib/apt/apt.systemd.daily", "lock_is_held", "update"), ("/usr/bin/bash", "/usr/sbin/dkms", "autoinstall"),
                                  ("/bin/sh", "/usr/sbin/update-grub"), ("/usr/bin/perl", "/usr/sbin/update-initramfs", "-u"),
                                  ("/usr/bin/python3.13", "-u", "/usr/bin/unattended-upgrade"), ("/bin/sh", "/usr/sbin/grub-mkconfig", "-o", "/boot/grub/grub.cfg"))):
            self.h.proc(5000 + i, *argv)
        self.assertEqual(self.pk()["package_processes"],
                         ["apt", "apt.systemd.daily", "aptitude", "dkms", "dpkg", "grub-mkconfig", "unattended-upgrade", "update-grub", "update-initramfs"])

    def test_the_program_of_an_argv(self):
        for argv, want in (([], ""), (["/usr/bin/apt-get", "-y"], "apt-get"), (["/usr/bin/python3", "/usr/bin/unattended-upgrade"], "unattended-upgrade"),
                           (["/usr/bin/python3", "-u", "/usr/bin/unattended-upgrade"], "unattended-upgrade"),
                           (["/bin/sh", "-c", "apt-get update"], "sh"), (["/usr/bin/python3", "-m", "pip"], "pip"), (["/usr/bin/python3"], "python3"),
                           (["/usr/bin/python3", "/usr/share/unattended-upgrades/unattended-upgrade-shutdown", "--wait-for-signal"], "unattended-upgrade-shutdown")):
            self.assertEqual(osu.process_program(argv), want, argv)

    def test_a_held_lock_is_a_package_run_for_each_of_the_four_locks(self):
        import fcntl
        self.assertEqual(sorted(osu.APT_LOCKS), ["/var/cache/apt/archives/lock", "/var/lib/apt/lists/lock", "/var/lib/dpkg/lock", "/var/lib/dpkg/lock-frontend"])
        for lock in ("/var/lib/dpkg/lock-frontend", "/var/lib/dpkg/lock", "/var/lib/apt/lists/lock", "/var/cache/apt/archives/lock"):
            f = open(self.h.root + lock, "w")
            fcntl.lockf(f, fcntl.LOCK_EX)
            try:
                pk = self.pk()  # a child process is refused the shared lock while this one holds the exclusive one
            finally:
                f.close()
            self.assertEqual(pk["locks_held"], [lock], lock)
            self.assertTrue(pk["dpkg_lock_held"], lock)
            self.assertEqual(pk["package_processes"], [], lock)

    def test_a_lock_file_that_nobody_holds_is_not_a_package_run(self):
        self.assertEqual(self.pk()["locks_held"], [])
        os.remove(self.h.root + "/var/lib/apt/lists/lock")  # an absent lock file is not a held one
        self.assertEqual(self.pk()["locks_held"], [])

    def test_the_busy_words_of_a_refusal(self):
        self.h.save()
        with self.in_fake_root():
            self.assertEqual(osu.package_run_busy(), "")
            self.h.proc(4242, "/usr/bin/apt-get", "upgrade")
            self.assertEqual(osu.package_run_busy(), "apt-get")

    @unittest.skipUnless(os.path.isdir("/proc/self"), "needs a real /proc")
    def test_a_scan_of_the_fake_root_never_sees_the_real_machines_processes(self):
        """The leak the sharded run exposed: a REAL process whose argv[0] is apt-get (exec -a) is invisible to a scan of the fake root and
        visible to a scan of the real one."""
        p = subprocess.Popen(["bash", "-c", "exec -a apt-get sleep 60"])
        try:
            deadline = time.time() + 10
            real = []
            while time.time() < deadline and "apt-get" not in real:
                time.sleep(0.05)
                with self.in_real_root():
                    real = osu.package_processes()
            self.assertIn("apt-get", real, "the real-root scan does not see a real apt-get: the control of this test is broken")
            with self.in_fake_root():
                self.assertEqual(osu.package_processes(), [])
                self.assertEqual(osu.package_run_busy(), "")
            # and a child process (the script as the playbook runs it) is held to OSU_ROOT too
            self.assertEqual(self.run_ok("survey")["packages"]["package_processes"], [])
        finally:
            p.kill()
            p.wait()

    @contextlib.contextmanager
    def in_real_root(self):
        old, osu.ROOT = osu.ROOT, ""
        try:
            yield
        finally:
            osu.ROOT = old

    def run_changing(self, *a):
        self.h.save()
        return self.h.run(*a)

    def pinned(self):
        self.h.save()
        pin = self.h.run("pin-grub", "--running", "7.0.0-31-generic", "--run-id", "5")
        self.assertEqual(pin.returncode, 0, pin.stderr)
        self.h.load()

    def test_every_refusal_to_go_on_into_a_package_run_names_the_real_process(self):
        self.pinned()
        self.h.proc(4242, "/usr/bin/apt-get", "-y", "full-upgrade")
        for args in (("arm-rehearsal", "--running", "7.0.0-31-generic", "--target", "7.0.0-31-generic", "--run-id", "6"),
                     ("arm-oneshot", "--running", "7.0.0-31-generic", "--target", "7.0.0-38-generic", "--run-id", "6")):
            r = self.run_changing(*args)
            self.assertEqual(r.returncode, 1, args)
            self.assertIn("dpkg or apt is busy (apt-get)", r.stdout + r.stderr)
        self.h.w("/usr/sbin/policy-rc.d", osu.POLICY, 0o755)
        c = self.j(self.run_changing("upgrade-cleanup", "--run-id", "42"))
        self.assertEqual(c["removed"], False)
        self.assertIn("dpkg or apt is still running (apt-get)", c["reason"])

    def test_with_only_the_idle_daemons_nothing_is_refused_and_the_cleanup_runs(self):
        self.pinned()
        self.h.w("/usr/sbin/policy-rc.d", osu.POLICY, 0o755)
        self.assertEqual(self.j(self.run_changing("upgrade-cleanup", "--run-id", "42"))["removed"], True)
        r = self.run_changing("arm-rehearsal", "--running", "7.0.0-31-generic", "--target", "7.0.0-31-generic", "--run-id", "6")
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)

    def test_a_held_lock_refuses_the_arming_and_the_cleanup(self):
        import fcntl
        self.pinned()
        self.h.save()
        f = open(self.h.root + "/var/lib/apt/lists/lock", "w")
        fcntl.lockf(f, fcntl.LOCK_EX)
        try:
            r = self.h.run("arm-rehearsal", "--running", "7.0.0-31-generic", "--target", "7.0.0-31-generic", "--run-id", "6")
            self.assertEqual(r.returncode, 1)
            self.assertIn("an apt or dpkg lock is held (/var/lib/apt/lists/lock)", r.stdout + r.stderr)
            self.h.w("/usr/sbin/policy-rc.d", osu.POLICY, 0o755)
            c = self.j(self.h.run("upgrade-cleanup", "--run-id", "42"))
            self.assertEqual(c["removed"], False)
        finally:
            f.close()


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

    def until_policy_rc_d(self):
        """The detached run writes its policy-rc.d first; a loaded machine may take a moment to start it."""
        end = time.time() + 15
        while time.time() < end and not os.path.exists(self.h.root + "/usr/sbin/policy-rc.d"):
            time.sleep(0.1)

    def test_a_died_run_is_reported_and_its_policy_rc_d_cleaned_up(self):
        import signal
        self.h.state["upgrade_sleep"] = 30
        self.h.save()
        started = self.j(self.h.run("upgrade-start", "--run-id", "42"))
        self.until_policy_rc_d()
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
        self.until_policy_rc_d()
        c = self.j(self.h.run("upgrade-cleanup", "--run-id", "42"))
        self.assertEqual(c["removed"], False)
        self.assertIn("still alive", c["reason"])
        self.assertTrue(os.path.exists(self.h.root + "/usr/sbin/policy-rc.d"))
        # when the run is over it is the run itself that removes it
        pid = int(open(self.h.root + "/var/log/celeris-os-upgrade/42/apt-upgrade.pid").read())
        import signal
        os.kill(pid, signal.SIGKILL)
        end = time.time() + 15  # the kill is reaped when the machine gets to it
        c = {}
        while time.time() < end:
            c = self.j(self.h.run("upgrade-cleanup", "--run-id", "42"))
            if c.get("removed"):
                break
            time.sleep(0.2)
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
        self.h.proc(4343, "/usr/bin/dpkg", "--configure", "-a")
        self.h.save()
        c = self.j(self.h.run("upgrade-cleanup", "--run-id", "42"))
        self.assertEqual(c["removed"], False)
        self.assertIn("dpkg or apt is still running", c["reason"])
        os.remove(self.h.root + "/proc/4343/cmdline")
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
        self.h.proc(4242, "/usr/bin/python3", "/usr/bin/unattended-upgrade")
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

    def armed_and_not_rebooted(self):
        self.after_upgrade()
        self.h.state["next_grub_cfg"] = self.h.root + "/boot/grub/grub.cfg"
        self.j(self.arm_for_record())
        self.h.load()
        return self.h.root + "/var/lib/celeris-os-upgrade/armed-7.0.0-40-generic.json"

    def test_the_record_keeps_the_boot_it_was_made_in(self):
        path = self.armed_and_not_rebooted()
        self.assertEqual(json.loads(open(path).read())["boot_id"], "aaaaaaaa-0000-0000-0000-000000000001")

    def test_a_disarm_in_the_same_boot_drops_the_record_of_a_kernel_that_never_booted(self):
        path = self.armed_and_not_rebooted()
        r = self.j(self.h.run("disarm"))
        self.assertEqual((r["was_armed"], r["forgot_armed"]), (True, ["7.0.0-40-generic"]))
        self.assertFalse(os.path.exists(path))
        self.assertEqual(self.run_ok("survey")["armed_kernels"], [])
        self.assertEqual(self.check()["target_armed_before"], [])

    def test_a_disarm_that_failed_keeps_the_record(self):
        path = self.armed_and_not_rebooted()
        self.h.state["unset_rc"] = 1  # the one-shot could not be cleared: it is still armed, and the kernel may yet boot
        self.h.save()
        r = self.h.run("disarm")
        self.assertEqual(r.returncode, 1)
        out = json.loads(r.stdout)
        self.assertEqual((out["ok"], out["was_armed"], out["forgot_armed"]), (False, True, []))
        self.assertTrue(os.path.exists(path), "a record was dropped for a one-shot that is still armed")

    def test_a_disarm_after_a_reboot_keeps_the_record(self):
        path = self.armed_and_not_rebooted()
        self.h.w("/proc/sys/kernel/random/boot_id", "bbbbbbbb-0000-0000-0000-000000000002\n")  # the host rebooted since the arming
        r = self.j(self.h.run("disarm"))
        self.assertEqual(r["forgot_armed"], [])
        self.assertTrue(os.path.exists(path), "a kernel that may have booted lost its suspicion")

    def test_a_record_from_before_the_boot_id_was_kept_is_never_dropped(self):
        self.h.w("/var/lib/celeris-os-upgrade/armed-7.0.0-40-generic.json", json.dumps({"kernel": "7.0.0-40-generic", "run_id": "1"}))
        self.assertEqual(self.j(self.h.run("disarm"))["forgot_armed"], [])
        self.assertTrue(os.path.exists(self.h.root + "/var/lib/celeris-os-upgrade/armed-7.0.0-40-generic.json"))

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

    def record(self):
        self.h.save()
        return self.j(self.h.run("record-rehearsal", "--kernel", "7.0.0-31-generic", "--run-id", "5"))

    def test_the_record_keeps_the_boot_chain_and_the_boot_proof_it_was_taken_with(self):
        rec = self.record()
        self.assertEqual(rec["chain"], "grub-common=2.14|grub-efi-arm64-bin=2.14|shim-signed=1.60")
        self.assertTrue(rec["efi_loader"] and rec["stub_uuid"])
        r = self.run_ok("survey")["rehearsal"]
        self.assertEqual((r["valid"], r["stale"]), (True, ""))

    def test_a_grub_or_shim_upgrade_makes_the_record_stale(self):
        self.record()
        for pkg in ("grub-efi-arm64-bin", "shim-signed", "grub-common"):
            st = self.h.load()
            old = st["packages"][pkg]["version"]
            st["packages"][pkg]["version"] = old + "+1"
            self.h.save()
            r = self.run_ok("survey")["rehearsal"]
            self.assertFalse(r["valid"], pkg)
            self.assertIn("grub or shim changed since the rehearsal", r["stale"])
            self.assertIn(pkg + "=" + old + "+1", r["stale"])
            st["packages"][pkg]["version"] = old
            self.h.save()
            self.assertTrue(self.run_ok("survey")["rehearsal"]["valid"])

    def test_a_new_grub_package_changes_the_chain_too(self):
        self.record()
        st = self.h.load()
        st["packages"]["grub-efi-arm64-signed"] = {"version": "1.200", "status": "installed"}
        self.h.save()
        self.assertFalse(self.run_ok("survey")["rehearsal"]["valid"])

    def test_a_changed_boot_proof_makes_the_record_stale(self):
        rec = self.record()
        path = self.h.root + "/var/lib/celeris-os-upgrade/rehearsal.json"
        with open(path) as fh:
            data = json.load(fh)
        data["efi_loader"] = "\\EFI\\other\\shimaa64.efi"
        with open(path, "w") as fh:
            json.dump(data, fh)
        r = self.run_ok("survey")["rehearsal"]
        self.assertFalse(r["valid"])
        self.assertIn("efi_loader", r["stale"])

    def test_an_old_record_without_a_chain_is_stale(self):
        self.h.w("/var/lib/celeris-os-upgrade/rehearsal.json", json.dumps({"kernel": "7.0.0-31-generic", "run_id": "1", "ok": True}))
        r = self.run_ok("survey")["rehearsal"]
        self.assertFalse(r["valid"])
        self.assertIn("names no boot-chain versions", r["stale"])

    def test_a_record_of_another_kernel_or_a_failed_one_is_stale(self):
        self.h.w("/var/lib/celeris-os-upgrade/rehearsal.json", json.dumps({"kernel": "7.0.0-25-generic", "ok": True, "chain": "x"}))
        self.assertIn("not the running kernel", self.run_ok("survey")["rehearsal"]["stale"])
        self.h.w("/var/lib/celeris-os-upgrade/rehearsal.json", json.dumps({"kernel": "7.0.0-31-generic", "ok": False, "chain": "x"}))
        self.assertFalse(self.run_ok("survey")["rehearsal"]["valid"])

    def test_it_uses_the_kernel_specific_entry_even_when_the_running_kernel_is_the_newest(self):
        os.remove(self.h.root + "/boot/vmlinuz-7.0.0-38-generic")
        self.h.state["packages"].pop("linux-image-7.0.0-38-generic")
        self.h.state["grub_template"] = os.path.join(FIXTURES, "grub-arm64-efi.cfg")
        self.h.state["next_grub_cfg"] = None
        self.pinned()
        r = self.j(self.h.run("arm-rehearsal", "--running", "7.0.0-31-generic", "--target", "7.0.0-31-generic"))
        # R3-M2: gnulinux-simple-<uuid> means "the newest kernel at boot time"; a kernel installed between arming and reboot would boot
        self.assertEqual(r["via"], "advanced entry")
        self.assertNotIn("simple", r["next_entry"])
        self.assertEqual(r["next_entry"], ID_31)
        self.assertEqual(r["next_entry"], r["saved_entry"])
        self.assertEqual(self.run_ok("survey")["boot"]["entry0_kernel"], "7.0.0-31-generic")

    def test_the_survey_names_the_kernel_that_grub_boots_when_it_ignores_the_environment_block(self):
        self.h.save()
        b = self.run_ok("survey")["boot"]
        self.assertEqual((b["entry0_kernel"], b["default_kernel"]), ("7.0.0-38-generic", "7.0.0-38-generic"))
        self.pinned()
        b = self.run_ok("survey")["boot"]  # pinned: the saved default is the running kernel, entry 0 still is -38
        self.assertEqual((b["entry0_kernel"], b["default_kernel"]), ("7.0.0-38-generic", "7.0.0-31-generic"))

    def test_it_refuses_when_the_running_kernel_is_not_the_saved_default(self):
        self.h.save()
        r = self.h.run("arm-rehearsal", "--running", "7.0.0-31-generic", "--target", "7.0.0-31-generic")
        self.assertEqual(r.returncode, 1)
        self.assertIn("pin it first", r.stdout + r.stderr)
        self.assertFalse(any(c.startswith("grub-reboot") for c in self.h.calls()))

    def test_it_refuses_into_a_running_package_run_and_disarms_what_it_cannot_verify(self):
        self.pinned()
        self.h.proc(4242, "/usr/bin/dpkg", "--configure", "-a")
        self.assertEqual(self.h.run("arm-rehearsal", "--running", "7.0.0-31-generic", "--target", "7.0.0-31-generic").returncode, 1)
        os.remove(self.h.root + "/proc/4242/comm")
        os.remove(self.h.root + "/proc/4242/cmdline")
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
        # a run whose process is gone (OOM, power loss, runner lost): holds on, no rc file. The dead process is a real, finished one,
        # not a kill raced against the reaping of a zombie (that made this test depend on the load of the machine)
        gone = subprocess.Popen(["true"])
        gone.wait()
        self.j(self.h.run("hold-access", "--run-id", "42"))
        self.h.w("/var/log/celeris-os-upgrade/42/apt-upgrade.log", "+ apt-get ... full-upgrade\n")
        self.h.w("/var/log/celeris-os-upgrade/42/apt-upgrade.pid", "%d\n" % gone.pid)
        self.assertIn("tailscale", self.h.load()["held"])
        c = self.j(self.h.run("upgrade-cleanup", "--run-id", "42"))
        self.assertIn("tailscale", c["unheld"])
        self.assertEqual(c["still_held"], [])
        self.assertEqual(self.h.load()["held"], [])

    def test_a_failing_apt_still_releases_the_holds(self):
        self.h.state["held"] = ["vim"]
        self.h.state["upgrade_rc"] = 100
        self.h.save()
        self.j(self.h.run("upgrade-start", "--run-id", "42", "--hold"))
        self.assertEqual(self.wait()["rc"], 100)
        self.assertEqual(self.h.load()["held"], ["vim"], "a failed apt left the access-path holds behind")


class HoldBeforeTheSimulation(HoldAccessPackages):
    """R3-m4: the holds are put BEFORE the plan is simulated, and the detached run adopts them. R3-m2: holds a dead run left are seen,
    refused with the exact repair, and released by it (only the ones this tool recorded)."""

    def test_hold_access_holds_records_and_the_simulation_sees_the_holds(self):
        self.h.state["held"] = ["vim", "shim-signed"]
        self.h.save()
        r = self.j(self.h.run("hold-access", "--run-id", "42"))
        self.assertEqual(r["held"], ["grub-common", "grub-efi-arm64-bin", "openssh-server", "tailscale"])
        self.assertEqual(self.h.load()["held"], ["grub-common", "grub-efi-arm64-bin", "openssh-server", "shim-signed", "tailscale", "vim"])
        self.assertEqual(open(self.h.root + "/var/log/celeris-os-upgrade/42/held.txt").read().split(), ["grub-common", "grub-efi-arm64-bin", "openssh-server", "tailscale"])
        s = self.run_ok("survey")
        self.assertEqual(s["packages"]["tool_holds"], [{"run_id": "42", "packages": ["grub-common", "grub-efi-arm64-bin", "openssh-server", "tailscale"]}])
        # the simulation is made with the holds in place: the stub prints what apt would see held
        self.assertEqual(self.run_ok("simulate")["rc"], 0)
        calls = self.h.calls()
        self.assertLess(max(i for i, c in enumerate(calls) if c.startswith("apt-mark hold")), max(i for i, c in enumerate(calls) if c.startswith("apt-get") and " -s" in c))

    def test_the_detached_run_adopts_the_holds_and_releases_them_at_its_end(self):
        self.h.state["held"] = ["vim"]
        self.h.save()
        self.j(self.h.run("hold-access", "--run-id", "42"))
        out = self.j(self.h.run("upgrade-start", "--run-id", "42", "--hold"))
        self.assertEqual(out["access_packages_held"], ["grub-common", "grub-efi-arm64-bin", "openssh-server", "shim-signed", "tailscale"])  # not [] after hold-access
        self.assertEqual(self.wait()["rc"], 0)
        log = self.h.run("upgrade-log", "--run-id", "42").stdout
        self.assertIn("held: grub-common grub-efi-arm64-bin openssh-server shim-signed tailscale vim", log)
        self.assertEqual(self.h.load()["held"], ["vim"])  # only the ones this run put go; vim was somebody's
        self.assertEqual(self.run_ok("survey")["packages"]["tool_holds"], [])

    def test_release_access_releases_what_the_run_held_and_not_the_rest(self):
        self.h.state["held"] = ["vim", "shim-signed"]
        self.h.save()
        self.j(self.h.run("hold-access", "--run-id", "42"))
        r = self.j(self.h.run("release-access", "--run-id", "42"))
        self.assertEqual(r["released"], ["grub-common", "grub-efi-arm64-bin", "openssh-server", "tailscale"])
        self.assertEqual(self.h.load()["held"], ["shim-signed", "vim"])

    def dead_run(self):
        self.h.state["held"] = ["vim", "shim-signed"]
        self.h.save()
        self.j(self.h.run("hold-access", "--run-id", "41"))  # the run died here: nobody released

    def test_repair_release_holds_releases_what_a_dead_run_left_and_records_it(self):
        self.dead_run()
        seen = len(self.h.calls())
        self.j(self.h.run("repair-start", "--run-id", "43", "--action", "release-holds"))
        self.assertEqual(self.wait("43")["rc"], 0)
        self.assertFalse(any(c.startswith(("dpkg ", "apt-get")) for c in self.h.calls()[seen:]), "release-holds touched dpkg or apt")
        self.assertEqual(self.h.load()["held"], ["shim-signed", "vim"])  # somebody else's holds stay
        log = self.h.run("upgrade-log", "--run-id", "43").stdout
        self.assertIn("HOLDS RELEASED: grub-common grub-efi-arm64-bin openssh-server tailscale (put by run 41)", log)
        self.assertEqual(self.run_ok("survey")["packages"]["tool_holds"], [])
        self.assertFalse(os.path.exists(self.h.root + "/usr/sbin/policy-rc.d"))

    def test_repair_release_holds_never_unholds_a_hold_it_did_not_record(self):
        self.h.state["held"] = ["linux-image-generic", "vim"]
        self.h.save()
        self.j(self.h.run("repair-start", "--run-id", "43", "--action", "release-holds"))
        self.assertEqual(self.wait("43")["rc"], 0)
        self.assertEqual(self.h.load()["held"], ["linux-image-generic", "vim"])
        self.assertIn("nothing to release", self.h.run("upgrade-log", "--run-id", "43").stdout)

    def test_a_hold_no_longer_in_place_is_not_reported(self):
        self.dead_run()
        self.h.state["held"] = []  # somebody released them by hand
        self.h.save()
        self.assertEqual(self.run_ok("survey")["packages"]["tool_holds"], [])


ACCESS_PATH = ["grub-common", "grub-efi-arm64-bin", "openssh-server", "shim-signed", "tailscale"]  # the access path of the fake host


class HoldReleaseIsVerified(Base):
    """R4-m1: `apt-mark unhold` fails (and changes nothing) while unattended-upgrades or PackageKit has the dpkg frontend lock. The tool used to
    say "HOLDS RELEASED", delete the record of the holds and leave the holds in place: every later upgrade was refused and release-holds
    answered "nothing to release". Now a hold counts as released only when `apt-mark showhold` no longer lists it, and the record of a
    hold that stays is kept, on all three release paths: release-access (a refusal), the end of the detached upgrade with its
    cleanup, and repair release-holds."""

    def wait(self, run_id):
        end = time.time() + 20
        while time.time() < end:
            st = self.j(self.h.run("upgrade-status", "--run-id", run_id))
            if st["state"] == "done":
                return st
            time.sleep(0.1)
        self.fail("run %s did not finish" % run_id)

    def held_txt(self, run_id):
        try:
            return open(self.h.root + "/var/log/celeris-os-upgrade/%s/held.txt" % run_id).read().split()
        except OSError:
            return None

    def hold(self, run_id="42"):
        self.h.save()
        self.j(self.h.run("hold-access", "--run-id", run_id))
        self.h.load()

    def failing(self, **kw):
        self.h.load()
        self.h.state.update(kw)
        self.h.save()

    def tool_holds(self):
        return self.run_ok("survey")["packages"]["tool_holds"]

    # ---- release-access (the refusal path)
    def test_release_access_with_a_failing_unhold_keeps_the_holds_the_record_and_says_so(self):
        self.hold()
        self.failing(unhold_rc=100)
        r = self.h.run("release-access", "--run-id", "42")
        self.assertEqual(r.returncode, 1)
        d = json.loads(r.stdout)
        self.assertEqual((d["ok"], d["released"], d["still_held"]), (False, [], ACCESS_PATH))
        self.assertIn("could NOT be released", d["error"])
        self.assertIn("repair_action=release-holds", d["error"])
        self.assertEqual(self.h.load()["held"], ACCESS_PATH)  # still held
        self.assertEqual(self.held_txt("42"), ACCESS_PATH)  # the record stays
        self.assertEqual(self.tool_holds(), [{"run_id": "42", "packages": ACCESS_PATH}])

    def test_release_access_where_one_hold_stays_releases_the_rest_and_keeps_that_one(self):
        self.hold()
        self.failing(unhold_keeps=["tailscale"])  # apt-mark exits 0 and tailscale stays held
        r = self.h.run("release-access", "--run-id", "42")
        self.assertEqual(r.returncode, 1)
        d = json.loads(r.stdout)
        self.assertEqual((d["released"], d["still_held"]), (["grub-common", "grub-efi-arm64-bin", "openssh-server", "shim-signed"], ["tailscale"]))
        self.assertEqual(self.held_txt("42"), ["tailscale"])
        self.assertEqual(self.h.load()["held"], ["tailscale"])

    def test_a_showhold_that_cannot_be_read_proves_nothing_released(self):
        self.hold()
        self.failing(showhold_rc=100)
        r = self.h.run("release-access", "--run-id", "42")
        self.assertEqual(r.returncode, 1)
        self.assertEqual(json.loads(r.stdout)["still_held"], ACCESS_PATH)
        self.assertEqual(self.held_txt("42"), ACCESS_PATH)

    def test_release_access_that_works_still_releases_and_removes_the_record(self):
        self.hold()
        r = self.j(self.h.run("release-access", "--run-id", "42"))
        self.assertEqual((r["ok"], r["released"], r["still_held"]), (True, ACCESS_PATH, []))
        self.assertIsNone(self.held_txt("42"))
        self.assertEqual(self.h.load()["held"], [])

    # ---- repair release-holds
    def dead_run(self):
        self.h.state["held"] = ["vim"]
        self.hold("41")  # the run died here: nobody released

    def test_repair_release_holds_with_a_failing_unhold_is_a_failed_repair_that_keeps_the_record(self):
        self.dead_run()
        self.failing(unhold_rc=100)
        self.j(self.h.run("repair-start", "--run-id", "43", "--action", "release-holds"))
        self.assertNotEqual(self.wait("43")["rc"], 0)
        log = self.h.run("upgrade-log", "--run-id", "43").stdout
        self.assertIn("HOLDS NOT RELEASED: %s (put by run 41)" % " ".join(ACCESS_PATH), log)
        self.assertNotIn("HOLDS RELEASED", log)
        self.assertEqual(self.held_txt("41"), ACCESS_PATH)
        self.assertEqual(self.h.load()["held"], sorted(ACCESS_PATH + ["vim"]))
        # the survey still names the holds, so the next upgrade still names the repair, and the repair is NOT "nothing to release"
        self.assertEqual(self.tool_holds(), [{"run_id": "41", "packages": ACCESS_PATH}])
        # and it can simply be dispatched again once apt-mark works
        self.failing(unhold_rc=0)
        self.j(self.h.run("repair-start", "--run-id", "44", "--action", "release-holds"))
        self.assertEqual(self.wait("44")["rc"], 0)
        self.assertIn("HOLDS RELEASED: %s (put by run 41)" % " ".join(ACCESS_PATH), self.h.run("upgrade-log", "--run-id", "44").stdout)
        self.assertEqual(self.h.load()["held"], ["vim"])
        self.assertIsNone(self.held_txt("41"))

    def test_repair_release_holds_where_one_hold_stays_says_both_and_fails(self):
        self.dead_run()
        self.failing(unhold_keeps=["openssh-server"])
        self.j(self.h.run("repair-start", "--run-id", "43", "--action", "release-holds"))
        self.assertNotEqual(self.wait("43")["rc"], 0)
        log = self.h.run("upgrade-log", "--run-id", "43").stdout
        self.assertIn("HOLDS RELEASED: grub-common grub-efi-arm64-bin shim-signed tailscale (put by run 41)", log)
        self.assertIn("HOLDS NOT RELEASED: openssh-server (put by run 41)", log)
        self.assertEqual(self.held_txt("41"), ["openssh-server"])

    # ---- the end of the detached upgrade, and its cleanup
    def test_a_detached_run_whose_unhold_fails_logs_it_keeps_the_record_and_its_cleanup_fails_the_run(self):
        self.h.state["unhold_rc_after_upgrade"] = 100  # apt-mark unhold starts failing while apt runs (unattended-upgrades took the lock)
        self.h.save()
        self.j(self.h.run("upgrade-start", "--run-id", "42", "--hold"))
        self.assertEqual(self.wait("42")["rc"], 0)  # apt itself succeeded
        log = self.h.run("upgrade-log", "--run-id", "42").stdout
        self.assertIn("HOLDS NOT RELEASED: ", log)
        self.assertEqual(self.held_txt("42"), ACCESS_PATH)
        self.assertEqual(self.h.load()["held"], ACCESS_PATH)
        c = self.h.run("upgrade-cleanup", "--run-id", "42")
        self.assertEqual(c.returncode, 1)
        d = json.loads(c.stdout)
        self.assertEqual((d["ok"], d["still_held"]), (False, ACCESS_PATH))
        self.assertIn("nothing is rebooted", d["error"])
        self.assertEqual(self.held_txt("42"), ACCESS_PATH)
        # once apt-mark works the same cleanup releases them
        self.failing(unhold_rc=0)
        c = self.j(self.h.run("upgrade-cleanup", "--run-id", "42"))
        self.assertEqual((c["unheld"], c["still_held"]), (ACCESS_PATH, []))
        self.assertIsNone(self.held_txt("42"))
        self.assertEqual(self.h.load()["held"], [])

    def test_the_cleanup_of_a_dead_run_removes_our_policy_rc_d_even_when_a_hold_stays(self):
        gone = subprocess.Popen(["true"])
        gone.wait()
        self.hold("42")
        self.h.w("/var/log/celeris-os-upgrade/42/apt-upgrade.log", "+ apt-get ... full-upgrade\n")
        self.h.w("/var/log/celeris-os-upgrade/42/apt-upgrade.pid", "%d\n" % gone.pid)
        self.h.w("/usr/sbin/policy-rc.d", osu.POLICY, 0o755)
        self.failing(unhold_rc=100)
        c = self.h.run("upgrade-cleanup", "--run-id", "42")
        self.assertEqual(c.returncode, 1)
        self.assertTrue(json.loads(c.stdout)["removed"])  # no policy-rc.d is left behind to keep services down
        self.assertFalse(os.path.exists(self.h.root + "/usr/sbin/policy-rc.d"))
        self.assertEqual(self.held_txt("42"), ACCESS_PATH)

    def test_a_cleanup_with_nothing_held_still_succeeds(self):
        self.h.save()
        c = self.j(self.h.run("upgrade-cleanup", "--run-id", "42"))
        self.assertEqual((c["still_held"], c["unheld"]), ([], []))

    # ---- R4-m6: a record that names a hold that is gone must not claim a hold somebody puts later
    def test_a_stale_record_is_pruned_by_the_next_tool_run_and_never_claims_a_later_hold(self):
        self.dead_run()
        self.failing(held=["vim"])  # somebody released the tool's holds by hand
        self.j(self.h.run("upgrade-cleanup", "--run-id", "99"))  # any later run of the tool as root prunes the dead run's record
        self.assertIsNone(self.held_txt("41"))
        self.failing(held=["openssh-server", "vim"])  # and later holds openssh-server, deliberately
        self.assertEqual(self.tool_holds(), [])
        rel = self.h.run("repair-start", "--run-id", "43", "--action", "release-holds")
        self.assertEqual(rel.returncode, 0)
        self.wait("43")
        self.assertEqual(self.h.load()["held"], ["openssh-server", "vim"])  # not released: it is not ours

    def test_hold_access_prunes_a_stale_record_before_it_holds(self):
        self.dead_run()
        self.failing(held=["vim"])  # somebody released the dead run's holds by hand
        self.j(self.h.run("hold-access", "--run-id", "42"))
        self.h.load()
        self.assertIsNone(self.held_txt("41"))
        self.assertEqual(self.held_txt("42"), ACCESS_PATH)
        self.assertEqual(self.tool_holds(), [{"run_id": "42", "packages": ACCESS_PATH}])

    def test_pruning_keeps_every_record_when_showhold_cannot_be_read(self):
        self.dead_run()
        self.failing(showhold_rc=100)
        self.h.run("upgrade-cleanup", "--run-id", "99")
        self.assertEqual(self.held_txt("41"), ACCESS_PATH)


FREEZE_UNITS = ["apt-daily.timer", "apt-daily-upgrade.timer", "unattended-upgrades.service"]
NR_DROPIN = "/etc/needrestart/conf.d/zzz-celeris-freeze.conf"
FREEZE_RECORD = "/var/lib/celeris-os-upgrade/freeze.json"


class FreezeThaw(Base):
    """mode=freeze / mode=thaw: the platform stays FIXED during the evidence campaign. These hosts update themselves (unattended-upgrades
    installed the kernel -38 on its own) and a 131 h benchmark must not see a package or a service change under it."""

    def setUp(self):
        super().setUp()
        # a PINNED host (grub's saved default is the running kernel): a freeze is refused otherwise (r2 F7); the Revision-2 order freezes after the upgrades
        self.h.w("/boot/grub/grub.cfg", cfg_text("grub-arm64-efi.cfg"))
        self.h.state["grubenv"]["saved_entry"] = ID_31
        self.h.save()

    def svc(self, unit):
        st = self.h.load()["services"]
        return st.get("is-active " + unit, "inactive"), st.get("is-enabled " + unit, "disabled")

    def set_svc(self, unit, active, enabled):
        self.h.load()
        self.h.state["services"]["is-active " + unit] = active
        self.h.state["services"]["is-enabled " + unit] = enabled
        self.h.save()

    def freeze(self, run="7", **state):
        self.h.load()
        self.h.state.update(state)
        self.h.save()
        return self.h.run("freeze", "--run-id", run)

    def thaw(self, run="8"):
        self.h.load()
        self.h.save()
        return self.h.run("thaw", "--run-id", run)

    def record(self):
        with open(self.h.root + FREEZE_RECORD) as f:
            return json.load(f)

    def sysctl_calls(self, *verbs):
        return [c for c in self.h.calls() if c.split(" ")[0] == "systemctl" and c.split(" ")[1] in verbs]

    def test_freeze_stops_and_disables_the_three_units_records_what_it_changed_and_the_platform(self):
        self.h.save()
        r = self.freeze()
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        for u in FREEZE_UNITS:
            self.assertEqual(self.svc(u), ("inactive", "disabled"), u)
        # nothing else was touched
        self.assertEqual(self.svc("tailscaled"), ("active", "enabled"))
        touched = {c.split(" ")[2] for c in self.sysctl_calls("stop", "disable", "start", "enable")}
        self.assertEqual(touched, set(FREEZE_UNITS))
        rec = self.record()
        self.assertEqual((rec["status"], rec["run_id"], rec["errors"]), ("frozen", "7", []))
        self.assertRegex(rec["utc"], r"^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$")
        for u in FREEZE_UNITS:
            self.assertEqual(rec["units"][u]["before"], {"active": "active", "enabled": "enabled"})
            self.assertEqual(rec["units"][u]["changes"], ["stopped", "disabled"])
            self.assertEqual(rec["after"][u], {"active": "inactive", "enabled": "disabled"})
        self.assertEqual(rec["needrestart"], {"installed": True, "mode_before": "", "dropin_written": True, "adopted": False})
        self.assertEqual(open(self.h.root + NR_DROPIN).read().count("$nrconf{restart} = 'l';"), 1)
        p = rec["platform"]
        self.assertEqual((p["kernel"], p["os"]), ("7.0.0-31-generic", "Ubuntu 26.04 LTS"))
        self.assertRegex(p["dpkg_sha256"], r"^[0-9a-f]{64}$")
        self.assertEqual(p["packages"], len(self.h.load()["packages"]))
        # no package, no kernel, no grub: a freeze changes timers and one needrestart file
        self.assertFalse([c for c in self.h.calls() if c.startswith(("apt-get -y", "update-grub", "dpkg --configure", "apt-mark hold", "apt-mark unhold")) or (c.startswith("grub-") and c != "grub-editenv list")])

    def test_the_platform_identity_names_the_packages_and_changes_with_them(self):
        a = self.freeze("7")
        self.assertEqual(a.returncode, 0)
        sha1 = self.record()["platform"]["dpkg_sha256"]
        self.assertEqual(self.thaw().returncode, 0)
        self.h.load()
        self.h.state["packages"]["linux-firmware"]["version"] = "20260319.git2"  # one package moves one version: the same names, another identity
        self.h.state["held"] = ["tailscale"]
        self.h.save()
        self.assertEqual(self.freeze("9").returncode, 0)
        p = self.record()["platform"]
        self.assertNotEqual(p["dpkg_sha256"], sha1)
        self.assertEqual(p["held"], ["tailscale"])
        # the same packages give the same identity
        self.assertEqual(self.thaw().returncode, 0)
        self.assertEqual(self.freeze("10").returncode, 0)
        self.assertEqual(self.record()["platform"]["dpkg_sha256"], p["dpkg_sha256"])

    def test_the_record_is_written_before_anything_changes_and_a_half_freeze_is_thawed(self):
        r = self.freeze(systemctl_fail=["stop apt-daily-upgrade.timer"])
        self.assertEqual(r.returncode, 1)
        self.assertIn("systemctl stop apt-daily-upgrade.timer", r.stdout + r.stderr)
        rec = self.record()
        self.assertEqual(rec["status"], "freezing-failed")
        self.assertEqual(rec["units"]["apt-daily.timer"]["changes"], ["stopped", "disabled"])
        self.assertEqual(rec["units"]["apt-daily-upgrade.timer"]["changes"], ["disabled"])  # the stop failed, the disable worked
        self.assertEqual(self.svc("apt-daily-upgrade.timer"), ("active", "disabled"))
        # thaw puts back exactly what the record says: the stop never happened, so no start
        self.h.load()
        self.h.state["systemctl_fail"] = []
        self.h.save()
        t = self.thaw()
        self.assertEqual(t.returncode, 0, t.stdout + t.stderr)
        for u in FREEZE_UNITS:
            self.assertEqual(self.svc(u), ("active", "enabled"), u)
        self.assertNotIn("systemctl start apt-daily-upgrade.timer", self.h.calls())

    def test_a_freeze_killed_before_its_first_change_has_already_recorded_the_previous_states(self):
        r = self.freeze(systemctl_kill=["stop apt-daily.timer"])
        self.assertNotEqual(r.returncode, 0)
        rec = self.record()  # written before anything changed
        self.assertEqual(rec["status"], "freezing")
        self.assertEqual([rec["units"][u]["changes"] for u in FREEZE_UNITS], [[], [], []])
        self.assertEqual([rec["units"][u]["before"] for u in FREEZE_UNITS], [{"active": "active", "enabled": "enabled"}] * 3)
        for u in FREEZE_UNITS:
            self.assertEqual(self.svc(u), ("active", "enabled"), u)  # nothing had changed
        self.h.load()
        self.h.state["systemctl_kill"] = []
        self.h.save()
        self.assertEqual(self.thaw().returncode, 0)  # a thaw of a freeze that changed nothing changes nothing
        self.assertFalse(os.path.exists(self.h.root + FREEZE_RECORD))

    def test_a_freeze_that_is_killed_half_way_leaves_a_record_a_thaw_can_use(self):
        # the record is written before the first change and after every change: a killed freeze is not a host nobody knows how to put back
        r = self.freeze(systemctl_kill=["disable apt-daily.timer"])
        self.assertNotEqual(r.returncode, 0)
        rec = self.record()
        self.assertEqual(rec["status"], "freezing")
        self.assertEqual(rec["units"]["apt-daily.timer"]["before"], {"active": "active", "enabled": "enabled"})
        self.assertEqual(rec["units"]["apt-daily.timer"]["changes"], ["stopped"])  # stopped, and the kill came before the disable
        self.assertEqual(self.svc("apt-daily.timer"), ("inactive", "enabled"))
        self.h.load()
        self.h.state["systemctl_kill"] = []
        self.h.save()
        t = self.thaw()
        self.assertEqual(t.returncode, 0, t.stdout + t.stderr)
        for u in FREEZE_UNITS:
            self.assertEqual(self.svc(u), ("active", "enabled"), u)

    def test_a_freeze_killed_after_it_wrote_the_needrestart_file_still_knows_to_remove_it(self):
        r = self.freeze(systemctl_kill_nth={"is-active apt-daily.timer": 2})  # the check after the changes
        self.assertNotEqual(r.returncode, 0)
        rec = self.record()
        self.assertEqual(rec["status"], "freezing")
        self.assertTrue(os.path.exists(self.h.root + NR_DROPIN))
        self.assertTrue(rec["needrestart"]["dropin_written"], "the record forgot the file the freeze had already written")
        self.h.load()
        self.h.state["systemctl_kill_nth"] = {}
        self.h.save()
        self.assertEqual(self.thaw().returncode, 0)
        self.assertFalse(os.path.exists(self.h.root + NR_DROPIN))

    def test_a_host_that_ignores_the_freeze_is_not_reported_frozen(self):
        r = self.freeze(systemctl_noop=["stop apt-daily.timer", "disable apt-daily.timer"])
        self.assertEqual(r.returncode, 1)
        self.assertIn("apt-daily.timer is still active/enabled after the freeze", r.stdout + r.stderr)
        self.assertEqual(self.record()["status"], "freezing-failed")

    def test_a_needrestart_that_stays_automatic_fails_the_freeze(self):
        self.h.w("/etc/needrestart/conf.d/zzzz-local.conf", "$nrconf{restart} = 'a';\n")  # sorts after the drop-in: it wins
        r = self.freeze()
        self.assertEqual(r.returncode, 1)
        self.assertIn("needrestart is still configured with restart mode 'a'", r.stdout + r.stderr)

    def test_freeze_refuses_while_the_shutdown_listener_installs_on_shutdown(self):
        # unattended-upgrade-shutdown is what the freeze stops; with InstallOnShutdown it installs packages at power-off and what stopping it
        # does then is not known: refused, with nothing changed, until a reviewed change turns it off
        for v in ("true", "True"):
            self.h.load()
            self.h.state["apt_config"]["Unattended-Upgrade::InstallOnShutdown"] = v
            self.h.save()
            r = self.freeze()
            self.assertEqual(r.returncode, 1)
            self.assertIn("InstallOnShutdown is '%s'" % v, r.stdout + r.stderr)
            self.assertFalse(os.path.exists(self.h.root + FREEZE_RECORD))
            self.assertEqual(self.sysctl_calls("stop", "disable"), [])
        self.h.load()
        self.h.state["apt_config"]["Unattended-Upgrade::InstallOnShutdown"] = "false"
        self.h.save()
        self.assertEqual(self.freeze().returncode, 0)
        self.assertEqual(self.record()["install_on_shutdown"], "false")
        self.assertEqual(self.run_ok("survey")["unattended_upgrades"]["install_on_shutdown"], "false")

    def test_freeze_refuses_while_a_package_run_is_going_on(self):
        self.h.proc(4242, "/usr/bin/unattended-upgrade")
        r = self.freeze()
        self.assertEqual(r.returncode, 1)
        self.assertIn("dpkg or apt is busy (unattended-upgrade)", r.stdout + r.stderr)
        self.assertFalse(os.path.exists(self.h.root + FREEZE_RECORD))
        self.assertEqual(self.sysctl_calls("stop", "disable"), [])

    def test_the_idle_daemons_do_not_stop_a_freeze(self):
        self.assertEqual(self.freeze().returncode, 0)  # the fake host runs unattended-upgrade-shutdown and packagekitd all along

    def test_a_second_freeze_never_overwrites_the_previous_states(self):
        self.assertEqual(self.freeze("7").returncode, 0)
        first = self.record()
        r = self.freeze("9")
        self.assertEqual(r.returncode, 1)
        self.assertIn("already frozen (run 7", r.stdout + r.stderr)
        self.assertEqual(self.record(), first)  # the "before" states are still the real ones, not the frozen ones

    def test_thaw_restores_only_what_the_freeze_changed(self):
        self.set_svc("apt-daily.timer", "active", "enabled")
        self.set_svc("apt-daily-upgrade.timer", "inactive", "disabled")  # a person had already switched it off
        self.set_svc("unattended-upgrades.service", "inactive", "enabled")  # enabled but not running
        self.assertEqual(self.freeze().returncode, 0)
        rec = self.record()
        self.assertEqual(rec["units"]["apt-daily.timer"]["changes"], ["stopped", "disabled"])
        self.assertEqual(rec["units"]["apt-daily-upgrade.timer"]["changes"], [])
        self.assertEqual(rec["units"]["unattended-upgrades.service"]["changes"], ["disabled"])
        seen = len(self.h.calls())
        t = self.thaw()
        self.assertEqual(t.returncode, 0, t.stdout + t.stderr)
        after = self.h.calls()[seen:]
        self.assertEqual(self.svc("apt-daily.timer"), ("active", "enabled"))
        self.assertEqual(self.svc("apt-daily-upgrade.timer"), ("inactive", "disabled"))  # not enabled behind the person's back
        self.assertEqual(self.svc("unattended-upgrades.service"), ("inactive", "enabled"))  # enabled again, not started
        self.assertFalse([c for c in after if "apt-daily-upgrade.timer" in c and c.startswith("systemctl") and c.split(" ")[1] in ("enable", "start")])
        self.assertNotIn("systemctl start unattended-upgrades.service", after)
        self.assertFalse(os.path.exists(self.h.root + NR_DROPIN))
        self.assertFalse(os.path.exists(self.h.root + FREEZE_RECORD))
        old = json.load(open(self.h.root + "/var/lib/celeris-os-upgrade/freeze-7-thawed.json"))  # the record is archived, not lost
        self.assertEqual((old["status"], old["thawed_by_run"]), ("thawed", "8"))
        self.assertIn("enable apt-daily.timer", " ".join(old["restored"]))

    def test_thaw_without_a_freeze_refuses_and_changes_nothing(self):
        r = self.thaw()
        self.assertEqual(r.returncode, 1)
        self.assertIn("nothing to thaw", r.stdout + r.stderr)
        self.assertEqual(self.sysctl_calls("stop", "disable", "start", "enable"), [])

    def test_a_thaw_that_did_not_take_keeps_the_record(self):
        self.assertEqual(self.freeze().returncode, 0)
        self.h.load()
        self.h.state["systemctl_noop"] = ["enable apt-daily.timer"]
        self.h.save()
        r = self.thaw()
        self.assertEqual(r.returncode, 1)
        self.assertIn("apt-daily.timer is active/disabled, was active/enabled before the freeze", r.stdout + r.stderr)
        self.assertTrue(os.path.exists(self.h.root + FREEZE_RECORD))
        self.h.load()
        self.h.state["systemctl_noop"] = []
        self.h.save()
        self.assertEqual(self.thaw().returncode, 0)  # and thaw can simply be dispatched again

    def test_needrestart_already_list_only_is_left_alone(self):
        self.h.w("/etc/needrestart/conf.d/10-local.conf", "$nrconf{restart} = 'l';\n")
        self.assertEqual(self.freeze().returncode, 0)
        self.assertEqual(self.record()["needrestart"], {"installed": True, "mode_before": "l", "dropin_written": False, "adopted": False})
        self.assertFalse(os.path.exists(self.h.root + NR_DROPIN))
        self.assertEqual(self.thaw().returncode, 0)
        self.assertTrue(os.path.exists(self.h.root + "/etc/needrestart/conf.d/10-local.conf"))

    def test_needrestart_set_to_automatic_is_put_back_by_the_thaw(self):
        self.h.w("/etc/needrestart/needrestart.conf", "$nrconf{restart} = 'a';\n")
        self.assertEqual(self.freeze().returncode, 0)
        self.assertEqual(self.record()["needrestart"], {"installed": True, "mode_before": "a", "dropin_written": True, "adopted": False})
        self.assertEqual(self.thaw().returncode, 0)
        self.assertFalse(os.path.exists(self.h.root + NR_DROPIN))
        self.assertIn("$nrconf{restart} = 'a';", open(self.h.root + "/etc/needrestart/needrestart.conf").read())  # the person's file was never touched

    def test_thaw_does_not_remove_a_needrestart_file_it_did_not_write(self):
        self.assertEqual(self.freeze().returncode, 0)
        self.h.w(NR_DROPIN, "$nrconf{restart} = 'a'; # a person's own\n")
        r = self.thaw()
        self.assertEqual(r.returncode, 1)
        self.assertIn("is no longer the file the freeze wrote: left alone", r.stdout + r.stderr)
        self.assertIn("a person's own", open(self.h.root + NR_DROPIN).read())

    def test_without_needrestart_nothing_is_written_for_it(self):
        self.h.load()
        del self.h.state["packages"]["needrestart"]
        self.h.save()
        self.assertEqual(self.freeze().returncode, 0)
        self.assertEqual(self.record()["needrestart"], {"installed": False, "mode_before": "", "dropin_written": False, "adopted": False})
        self.assertFalse(os.path.exists(self.h.root + NR_DROPIN))

    def test_a_unit_the_host_does_not_have_is_not_touched(self):
        self.set_svc("apt-daily-upgrade.timer", "inactive", "not-found")
        r = self.freeze()
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertFalse(self.record()["units"]["apt-daily-upgrade.timer"]["present"])
        self.assertFalse([c for c in self.sysctl_calls("stop", "disable") if c.endswith("apt-daily-upgrade.timer")])

    def test_the_survey_says_whether_the_platform_is_frozen_and_whether_it_still_holds(self):
        self.h.save()
        self.assertEqual(self.run_ok("survey")["freeze"], {})
        self.assertEqual(self.freeze().returncode, 0)
        self.h.load()  # run_ok saves the in-memory state: it must be the one the freeze left
        s = self.run_ok("survey")["freeze"]
        self.assertEqual((s["status"], s["run_id"], s["drifted"]), ("frozen", "7", []))
        self.assertEqual(s["platform"]["kernel"], "7.0.0-31-generic")
        # a package upgrade (or a person) switches a timer back on, and the needrestart file goes: the survey names both
        self.set_svc("apt-daily.timer", "active", "enabled")
        os.remove(self.h.root + NR_DROPIN)
        d = self.run_ok("survey")["freeze"]["drifted"]
        self.assertEqual(d, ["apt-daily.timer=active/enabled", "needrestart restart mode is '', not 'l'"])

    def frozen_survey(self):
        self.assertEqual(self.freeze().returncode, 0)
        self.h.load()
        self.assertEqual(self.run_ok("survey")["freeze"]["drifted"], [])

    def test_the_survey_names_what_a_freeze_does_not_stop(self):
        self.h.save()
        u = self.run_ok("survey")["self_updaters"]
        self.assertEqual((u["tailscale_auto_update"], u["tailscale_check_updates"]), ("false", "true"))
        self.assertIn("timer: 00:00~24:00/4", u["snap_refresh"])
        self.h.state["tailscale_autoupdate"] = {"Check": True, "Apply": True}
        self.h.state["snap_refresh_rc"] = 1
        self.h.save()
        u = self.run_ok("survey")["self_updaters"]
        self.assertEqual((u["tailscale_auto_update"], u["snap_refresh"]), ("true", "unknown"))
        self.h.state["tailscale_prefs_rc"] = 1
        self.h.save()
        self.assertEqual(self.run_ok("survey")["self_updaters"]["tailscale_auto_update"], "unknown")

    def test_these_reads_are_on_the_read_only_list_and_no_other_tailscale_or_snap_command_is(self):
        for argv, ok in ((["tailscale", "debug", "prefs"], True), (["snap", "refresh", "--time"], True), (["tailscale", "set", "--auto-update=false"], False),
                         (["tailscale", "up"], False), (["snap", "refresh"], False), (["snap", "refresh", "--hold"], False), (["snap", "remove", "x"], False)):
            self.assertEqual(osu.ro_allowed(argv), ok, argv)

    def test_a_package_that_moves_under_a_freeze_is_drift(self):
        self.frozen_survey()
        self.h.state["packages"]["linux-firmware"]["version"] = "20260319.git2"  # tailscale's own updater, a manual apt ...
        d = self.run_ok("survey")["freeze"]["drifted"]
        self.assertEqual(len(d), 1)
        self.assertRegex(d[0], r"^package list \(sha256\) changed: recorded [0-9a-f]{64}, now [0-9a-f]{64}$")

    def test_a_kernel_that_changes_under_a_freeze_is_drift(self):
        self.frozen_survey()
        self.h.w("/proc/sys/kernel/osrelease", "7.0.0-38-generic\n")  # a reboot onto the saved default
        self.assertEqual(self.run_ok("survey")["freeze"]["drifted"], ["running kernel changed: recorded 7.0.0-31-generic, now 7.0.0-38-generic"])

    def test_a_hold_that_appears_under_a_freeze_is_drift(self):
        self.frozen_survey()
        self.h.state["held"] = ["tailscale"]
        self.assertEqual(self.run_ok("survey")["freeze"]["drifted"], ["holds changed: recorded none, now tailscale"])

    def test_a_grub_default_that_changes_under_a_freeze_is_drift(self):
        self.frozen_survey()
        self.h.state["grubenv"]["saved_entry"] = ID_38  # a pin moved, or a person ran grub-set-default
        d = self.run_ok("survey")["freeze"]["drifted"]
        self.assertEqual(d, ["grub saved default kernel changed: recorded 7.0.0-31-generic, now 7.0.0-38-generic"])

    def test_a_freeze_is_refused_while_grubs_saved_default_is_not_the_running_kernel(self):
        # r2 F7: any reboot boots the saved default, so the platform the freeze would record is not the one that comes back
        self.h.w("/boot/grub/grub.cfg", unpinned_cfg(cfg_text("grub-arm64-efi.cfg")))  # GRUB_DEFAULT=0: the default is entry 0, -38
        self.h.state["grubenv"]["saved_entry"] = ""
        r = self.freeze()
        self.assertEqual(r.returncode, 1)
        self.assertIn("grub's saved default kernel is '7.0.0-38-generic' and the host runs '7.0.0-31-generic'", r.stdout + r.stderr)
        self.assertFalse(os.path.exists(self.h.root + FREEZE_RECORD))
        self.assertEqual(self.sysctl_calls("stop", "disable"), [])
        # a thaw does not care
        self.assertEqual(self.thaw().returncode, 1)  # nothing to thaw, for its own reason
        self.assertIn("nothing to thaw", self.h.run("thaw", "--run-id", "9").stdout)

    def test_an_old_record_without_a_platform_is_not_compared(self):
        self.frozen_survey()
        rec = self.record()
        del rec["platform"]
        self.h.w(FREEZE_RECORD, json.dumps(rec))
        self.h.state["packages"]["linux-firmware"]["version"] = "x"
        self.assertEqual(self.run_ok("survey")["freeze"]["drifted"], [])

    # ---- review m4: each of these killed a mutant that had survived
    def test_a_timer_enabled_by_hand_but_not_started_is_drift_and_so_is_one_started_but_not_enabled(self):
        self.frozen_survey()
        self.set_svc("apt-daily.timer", "inactive", "enabled")
        self.set_svc("apt-daily-upgrade.timer", "active", "disabled")
        self.assertEqual(self.run_ok("survey")["freeze"]["drifted"], ["apt-daily-upgrade.timer=active/disabled", "apt-daily.timer=inactive/enabled"])

    def test_a_disable_that_did_not_take_fails_the_freeze_though_the_stop_did(self):
        r = self.freeze(systemctl_noop=["disable apt-daily.timer"])
        self.assertEqual(r.returncode, 1)
        self.assertIn("apt-daily.timer is still inactive/enabled after the freeze", r.stdout + r.stderr)

    def test_a_start_that_did_not_take_fails_the_thaw_though_the_enable_did(self):
        self.assertEqual(self.freeze().returncode, 0)
        self.h.load()
        self.h.state["systemctl_noop"] = ["start apt-daily.timer"]
        self.h.save()
        r = self.thaw()
        self.assertEqual(r.returncode, 1)
        self.assertIn("apt-daily.timer is inactive/enabled, was active/enabled before the freeze", r.stdout + r.stderr)
        self.assertTrue(os.path.exists(self.h.root + FREEZE_RECORD))

    def test_a_thaw_that_leaves_needrestart_in_another_mode_than_before_fails(self):
        self.assertEqual(self.freeze().returncode, 0)
        self.h.w("/etc/needrestart/conf.d/zzzz-person.conf", "$nrconf{restart} = 'a';\n")  # a person's file, written after the freeze
        r = self.thaw()
        self.assertEqual(r.returncode, 1)
        self.assertIn("needrestart restart mode is 'a', was '' before the freeze", r.stdout + r.stderr)
        self.assertTrue(os.path.exists(self.h.root + FREEZE_RECORD))

    def test_install_on_shutdown_is_refused_in_every_spelling_apt_reads_as_true(self):
        for v in ("TRUE", "1", "yes", "on", "with", "enable"):
            self.h.load()
            self.h.state["apt_config"]["Unattended-Upgrade::InstallOnShutdown"] = v
            self.h.save()
            r = self.freeze()
            self.assertEqual(r.returncode, 1, v)
            self.assertIn("InstallOnShutdown is", r.stdout + r.stderr)

    # ---- review m5: the freeze and the thaw converge
    def test_a_freeze_killed_right_after_a_change_and_before_its_record_is_thawed_to_the_recorded_states(self):
        r = self.freeze(systemctl_kill_after=["stop apt-daily.timer"])
        self.assertNotEqual(r.returncode, 0)
        self.assertEqual(self.svc("apt-daily.timer"), ("inactive", "enabled"))  # the stop WAS applied
        rec = self.record()
        self.assertEqual(rec["units"]["apt-daily.timer"]["changes"], [])  # and not recorded as done ...
        self.assertEqual(rec["units"]["apt-daily.timer"]["pending"], "stopped")  # ... but recorded as about to be
        self.h.load()
        self.h.state["systemctl_kill_after"] = []
        self.h.save()
        t = self.thaw()
        self.assertEqual(t.returncode, 0, t.stdout + t.stderr)
        for u in FREEZE_UNITS:
            self.assertEqual(self.svc(u), ("active", "enabled"), u)

    def test_a_pending_change_that_never_happened_is_not_undone_by_a_thaw(self):
        r = self.freeze(systemctl_kill=["stop apt-daily.timer"])  # killed BEFORE the stop
        self.assertNotEqual(r.returncode, 0)
        self.assertEqual(self.record()["units"]["apt-daily.timer"]["pending"], "stopped")
        self.h.load()
        self.h.state["systemctl_kill"] = []
        self.h.save()
        seen = len(self.h.calls())
        self.assertEqual(self.thaw().returncode, 0)
        self.assertEqual(self.sysctl_calls_since(seen, "start", "enable"), [])  # it was never stopped: nothing to start

    def sysctl_calls_since(self, n, *verbs):
        return [c for c in self.h.calls()[n:] if c.split(" ")[0] == "systemctl" and c.split(" ")[1] in verbs]

    def test_a_thaw_that_stopped_half_way_is_finished_by_the_next_one_and_changes_nothing_twice(self):
        self.assertEqual(self.freeze().returncode, 0)
        self.h.load()
        self.h.state["systemctl_fail"] = ["start unattended-upgrades.service"]
        self.h.save()
        self.assertEqual(self.thaw().returncode, 1)
        self.assertEqual(self.svc("apt-daily.timer"), ("active", "enabled"))  # the others are back
        self.h.load()
        self.h.state["systemctl_fail"] = []
        self.h.save()
        seen = len(self.h.calls())
        self.assertEqual(self.thaw("9").returncode, 0)
        self.assertEqual([c for c in self.sysctl_calls_since(seen, "start", "enable")], ["systemctl start unattended-upgrades.service"])
        for u in FREEZE_UNITS:
            self.assertEqual(self.svc(u), ("active", "enabled"), u)

    def test_a_unit_masked_by_hand_after_the_freeze_is_named_and_the_rest_is_restored(self):
        self.assertEqual(self.freeze().returncode, 0)
        self.set_svc("apt-daily.timer", "inactive", "masked")
        r = self.thaw()
        self.assertEqual(r.returncode, 1)
        self.assertIn("apt-daily.timer is MASKED (by hand?)", r.stdout + r.stderr)
        self.assertEqual(self.svc("unattended-upgrades.service"), ("active", "enabled"))
        self.set_svc("apt-daily.timer", "inactive", "disabled")  # unmasked
        self.assertEqual(self.thaw("9").returncode, 0)
        self.assertEqual(self.svc("apt-daily.timer"), ("active", "enabled"))

    def test_a_timer_enabled_only_until_the_next_boot_is_frozen_and_thawed_as_it_was(self):
        self.set_svc("apt-daily.timer", "active", "enabled-runtime")
        r = self.freeze()
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertEqual(self.svc("apt-daily.timer"), ("inactive", "disabled"))
        self.assertEqual(self.record()["units"]["apt-daily.timer"]["changes"], ["stopped", "disabled"])
        self.assertEqual(self.thaw().returncode, 0)
        self.assertEqual(self.svc("apt-daily.timer"), ("active", "enabled-runtime"))
        self.assertIn("systemctl enable --runtime apt-daily.timer", self.h.calls())

    def test_a_needrestart_file_an_earlier_freeze_left_is_adopted_by_the_next_freeze(self):
        self.h.w(NR_DROPIN, "# Managed by probatorium ansible/os-upgrade.yml (mode=freeze): needrestart lists the services that need a restart and restarts NONE, so the\n"
                 "# platform stays fixed under a measurement. mode=thaw removes this file.\n$nrconf{restart} = 'l';\n")  # its record was lost
        self.assertEqual(self.freeze().returncode, 0)
        self.assertEqual(self.record()["needrestart"], {"installed": True, "mode_before": "", "dropin_written": True, "adopted": True})
        self.assertEqual(self.thaw().returncode, 0)
        self.assertFalse(os.path.exists(self.h.root + NR_DROPIN))

    def test_a_unit_that_a_package_brings_after_the_freeze_is_drift(self):
        self.set_svc("apt-daily-upgrade.timer", "inactive", "not-found")
        self.frozen_survey()
        self.set_svc("apt-daily-upgrade.timer", "active", "enabled")
        self.assertEqual(self.run_ok("survey")["freeze"]["drifted"], ["apt-daily-upgrade.timer=active/enabled"])

    def test_the_freeze_record_is_written_durably(self):
        calls = []
        real = os.fsync
        os.fsync = lambda fd: (calls.append(fd), real(fd))[1]
        try:
            with self.in_fake_root():
                osu.write("/var/lib/x/rec.json", "{}", durable=True)
                self.assertEqual(len(calls), 2)  # the file, then its directory
                osu.write("/var/lib/x/other.json", "{}")
                self.assertEqual(len(calls), 2)
        finally:
            os.fsync = real

    # ---- r2
    def test_a_stop_that_timed_out_but_took_effect_is_still_undone_by_the_thaw(self):  # F1
        r = self.freeze(systemctl_apply_fail=["stop unattended-upgrades.service"])
        self.assertEqual(r.returncode, 1)
        self.assertEqual(self.svc("unattended-upgrades.service"), ("inactive", "disabled"))  # the stop WAS applied
        rec = self.record()
        self.assertEqual(rec["units"]["unattended-upgrades.service"]["attempted"], ["stopped"])
        self.assertEqual(rec["units"]["unattended-upgrades.service"]["changes"], ["disabled"])
        self.h.load()
        self.h.state["systemctl_apply_fail"] = []
        self.h.save()
        t = self.thaw()
        self.assertEqual(t.returncode, 0, t.stdout + t.stderr)
        for u in FREEZE_UNITS:
            self.assertEqual(self.svc(u), ("active", "enabled"), u)

    def test_a_disable_that_timed_out_but_took_effect_is_still_undone_by_the_thaw(self):  # F1
        r = self.freeze(systemctl_apply_fail=["disable apt-daily.timer"])
        self.assertEqual(r.returncode, 1)
        self.assertEqual(self.record()["units"]["apt-daily.timer"]["attempted"], ["disabled"])
        self.h.load()
        self.h.state["systemctl_apply_fail"] = []
        self.h.save()
        self.assertEqual(self.thaw().returncode, 0)
        self.assertEqual(self.svc("apt-daily.timer"), ("active", "enabled"))

    def test_the_thaw_records_what_moved_since_the_freeze_before_it_changes_anything(self):  # F2
        self.assertEqual(self.freeze().returncode, 0)
        self.h.load()
        self.h.state["packages"]["linux-firmware"]["version"] = "20260319.git2"
        self.h.save()
        t = self.thaw()
        self.assertEqual(t.returncode, 0, t.stdout + t.stderr)
        d = json.loads(t.stdout)["drifted_at_thaw"]
        self.assertEqual(len(d), 1)
        self.assertTrue(d[0].startswith("package list (sha256) changed"))
        arch = json.load(open(self.h.root + "/var/lib/celeris-os-upgrade/freeze-7-thawed.json"))
        self.assertEqual(arch["drifted_at_thaw"], d)

    # ---- r3
    def test_a_failed_thaw_carries_the_drift_it_found_before_it_changed_anything(self):  # r3 1a
        self.assertEqual(self.freeze().returncode, 0)
        self.h.load()
        self.h.state["packages"]["linux-firmware"]["version"] = "20260319.git2"
        self.h.save()
        self.set_svc("apt-daily.timer", "inactive", "masked")
        r = self.thaw()
        self.assertEqual(r.returncode, 1)
        out = json.loads(r.stdout)
        self.assertFalse(out["ok"])
        self.assertEqual(len(out["drifted_at_thaw"]), 1)
        self.assertTrue(out["drifted_at_thaw"][0].startswith("package list (sha256) changed"))
        self.assertTrue(out["drift_proof"])

    def test_a_retried_thaw_reuses_the_drift_of_the_first_attempt_and_not_what_the_first_attempt_restored(self):  # r3 1b
        self.assertEqual(self.freeze().returncode, 0)
        self.set_svc("apt-daily.timer", "inactive", "masked")
        self.assertEqual(self.thaw().returncode, 1)  # the other units are back: they now read as "on"
        self.assertEqual(self.record()["drifted_at_thaw"], [])  # stored by the first attempt, before it changed anything
        self.set_svc("apt-daily.timer", "inactive", "disabled")
        t = self.thaw("9")
        self.assertEqual(t.returncode, 0, t.stdout + t.stderr)
        self.assertEqual(json.loads(t.stdout)["drifted_at_thaw"], [])
        arch = json.load(open(self.h.root + "/var/lib/celeris-os-upgrade/freeze-7-thawed.json"))
        self.assertEqual(arch["drifted_at_thaw"], [])

    def test_a_retry_does_not_overwrite_the_archive_with_drift_the_thaw_itself_caused(self):  # r3 1c
        self.assertEqual(self.freeze().returncode, 0)
        self.assertEqual(self.thaw().returncode, 0)
        arch_path = self.h.root + "/var/lib/celeris-os-upgrade/freeze-7-thawed.json"
        first = open(arch_path).read()
        # killed between the archive and the removal of the record: the record is still there, as the first attempt saved it
        rec = json.loads(first)
        for k in ("thawed_utc", "thawed_by_run", "restored"):
            rec.pop(k)
        rec["status"] = "frozen"
        self.h.w(FREEZE_RECORD, json.dumps(rec))
        self.h.load()
        self.h.state["packages"]["linux-firmware"]["version"] = "20260319.git2"  # and the units are on: both would read as drift now
        self.h.save()
        t = self.thaw("9")
        self.assertEqual(t.returncode, 0, t.stdout + t.stderr)
        again = json.load(open(arch_path))
        self.assertEqual(again["drifted_at_thaw"], [])
        self.assertEqual(json.loads(first)["drifted_at_thaw"], [])

    def test_a_record_without_a_platform_gives_no_drift_proof(self):  # r3 2
        r = self.freeze(systemctl_kill=["stop apt-daily.timer"])
        self.assertNotEqual(r.returncode, 0)
        self.assertNotIn("platform", self.record())
        self.h.load()
        self.h.state["systemctl_kill"] = []
        self.h.state["packages"]["linux-firmware"]["version"] = "20260319.git2"
        self.h.save()
        t = self.thaw()
        self.assertEqual(t.returncode, 0, t.stdout + t.stderr)
        out = json.loads(t.stdout)
        self.assertFalse(out["drift_proof"])  # nothing was compared: no "did not move"

    def test_an_old_frozen_record_without_a_platform_gives_no_drift_proof(self):  # r3 2
        self.assertEqual(self.freeze().returncode, 0)
        rec = self.record()
        del rec["platform"]
        self.h.w(FREEZE_RECORD, json.dumps(rec))
        self.assertFalse(json.loads(self.thaw().stdout)["drift_proof"])

    def test_a_freeze_that_did_not_complete_gives_no_drift_proof_though_it_recorded_a_platform(self):  # r3 2
        r = self.freeze(systemctl_noop=["disable apt-daily.timer"])
        self.assertEqual(r.returncode, 1)
        self.assertIn("platform", self.record())
        self.assertEqual(self.record()["status"], "freezing-failed")
        self.h.load()
        self.h.state["systemctl_noop"] = []
        self.h.save()
        self.assertFalse(json.loads(self.thaw().stdout)["drift_proof"])

    def test_the_newest_archive_is_the_last_thaw(self):  # r3 7
        for run, thaw in (("7", "8"), ("9", "10")):
            self.assertEqual(self.freeze(run).returncode, 0)
            self.h.load()
            self.assertEqual(self.thaw(thaw).returncode, 0)
            time.sleep(1.1)  # the archives are told apart by the second of their thaw
        self.h.load()
        self.assertEqual(self.run_ok("survey")["freeze_last_thaw"]["run_id"], "9")

    def test_a_completed_freeze_gives_a_drift_proof(self):  # r3 2
        self.assertEqual(self.freeze().returncode, 0)
        self.assertTrue(json.loads(self.thaw().stdout)["drift_proof"])

    def test_a_timer_that_came_back_is_in_the_drift_at_thaw_time(self):  # r3 5
        self.assertEqual(self.freeze().returncode, 0)
        self.set_svc("apt-daily.timer", "active", "enabled")  # a package upgrade switched it back on
        out = json.loads(self.thaw().stdout)
        self.assertEqual(out["drifted_at_thaw"], ["apt-daily.timer=active/enabled"])

    def test_a_freeze_refusal_for_the_saved_default_names_the_real_ways_out(self):  # r3 6
        self.h.w("/boot/grub/grub.cfg", unpinned_cfg(cfg_text("grub-arm64-efi.cfg")))
        self.h.state["grubenv"]["saved_entry"] = ""
        msg = " ".join((self.freeze().stdout).split())
        self.assertIn("an un-upgraded host whose first menu entry is another kernel needs a console rehearsal", msg)
        self.assertIn("cluster-reboot workflow", msg)
        self.assertNotIn("finish the upgrade", msg)

    def test_a_thaw_killed_after_it_finished_is_told_so_by_the_next_one(self):  # r3 7
        self.assertEqual(self.freeze().returncode, 0)
        self.assertEqual(self.thaw().returncode, 0)  # the record is gone and the archive complete: what a kill before the answer leaves
        r = self.thaw("9")
        self.assertEqual(r.returncode, 1)
        msg = r.stdout + r.stderr
        self.assertIn("the last freeze (run 7) was thawed by run 8", msg)
        self.assertIn("freeze-7-thawed.json", msg)
        self.assertIn("do not thaw again", msg)
        self.h.load()
        self.assertEqual(self.run_ok("survey")["freeze_last_thaw"]["thawed_by_run"], "8")

    def test_a_thaw_of_a_platform_that_did_not_move_says_so(self):  # F2
        self.assertEqual(self.freeze().returncode, 0)
        self.h.load()
        t = self.thaw()
        self.assertEqual(json.loads(t.stdout)["drifted_at_thaw"], [])

    def test_the_message_for_an_unreadable_record_names_the_command_that_clears_it(self):  # F3
        self.h.w(FREEZE_RECORD, "{ not json")
        r = self.thaw()
        self.assertEqual(r.returncode, 1)
        self.assertIn("rm %s %s " % (NR_DROPIN, FREEZE_RECORD), (r.stdout + r.stderr).replace("\\n", " "))
        self.assertIn("an upgrade and a new freeze are refused", r.stdout + r.stderr)

    def test_a_needrestart_file_of_ours_that_the_record_does_not_know_is_removed_by_the_thaw(self):  # F4
        r = self.freeze(systemctl_kill_nth={"is-active apt-daily.timer": 2})
        self.assertNotEqual(r.returncode, 0)
        rec = self.record()
        rec["needrestart"]["dropin_written"] = False  # the kill came between the write of the file and the save of the record
        self.h.w(FREEZE_RECORD, json.dumps(rec))
        self.assertTrue(os.path.exists(self.h.root + NR_DROPIN))
        self.h.load()
        self.h.state["systemctl_kill_nth"] = {}
        self.h.save()
        self.assertEqual(self.thaw().returncode, 0)
        self.assertFalse(os.path.exists(self.h.root + NR_DROPIN))

    def test_a_needrestart_file_of_somebody_else_at_that_path_is_not_removed_without_a_record(self):  # F4
        self.assertEqual(self.freeze().returncode, 0)
        rec = self.record()
        rec["needrestart"]["dropin_written"] = False
        self.h.w(FREEZE_RECORD, json.dumps(rec))
        self.h.w(NR_DROPIN, "$nrconf{restart} = 'a'; # a person's own\n")
        self.assertEqual(self.thaw().returncode, 0)
        self.assertIn("a person's own", open(self.h.root + NR_DROPIN).read())

    def test_a_unit_the_freeze_found_off_and_left_alone_is_drift_too_when_it_comes_back(self):
        self.set_svc("apt-daily-upgrade.timer", "inactive", "disabled")  # a person had switched it off: the freeze leaves it
        self.assertEqual(self.freeze().returncode, 0)
        self.assertEqual(self.record()["units"]["apt-daily-upgrade.timer"]["changes"], [])
        self.h.load()
        self.assertEqual(self.run_ok("survey")["freeze"]["drifted"], [])
        self.set_svc("apt-daily-upgrade.timer", "active", "enabled")  # and a package upgrade switches it back on
        self.assertEqual(self.run_ok("survey")["freeze"]["drifted"], ["apt-daily-upgrade.timer=active/enabled"])

    def test_a_unit_the_host_does_not_have_never_drifts(self):
        self.set_svc("apt-daily-upgrade.timer", "inactive", "not-found")
        self.assertEqual(self.freeze().returncode, 0)
        self.h.load()
        self.assertEqual(self.run_ok("survey")["freeze"]["drifted"], [])

    def test_the_survey_is_still_read_only_with_a_freeze_in_place(self):
        self.assertEqual(self.freeze().returncode, 0)
        self.h.load()
        before = self.h.snapshot()
        calls = len(self.h.calls())
        self.assertEqual(self.h.run("survey").returncode, 0)
        self.assertEqual(self.h.snapshot(), before)
        self.assertFalse(any(c.split(" ")[1:2] and c.split(" ")[0] == "systemctl" and c.split(" ")[1] in ("stop", "start", "enable", "disable") for c in self.h.calls()[calls:]))

    def test_the_evidence_dumps(self):
        self.h.save()
        d = self.h.run("dump", "dpkg-l")
        self.assertEqual(d.returncode, 0)
        self.assertIn("linux-image-7.0.0-31-generic", d.stdout)
        self.h.load()
        self.h.state["held"] = ["tailscale", "grub-common"]
        self.h.save()
        self.assertEqual(self.h.run("dump", "held").stdout, "grub-common\ntailscale\n")
        k = self.h.run("dump", "kernel").stdout
        for want in ("os: Ubuntu 26.04 LTS", "running_kernel: 7.0.0-31-generic", "installed_kernels: 7.0.0-31-generic 7.0.0-38-generic", "newest_same_flavor: 7.0.0-38-generic", "boot_id: aaaaaaaa-0000"):
            self.assertIn(want, k)


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

    def test_a_purge_that_reports_an_error_keeps_the_record_even_when_the_packages_are_gone(self):
        self.broken()
        self.arm_record("7.0.0-40-generic")
        self.h.state["remove_rc_after"] = 100
        self.h.state["configure_ok"] = True
        self.h.save()
        self.j(self.h.run("repair-start", "--run-id", "9", "--action", "abort-new-kernel"))
        self.assertNotEqual(self.wait()["rc"], 0)
        self.assertNotIn("linux-image-7.0.0-40-generic", self.h.load()["packages"])
        self.assertTrue(self.record("7.0.0-40-generic"), "the record went although the purge did not exit 0")

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


def load_tests(loader, tests, pattern):
    """OSU_SHARD=i/n runs every n-th test (by position in the full suite) so that a caller with several cores can run n processes at once:
    workflow_cluster_os_upgrade_test.go does, to keep the root package's go test fast. Without it, or with a test named on the command line,
    nothing changes."""
    shard = os.environ.get("OSU_SHARD", "")
    if not shard:
        return tests
    i, n = (int(x) for x in shard.split("/"))

    def flat(suite):
        for t in suite:
            if isinstance(t, unittest.TestSuite):
                yield from flat(t)
            else:
                yield t
    picked = unittest.TestSuite(t for k, t in enumerate(flat(tests)) if k % n == i)
    return picked


if __name__ == "__main__":
    unittest.main(verbosity=1)
