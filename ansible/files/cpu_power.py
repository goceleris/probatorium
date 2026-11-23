#!/usr/bin/env python3
"""cpu_power.py: record, change and restore the CPU frequency governor and the deep idle states of ONE host (python3, stdlib only).

GOVAB (THROWAWAY ab/gov-ab-20261122, not for merge): the A/B of the CPU power state on msa2-server and msa2-client. Staged by ansible/tasks/cpu_power.yml into
/run/celeris (tmpfs, nothing persistent); read-only modes (record, snap, log) need no privilege, apply needs root (ansible `become: true`).

  cpu_power.py record  --out F [--tag T] [--label L] [--samples N] [--gap S] [--once]
        full record: per CPU scaling_driver, scaling_governor, available governors, energy_performance_preference, scaling_{min,max,cur}_freq, the cpuidle states
        (name, disable, latency, residency, usage, time), the host-wide boost / no_turbo / pstate status / cpuidle driver+governor, and N frequency samples S seconds
        apart (scaling_cur_freq and /proc/cpuinfo MHz of every CPU at each instant; default 3 samples, 1 s apart). --once refuses to overwrite F (the ORIGINAL record).
  cpu_power.py snap    --out F          the same record with ONE frequency sample (the per-cell snap)
  cpu_power.py log     --out F [--seconds N] [--interval S]   one JSON line per interval: epoch, scaling_cur_freq and cpuinfo MHz min/med/max, governor summary
  cpu_power.py apply   --arm ARM --orig F [--out R]   ARM = as-found | performance | performance-c2off. Moves the host to the arm's state FROM THE ORIGINAL RECORD
        (never from the current state): as-found = every recorded value (governor, EPP, idle `disable` flags) written back; performance = recorded idle flags, governor
        performance on every CPU, EPP performance where the file exists; performance-c2off = performance + `disable=1` on every cpuidle state NAMED C2 or C3 (POLL and C1 stay).
        Every change is read back; exit 1 when any value did not take (the caller runs no benchmark). The result JSON R (ok, errors, warnings, expected, got) is written either way.
  cpu_power.py compare --a F --b G      exit 0 when the mutable state (governor, driver, EPP, idle flags by state name, host-wide settings) is equal; prints the differences
  cpu_power.py stop-rule --orig F       exit 0 when NO CPU of the record is on the performance governor (the as-found state of the contrast), else 3

Frequencies, usage, time and MHz are measurements, never part of the state that is compared. Test hooks: CPU_SYSFS (default /sys/devices/system/cpu), CPUINFO (default /proc/cpuinfo).
Exit codes: 0 ok, 1 a setting did not take / states differ, 2 usage or unreadable record, 3 stop rule.
"""
import json
import os
import re
import socket
import sys
import time

sys.dont_write_bytecode = True
ARMS = ("as-found", "performance", "performance-c2off")
DEEP = ("C2", "C3")


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
    """the CPUs that have a cpufreq directory (an offline CPU has none and is not part of any record), in numeric order"""
    out = []
    try:
        names = os.listdir(sysfs())
    except OSError:
        return out
    for n in names:
        m = re.fullmatch(r"cpu(\d+)", n)
        if m and os.path.isdir(os.path.join(sysfs(), n, "cpufreq")):
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
    rec = {"schema": 1, "host": socket.gethostname(), "tag": tag, "label": label, "epoch": round(time.time(), 3),
           "iso": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "kernel": rd("/proc/sys/kernel/osrelease", ""),
           "global": host_wide(), "cpus": {"cpu%d" % c: cpu_entry(c) for c in cpus}, "freq_samples": []}
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
        for k in ("driver", "governor", "epp"):
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
    if not isinstance(d, dict) or "cpus" not in d or not d["cpus"]:
        print("cpu_power: %s is not a record with CPUs" % path, file=sys.stderr)
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
    """the state the host must be in after ARM: a state_view derived from the original record"""
    view = json.loads(json.dumps(state_view(orig)))
    if arm == "as-found":
        return view, {}
    epp_wanted = {}
    for name, e in orig["cpus"].items():
        view["cpus"][name]["governor"] = "performance"
        if e.get("epp") is not None:
            epp_wanted[name] = "performance"
            view["cpus"][name]["epp"] = "performance"
        if arm == "performance-c2off":
            for s in view["cpus"][name]["idle"]:
                if s in DEEP:
                    view["cpus"][name]["idle"][s] = 1
    return view, epp_wanted


def apply_arm(arm, orig):
    """returns the result dict (ok, errors, warnings, expected, got, changed); never raises on a refused write"""
    errors, warnings, changed = [], [], 0
    cpus = [int(n[3:]) for n in sorted(orig["cpus"], key=lambda n: int(n[3:]))]
    online = cpu_ids()
    if cpus != online:
        errors.append("the CPUs with cpufreq differ from the original record (record %d, now %d): an offline or hot-plugged CPU" % (len(cpus), len(online)))
    want, epp_perf = expected_state(orig, arm)
    # 1. governor, then EPP, then idle flags (the restore order of the preview governor: governor first, EPP after it)
    for c in cpus:
        name = "cpu%d" % c
        d = os.path.join(sysfs(), name, "cpufreq")
        if not os.path.isdir(d):
            continue
        g = want["cpus"][name]["governor"]
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
    for c in cpus:
        name = "cpu%d" % c
        have = {s["name"]: s for s in idle_states(c)}
        for s, v in want["cpus"][name]["idle"].items():
            if s in have and have[s]["disable"] != v and v is not None:
                err = wr(os.path.join(sysfs(), name, "cpuidle", have[s]["state"], "disable"), v)
                changed += 1
                if err:
                    errors.append("write failed " + err)
        if arm == "performance-c2off" and not any(s in DEEP for s in have):
            errors.append("%s: no cpuidle state named %s: nothing to disable (states: %s)" % (name, "/".join(DEEP), ",".join(have) or "none"))
    # 2. read back everything and compare with the expected state
    now = {"global": host_wide(), "cpus": {"cpu%d" % c: cpu_entry(c) for c in cpu_ids()}}
    got = state_view(now)
    for line in diff_views(want, got):
        if arm != "as-found" and re.match(r"cpu\d+\.epp:", line):
            warnings.append("EPP did not take: " + line)  # warning only (see above); the reader reports it per host
        else:
            errors.append("read-back differs: " + line)
    return {"ok": not errors, "arm": arm, "errors": errors, "warnings": warnings, "changed_writes": changed, "epoch": round(time.time(), 3),
            "expected": want, "got": got}


def cmd_record(a, samples):
    out = a["--out"]
    if "--once" in a and os.path.exists(out):
        print("cpu_power: %s exists: the original record is write-once" % out, file=sys.stderr)
        return 1
    rec = record(a.get("--tag", ""), a.get("--label", ""), samples, float(a.get("--gap", 1.0)))
    if not rec["cpus"]:
        print("cpu_power: no cpufreq on this host", file=sys.stderr)
        return 1
    dump(out, rec)
    print("cpu_power: recorded %d CPUs -> %s" % (len(rec["cpus"]), out))
    return 0


def med(xs):
    s = sorted(xs)
    n = len(s)
    return (s[n // 2] if n % 2 else (s[n // 2 - 1] + s[n // 2]) / 2.0) if n else None


def cmd_log(a):
    out = a["--out"]
    secs = float(a.get("--seconds", 3600))
    iv = float(a.get("--interval", 1.0))
    try:  # never preempt the SUT: like the fine host sampler
        os.sched_setscheduler(0, os.SCHED_IDLE, os.sched_param(0))
    except (AttributeError, OSError):
        pass
    cpus = cpu_ids()
    end = time.time() + secs
    with open(out, "a") as f:
        while time.time() < end:
            t0 = time.time()
            khz = [v for v in (rdint(os.path.join(sysfs(), "cpu%d" % c, "cpufreq", "scaling_cur_freq")) for c in cpus) if v is not None]
            mhz = cpuinfo_mhz()
            gov = {}
            for c in cpus:
                g = rd(os.path.join(sysfs(), "cpu%d" % c, "cpufreq", "scaling_governor"), "-")
                gov[g] = gov.get(g, 0) + 1
            f.write(json.dumps({"epoch": round(t0, 3), "khz_min": min(khz) if khz else None, "khz_med": med(khz), "khz_max": max(khz) if khz else None,
                                "mhz_min": min(mhz) if mhz else None, "mhz_med": med(mhz), "mhz_max": max(mhz) if mhz else None,
                                "governor": "+".join("%s=%d" % kv for kv in sorted(gov.items()))}, sort_keys=True) + "\n")
            f.flush()
            time.sleep(max(0.0, iv - (time.time() - t0)))
    return 0


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
    if mode in ("record", "snap", "log") and "--out" not in a:
        print(__doc__)
        return 2
    if mode == "record":
        return cmd_record(a, int(a.get("--samples", 3)))
    if mode == "snap":
        return cmd_record(a, 1)
    if mode == "log":
        return cmd_log(a)
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
        print("cpu_power: stop rule ok: governors as found = %s" % ",".join(sorted({e.get("governor") for e in rec["cpus"].values()})))
        return 0
    print(__doc__)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
