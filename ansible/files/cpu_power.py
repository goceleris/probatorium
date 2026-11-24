#!/usr/bin/env python3
"""cpu_power.py: record, set and restore the CPU frequency governor of ONE host, and record its idle states (python3, stdlib only).

The release runs (benchmark-tier, matrix-race-tier) put the CPU governor on `performance` before the run starts and put it back afterwards; every host of a run is RECORDED (scaling
driver, governor, energy_performance_preference, min/max/cur frequency, boost, cpuidle driver/governor and every idle state with its `disable` flag) before, after the change, before the
restore and after it. Idle states and the scaling limits are NEVER written by a `performance` run (left as found, recorded); the restore writes back only the governor, EPP and idle flags
(a moved scaling limit is not written: it fails the compare). Staged by ansible/tasks/cpu_power.yml onto tmpfs; read-only modes (record, summary, compare, gate, stop-rule) need no
privilege, apply needs root (ansible `become: true`). The celeris-stress cluster host job is NOT wired.

  cpu_power.py record  --out F [--tag T] [--label L] [--samples N] [--gap S] [--once]
        full record of the host: per CPU the scaling driver, governor, available governors, energy_performance_preference, scaling_{min,max,cur}_freq and the cpuidle states
        (name, disable, latency, residency, usage, time); host-wide boost / no_turbo / pstate status / cpuidle driver and governor; N frequency samples S seconds apart
        (scaling_cur_freq and /proc/cpuinfo MHz of every CPU; default 3 samples, 1 s apart). --once refuses to overwrite F (the ORIGINAL record is write-once).
        A host without cpufreq (no governor to set) is recorded with `has_cpufreq: false` and its cpuidle states; it is not an error.
  cpu_power.py apply   --arm ARM --orig F [--out R]   ARM = performance | as-found. Moves the host to the arm's state FROM THE ORIGINAL RECORD (never from the current state):
        as-found = every recorded value written back (governor, then EPP, then idle `disable` flags); performance = governor `performance` on every CPU that has a
        governor (EPP `performance` where the file exists, a warning when the kernel refuses it), every other recorded value (idle flags, limits) untouched. A value that already equals
        the wanted one is NOT written (a host already on performance gets zero writes). Every value is read back; exit 1 when one did not take. The result JSON R (ok, errors, warnings,
        expected, got, changed_writes) is written either way.
  cpu_power.py compare --a F --b G    exit 0 when the settable state (driver, governor, EPP, scaling min/max, idle flags by state name, host-wide settings) of the two records is equal
  cpu_power.py gate    --dir D [--governor performance|keep] [--hosts h1,h2]   the reader's hard gate on a run's results/cpu-power directory D: restore-status.txt is RESTORED-EQUAL
        (keep: KEPT-EQUAL), every host has orig / final records and they compare EQUAL (re-computed here, not taken from the status text), for performance every CPU that has a governor was
        on `performance` in the applied record AND in the `end` record (taken just before the restore, same boot id as the original) with its idle flags as found, --hosts (when given) names exactly the recorded hosts; prints one line per host and label; exit 0 only when all hold
  cpu_power.py stop-rule --orig F     exit 0 when NO CPU of the record is on the performance governor, else 3: a host whose known as-found state is not performance (msa2-server, msa2-client)
        that is ALREADY on performance was left changed by something else (a previous run that did not restore): the set step refuses it instead of taking it as the original
  cpu_power.py summary --rec F        one line: what the host runs on (driver, governor, EPP, boost, pstate, cpuidle driver/governor, idle states and their disable flags, frequency range)

Frequencies, usage, time and MHz are measurements, never part of the state that is compared. Test hooks: CPU_SYSFS (default /sys/devices/system/cpu), CPUINFO (default /proc/cpuinfo).
Exit codes: 0 ok, 1 a setting did not take / states differ, 2 usage or unreadable record.
"""
import json
import os
import re
import socket
import sys
import time

sys.dont_write_bytecode = True
ARMS = ("performance", "as-found")


def sysfs():
    return os.environ.get("CPU_SYSFS", "/sys/devices/system/cpu")


def cpuinfo_path():
    return os.environ.get("CPUINFO", "/proc/cpuinfo")


def rd(path, default=None):
    try:
        with open(path) as f:
            return f.read().strip()
    except OSError:
        return default


def rdint(path):
    v = rd(path)
    try:
        return int(v)
    except (TypeError, ValueError):
        return None


def wr(path, value):
    """write VALUE to a sysfs file; None on success, the error text otherwise"""
    try:
        with open(path, "w") as f:
            f.write(str(value))
        return None
    except OSError as e:
        return "%s: %s" % (path, e.strerror or e)


def cpu_ids():
    """the CPUs that have a cpufreq or a cpuidle directory (an offline CPU has neither and is not part of any record), in numeric order"""
    out = []
    try:
        names = os.listdir(sysfs())
    except OSError:
        return out
    for n in names:
        m = re.fullmatch(r"cpu(\d+)", n)
        if m and (os.path.isdir(os.path.join(sysfs(), n, "cpufreq")) or os.path.isdir(os.path.join(sysfs(), n, "cpuidle"))):
            out.append(int(m.group(1)))
    return sorted(out)


def idle_states(c):
    d = os.path.join(sysfs(), "cpu%d" % c, "cpuidle")
    out = []
    try:
        names = os.listdir(d)
    except OSError:
        return out
    keyed = []
    for n in names:
        m = re.fullmatch(r"state(\d+)", n)
        if m:
            keyed.append((int(m.group(1)), n))
    for _, n in sorted(keyed):
        p = os.path.join(d, n)
        out.append({"state": n, "name": rd(p + "/name", ""), "disable": rdint(p + "/disable"), "latency": rdint(p + "/latency"),
                    "residency": rdint(p + "/residency"), "usage": rdint(p + "/usage"), "time": rdint(p + "/time")})
    return out


def cpu_entry(c):
    d = os.path.join(sysfs(), "cpu%d" % c, "cpufreq")
    return {"driver": rd(d + "/scaling_driver", "-"), "governor": rd(d + "/scaling_governor", "-"),
            "available_governors": (rd(d + "/scaling_available_governors", "") or "").split(),
            "epp": rd(d + "/energy_performance_preference"), "epp_available": (rd(d + "/energy_performance_available_preferences", "") or "").split(),
            "min_khz": rdint(d + "/scaling_min_freq"), "max_khz": rdint(d + "/scaling_max_freq"),
            "idle": idle_states(c)}


def host_wide():
    s = sysfs()
    return {"cpufreq_boost": rd(s + "/cpufreq/boost"), "intel_pstate_no_turbo": rd(s + "/intel_pstate/no_turbo"),
            "intel_pstate_status": rd(s + "/intel_pstate/status"), "amd_pstate_status": rd(s + "/amd_pstate/status"),
            "cpuidle_driver": rd(s + "/cpuidle/current_driver"), "cpuidle_governor": rd(s + "/cpuidle/current_governor")}


def cpuinfo_mhz():
    out = []
    try:
        with open(cpuinfo_path()) as f:
            for line in f:
                if line.startswith("cpu MHz"):
                    try:
                        out.append(float(line.split(":", 1)[1]))
                    except ValueError:
                        pass
    except OSError:
        pass
    return out


def freq_sample(cpus):
    return {"epoch": round(time.time(), 3),
            "scaling_cur_khz": [rdint(os.path.join(sysfs(), "cpu%d" % c, "cpufreq", "scaling_cur_freq")) for c in cpus],
            "cpuinfo_mhz": cpuinfo_mhz()}


def record(tag="", label="", samples=3, gap=1.0):
    cpus = cpu_ids()
    rec = {"schema": 2, "host": socket.gethostname(), "tag": tag, "label": label, "epoch": round(time.time(), 3),
           "iso": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "kernel": rd("/proc/sys/kernel/osrelease", ""), "boot_id": rd("/proc/sys/kernel/random/boot_id", ""),
           "global": host_wide(), "cpus": {"cpu%d" % c: cpu_entry(c) for c in cpus}, "freq_samples": []}
    rec["has_cpufreq"] = any(e["governor"] != "-" for e in rec["cpus"].values())
    for i in range(max(1, samples)):
        if i:
            time.sleep(gap)
        rec["freq_samples"].append(freq_sample(cpus))
    return rec


def state_view(rec):
    """the compared part of a record: what a setting can change, never a measurement (frequency, usage, time, MHz)"""
    cpus = {}
    for name, e in sorted(rec.get("cpus", {}).items()):
        cpus[name] = {"driver": e.get("driver"), "governor": e.get("governor"), "epp": e.get("epp"),
                      "min_khz": e.get("min_khz"), "max_khz": e.get("max_khz"),
                      "idle": {s.get("name"): s.get("disable") for s in e.get("idle", [])}}
    return {"global": rec.get("global", {}), "cpus": cpus}


def diff_views(a, b):
    out = []
    ga, gb = a.get("global", {}), b.get("global", {})
    for k in sorted(set(ga) | set(gb)):
        if ga.get(k) != gb.get(k):
            out.append("global.%s: %r -> %r" % (k, ga.get(k), gb.get(k)))
    ca, cb = a.get("cpus", {}), b.get("cpus", {})
    for c in sorted(set(ca) | set(cb), key=lambda n: int(n[3:])):
        if c not in ca or c not in cb:
            out.append("%s: present in only one of the two records" % c)
            continue
        for k in ("driver", "governor", "epp", "min_khz", "max_khz"):
            if ca[c][k] != cb[c][k]:
                out.append("%s.%s: %r -> %r" % (c, k, ca[c][k], cb[c][k]))
        ia, ib = ca[c]["idle"], cb[c]["idle"]
        for s in sorted(set(ia) | set(ib), key=str):
            if ia.get(s) != ib.get(s):
                out.append("%s.idle[%s].disable: %r -> %r" % (c, s, ia.get(s), ib.get(s)))
    return out


def load(path):
    try:
        with open(path) as f:
            d = json.load(f)
    except (OSError, ValueError) as e:
        print("cpu_power: cannot read the record %s: %s" % (path, e), file=sys.stderr)
        sys.exit(2)
    if not isinstance(d, dict) or not isinstance(d.get("cpus"), dict) or not isinstance(d.get("global"), dict):
        print("cpu_power: %s is not a cpu_power record" % path, file=sys.stderr)
        sys.exit(2)
    return d


def dump(path, obj):
    os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        json.dump(obj, f, indent=1, sort_keys=True)
        f.write("\n")
    os.replace(tmp, path)


def expected_state(orig, arm):
    """the state the host must be in after ARM, derived from the original record; also the EPP values a performance arm wants (a warning when they do not take)"""
    view = json.loads(json.dumps(state_view(orig)))
    if arm == "as-found":
        return view, {}
    epp_wanted = {}
    for name, e in orig["cpus"].items():
        if e.get("governor") in (None, "-"):
            continue  # no cpufreq on this CPU: no governor to set
        view["cpus"][name]["governor"] = "performance"
        if e.get("epp") is not None:
            epp_wanted[name] = "performance"
            view["cpus"][name]["epp"] = "performance"
    return view, epp_wanted


def apply_arm(arm, orig):
    """returns the result dict (ok, errors, warnings, expected, got, changed_writes); never raises on a refused write"""
    errors, warnings, changed = [], [], 0
    cpus = [int(n[3:]) for n in sorted(orig["cpus"], key=lambda n: int(n[3:]))]
    online = cpu_ids()
    if cpus != online:
        errors.append("the CPUs with cpufreq/cpuidle differ from the original record (record %d, now %d): an offline or hot-plugged CPU" % (len(cpus), len(online)))
    want, epp_perf = expected_state(orig, arm)
    # 1. governor first, then EPP (writable again under powersave on a restore), then (restore only) the idle flags; the scaling limits are never written
    for c in cpus:
        name = "cpu%d" % c
        d = os.path.join(sysfs(), name, "cpufreq")
        g = want["cpus"][name]["governor"]
        if g in (None, "-") or not os.path.isdir(d):
            continue
        if arm != "as-found" and g not in cpu_entry(c)["available_governors"]:
            errors.append("%s: governor %s is not in scaling_available_governors" % (name, g))
            continue
        if rd(d + "/scaling_governor") != g:
            err = wr(d + "/scaling_governor", g)
            changed += 1
            if err:
                errors.append("write failed " + err)
    for c in cpus:
        name = "cpu%d" % c
        epp_file = os.path.join(sysfs(), name, "cpufreq", "energy_performance_preference")
        if not os.path.exists(epp_file):
            continue
        w = epp_perf.get(name, orig["cpus"][name].get("epp"))
        if w is None or rd(epp_file) == w:
            continue
        err = wr(epp_file, w)
        changed += 1
        if err:
            # under the performance governor the kernel may refuse an EPP write (it pins the hint): a warning, never for the as-found restore
            (errors if arm == "as-found" else warnings).append("EPP write failed " + err)
    for c in cpus if arm == "as-found" else []:  # a performance run NEVER writes an idle state (left as found); a flag that moved meanwhile fails the read-back below
        name = "cpu%d" % c
        have = {s["name"]: s for s in idle_states(c)}
        for s, v in want["cpus"][name]["idle"].items():
            if s in have and have[s]["disable"] != v and v is not None:
                err = wr(os.path.join(sysfs(), name, "cpuidle", have[s]["state"], "disable"), v)
                changed += 1
                if err:
                    errors.append("write failed " + err)
    # 2. read back everything and compare with the expected state
    now = {"global": host_wide(), "cpus": {"cpu%d" % c: cpu_entry(c) for c in cpu_ids()}}
    got = state_view(now)
    for line in diff_views(want, got):
        if arm != "as-found" and re.match(r"cpu\d+\.epp:", line):
            warnings.append("EPP did not take: " + line)  # warning only (see above); the record shows what it is
        else:
            errors.append("read-back differs: " + line)
    return {"ok": not errors, "arm": arm, "errors": errors, "warnings": warnings, "changed_writes": changed, "epoch": round(time.time(), 3),
            "expected": want, "got": got}


def cmd_record(a):
    out = a["--out"]
    if "--once" in a and os.path.exists(out):
        print("cpu_power: %s exists: the original record is write-once" % out, file=sys.stderr)
        return 1
    rec = record(a.get("--tag", ""), a.get("--label", ""), int(a.get("--samples", 3)), float(a.get("--gap", 1.0)))
    dump(out, rec)
    print("cpu_power: recorded %d CPUs (cpufreq: %s) -> %s" % (len(rec["cpus"]), "yes" if rec["has_cpufreq"] else "NO", out))
    return 0


def uniq(vals):
    seen = {}
    for v in vals:
        seen[v] = seen.get(v, 0) + 1
    return "+".join("%s=%d" % (k if k is not None else "none", n) for k, n in sorted(seen.items(), key=lambda kv: str(kv[0])))


def summary(rec):
    cpus = rec["cpus"]
    g = rec.get("global", {})
    if not cpus:
        return "no cpufreq and no cpuidle directory on this host (cpuidle driver %s)" % g.get("cpuidle_driver")
    idle = {}
    for e in cpus.values():
        for s in e.get("idle", []):
            idle.setdefault(s["name"], []).append(s["disable"])
    idle_txt = ",".join("%s(%s)" % (n, "enabled" if set(v) == {0} else ("disabled" if set(v) == {1} else "mixed")) for n, v in idle.items()) or "none"
    mx = [e["max_khz"] for e in cpus.values() if e.get("max_khz") is not None]
    mn = [e["min_khz"] for e in cpus.values() if e.get("min_khz") is not None]
    cur = [v for s in rec.get("freq_samples", [])[-1:] for v in s.get("scaling_cur_khz", []) if v is not None]
    return ("cpus=%d driver=%s governor=%s epp=%s boost=%s pstate=%s limits=%s-%s kHz cur=%s-%s kHz cpuidle=%s/%s idle_states=%s"
            % (len(cpus), uniq(e.get("driver") for e in cpus.values()), uniq(e.get("governor") for e in cpus.values()), uniq(e.get("epp") for e in cpus.values()),
               g.get("cpufreq_boost") if g.get("cpufreq_boost") is not None else (("no_turbo=" + g["intel_pstate_no_turbo"]) if g.get("intel_pstate_no_turbo") is not None else "n/a"),
               g.get("amd_pstate_status") or g.get("intel_pstate_status") or "n/a", min(mn) if mn else "n/a", max(mx) if mx else "n/a",
               min(cur) if cur else "n/a", max(cur) if cur else "n/a", g.get("cpuidle_driver"), g.get("cpuidle_governor"), idle_txt))


def cmd_gate(a):
    d = a["--dir"]
    want = a.get("--governor", "performance")
    bad = []
    try:
        with open(os.path.join(d, "restore-status.txt")) as f:
            status = f.read().strip()
    except OSError:
        status = ""
    need = "RESTORED-EQUAL" if want == "performance" else "KEPT-EQUAL"
    print("gate: restore-status: %s" % (status or "MISSING"))
    if not status.startswith(need):
        bad.append("restore-status is not %s" % need)
    try:
        hosts = sorted(n[:-5] for n in os.listdir(os.path.join(d, "orig")) if n.endswith(".json"))
    except OSError:
        hosts = []
    if not hosts:
        bad.append("no orig record")
    if "--hosts" in a and sorted(a["--hosts"].split(",")) != hosts:
        bad.append("recorded hosts %s != expected %s" % (hosts, sorted(a["--hosts"].split(","))))
    for h in hosts:
        recs = {}
        for label in ("orig", "applied", "end", "final"):
            path = os.path.join(d, label, h + ".json")
            if label in ("applied", "end") and want == "keep":
                continue
            if not os.path.exists(path):
                bad.append("%s: no %s record" % (h, label))
                continue
            recs[label] = load(path)
            print("gate: %s %s: %s" % (h, label, summary(recs[label])))
        if "orig" in recs and "final" in recs:
            diff = diff_views(state_view(recs["orig"]), state_view(recs["final"]))
            if diff:
                bad.append("%s: final differs from orig: %s" % (h, "; ".join(diff[:5])))
        if want == "performance" and "orig" in recs and "applied" in recs:
            for name, e in recs["applied"]["cpus"].items():
                if e.get("governor") not in ("performance", "-"):
                    bad.append("%s.%s: applied governor is %s, not performance" % (h, name, e.get("governor")))
            ia = {n: {s["name"]: s["disable"] for s in e.get("idle", [])} for n, e in recs["applied"]["cpus"].items()}
            io = {n: {s["name"]: s["disable"] for s in e.get("idle", [])} for n, e in recs["orig"]["cpus"].items()}
            if ia != io:
                bad.append("%s: the idle flags of the applied record differ from the original (idle states must be left as found)" % h)
            if set(recs["applied"]["cpus"]) != set(recs["orig"]["cpus"]):
                bad.append("%s: the applied record has other CPUs than the original" % h)
        if want == "performance" and "orig" in recs and "end" in recs:
            for name, e in recs["end"]["cpus"].items():
                if e.get("governor") not in ("performance", "-"):
                    bad.append("%s.%s: the governor was %s just before the restore, not performance (it did not hold for the whole run)" % (h, name, e.get("governor")))
            ie = {n: {s["name"]: s["disable"] for s in e.get("idle", [])} for n, e in recs["end"]["cpus"].items()}
            io = {n: {s["name"]: s["disable"] for s in e.get("idle", [])} for n, e in recs["orig"]["cpus"].items()}
            if ie != io:
                bad.append("%s: the idle flags just before the restore differ from the original" % h)
            if recs["orig"].get("boot_id") and recs["end"].get("boot_id") != recs["orig"].get("boot_id"):
                bad.append("%s: the boot id changed during the run (the host rebooted; the governor of the run is not proven)" % h)
    for b in bad:
        print("gate: FAIL: " + b)
    print("gate: %s" % ("OK" if not bad else "FAILED (%d)" % len(bad)))
    return 0 if not bad else 1


def parse(argv):
    a, i = {}, 0
    while i < len(argv):
        t = argv[i]
        if t == "--once":
            a[t] = True
            i += 1
        elif t.startswith("--") and i + 1 < len(argv):
            a[t] = argv[i + 1]
            i += 2
        else:
            print(__doc__)
            sys.exit(2)
    return a


def main(argv):
    if len(argv) < 1:
        print(__doc__)
        return 2
    mode, a = argv[0], parse(argv[1:])
    if mode == "record":
        if "--out" not in a:
            print(__doc__)
            return 2
        return cmd_record(a)
    if mode == "apply":
        if a.get("--arm") not in ARMS or "--orig" not in a:
            print(__doc__)
            return 2
        res = apply_arm(a["--arm"], load(a["--orig"]))
        if "--out" in a:
            dump(a["--out"], res)
        for line in res["warnings"]:
            print("cpu_power: warning: " + line)
        for line in res["errors"]:
            print("cpu_power: ERROR: " + line)
        print("cpu_power: arm %s %s (%d writes, %d CPUs)" % (a["--arm"], "ok, read back" if res["ok"] else "FAILED", res["changed_writes"], len(res["got"]["cpus"])))
        return 0 if res["ok"] else 1
    if mode == "compare":
        if "--a" not in a or "--b" not in a:
            print(__doc__)
            return 2
        d = diff_views(state_view(load(a["--a"])), state_view(load(a["--b"])))
        for line in d:
            print("cpu_power: differs: " + line)
        print("cpu_power: compare %s" % ("EQUAL" if not d else "%d difference(s)" % len(d)))
        return 0 if not d else 1
    if mode == "stop-rule":
        if "--orig" not in a:
            print(__doc__)
            return 2
        rec = load(a["--orig"])
        perf = sorted(n for n, e in rec["cpus"].items() if e.get("governor") == "performance")
        if perf:
            print("cpu_power: STOP RULE: %d of %d CPUs are already on the performance governor as found" % (len(perf), len(rec["cpus"])))
            return 3
        print("cpu_power: stop rule ok: governors as found = %s" % ",".join(sorted({str(e.get("governor")) for e in rec["cpus"].values()})))
        return 0
    if mode == "gate":
        if "--dir" not in a or a.get("--governor", "performance") not in ("performance", "keep"):
            print(__doc__)
            return 2
        return cmd_gate(a)
    if mode == "summary":
        if "--rec" not in a:
            print(__doc__)
            return 2
        print(summary(load(a["--rec"])))
        return 0
    print(__doc__)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
