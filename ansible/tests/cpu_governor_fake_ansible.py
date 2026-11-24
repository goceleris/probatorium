#!/usr/bin/env python3
"""A stand-in for `ansible-playbook -i inventory.yml cpu-power.yml ...` used by test_cpu_governor.sh: it models the PLAYBOOK PROTOCOL of ansible/tasks/cpu_power.yml per host
(record: orig is write-once and fetched to <dir>/<label>/<host>.json; apply: needs the controller's original, runs the REAL cpu_power.py against that host's fake sysfs, fetches the
result and the record even when it failed, then fails; restore = apply as-found; unstage = no-op; a host without an original is skipped on restore) so the real
.github/scripts/cpu-governor.sh runs end to end. Per host fake sysfs: $FAKE_SYS_ROOT/<host>/cpu (CPU_SYSFS), cpuinfo next to it. $FAKE_ANSIBLE_LOG gets one line per call.
FAKE_RESTORE_NOOP=1: the restore does nothing but still records and exits 0 (a restore that silently did not restore)."""
import json
import os
import shutil
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
PY = os.path.join(HERE, "..", "files", "cpu_power.py")
ALL = ["msa2-server", "msr1", "msa2-client"]


def main(argv):
    ev, limit, i = {}, None, 0
    while i < len(argv):
        if argv[i] == "-e":
            k, _, v = argv[i + 1].partition("=")
            ev[k] = v
            i += 2
        elif argv[i] == "--limit":
            limit = argv[i + 1].split(",")
            i += 2
        else:
            i += 1
    with open(os.environ["FAKE_ANSIBLE_LOG"], "a") as f:
        f.write("%s %s %s limit=%s\n" % (ev.get("cpu_power_action"), ev.get("cpu_power_label"), ev.get("cpu_power_arm", ""), ",".join(limit) if limit else "-"))
    action, label, d = ev["cpu_power_action"], ev["cpu_power_label"], ev["cpu_power_dir"]
    rc = 0
    for h in (limit or ALL):
        root = os.path.join(os.environ["FAKE_SYS_ROOT"], h)
        env = dict(os.environ, CPU_SYSFS=root + "/cpu", CPUINFO=root + "/cpuinfo")
        orig = "%s/orig/%s.json" % (d, h)
        host_tmp = os.path.join(os.environ["FAKE_SYS_ROOT"], h + ".run")
        os.makedirs(host_tmp, exist_ok=True)

        def py(*a):
            return subprocess.run([sys.executable, "-I", "-B", PY] + list(a), env=env, capture_output=True, text=True)

        def fetch(src, dst):
            os.makedirs(os.path.dirname(dst), exist_ok=True)
            if os.path.exists(src):
                shutil.copy(src, dst)

        if action == "unstage":
            continue
        if action == "record":
            if label == "orig" and os.path.exists(orig):
                print("%s: the original record exists: write-once" % h)
                rc = 2
                continue
            p = py("record", "--out", host_tmp + "/rec.json", "--tag", ev.get("cpu_power_tag", ""), "--label", label, "--samples", "1", "--gap", "0", *(["--once"] if label == "orig" else []))
            sys.stdout.write(p.stdout + p.stderr)
            if p.returncode:
                rc = 2
                continue
            fetch(host_tmp + "/rec.json", "%s/%s/%s.json" % (d, label, h))
        elif action in ("apply", "restore"):
            if not os.path.exists(orig):
                if action == "restore":
                    print("%s: no original record: nothing to restore" % h)
                    continue
                print("%s: no original record" % h)
                rc = 2
                continue
            arm = "as-found" if action == "restore" else ev["cpu_power_arm"]
            res = host_tmp + "/res.json"
            if action == "restore" and os.environ.get("FAKE_RESTORE_NOOP") == "1":
                p = subprocess.CompletedProcess([], 0, "restore skipped by the stand-in\n", "")
            else:
                p = py("apply", "--arm", arm, "--orig", orig, "--out", res)
            sys.stdout.write("%s: %s" % (h, p.stdout + p.stderr))
            fetch(res, "%s/%s/%s.apply.json" % (d, label, h))
            py("record", "--out", host_tmp + "/rec2.json", "--tag", ev.get("cpu_power_tag", ""), "--label", label, "--samples", "1", "--gap", "0")
            fetch(host_tmp + "/rec2.json", "%s/%s/%s.json" % (d, label, h))
            if p.returncode:
                rc = 2
    return rc


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
