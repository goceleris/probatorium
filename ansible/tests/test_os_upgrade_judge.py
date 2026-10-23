#!/usr/bin/env python3
"""Runs the REAL judgement of ansible/os-upgrade.yml (ansible/vars/os_upgrade_judge.yml: the Jinja that decides
whether to refuse an upgrade, whether to refuse the reboot, and whether a rebooted host is verified) under
ansible-playbook on localhost, against records produced by the real ansible/files/os_upgrade.py on a fake host, and
checks each verdict, every refusal of the maintainer's list one by one:

    python3 ansible/tests/test_os_upgrade_judge.py     (needs ansible-playbook; skips with exit 0 without)

A Jinja template that only ever runs on a cluster host being upgraded would be a template nobody has run.
"""
import copy
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
sys.path.insert(0, os.path.join(HERE, "..", "files"))
from osu_fakehost import FakeHost, FIXTURES  # noqa: E402

ANSIBLE = shutil.which("ansible-playbook")
JUDGE = os.path.join(HERE, "..", "vars", "os_upgrade_judge.yml")
UUID = "0b1c2d3e-aaaa-bbbb-cccc-000000000001"


def cfg(name):
    with open(os.path.join(FIXTURES, name)) as f:
        return f.read()


def build_baseline():
    """(survey, sim, dkms_check) as the real script prints them for a host that is ready to be upgraded."""
    tmp = tempfile.mkdtemp(prefix="osu-judge-")
    try:
        h = FakeHost(tmp)
        # ready: the cfg is the unpinned one; the running kernel has an entry
        text = cfg("grub-arm64-efi.cfg")
        start = text.index('if [ "${next_entry}" ] ; then')
        end = text.index("fi\n", text.index('set default="${saved_entry}"')) + 3
        h.w("/boot/grub/grub.cfg", text[:start] + 'set default="0"\n' + text[end:])
        h.save()
        survey = json.loads(h.run("survey").stdout)
        sim = json.loads(h.run("simulate").stdout)
        # after the upgrade: -40 is there, pinned, built
        k = "7.0.0-40-generic"
        for p in ("/boot/vmlinuz-" + k, "/boot/initrd.img-" + k, "/lib/modules/%s/build/Makefile" % k):
            h.w(p)
        h.w("/lib/modules/%s/modules.alias" % k, "alias pci:v000010ECd00008127sv*sd*bc*sc*i* r8127\n")
        h.w("/boot/grub/grub.cfg", text.replace("7.0.0-38-generic", k))
        h.w("/usr/lib/systemd/system-shutdown/quiesce-r8127.shutdown", "#!/bin/sh\n", 0o755)
        h.load()
        eid = "gnulinux-advanced-%s>gnulinux-7.0.0-31-generic-advanced-%s" % (UUID, UUID)
        h.state["grubenv"]["saved_entry"] = eid
        h.state["modinfo"][k + "/r8169"] = {"filename": "/x/r8169.ko"}
        h.state["modinfo"][k + "/r8127"] = {"filename": "/lib/modules/%s/updates/dkms/r8127.ko.zst" % k, "version": "11.015.00", "signer": "Albert (MOK)"}
        h.state["dkms_status"] = ["r8127/11.015.00, 7.0.0-31-generic, aarch64: installed", "r8127/11.015.00, %s, aarch64: installed" % k]
        h.save()
        dk = json.loads(h.run("dkms-check", "--running", "7.0.0-31-generic", "--target", k).stdout)
        return survey, sim, dk
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


def snapshot(boot_id, kernel, total, crash=()):
    """The reboot_snapshot.sh record, with the fields the judgement reads."""
    return {
        "boot_id": boot_id, "kernel": kernel, "crash": list(crash), "r8127": True, "modinfo_r8127": True,
        "manifest_present": False, "results_entries": 0, "results_list": [], "resume_pending": False, "runner_workers": 0,
        "prereq": {"tailscaled_active": True, "tailscaled_enabled": "enabled", "apcupsd_active": True, "resume_check_enabled": True,
                   "power_event_installed": True, "tmpfiles_conf": True, "quiesce_hook": True, "run_celeris_owner": "mini"},
        "placement_vector_present": True, "lan_ip_present": True, "peer_ping_ok": True,
        "bonds": [{"bond": "bond0", "mode": "802.3ad", "slaves": ["enp1s0=up", "enp49s0=up"]}],
        "max_vector_total": total, "failed_units": [],
    }


SERVER_MARKER = {"permanent": True, "kernel_after": "6.14.0-44-generic", "run_id": "1"}


def base_case(survey, sim, dk):
    pre_survey = copy.deepcopy(survey)
    pre_survey["rehearsal"] = {"kernel": survey["kernel"], "run_id": "11", "ok": True, "valid": True, "stale": ""}  # an upgrade needs a rehearsal on the running kernel
    post_survey = copy.deepcopy(survey)
    post_survey["kernel"] = "7.0.0-40-generic"
    servers = {"msa2-server": {"kernel": "6.14.0-44-generic", "marker": copy.deepcopy(SERVER_MARKER)},
               "msr1": {"kernel": "7.0.0-40-generic", "marker": {"permanent": True, "kernel_after": "7.0.0-40-generic"}},
               "msa2-client": {"kernel": "6.14.0-37-generic", "marker": {}}}
    return {
        "os_host": "msr1",
        "os_confirm": "UPGRADE-msr1",
        "allow_results_loss": False,
        "pre_target": snapshot("aaaa", "7.0.0-31-generic", 4050000000),
        "pre_survey": pre_survey,
        "os_surveys": servers,
        "sim": copy.deepcopy(sim),
        "dkms_check": copy.deepcopy(dk),
        "target_kernel": "7.0.0-40-generic",
        "post_raw": {"stdout": json.dumps(snapshot("bbbb", "7.0.0-40-generic", 1200))},
        "post_survey": post_survey,
        "toolcheck": {"cc_ok": True, "https_ok": True, "endpoints": {}, "warnings": []},
        "pre_toolcheck": {"cc_ok": True, "https_ok": True, "endpoints": {}, "warnings": []},
        "blacklist_r8169": False,
    }


PLAY = """
- hosts: localhost
  connection: local
  gather_facts: false
  vars_files:
    - {judge}
  vars:
    ansible_user: mini
    results_root: /tmp/celeris-results
    placement_vector: enp1s0-8
    lan_ip: 192.168.50.199
    fabric_peer: 192.168.50.195
    os_bench_targets: [msa2-server, msr1]
    os_loadgen_hosts: [msa2-client]
  tasks:
    - ansible.builtin.copy:
        content: "{{{{ {{'refusals': refusals_str | trim, 'plan': plan_refusals_str | trim, 'prereboot': pre_reboot_problems_str | trim, 'post': post_problems_str | trim, 'maint': maint_refusals_str | trim}} | to_json }}}}"
        dest: "{{{{ out_dir }}}}/{{{{ item.key }}}}.json"
      loop: "{{{{ cases | dict2items }}}}"
      loop_control:
        label: "{{{{ item.key }}}}"
      vars:
        os_host: "{{{{ item.value.os_host }}}}"
        os_mode: "{{{{ item.value.os_mode | default('upgrade') }}}}"
        os_confirm: "{{{{ item.value.os_confirm | default('') }}}}"
        retry_kernel: "{{{{ item.value.retry_kernel | default('') }}}}"
        repair_action: "{{{{ item.value.repair_action | default('configure') }}}}"
        blacklist_r8169: "{{{{ item.value.blacklist_r8169 | default(false) }}}}"
        pre_toolcheck: "{{{{ item.value.pre_toolcheck }}}}"
        allow_results_loss: "{{{{ item.value.allow_results_loss }}}}"
        pre_target: "{{{{ item.value.pre_target }}}}"
        pre_survey: "{{{{ item.value.pre_survey }}}}"
        os_surveys: "{{{{ item.value.os_surveys }}}}"
        sim: "{{{{ item.value.sim }}}}"
        dkms_check: "{{{{ item.value.dkms_check }}}}"
        target_kernel: "{{{{ item.value.target_kernel }}}}"
        post_raw: "{{{{ item.value.post_raw }}}}"
        post_survey: "{{{{ item.value.post_survey }}}}"
        toolcheck: "{{{{ item.value.toolcheck }}}}"
"""


def judge_all(cases):
    work = tempfile.mkdtemp(prefix="osu-judge-run-")
    try:
        with open(os.path.join(work, "play.yml"), "w") as f:
            f.write(PLAY.format(judge=JUDGE))
        with open(os.path.join(work, "cases.json"), "w") as f:
            json.dump({"cases": cases, "out_dir": os.path.join(work, "out")}, f)
        os.makedirs(os.path.join(work, "out"))
        env = dict(os.environ, ANSIBLE_LOCALHOST_WARNING="False", ANSIBLE_INVENTORY_UNPARSED_WARNING="False", ANSIBLE_DEPRECATION_WARNINGS="False")
        r = subprocess.run([ANSIBLE, "-i", "localhost,", os.path.join(work, "play.yml"), "-e", "@" + os.path.join(work, "cases.json")],
                           capture_output=True, text=True, env=env, cwd=work, timeout=600)
        if r.returncode != 0:
            raise AssertionError("ansible-playbook failed:\n" + r.stdout[-3000:] + r.stderr[-3000:])
        out = {}
        for n in cases:
            with open(os.path.join(work, "out", n + ".json")) as f:
                out[n] = json.load(f)
        return out
    finally:
        shutil.rmtree(work, ignore_errors=True)


if os.environ.get("CI") == "true" and ANSIBLE is None:
    raise SystemExit("CI=true: this suite needs ansible-playbook and must not skip itself (a broken ansible install would read as green)")


@unittest.skipIf(ANSIBLE is None, "ansible-playbook not installed")
class Judge(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        survey, sim, dk = build_baseline()
        cls.survey, cls.sim, cls.dk = survey, sim, dk
        cls.cases = {}

        def case(name, expect_in, mutate=None, expect_nothing_else=False):
            c = base_case(survey, sim, dk)
            if mutate:
                mutate(c)
            cls.cases[name] = (c, expect_in)

        # ---- refusals (before anything is touched)
        case("healthy", {})
        case("manifest", {"refusals": "manifest exists"}, lambda c: c["pre_target"].update(manifest_present=True))
        case("results", {"refusals": "results_root /tmp/celeris-results holds 3 entr(ies) the reboot destroys (a, b)"},
             lambda c: c["pre_target"].update(results_entries=3, results_list=["a", "b"]))
        case("results_allowed", {}, lambda c: (c["pre_target"].update(results_entries=3, results_list=["a"]), c.update(allow_results_loss=True)))
        case("resume", {"refusals": "resume-pending.json exists"}, lambda c: c["pre_target"].update(resume_pending=True))
        case("runner", {"refusals": "1 runner job(s) are executing"}, lambda c: c["pre_target"].update(runner_workers=1))
        case("no_hook", {"refusals": "no shutdown quiesce hook"}, lambda c: c["pre_target"]["prereq"].update(quiesce_hook=False))
        case("no_modinfo", {"refusals": "r8127 module is not available for the running kernel"}, lambda c: c["pre_target"].update(modinfo_r8127=False))
        case("tailscaled_not_enabled", {"refusals": 'tailscaled is running but systemctl is-enabled says "disabled"'},
             lambda c: c["pre_target"]["prereq"].update(tailscaled_enabled="disabled"))
        case("disk_root", {"refusals": "not enough free disk on /: 1000 KiB free"}, lambda c: c["pre_survey"]["disk"]["/"].update(free_kib=1000))
        case("disk_boot", {"refusals": "not enough free disk on /boot: 100000 KiB free"}, lambda c: c["pre_survey"]["disk"]["/boot"].update(free_kib=100000))
        case("disk_efi", {"refusals": "not enough free disk on /boot/efi"}, lambda c: c["pre_survey"]["disk"]["/boot/efi"].update(free_kib=100))
        case("disk_unreadable", {"refusals": "not enough free disk on /: unreadable"}, lambda c: c["pre_survey"]["disk"].update({"/": None}))
        case("dpkg_lock", {"refusals": "dpkg or apt is busy (an apt or dpkg lock is held)"}, lambda c: c["pre_survey"]["packages"].update(dpkg_lock_held=True))
        case("apt_lists_lock", {"refusals": "dpkg or apt is busy (an apt or dpkg lock is held (/var/lib/apt/lists/lock))"},
             lambda c: c["pre_survey"]["packages"].update(dpkg_lock_held=True, locks_held=["/var/lib/apt/lists/lock"]))
        case("apt_running", {"refusals": "dpkg or apt is busy (unattended-upgrade)"}, lambda c: c["pre_survey"]["packages"].update(package_processes=["unattended-upgrade"]))
        case("apt_get_running", {"refusals": "dpkg or apt is busy (apt-get, dpkg)"}, lambda c: c["pre_survey"]["packages"].update(package_processes=["apt-get", "dpkg"]))
        # the idle daemons of every Ubuntu host (kernel comm names, as the first release of the tool recorded them) are not a package run
        case("idle_daemons_are_not_apt_running", {}, lambda c: c["pre_survey"]["packages"].update(package_processes=["packagekitd", "unattended-upgr"]))
        case("idle_daemons_beside_a_real_run", {"refusals": "dpkg or apt is busy (dpkg)"},
             lambda c: c["pre_survey"]["packages"].update(package_processes=["dpkg", "packagekitd", "unattended-upgr"]))
        case("half_configured", {"refusals": "dpkg --audit reports half-configured or broken packages: foo"},
             lambda c: c["pre_survey"]["packages"].update(dpkg_audit=["foo"]))
        case("held_kernel", {"refusals": "held kernel, boot or DKMS packages: linux-image-generic"},
             lambda c: c["pre_survey"]["packages"].update(held_kernel=["linux-image-generic"]))
        case("no_rehearsal", {"refusals": "no successful one-shot rehearsal of this host on the running kernel 7.0.0-31-generic (record: none)"},
             lambda c: c["pre_survey"].update(rehearsal={}))
        case("a_stale_rehearsal", {"refusals": "(record: 7.0.0-31-generic; STALE: grub or shim changed since the rehearsal"},
             lambda c: c["pre_survey"]["rehearsal"].update(valid=False, stale="grub or shim changed since the rehearsal (then a; now b): repeat the rehearsal"))
        case("a_rehearsal_of_another_kernel", {"refusals": "(record: 7.0.0-25-generic)"},
             lambda c: c["pre_survey"].update(rehearsal={"kernel": "7.0.0-25-generic"}))
        def rh(confirm, entry0, nic=True):
            def f(c):
                c.update(os_mode="rehearse", os_confirm=confirm)
                c["pre_survey"].update(rehearsal={})
                c["pre_survey"]["boot"].update(entry0_kernel=entry0)
            return f
        refuse_rh = "the first menu entry of grub is 7.0.0-38-generic, not the running kernel 7.0.0-31-generic"
        case("rehearse_while_entry0_is_another_kernel", {"refusals": refuse_rh}, rh("REHEARSE-msr1", "7.0.0-38-generic"))
        case("rehearse_names_what_it_would_boot", {"refusals": "no one-shot to fall back on"}, rh("REHEARSE-msr1", "7.0.0-38-generic"))
        case("rehearse_refusal_points_to_the_readme_not_to_the_override", {"refusals": "the console attestation described in the README (Rehearsal at a console), which no refusal and no workflow text spells out"}, rh("REHEARSE-msr1", "7.0.0-38-generic"))
        case("rehearse_with_the_attestation_of_another_host_is_refused", {"refusals": refuse_rh}, rh("REHEARSE-msa2-server-CONSOLE", "7.0.0-38-generic"))
        case("rehearse_with_a_console_is_allowed", {}, rh("REHEARSE-msr1-CONSOLE", "7.0.0-38-generic"))
        case("rehearse_when_entry0_is_the_running_kernel", {}, rh("REHEARSE-msr1", "7.0.0-31-generic"))
        case("rehearse_when_the_survey_has_no_entry0", {"refusals": "first menu entry of grub is unknown"}, rh("REHEARSE-msr1", ""))
        case("the_console_confirmation_does_not_help_an_upgrade", {"refusals": "no successful one-shot rehearsal"},
             lambda c: (c.update(os_confirm="REHEARSE-msr1-CONSOLE"), c["pre_survey"].update(rehearsal={})))
        case("a_rehearsal_needs_no_rehearsal", {}, lambda c: (c.update(os_mode="rehearse"), c["pre_survey"].update(rehearsal={}), c["pre_survey"]["boot"].update(entry0_kernel=c["pre_survey"]["kernel"])))
        case("a_rehearsal_has_no_order_rule", {}, lambda c: (c.update(os_mode="rehearse", os_host="msa2-client"), c["pre_survey"].update(rehearsal={}), c["pre_survey"]["boot"].update(entry0_kernel=c["pre_survey"]["kernel"]),
                                                              c.update(os_surveys={"msa2-server": {"kernel": "x", "marker": {}}, "msr1": {"kernel": "x", "marker": {}}, "msa2-client": {"kernel": "x", "marker": {}}})))
        case("the_way_in_is_already_broken", {"refusals": "the way in is ALREADY not intact on the current userland (sshd -t fails: x)"},
             lambda c: c["pre_survey"].update(access_problems=["sshd -t fails: x"]))
        case("policy_rc_d", {"refusals": "policy-rc.d exists already"}, lambda c: c["pre_survey"]["packages"].update(policy_rc_d_present=True))
        case("no_grub", {"refusals": "no usable GRUB"}, lambda c: c["pre_survey"]["boot"].update(grub_reboot=False))
        case("no_grub_cfg", {"refusals": "no usable GRUB"}, lambda c: c["pre_survey"]["boot"].update(grub_cfg_present=False))
        case("grub_not_proven", {"refusals": "GRUB is not proven to have booted this host"},
             lambda c: c["pre_survey"]["boot"]["proof"].update(ok=False, problems=["/proc/cmdline has no BOOT_IMAGE="]))
        case("savedefault", {"refusals": "GRUB_SAVEDEFAULT=true"}, lambda c: c["pre_survey"]["boot"].update(savedefault=True))
        case("boot_btrfs", {"refusals": "/boot/grub is on btrfs"}, lambda c: c["pre_survey"]["boot"].update(boot_stack="btrfs"))
        case("boot_lvm", {"refusals": "/boot/grub is on lvm-or-mdraid"}, lambda c: c["pre_survey"]["boot"].update(boot_stack="lvm-or-mdraid"))
        case("no_entry_for_running", {"refusals": "no menu entry for the running kernel 7.0.0-31-generic"},
             lambda c: c["pre_survey"]["boot"].update(running_entry_id=""))

        def client(c, markers):
            c["os_host"] = "msa2-client"
            c["os_surveys"] = markers
        ok_markers = {"msa2-server": {"kernel": "6.14.0-44-generic", "marker": {"permanent": True, "kernel_after": "6.14.0-44-generic"}},
                      "msr1": {"kernel": "7.0.0-40-generic", "marker": {"permanent": True, "kernel_after": "7.0.0-40-generic"}},
                      "msa2-client": {"kernel": "6.14.0-37-generic", "marker": {}}}
        case("client_after_both_servers", {}, lambda c: client(c, copy.deepcopy(ok_markers)))

        def only_one(c):
            m = copy.deepcopy(ok_markers)
            m["msr1"]["marker"] = {}
            client(c, m)
        case("client_before_msr1", {"refusals": "order: msr1 has not been upgraded and verified yet"}, only_one)

        def none(c):
            m = copy.deepcopy(ok_markers)
            m["msa2-server"]["marker"] = {}
            m["msr1"]["marker"] = {}
            client(c, m)
        case("client_first", {"refusals": "order: msa2-server has not been upgraded"}, none)

        def drifted(c):
            m = copy.deepcopy(ok_markers)
            m["msa2-server"]["kernel"] = "6.14.0-37-generic"  # rebooted into another kernel since its record
            client(c, m)
        case("client_after_a_server_drifted", {"refusals": "order: msa2-server has not been upgraded and verified yet (or runs another kernel than its record says: 6.14.0-37-generic vs 6.14.0-44-generic)"}, drifted)
        case("a_server_needs_no_marker", {}, lambda c: c.update(os_host="msa2-server", os_surveys={"msa2-server": {"kernel": "x", "marker": {}}, "msr1": {"kernel": "x", "marker": {}}, "msa2-client": {"kernel": "x", "marker": {}}}))

        case("next_entry_pending", {"refusals": "grub has a one-shot entry pending (next_entry=gnulinux-x)"},
             lambda c: c["pre_survey"]["boot"].update(next_entry="gnulinux-x"))
        case("earlier_run_left_an_unverified_default", {"refusals": "an earlier OS upgrade left grub saved default 7.0.0-31-generic while the host runs 7.0.0-40-generic"},
             lambda c: (c["pre_survey"].update(kernel="7.0.0-40-generic"), c["pre_survey"]["boot"].update(dropin_present=True, default_kernel="7.0.0-31-generic")))
        case("earlier_run_refusal_names_the_kernel_grub_boots_and_the_true_way_back",
             {"refusals": "If GRUB does NOT (a rehearsal or an upgrade ended with a one-shot still pending: survey boot.next_entry), the host runs GRUB's first menu entry (boot.entry0_kernel 7.0.0-40-generic, not the saved default) and cluster-reboot boots that kernel again: only the console (Esc or Shift at power-on, choose 7.0.0-31-generic) or a reviewed change brings 7.0.0-31-generic back"},
             lambda c: (c["pre_survey"].update(kernel="7.0.0-40-generic"), c["pre_survey"]["boot"].update(dropin_present=True, default_kernel="7.0.0-31-generic", entry0_kernel="7.0.0-40-generic")))
        case("earlier_run_refusal_says_unknown_without_entry0", {"refusals": "(boot.entry0_kernel unknown, not the saved default)"},
             lambda c: (c["pre_survey"].update(kernel="7.0.0-40-generic"), c["pre_survey"]["boot"].update(dropin_present=True, default_kernel="7.0.0-31-generic", entry0_kernel="")))
        case("default_differs_but_no_earlier_run", {}, lambda c: c["pre_survey"]["boot"].update(dropin_present=False, default_kernel="7.0.0-38-generic"))
        case("fell_back_default_is_the_running_kernel", {}, lambda c: c["pre_survey"]["boot"].update(dropin_present=True, default_kernel="7.0.0-31-generic"))

        def r8169_nic(c):
            c.update(blacklist_r8169=True)
            c["pre_survey"]["nics"][0]["driver"] = "r8169"
        case("blacklist_with_an_r8169_nic", {"refusals": "blacklist_r8169=true, but a NIC is bound to r8169 (enp1s0)"}, r8169_nic)
        case("blacklist_without_an_r8169_nic", {}, lambda c: c.update(blacklist_r8169=True))

        # ---- mode=repair / mode=disarm
        def repair(action="configure", **kw):
            def m(c):
                c.update(os_mode="repair", repair_action=action)
                c["pre_survey"]["packages"].update(dpkg_audit=["linux-headers half-configured"], broken_kernel_pkgs=["linux-headers-7.0.0-40-generic"], new_kernel_pkgs=["linux-headers-7.0.0-40-generic"])
                c["pre_survey"]["boot"].update(dropin_present=True, default_kernel="7.0.0-31-generic")
                for k, v in kw.items():
                    section, key = k.split("__")
                    (c["pre_target"] if section == "t" else c["pre_survey"]["packages"] if section == "p" else c["pre_survey"]["boot"]).update({key: v})
            return m
        TH = [{"run_id": "41", "packages": ["grub-common", "tailscale"]}]
        case("repair_ok", {}, repair())
        case("release_holds_with_holds_left", {}, repair("release-holds", p__tool_holds=TH, p__dpkg_audit=[], p__broken_kernel_pkgs=[]))
        case("release_holds_with_nothing_to_release", {"maint": "nothing to release: no hold put by this tool is in place"}, repair("release-holds", p__tool_holds=[]))
        case("configure_hints_at_leftover_holds", {"maint": "nothing to repair: dpkg --audit is clean (this tool left holds in place: use repair_action=release-holds)"},
             repair("configure", p__dpkg_audit=[], p__tool_holds=TH))
        case("release_holds_into_a_running_apt", {"maint": "dpkg or apt is busy (apt)"}, repair("release-holds", p__tool_holds=TH, p__package_processes=["apt"]))
        case("held_by_a_dead_run_names_the_repair", {"refusals": "THIS TOOL put the holds of 41 (grub-common tailscale) and a run that died left them: dispatch mode=repair with repair_action=release-holds"},
             lambda c: c["pre_survey"]["packages"].update(held_kernel=["grub-common", "tailscale"], tool_holds=TH))
        case("held_by_somebody_else_does_not_offer_the_release", {"refusals": "held kernel, boot or DKMS packages: grub-common (apt-mark unhold them deliberately, or leave the host)"},
             lambda c: c["pre_survey"]["packages"].update(held_kernel=["grub-common"], tool_holds=[]))
        case("repair_abort_ok", {}, repair("abort-new-kernel"))
        case("repair_beside_a_bench", {"maint": "a bench or validation manifest exists"}, repair(t__manifest_present=True))
        case("repair_beside_a_runner_job", {"maint": "1 runner job(s) are executing"}, repair(t__runner_workers=1))
        case("repair_into_a_running_apt", {"maint": "dpkg or apt is busy (apt)"}, repair(p__package_processes=["apt"]))
        case("repair_with_a_one_shot_pending", {"maint": "dispatch mode=disarm first"}, repair(b__next_entry="gnulinux-x"))
        case("repair_with_a_foreign_policy_rc_d", {"maint": "is not the one this workflow writes"}, repair(p__policy_rc_d_present=True, p__policy_rc_d_ours=False))
        case("repair_with_our_leftover_policy_rc_d", {}, repair(p__policy_rc_d_present=True, p__policy_rc_d_ours=True))
        case("repair_nothing_to_configure", {"maint": "nothing to repair: dpkg --audit is clean"}, repair(p__dpkg_audit=[]))
        case("abort_nothing_broken", {"maint": "nothing to abort"}, repair("abort-new-kernel", p__broken_kernel_pkgs=[]))
        case("abort_with_the_default_not_the_running_kernel", {"maint": "removing kernels could change what boots"}, repair("abort-new-kernel", b__default_kernel="7.0.0-40-generic"))
        case("abort_without_the_pin", {"maint": "removing kernels could change what boots"}, repair("abort-new-kernel", b__dropin_present=False))
        case("disarm_has_no_refusals", {}, lambda c: c.update(os_mode="disarm"))

        # ---- mode=freeze / mode=thaw
        FROZEN = {"run_id": "41", "utc": "2026-10-23T10:00:00Z", "status": "frozen", "drifted": [], "units": {}, "needrestart": {}}

        def fz(mode, frozen=None, **kw):
            def m(c):
                c.update(os_mode=mode)
                c["pre_survey"]["boot"].update(default_kernel=c["pre_survey"]["kernel"])  # a pinned host: a freeze is refused otherwise (F7)
                if frozen is not None:
                    c["pre_survey"]["freeze"] = copy.deepcopy(frozen)
                for k, v in kw.items():
                    section, key = k.split("__")
                    (c["pre_target"] if section == "t" else c["pre_survey"]["packages"]).update({key: v})
            return m
        case("freeze_ok", {}, fz("freeze"))
        case("freeze_beside_idle_daemons_is_fine", {}, fz("freeze", p__package_processes=["packagekitd", "unattended-upgr"]))
        case("freeze_beside_a_bench", {"maint": "a bench or validation manifest exists"}, fz("freeze", t__manifest_present=True))
        case("freeze_with_a_power_loss_resume_pending", {"maint": "a power-loss resume is pending (cluster-power-restore --list). A freeze changes the platform"}, fz("freeze", t__resume_pending=True))
        case("thaw_with_a_power_loss_resume_pending", {"maint": "A thaw re-enables the apt timers, which are persistent and can run unattended-upgrades within minutes"}, fz("thaw", FROZEN, t__resume_pending=True))
        case("freeze_beside_a_runner_job", {"maint": "1 runner job(s) are executing"}, fz("freeze", t__runner_workers=1))
        case("freeze_into_a_running_apt", {"maint": "dpkg or apt is busy (apt-get): a freeze would stop unattended-upgrades or the apt timers under a package run"}, fz("freeze", p__package_processes=["apt-get"]))
        case("freeze_into_a_held_lock", {"maint": "dpkg or apt is busy (an apt or dpkg lock is held (/var/lib/dpkg/lock)): a freeze would stop"},
             fz("freeze", p__dpkg_lock_held=True, p__locks_held=["/var/lib/dpkg/lock"]))
        case("freeze_with_install_on_shutdown", {"maint": "Unattended-Upgrade::InstallOnShutdown is true: the shutdown listener a freeze stops installs packages when the host shuts down"},
             lambda c: (c.update(os_mode="freeze"), c["pre_survey"]["boot"].update(default_kernel=c["pre_survey"]["kernel"]), c["pre_survey"]["unattended_upgrades"].update(install_on_shutdown="true")))
        for k, v in enumerate(("True", "TRUE", "yes", "on", "1", "with", "enable")):
            case("freeze_with_install_on_shutdown_%d_%s" % (k, v.lower() + ("_upper" if v.isupper() else "")), {"maint": "Unattended-Upgrade::InstallOnShutdown is %s: the shutdown listener" % v},
                 (lambda v: lambda c: (c.update(os_mode="freeze"), c["pre_survey"]["boot"].update(default_kernel=c["pre_survey"]["kernel"]), c["pre_survey"]["unattended_upgrades"].update(install_on_shutdown=v)))(v))
        case("freeze_with_install_on_shutdown_off_is_fine", {}, lambda c: (c.update(os_mode="freeze"), c["pre_survey"]["boot"].update(default_kernel=c["pre_survey"]["kernel"]), c["pre_survey"]["unattended_upgrades"].update(install_on_shutdown="false")))
        case("thaw_ignores_install_on_shutdown", {}, lambda c: (c.update(os_mode="thaw"), c["pre_survey"].update(freeze=dict(FROZEN)), c["pre_survey"]["unattended_upgrades"].update(install_on_shutdown="true")))
        case("upgrade_of_a_frozen_host", {"refusals": "this host is frozen (run 41, 2026-10-23T10:00:00Z): an upgrade changes the platform the freeze recorded"},
             lambda c: c["pre_survey"].update(freeze=copy.deepcopy(FROZEN)))
        case("rehearse_of_a_frozen_host_is_fine", {}, lambda c: (c.update(os_mode="rehearse", os_confirm="REHEARSE-msr1-CONSOLE"), c["pre_survey"].update(freeze=copy.deepcopy(FROZEN), rehearsal={})))
        case("freeze_while_the_saved_default_is_not_the_running_kernel", {"maint": "grub saved default kernel is 7.0.0-38-generic and the host runs 7.0.0-31-generic: any reboot"},
             lambda c: (c.update(os_mode="freeze"), c["pre_survey"]["boot"].update(default_kernel="7.0.0-38-generic")))
        case("freeze_while_the_saved_default_is_unknown", {"maint": "grub saved default kernel is unknown and the host runs"},
             lambda c: (c.update(os_mode="freeze"), c["pre_survey"]["boot"].update(default_kernel="")))
        case("thaw_ignores_the_saved_default", {}, lambda c: (c.update(os_mode="thaw"), c["pre_survey"].update(freeze=copy.deepcopy(FROZEN)), c["pre_survey"]["boot"].update(default_kernel="7.0.0-38-generic")))
        case("freeze_twice", {"maint": "this host is already frozen (run 41, 2026-10-23T10:00:00Z, status frozen): dispatch mode=thaw first. The record of what the first freeze changed is kept"}, fz("freeze", FROZEN))
        case("freeze_over_a_failed_freeze", {"maint": "status freezing-failed): dispatch mode=thaw first"}, fz("freeze", dict(FROZEN, status="freezing-failed")))
        case("thaw_ok", {}, fz("thaw", FROZEN))
        case("thaw_of_a_failed_freeze_is_fine", {}, fz("thaw", dict(FROZEN, status="freezing-failed")))
        case("thaw_nothing_after_a_completed_thaw_points_to_the_archive", {"maint": "the last freeze (run 41) was thawed by run 42 at 2026-10-23T11:00:00Z: read freeze-41-thawed.json"},
             lambda c: (c.update(os_mode="thaw"), c["pre_survey"].update(freeze_last_thaw={"run_id": "41", "thawed_by_run": "42", "thawed_utc": "2026-10-23T11:00:00Z", "drifted_at_thaw": []})))
        case("thaw_nothing", {"maint": "nothing to thaw: no freeze record on this host"}, fz("thaw"))
        case("thaw_beside_a_bench", {"maint": "a bench or validation manifest exists"}, fz("thaw", FROZEN, t__manifest_present=True))
        case("thaw_beside_apt_is_fine", {}, fz("thaw", FROZEN, p__package_processes=["apt-get"]))  # a thaw only puts units back; it does not race a package run

        # ---- the plan
        case("plan_removes_a_kernel", {"plan": "would REMOVE protected package(s): linux-image-7.0.0-31-generic"},
             lambda c: c["sim"].update(remv=["linux-image-7.0.0-31-generic", "libfoo"]))
        for pkg in ("grub-efi-arm64", "dkms", "openssh-server", "tailscale", "apcupsd", "docker.io", "r8127-dkms", "netplan.io", "systemd-resolved"):
            case("plan_removes_" + pkg, {"plan": "REMOVE protected package(s): " + pkg}, (lambda p: lambda c: c["sim"].update(remv=[p]))(pkg))
        case("plan_removes_an_unrelated_package", {}, lambda c: c["sim"].update(remv=["libfoo"]))
        case("plan_simulation_failed", {"plan": "apt-get -s full-upgrade failed: broken"}, lambda c: c["sim"].update(rc=100, error="broken"))

        # ---- the new kernel is not safe to boot
        case("no_target", {"prereboot": "no kernel of the running flavor is installed"}, lambda c: c.update(target_kernel=""))
        case("no_vmlinuz", {"prereboot": "/boot/vmlinuz-7.0.0-40-generic is missing"}, lambda c: c["dkms_check"].update(vmlinuz_present=False))
        case("no_initrd", {"prereboot": "/boot/initrd.img-7.0.0-40-generic is missing"}, lambda c: c["dkms_check"].update(initrd_present=False))
        case("no_entry", {"prereboot": "grub.cfg has no menu entry for 7.0.0-40-generic"}, lambda c: c["dkms_check"].update(target_entry_id=""))
        case("fallback_not_intact", {"prereboot": "the grub default would NOT boot the old kernel 7.0.0-31-generic"},
             lambda c: c["dkms_check"].update(saved_resolves_to="7.0.0-40-generic"))
        case("cfg_not_saved", {"prereboot": "grub.cfg does not use the saved entry"}, lambda c: c["dkms_check"].update(cfg_uses_saved=False))
        case("recordfail_forever", {"prereboot": "waits at the menu for ever"}, lambda c: c["dkms_check"].update(recordfail_timeout="-1"))
        case("no_headers", {"prereboot": "linux-headers for 7.0.0-40-generic are not installed"}, lambda c: c["dkms_check"].update(headers_present=False))
        case("dkms_missing", {"prereboot": "DKMS module(s) r8127 are installed for the running kernel and not for 7.0.0-40-generic"},
             lambda c: c["dkms_check"].update(missing_modules=["r8127"]))
        case("dkms_build_failed", {"prereboot": "dkms autoinstall -k 7.0.0-40-generic failed (exit 10)"}, lambda c: c["dkms_check"].update(build_rc=10))
        case("nic_driver_missing", {"prereboot": "the driver(s) of the NICs in use are not available for 7.0.0-40-generic: r8127"},
             lambda c: c["dkms_check"].update(nic_drivers_missing=["r8127"]))
        case("r8127_not_built", {"prereboot": "r8127 is NOT built for 7.0.0-40-generic"}, lambda c: c["dkms_check"]["r8127"].update(found=False))
        case("r8127_no_aliases", {"prereboot": "no PCI aliases"}, lambda c: c["dkms_check"].update(r8127_aliases=False))
        case("secure_boot_unsigned", {"prereboot": "Secure Boot is enabled and the r8127 module of 7.0.0-40-generic is not signed by the key"},
             lambda c: c["dkms_check"].update(secure_boot="enabled") or c["dkms_check"]["r8127"].update(signer=""))
        case("secure_boot_signed", {}, lambda c: c["dkms_check"].update(secure_boot="enabled"))
        case("target_hook_missing", {"prereboot": "shutdown quiesce hook of the r8127 NIC is missing"}, lambda c: c["dkms_check"].update(quiesce_hook=False))
        RACE = "in-tree r8169 of 7.0.0-40-generic lists the same PCI id as r8127 and r8169 is not blacklisted in the new kernel initramfs as well"
        case("intree_races_r8127", {"prereboot": RACE},
             lambda c: c["dkms_check"].update(intree_lists_id=True, intree_lists_id_running=False, r8169_blacklisted=False, r8169_blacklist_in_initrd=False))
        # the old exemption is gone: a running kernel whose r8169 also lists the id is no reason to trust a newer r8169
        case("intree_lists_it_on_the_running_kernel_too", {"prereboot": RACE},
             lambda c: c["dkms_check"].update(intree_lists_id=True, intree_lists_id_running=True, r8169_blacklisted=False, r8169_blacklist_in_initrd=False))
        case("intree_blacklisted_but_the_initramfs_is_older", {"prereboot": RACE},
             lambda c: c["dkms_check"].update(intree_lists_id=True, r8169_blacklisted=True, r8169_blacklist_in_initrd=False))
        case("intree_blacklisted_in_the_initramfs_too", {},
             lambda c: c["dkms_check"].update(intree_lists_id=True, r8169_blacklisted=True, r8169_blacklist_in_initrd=True))
        case("intree_does_not_list_the_id", {}, lambda c: c["dkms_check"].update(intree_lists_id=False))
        case("access_broken", {"prereboot": "the way in is not intact on the upgraded userland (both kernels share it, a power-cycle would not undo it): sshd -t fails: x / netplan generate fails"},
             lambda c: c["dkms_check"].update(access_problems=["sshd -t fails: x", "netplan generate fails"]))
        case("target_was_armed_before", {"prereboot": "was armed as a one-shot by run 77 and the host may have booted it since"},
             lambda c: c["dkms_check"].update(target_armed_before=[{"kernel": "7.0.0-40-generic", "run_id": "77"}]))
        case("target_was_armed_before_and_a_retry_is_asked", {},
             lambda c: (c["dkms_check"].update(target_armed_before=[{"kernel": "7.0.0-40-generic", "run_id": "77"}]), c.update(retry_kernel="7.0.0-40-generic")))
        case("a_retry_of_another_kernel_does_not_count", {"prereboot": "was armed as a one-shot by run 77"},
             lambda c: (c["dkms_check"].update(target_armed_before=[{"kernel": "7.0.0-40-generic", "run_id": "77"}]), c.update(retry_kernel="7.0.0-38-generic")))
        case("target_older_than_running", {"prereboot": "is older than the running one"}, lambda c: c["dkms_check"].update(target_older_than_running=True))
        case("secure_boot_other_key", {"prereboot": "not signed by the key the running module is signed with"},
             lambda c: c["dkms_check"].update(secure_boot="enabled", running_r8127_signer="Albert MOK") or c["dkms_check"]["r8127"].update(signer="Someone Else"))
        case("secure_boot_same_key", {}, lambda c: c["dkms_check"].update(secure_boot="enabled", running_r8127_signer="Albert MOK") or c["dkms_check"]["r8127"].update(signer="Albert MOK"))
        case("secure_boot_unknown_and_unsigned", {"prereboot": "Secure Boot is unknown"},
             lambda c: c["dkms_check"].update(secure_boot="unknown") or c["dkms_check"]["r8127"].update(signer=""))
        case("a_host_without_r8127_needs_none_of_it", {}, lambda c: (c["pre_target"].update(r8127=False, modinfo_r8127=None),
                                                                     c["dkms_check"]["r8127"].update(found=False)))

        # ---- verification after the reboot
        def post(c, **kw):
            s = json.loads(c["post_raw"]["stdout"])
            s.update(kw)
            c["post_raw"]["stdout"] = json.dumps(s)
        case("post_same_boot_id", {"post": "boot_id did not change"}, lambda c: post(c, boot_id="aaaa"))
        case("post_fell_back", {"post": "booted the OLD kernel 7.0.0-31-generic again: the one-shot entry for 7.0.0-40-generic fell back"},
             lambda c: post(c, kernel="7.0.0-31-generic"))
        case("post_other_kernel", {"post": "runs 7.0.0-38-generic, not the intended 7.0.0-40-generic"}, lambda c: post(c, kernel="7.0.0-38-generic"))
        case("post_crash", {"post": "new crash dump(s) in /var/crash: 20261022"}, lambda c: post(c, crash=["20261022"]))
        for k in ("tailscaled_active", "apcupsd_active", "resume_check_enabled", "power_event_installed", "tmpfiles_conf", "quiesce_hook"):
            case("post_regressed_" + k, {"post": "prerequisite regressed: " + k},
                 (lambda k: lambda c: post(c, prereq=dict(json.loads(c["post_raw"]["stdout"])["prereq"], **{k: False})))(k))
        case("post_run_celeris_owner", {"post": "/run/celeris is not owned by mini (root)"},
             lambda c: post(c, prereq=dict(json.loads(c["post_raw"]["stdout"])["prereq"], run_celeris_owner="root")))
        case("post_vector", {"post": "NIC vector enp1s0-8 (bench.yml pins it) is not in /proc/interrupts"}, lambda c: post(c, placement_vector_present=False))
        case("post_lan", {"post": "fabric address 192.168.50.199 is not configured"}, lambda c: post(c, lan_ip_present=False))
        case("post_peer", {"post": "no answer from 192.168.50.195 over the fabric"}, lambda c: post(c, peer_ping_ok=False))
        case("post_bond", {"post": "bond lost a slave: 2 up before, 1 up now"},
             lambda c: post(c, bonds=[{"bond": "bond0", "mode": "802.3ad", "slaves": ["enp1s0=up", "enp49s0=down"]}]))
        case("post_counters", {"post": "the counters were not reset"}, lambda c: post(c, max_vector_total=4050000000))
        case("post_failed_unit", {"post": "new failed unit(s): foo.service"}, lambda c: post(c, failed_units=["foo.service"]))
        case("post_one_shot_not_consumed", {"post": "a one-shot is still pending (gnulinux-x) after the host booted: GRUB did not consume it"},
             lambda c: c["post_survey"]["boot"].update(next_entry="gnulinux-x"))
        case("post_one_shot_not_consumed_names_what_a_boot_gives", {"post": "boots the first menu entry of grub (boot.entry0_kernel 7.0.0-38-generic), NOT the saved default (7.0.0-31-generic): only the console brings another kernel back"},
             lambda c: c["post_survey"]["boot"].update(next_entry="gnulinux-x", entry0_kernel="7.0.0-38-generic", default_kernel="7.0.0-31-generic"))
        case("post_no_snapshot", {"post": "no readable snapshot after the reboot"}, lambda c: c["post_raw"].update(stdout=""))
        case("post_nic_driver_changed", {"post": "NIC enp1s0 is bound to r8169, was r8127"},
             lambda c: c["post_survey"]["nics"][0].update(driver="r8169"))
        case("post_nic_speed", {"post": "NIC enp49s0 link speed 1000 Mb/s, was 10000"}, lambda c: c["post_survey"]["nics"][1].update(speed_mbps=1000))
        case("post_nic_gone", {"post": "NIC enp49s0 (r8127) is gone"}, lambda c: c["post_survey"].update(nics=c["post_survey"]["nics"][:1]))
        case("post_time_sync", {"post": "the clock is not synchronized after the reboot"}, lambda c: c["post_survey"]["time_sync"].update(ntp_synchronized=False))
        case("post_time_sync_was_never_on", {}, lambda c: (c["pre_survey"]["time_sync"].update(ntp_synchronized=False),
                                                           c["post_survey"]["time_sync"].update(ntp_synchronized=False)))
        case("post_no_survey", {"post": "no readable survey after the reboot"}, lambda c: c.update(post_survey={}))
        case("post_cc", {"post": "the system C compiler built and ran a program before the upgrade and cannot now"}, lambda c: c["toolcheck"].update(cc_ok=False))
        case("post_tls", {"post": "TLS to github.com (the runner tarball) verified before the upgrade and does not now"}, lambda c: c["toolcheck"].update(https_ok=False))
        case("post_no_compiler_before_or_after", {}, lambda c: (c["pre_toolcheck"].update(cc_ok=False), c["toolcheck"].update(cc_ok=False)))
        case("post_no_tls_before_or_after", {}, lambda c: (c["pre_toolcheck"].update(https_ok=False), c["toolcheck"].update(https_ok=False)))
        case("post_toolcheck_missing", {"post": "the toolchain bootstrap check did not run"}, lambda c: c.update(toolcheck={}))

        cls.out = judge_all({n: c for n, (c, _) in cls.cases.items()})

    def test_every_case(self):
        failures = []
        for name, (_, expect) in self.cases.items():
            got = self.out[name]
            if not expect:
                if self.cases[name][0].get("os_mode", "upgrade") in ("repair", "disarm", "freeze", "thaw"):
                    got = {"maint": got["maint"]}  # a maintenance mode is judged by maint_refusals_str alone
                if any(got.values()):
                    failures.append("%s: expected nothing, got %r" % (name, got))
                continue
            for key, want in expect.items():
                if want not in got[key]:
                    failures.append("%s: %s: expected %r in %r" % (name, key, want, got[key]))
        self.assertEqual(failures, [], "\n" + "\n".join(failures))

    def test_no_verdict_spells_out_the_console_override(self):
        for name, out in self.out.items():
            for k, v in out.items():
                self.assertNotRegex(v, r"REHEARSE-\S*CONSOLE", "%s: %s" % (name, k))

    def test_a_healthy_host_has_nothing_to_report(self):
        self.assertEqual(self.out["healthy"], {"refusals": "", "plan": "", "prereboot": "", "post": "", "maint": ""})

    def test_the_judgement_reads_only_fields_the_scripts_write(self):
        """Every s.<field> / d.<field> / a.<field> of the judgement is a field of the record the script prints."""
        with open(JUDGE) as f:
            text = re.sub(r"(?m)^\s*#.*$", "", f.read())
        survey_keys, dkms_keys = set(), set()

        def walk(o, into):
            if isinstance(o, dict):
                for k, v in o.items():
                    into.add(k)
                    walk(v, into)
            elif isinstance(o, list):
                for v in o:
                    walk(v, into)
        walk(self.survey, survey_keys)
        walk(self.dk, dkms_keys)
        walk(self.sim, survey_keys)
        walk(snapshot("a", "k", 1), survey_keys)
        for var, keys in (("s", survey_keys), ("pk", survey_keys), ("bs", survey_keys), ("d", dkms_keys), ("sim", survey_keys), ("tc", {"cc_ok", "https_ok"} | survey_keys)):
            for m in re.finditer(r"\b%s\.([a-z_0-9]+)" % var, text):
                self.assertIn(m.group(1), keys, "the judgement reads %s.%s, which the script does not write" % (var, m.group(1)))

    def test_a_new_message_needs_a_case(self):
        """The judgement has this many messages and every one has a case above (grouped by verdict below). Adding a
        message without a case makes this fail until the count and the cases are updated together."""
        with open(JUDGE) as f:
            n = len(re.findall(r"p\.append\(", re.sub(r"(?m)^\s*#.*$", "", f.read())))
        self.assertEqual(n, 88, "the judgement has %d messages; add a case for each new one and update this count" % n)
        covered = {"refusals": 0, "plan": 0, "prereboot": 0, "post": 0, "maint": 0}
        for _, (_, expect) in self.cases.items():
            for k in expect:
                covered[k] += 1
        self.assertGreaterEqual(sum(covered.values()), 70)


if __name__ == "__main__":
    if ANSIBLE is None:
        print("SKIP: ansible-playbook not installed")
        sys.exit(0)
    unittest.main(verbosity=1)
