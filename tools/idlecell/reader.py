#!/usr/bin/env python3
"""The reader of the bare-metal idle cell (tools/idlecell, idle-cell-baremetal.yml): turns the raw logs of a run
into the tables of the celeris#1485 decision. Builtins and a few stdlib modules (re, os, glob, math, sys) only; run as `python3 -I reader.py ...`.

  reader.py stage <a|b|c> <out>        the table of one stage (what the host job uploads when the stage ends)
  reader.py report <out>               the facts, every stage that ran, the data-quality lines (the collect job)
  reader.py gate <dir> [--alias a=b,..] [--prefix iouring-] [--n 100,1000]
                                       the gate tables of ONE raw directory (also reads the laptop-VM raw data of m1485)
  reader.py empty <dir>                the paired first-request cell on an empty server: old build against main
  reader.py held <dir> <A> <B>         the paired first-request cell with 1,000 held: A against B (labels of p<k>-<label>)

The raw layout is the one of the phase-1 harness: <tag>.srv.txt (the tool's IDLE/SCHED2/ENERGY lines), <tag>.perf.txt
(perf stat -x, of the server process), <tag>.pe.txt (perf stat -x, of the package energy), INDEX.tsv (one line per cell).
"""
import glob
import os
import re
import sys
from math import comb

# ---------------------------------------------------------------- the decision rule (the bot's ruling, 2026-11-27)
# No fixed tolerance decides anything: the tolerance is the main-against-main twin's spread (see decide). These are the floors used ONLY when
# the twin's spread is 0 (identical figures in every pass), and are printed when used.
TOL_CPU_FLOOR = 0.05  # CPU is counted in clock ticks of 10 ms over a 20 s window: 0.05 point is one tick
INS_FLOOR_REL = 0.01  # instructions per second: 1 % of main's
FLOOR_W = 0.05        # package power, W
LOW_N = (100, 1000)  # "loss reproduced at 100 to 1,000"
EXPECT_NS = (100, 1000, 10000, 50000)  # the counts of stage a
VARIANTS = ("main", "head", "head-aw", "main-skip", "main-twin")


def kv(line):
    out = {}
    for m in re.finditer(r'(\w+)=(\[[^\]]*\]|"[^"]*"|\S+)', line):
        out[m.group(1)] = m.group(2).strip('"')
    return out


def read(path):
    try:
        with open(path, errors="replace") as f:
            return f.read()
    except OSError:
        return None


def num(s):
    try:
        return float(s)
    except (TypeError, ValueError):
        return None


def dur_ms(s):
    m = re.fullmatch(r"([\d.]+)(ns|µs|us|ms|s)", s)
    if not m:
        return None
    v, u = float(m.group(1)), m.group(2)
    return v / 1e6 if u == "ns" else v / 1e3 if u in ("µs", "us") else v if u == "ms" else v * 1e3


def med(v):
    """median, by hand: the reader runs on the host, whose python3 may be a minimal one without the statistics module"""
    x = sorted(v)
    n = len(x)
    return x[n // 2] if n % 2 else (x[n // 2 - 1] + x[n // 2]) / 2


def mean(v):
    return sum(v) / len(v)


def rng2(v, fmt="%.2f"):
    return ("%s [%s..%s]" % (fmt % med(v), fmt % min(v), fmt % max(v))) if v else "-"


def sign_p(k, n):
    """two-sided exact binomial sign test, ties already dropped"""
    if n == 0:
        return 1.0
    k2 = min(k, n - k)
    return min(1.0, 2 * sum(comb(n, i) for i in range(k2 + 1)) / 2 ** n)


# ---------------------------------------------------------------- INDEX
def read_index(d):
    rows = {}
    t = read(os.path.join(d, "INDEX.tsv"))
    for line in (t or "").splitlines():
        f = line.split("\t")
        if len(f) < 5:
            continue
        r = {"time": f[0], "tag": f[1], "kind": f[2], "variant": f[3], "param": f[4]}
        for x in f[5:]:
            if "=" in x:
                a, b = x.split("=", 1)
                r[a] = b
        r["load1"] = num((r.get("host_load") or "").split(" ")[0])
        rows[f[1]] = r
    return rows


# ---------------------------------------------------------------- one idle cell
def parse_perf(path):
    """perf stat -x, output -> {event: value or None}; None = not counted / not supported"""
    t = read(path)
    if not t:
        return None
    ev = {}
    for line in t.splitlines():
        f = line.split(",")
        if len(f) < 3 or not f[2]:
            continue
        name = f[2].strip()
        v = num(f[0])
        ev[name] = v if v is not None else None
    return ev or None


def perf_sum(ev, base, mode):
    """the count of `base` (instructions, cycles) in `mode` (u, k) summed over every PMU perf printed a line for: a heterogeneous
    host (msr1's big.LITTLE SoC) prints one line per core type, e.g. `cpu_core/instructions/u` or `armv8_pmuv3_0/instructions:u/`,
    and the process ran on either. None when no such line counted."""
    if not ev:
        return None
    vals_ = []
    for name, v in ev.items():
        n = name.lower()
        if base not in n or (base == "cycles" and "ref-cycles" in n) or v is None:
            continue
        m = re.search(r"[:/]([uk]+)/?$", n)
        if (m.group(1) if m else "") == mode:
            vals_.append(v)
    return sum(vals_) if vals_ else None


def per_second(ev, base, mode, window):
    v = perf_sum(ev, base, mode)
    if v is None:
        return None
    dur = ev.get("duration_time")
    secs = dur / 1e9 if dur else window
    return v / secs if secs else None


def parse_cell(d, tag):
    t = read(os.path.join(d, tag + ".srv.txt"))
    if t is None:
        return None
    m = re.search(r"^IDLE .*$", t, re.M)
    if not m:
        return {"tag": tag, "error": "no IDLE line"}
    o = kv(m.group(0))
    wk = [float(x) for x in o.get("loop_wakeups_per_s", "[]").strip("[]").split()]
    win = num(o.get("window", "20s").rstrip("s")) or 20.0
    r = {
        "tag": tag, "cpu": num(o.get("process_cpu_pct")), "wk": wk, "wake_mean": mean(wk) if wk else None,
        "loopcpu": num(o.get("sched_loop_cpu_pct")), "loops": int(o.get("loop_threads", 0)), "window": win,
        "other": num(o.get("other_threads_voluntary_per_s")), "rss": num(o.get("rss_mib")),
        "loop_cpus": o.get("loop_cpus", "").strip("[]"),
    }
    s2 = re.search(r"^SCHED2 .*us_per_slice=([\d.]+)", t, re.M)
    if s2:
        r["us_per_wake"] = float(s2.group(1))
    elif wk and r["loopcpu"] is not None and sum(wk) > 0:
        r["us_per_wake"] = r["loopcpu"] * 10000.0 / sum(wk)  # % of one CPU -> us per second, over wake-ups per second
    sd = re.findall(r"^(SHUTDOWN\S* .*)$", t, re.M)
    r["shutdown"] = sd[-1] if sd else ""
    # perf: this process, user and kernel
    pf = parse_perf(os.path.join(d, tag + ".perf.txt"))
    r["perf_raw"] = pf
    if pf:
        for key, base, mode in (("ins_u", "instructions", "u"), ("ins_k", "instructions", "k"), ("cyc_u", "cycles", "u"), ("cyc_k", "cycles", "k")):
            r[key] = per_second(pf, base, mode, win)
    # energy: counters (W over the window), gauges, perf's package energy
    en = []
    for line in t.splitlines():
        if line.startswith("ENERGY src="):
            e = kv(line)
            if "watts" in e:
                en.append((e["src"], float(e["watts"])))
        elif line.startswith("POWER src="):
            e = kv(line)
            if "mean_w" in e:
                en.append((e["src"], float(e["mean_w"])))
    pe = parse_perf(os.path.join(d, tag + ".pe.txt"))
    if pe:
        for name, v in pe.items():
            if "energy" in name and v is not None:
                dur = pe.get("duration_time")
                en.append(("perf:" + name, v / ((dur / 1e9) if dur else win)))
    r["energy"] = dict(en)
    return r


ENERGY_PREF = ("perf:power/energy-pkg", "package", "pkg", "rapl")


def pick_energy(cells):
    """the one source used for the table: the first source (by preference) that every given cell has"""
    common = None
    for c in cells:
        s = set(c["energy"]) if c else set()
        common = s if common is None else common & s
    if not common:
        return None
    for p in ENERGY_PREF:
        for s in sorted(common):
            if p in s.lower():
                return s
    return sorted(common)[0]


# ---------------------------------------------------------------- the gate tables
TAG = re.compile(r"^(?P<pre>iouring-)?(?P<var>.+)-n(?P<n>\d+)-pass(?P<p>\d+)$")


def load_cells(d, alias=None, prefix=""):
    alias = alias or {}
    cells = {}
    base = {}
    for f in sorted(glob.glob(os.path.join(d, "*.srv.txt"))):
        tag = os.path.basename(f)[:-8]
        m = TAG.match(tag)
        if not m or ((m.group("pre") or "") != prefix and m.group("var") != "baseline"):
            continue
        var = alias.get(m.group("var"), m.group("var"))
        c = parse_cell(d, tag)
        if not c or "error" in c:
            continue
        key = (var, int(m.group("n")))
        if var == "baseline":
            base.setdefault("baseline", []).append((int(m.group("p")), c))
        else:
            cells.setdefault(key, []).append((int(m.group("p")), c))
    for k in cells:
        cells[k].sort(key=lambda x: x[0])
    return cells, [c for _, c in sorted(base.get("baseline", []), key=lambda x: x[0])]


def vals(cs, key):
    return [c[key] for _, c in cs if c.get(key) is not None]


def sumk(c, a, b):
    if c.get(a) is None:
        return None
    return c[a] + (c[b] or 0.0 if c.get(b) is not None else 0.0)


def gate_tables(d, alias=None, prefix="", only_n=None, title="Gate cells"):
    cells, base = load_cells(d, alias, prefix)
    idx = read_index(d)
    out = []
    ns = sorted({n for (_, n) in cells})
    if only_n:
        ns = [n for n in ns if n in only_n]
    vs = [v for v in VARIANTS if any(k[0] == v for k in cells)] + sorted({k[0] for k in cells} - set(VARIANTS))
    if not cells:
        return ["(no cells in %s)" % d], {}
    out.append("### %s: process CPU %% (median [min..max] over passes; per pass in brackets, pass order)\n" % title)
    out.append("| N idle connections | " + " | ".join(vs) + " |")
    out.append("|---|" + "---|" * len(vs))
    for n in ns:
        row = []
        for v in vs:
            cs = cells.get((v, n), [])
            x = vals(cs, "cpu")
            row.append("%s (%s)" % (rng2(x), ", ".join("%.2f" % y for y in x)) if x else "-")
        out.append("| %s | %s |" % ("{:,}".format(n), " | ".join(row)))
    out.append("")
    # per-pass differences head - main, and the isolation builds against their base
    out.append("### CPU difference by pass (point of one CPU; same pass)\n")
    out.append("| N | head - main | head-aw - head | main-skip - main | main-twin - main (the noise) |")
    out.append("|---|---|---|---|---|")
    for n in ns:
        def diff(a, b):
            ca, cb = dict(cells.get((a, n), [])), dict(cells.get((b, n), []))
            x = [ca[p]["cpu"] - cb[p]["cpu"] for p in sorted(set(ca) & set(cb)) if ca[p].get("cpu") is not None and cb[p].get("cpu") is not None]
            return ", ".join("%+.2f" % y for y in x) if x else "-"
        out.append("| %s | %s | %s | %s | %s |" % ("{:,}".format(n), diff("head", "main"), diff("head-aw", "head"), diff("main-skip", "main"), diff("main-twin", "main")))
    out.append("")
    _all = [c for cs in cells.values() for _, c in cs]
    src_ = pick_energy(_all + base) if base else pick_energy(_all)
    if any(k[0] == "main-twin" for k in cells):
        out.append("### The twin's spread: the tolerance of the decision (max over the passes of |main - main-twin|, same pass; 0 means the documented floor is used)\n")
        out.append("| N | CPU (point) | instructions (M/s, user+kernel when counted) | energy (W) |")
        out.append("|---|---|---|---|")
        for n in ns:
            cm_, ct_ = dict(cells.get(("main", n), [])), dict(cells.get(("main-twin", n), []))
            common = sorted(set(cm_) & set(ct_))
            if not common:
                continue

            def sp(fn, scale=1.0):
                d = [abs(fn(ct_[p]) - fn(cm_[p])) for p in common if fn(ct_[p]) is not None and fn(cm_[p]) is not None]
                return "%.3g" % (max(d) / scale) if d else "not measured"
            ins = lambda c: (c["ins_u"] + c["ins_k"]) if c.get("ins_u") is not None and c.get("ins_k") is not None else c.get("ins_u")
            out.append("| %s | %s | %s | %s |" % ("{:,}".format(n), sp(lambda c: c.get("cpu")), sp(ins, 1e6), sp(lambda c, s=src_: c["energy"].get(s)) if src_ else "not measured"))
        out.append("")
    out.append("### Wake-ups, loop threads, time per wake (median [min..max] over passes)\n")
    out.append("| N | build | wake-ups/s per loop | loop-thread CPU % (sum) | us per wake (loop CPU / wake-ups) | other-thread switches/s | RSS MiB |")
    out.append("|---|---|---|---|---|---|---|")
    for n in ns:
        for v in vs:
            cs = cells.get((v, n), [])
            if not cs:
                continue
            out.append("| %s | %s | %s | %s | %s | %s | %s |" % ("{:,}".format(n), v, rng2(vals(cs, "wake_mean"), "%.1f"), rng2(vals(cs, "loopcpu"), "%.3f"),
                       rng2(vals(cs, "us_per_wake"), "%.1f"), rng2(vals(cs, "other"), "%.0f"), rng2(vals(cs, "rss"), "%.0f")))
    out.append("")
    # instructions and cycles
    have_perf = any(c.get("ins_u") is not None or c.get("ins_k") is not None for cs in cells.values() for _, c in cs)
    out.append("### Instructions and cycles of the server process (perf stat over the window; millions per second)\n")
    if not have_perf:
        out.append("none: perf did not count on this host (not installed, or this user may not read the PMU): see facts/host-facts.txt.\n")
    else:
        out.append("| N | build | instructions user | instructions kernel | cycles user | cycles kernel |")
        out.append("|---|---|---|---|---|---|")
        for n in ns:
            for v in vs:
                cs = cells.get((v, n), [])
                if not cs:
                    continue
                def m(key):
                    x = [y / 1e6 for y in vals(cs, key)]
                    return rng2(x, "%.2f") if x else "not counted"
                out.append("| %s | %s | %s | %s | %s | %s |" % ("{:,}".format(n), v, m("ins_u"), m("ins_k"), m("cyc_u"), m("cyc_k")))
        out.append("")
    # energy
    allc = [c for cs in cells.values() for _, c in cs]
    src = pick_energy(allc + base) if base else pick_energy(allc)
    out.append("### Package energy (W, the host's idle baseline subtracted)\n")
    bw = []
    if src and base:
        bw = [c["energy"][src] for c in base if src in c["energy"]]
    if not src:
        have = sorted({s for c in allc for s in c["energy"]})
        out.append("none: %s\n" % ("no source is common to every cell (%s)" % ", ".join(have) if have else "no power or energy source was readable on this host (RAPL powercap and hwmon unreadable or absent, perf energy not usable): see facts/host-facts.txt"))
    else:
        out.append("source: `%s`. Idle baseline (nothing served, same window, %d reads): %s W.\n" % (src, len(bw), rng2(bw, "%.3f") if bw else "NOT MEASURED, absolute watts shown"))
        out.append("| N | build | W (total) | W above idle |")
        out.append("|---|---|---|---|")
        b0 = med(bw) if bw else 0.0
        for n in ns:
            for v in vs:
                cs = cells.get((v, n), [])
                x = [c["energy"][src] for _, c in cs if src in c["energy"]]
                if x:
                    out.append("| %s | %s | %s | %s |" % ("{:,}".format(n), v, rng2(x, "%.3f"), rng2([y - b0 for y in x], "%.3f") if bw else "-"))
        out.append("")
    # data quality
    out.append("### Data quality of these cells\n")
    tags = [c["tag"] for cs in cells.values() for _, c in cs]
    forced = [t for t in tags if idx.get(t, {}).get("forced") == "1"]
    loads = [idx[t]["load1"] for t in tags if t in idx and idx[t].get("load1") is not None]
    bad = [t for t in tags if idx.get(t, {}).get("rc") not in (None, "0")]
    out.append("- cells (baselines excluded): %d; load1 at the start of a cell: %s; waited for a quiet host: %d cell(s) waited, **%d forced** (ran above the threshold after the wait)%s; cells with rc != 0: %s."
               % (len(tags), "%.2f..%.2f" % (min(loads), max(loads)) if loads else "unknown", sum(1 for t in tags if idx.get(t, {}).get("wait") not in (None, "0")), len(forced),
                  (": " + ", ".join(forced)) if forced else "", ", ".join(bad) if bad else "none"))
    sd = [c["shutdown"] for cs in cells.values() for _, c in cs if "SHUTDOWN " not in c["shutdown"] and c["shutdown"] is not None]
    lc = sorted({c.get("loop_cpus", "") for cs in cells.values() for _, c in cs if c.get("loop_cpus")})
    if lc:
        out.append("- the loop threads were pinned to CPUs (loop_cpus, as the runs saw them): %s." % "; ".join(lc[:6]))
    out.append("- every run printed SHUTDOWN: %s." % ("yes" if not sd else "NO, %d did not: %s" % (len(sd), ", ".join(sorted({s[:40] for s in sd})))))
    return out, {"cells": cells, "src": src, "base": bw, "idx": idx}


def gt(diff, tol):
    """diff > tol, compared in millionths: the CPU figures come in steps of one clock tick (0.05 point over 20 s) and 1.95 - 1.90 must not
    be 'more than one tick' because of how binary floats round (1.95 - 1.90 = 0.04999..., 2.45 - 2.40 = 0.05000...)"""
    return round(diff * 1e6) > round(tol * 1e6)


def decide(cells, src, base_w, expect_ns=EXPECT_NS, idx=None):
    """The decision rule, per count and overall. The tolerance is NOT a constant: it is the spread of the main-against-main TWIN on this host at this
    count: the same main binary run as the variants 'main' and 'main-twin', interleaved in the same passes, so the largest absolute per-pass difference
    between the two, max over passes of |main_p - main-twin_p|, is what two identical builds differ by here. A head - main difference inside it is
    'equal'. Only when that spread is 0 (identical figures in every pass: a twin that cannot show noise) a documented floor is used: one clock tick
    (0.05 point) for CPU, 1 % of main for instructions, FLOOR_W for energy.
    (i)   head's CPU not above main's at every count => NO LOSS ('NO LOSS ON CPU ONLY (instructions and energy not measured)' when they were not both measured);
    (ii)  head's CPU above main's at any count => UNDECIDED, and ACCOUNTING EFFECT only when instructions (user AND kernel) and energy were measured
          and neither is higher;
    (iii) instructions or energy higher on head => REAL LOSS, only when measured, with the numbers beside the word.
    A count missing for main, head or the twin in any pass is named and the whole reads UNDECIDED (INCOMPLETE), never 'at every count'."""
    idx = idx or {}
    seen = [p for cs in cells.values() for p, _ in cs] + [int(m.group(1)) for t in idx for m in [re.search(r"-pass(\d+)$", t)] if m and not t.startswith("baseline")]
    passes_exp = max(seen or [1])
    out, verdicts, missing = [], {}, []
    fn_ = lambda n: "{:,}".format(n)

    for n in expect_ns:
        got = {v: dict(cells.get((v, n), [])) for v in ("main", "head", "main-twin")}
        why = []
        for var in ("main", "head", "main-twin"):
            lost = [p for p in range(1, passes_exp + 1) if p not in got[var]]
            if lost:
                reasons = sorted({idx.get("%s-n%d-pass%d" % (var, n, p), {}).get("rc", "no result") for p in lost})
                why.append("%s pass %s (%s)" % (var, ",".join(map(str, lost)), "; ".join(reasons)))
        if why:
            missing.append(n)
            verdicts[n] = ("MISSING", "N = %s: NOT COMPARED, missing: %s." % (fn_(n), "; ".join(why)))
            continue
        cm, ch, ct = got["main"], got["head"], got["main-twin"]
        ps = sorted(set(cm) & set(ch) & set(ct))

        def prs(a, b, fn):
            return [(fn(a[p]), fn(b[p])) for p in ps if fn(a[p]) is not None and fn(b[p]) is not None]

        def measure(fn, floor_of):
            """(head-vs-main pairs, twin-vs-main pairs, tolerance, twin spread, floor used)"""
            hp, tp = prs(ch, cm, fn), prs(ct, cm, fn)
            if not hp or len(tp) != len(hp):
                return None
            mm = med([b for _, b in hp])
            sp = max(abs(a - b) for a, b in tp)
            floor_used = round(sp * 1e6) == 0
            return hp, tp, (floor_of(mm) if floor_used else sp), sp, floor_used

        def verdict_of(m):
            hp = m[0]
            mh, mm = med([a for a, _ in hp]), med([b for _, b in hp])
            wins = sum(1 for a, b in hp if a > b)
            return gt(mh - mm, m[2]) and wins * 2 > len(hp), mm, mh, wins, len(hp)

        tw = lambda sp, fl, unit, scale=1.0: "twin spread %s%s%s" % ("%.3g" % (sp / scale), unit, " (0: the documented floor %.3g%s is used)" % (fl / scale, unit) if round(sp * 1e6) == 0 else "")
        ins_fn = lambda c: (c["ins_u"] + c["ins_k"]) if c.get("ins_u") is not None and c.get("ins_k") is not None else None
        cpu_m = measure(lambda c: c.get("cpu"), lambda mm: TOL_CPU_FLOOR)
        ins_full = measure(ins_fn, lambda mm: INS_FLOOR_REL * mm)
        ins_m = ins_full or measure(lambda c: c.get("ins_u"), lambda mm: INS_FLOOR_REL * mm)
        en_m = measure(lambda c, s=src: c["energy"].get(s), lambda mm: FLOOR_W) if src else None
        notm = []
        if ins_m is None:
            notm.append("instructions")
        elif ins_full is None:
            notm.append("kernel-side instructions (user mode only)")
        if en_m is None:
            notm.append("energy")
        if cpu_m is None:
            missing.append(n)
            verdicts[n] = ("MISSING", "N = %s: NOT COMPARED, no CPU figure." % fn_(n))
            continue
        cpu_h, mc, hc, wc, nc = verdict_of(cpu_m)
        nums = ["CPU main %.2f head %.2f point (head above main in %d of %d passes; %s; tolerance %.3g)" % (mc, hc, wc, nc, tw(cpu_m[3], TOL_CPU_FLOOR, " point"), cpu_m[2])]
        ins_h = en_h = False
        if ins_m:
            ins_h, mi, hi, wi, ni = verdict_of(ins_m)
            nums.append("instructions%s main %.2f head %.2f M/s (%+.1f %%, head above in %d of %d passes; twin spread %.3g M/s%s; tolerance %.3g M/s)"
                        % ("" if ins_full else " (user mode only)", mi / 1e6, hi / 1e6, 100 * (hi - mi) / mi if mi else 0, wi, ni, ins_m[3] / 1e6, " (0: floor used)" if ins_m[4] else "", ins_m[2] / 1e6))
        if en_m:
            en_h, mw, hw, ww, nw = verdict_of(en_m)
            nums.append("energy main %.3f head %.3f W (%+.3f W, head above in %d of %d passes; twin spread %.3f W%s; tolerance %.3f W)"
                        % (mw, hw, hw - mw, ww, nw, en_m[3], " (0: floor used)" if en_m[4] else "", en_m[2]))
        num_s = "; ".join(nums)
        full = ins_full is not None and en_m is not None
        if ins_h or en_h:
            kinds = " and ".join(k for k, f in (("instructions", ins_h), ("energy", en_h)) if f)
            verdicts[n] = ("REAL LOSS", "N = %s: REAL LOSS (%s higher on head, measured). %s." % (fn_(n), kinds, num_s))
        elif cpu_h:
            if full:
                verdicts[n] = ("ACCOUNTING EFFECT", "N = %s: ACCOUNTING EFFECT (head's CPU is higher; instructions user+kernel and energy were measured and are not higher). %s." % (fn_(n), num_s))
            else:
                verdicts[n] = ("UNDECIDED", "N = %s: UNDECIDED (head's CPU is higher; NOT MEASURED: %s). %s." % (fn_(n), ", ".join(notm), num_s))
        elif full:
            verdicts[n] = ("NO LOSS", "N = %s: NO LOSS (head's CPU is not higher; instructions and energy are not higher). %s." % (fn_(n), num_s))
        else:
            verdicts[n] = ("NO LOSS ON CPU ONLY", "N = %s: NO LOSS ON CPU ONLY (head's CPU is not higher; NOT MEASURED: %s). %s." % (fn_(n), ", ".join(notm), num_s))

    for n in expect_ns:
        out.append("- " + verdicts[n][1])
    out.append("")
    kinds = {n: verdicts[n][0] for n in expect_ns}
    fmt = lambda ns: ", ".join(fn_(n) for n in ns)
    real_low = [n for n in expect_ns if n in LOW_N and kinds[n] == "REAL LOSS"]
    real_any = [n for n in expect_ns if kinds[n] == "REAL LOSS"]
    undec = [n for n in expect_ns if kinds[n] == "UNDECIDED"]
    acct = [n for n in expect_ns if kinds[n] == "ACCOUNTING EFFECT"]
    cpu_only = [n for n in expect_ns if kinds[n] == "NO LOSS ON CPU ONLY"]
    note = (" Counts NOT COMPARED: %s." % fmt(missing)) if missing else ""
    if real_low:
        out.append("**OVERALL: REAL LOSS reproduced at %s (numbers in the lines above). NO MERGE: the bot takes the table to Albert.**%s" % (fmt(real_low), note))
    elif real_any:
        out.append("**OVERALL: REAL LOSS at %s (not at 100 to 1,000; numbers above). No merge: the table goes to the bot.**%s" % (fmt(real_any), note))
    elif missing:
        out.append("**OVERALL: UNDECIDED, INCOMPLETE: not every count was compared, so there is no verdict for the run. Counts NOT COMPARED: %s (see the lines above for what is missing).**" % fmt(missing))
    elif undec:
        out.append("**OVERALL: UNDECIDED at %s: head's CPU is higher and instructions (user and kernel) or energy were NOT MEASURED, so neither a loss nor an accounting effect is shown. No merge tonight: the table plus the isolation builds' rows go to the bot.**" % fmt(undec))
    elif acct:
        out.append("**OVERALL: ACCOUNTING EFFECT at %s (head's CPU is higher; instructions user+kernel and energy measured, not higher); no loss at the other counts.**" % fmt(acct))
    elif cpu_only:
        out.append("**OVERALL: NO LOSS ON CPU ONLY (instructions and energy not measured): head's CPU is not above main's, beyond the twin's spread, at any of %s. This is a CPU comparison, not a measurement of instructions or energy.**" % fmt(expect_ns))
    else:
        out.append("**OVERALL: NO LOSS: head is at or below main at every count (%s), instructions and energy measured and not higher.**" % fmt(expect_ns))
    return out


RULE = """**The decision rule** (reader.py). The tolerance is the spread of the MAIN-AGAINST-MAIN TWIN on this host at this count, not a constant: stage a runs the same main binary as two variants, `main` and `main-twin`, interleaved in the same passes (pass 2 reversed); the twin spread of a measure is the largest per-pass absolute difference between them, max over the passes of |main - main-twin| (CPU in points, instructions in M/s, energy in W); a head - main difference inside it is equal, and "higher" needs the median over the passes above main's by more than it AND head above main in more than half of the same-pass pairs. Only when the twin spread is 0 (identical figures in every pass) a documented floor is used and printed: one clock tick (%.2f point) for CPU, %d %% of main's for instructions, %.2f W for energy. CPU is compared in integer millionths, so float rounding cannot decide a one-tick difference.
(i) head's CPU not above main's at every count => **NO LOSS** (when instructions and energy were not both measured: **NO LOSS ON CPU ONLY (instructions and energy not measured)**); (ii) head's CPU above main's at any count => **UNDECIDED**, and **ACCOUNTING EFFECT** only when instructions (user and kernel) and energy were measured and neither is higher; (iii) instructions or energy higher on head => **REAL LOSS**, only when measured, with the numbers beside the word; a real loss at 100 to 1,000 => **NO MERGE: the bot takes the table to Albert**. A count missing for main, head or the twin in any pass is named and the whole reads UNDECIDED (INCOMPLETE). A figure the host could not measure is said so, never read as equal.

| verdict on a host | action |
|---|---|
| NO LOSS on both hosts (with or without "ON CPU ONLY") | merge #1485 through the chain with both tables in its body; then #1488 retargets and merges on green |
| REAL LOSS on either host | no merge; the table goes to the bot |
| UNDECIDED (CPU higher somewhere, nothing else measured), or INCOMPLETE | no merge tonight; the table plus the isolation builds' rows go to the bot |""" % (TOL_CPU_FLOOR, INS_FLOOR_REL * 100, FLOOR_W)


# ---------------------------------------------------------------- first-request tables
def first_lines(path):
    t = read(path) or ""
    out = {}
    for m in re.finditer(r"^FIRST (.*)$", t, re.M):
        o = kv(m.group(1))
        out[o.get("idle", "?")] = (dur_ms(o.get("median", "")), dur_ms(o.get("max", "")))
    return out


def probe(path, kind):
    t = read(path) or ""
    m = re.search(r"^PROBE %s .* all_us=\[([^\]]*)\]" % kind, t, re.M)
    return [int(x) / 1000 for x in m.group(1).split()] if m else []


def paired(rows, la, lb):
    """rows: [(pair, A median, B median, A worst, B worst)] -> markdown lines"""
    if not rows:
        return ["(no complete pair)"]
    diffs = [b - a for _, a, b, _, _ in rows]
    nz = [x for x in diffs if x != 0]
    k = sum(1 for x in nz if x > 0)
    return ["| %d | %s -> %s | %.2f -> %.2f ms | **%+.2f [%+.2f..%+.2f] ms** | %d of %d | p = %.3f | %.2f / %.2f ms |" % (
        len(rows), la, lb, med([a for _, a, _, _, _ in rows]), med([b for _, _, b, _, _ in rows]), med(diffs), min(diffs), max(diffs), k, len(nz), sign_p(k, len(nz)),
        max(r[3] for r in rows), max(r[4] for r in rows))]


FR_HEAD = ["| pairs | A -> B | median of per-run medians | paired difference B-A median [min..max] | B slower | sign test (two-sided, ties dropped) | worst single request A / B |", "|---|---|---|---|---|---|---|"]


def empty_tables(d):
    idx = read_index(d)
    out = ["### First request after a 2 s gap, empty server: the OLD 4 ms build (A) against main (B)\n"]
    engines = sorted({re.match(r"(.+)-p\d+-", os.path.basename(f)).group(1) for f in glob.glob(os.path.join(d, "*-p*-*.txt")) if re.match(r".+-p\d+-", os.path.basename(f))},
                     key=lambda e: ("epoll", "iouring", "adaptive").index(e) if e in ("epoll", "iouring", "adaptive") else 9)
    if not engines:
        return out + ["(no first-request runs)"]
    out += ["| engine | " + FR_HEAD[0][1:], "|---|" + FR_HEAD[1][1:]]
    skipped = []
    for e in engines:
        rows = []
        pairs = sorted({int(re.match(r".+-p(\d+)-", os.path.basename(f)).group(1)) for f in glob.glob(os.path.join(d, e + "-p*-*.txt"))})
        for p in pairs:
            a = first_lines(os.path.join(d, "%s-p%d-old.txt" % (e, p))).get("2s")
            b = first_lines(os.path.join(d, "%s-p%d-main.txt" % (e, p))).get("2s")
            if a and b and a[0] is not None and b[0] is not None:
                rows.append((p, a[0], b[0], a[1] or 0, b[1] or 0))
            else:
                skipped.append("%s pair %d" % (e, p))
        for line in paired(rows, "old", "main"):
            out.append("| %s %s" % (e, line[0:] if line.startswith("(") else line))
    if skipped:
        out.append("\nPairs without both runs (failed, skipped or io_uring unusable): " + ", ".join(skipped))
    out.append("\nA run is 8 requests, each on a NEW connection after a 2 s idle gap (idlecost -gaps 2s:N, N = 8; 3 in the dry-run); the figure of a run is the median of its 8. Pairs alternate A B / B A.")
    return out


def held_tables(d, la, lb, title):
    out = ["### %s\n" % title]
    pairs = sorted({int(re.match(r"p(\d+)-", os.path.basename(f)).group(1)) for f in glob.glob(os.path.join(d, "p*-*.cli.txt"))})
    out += ["| kind | " + FR_HEAD[0][1:], "|---|" + FR_HEAD[1][1:]]
    for kind, name in (("new", "new connection"), ("held", "held connection")):
        rows = []
        for p in pairs:
            a = probe(os.path.join(d, "p%d-%s.cli.txt" % (p, la)), kind)
            b = probe(os.path.join(d, "p%d-%s.cli.txt" % (p, lb)), kind)
            if a and b:
                rows.append((p, med(a), med(b), max(a), max(b)))
        for line in paired(rows, la, lb):
            out.append("| %s %s" % (name, line))
    out.append("\n8 requests of each kind per run after a 2 s gap, 1,000 idle connections held; the figure of a run is the median of its 8; pairs alternate A B / B A.")
    return out


# ---------------------------------------------------------------- stages and report
def facts_summary(out):
    t = read(os.path.join(out, "facts", "host-facts.txt"))
    if not t:
        return ["(no facts file)"]
    keep = []
    for line in t.splitlines():
        if re.match(r"^(host label|kernel:|PRETTY_NAME|loadavg|Model name|Architecture|CPU\(s\)|Thread|Core|Socket|cpufreq/(scaling_governor|scaling_driver|energy)|intel_pstate|cpufreq/boost|cpuidle driver|  state|smt|== CPU sets|== power sources|== perf|== open-files|ulimit|tools:|runner agent|io_uring_disabled)", line):
            keep.append(line)
    return ["```"] + keep + ["```"]


def availability(out):
    """what this host could measure, from facts/availability.txt (written by host-run.sh facts): the table's header says CPU-only when it was"""
    t = read(os.path.join(out, "facts", "availability.txt"))
    if not t:
        return ["**Measurement availability: not recorded (facts/availability.txt is missing).**", ""]
    return ["**Measurement availability on this host** (as the host job found it):", ""] + ["- " + l for l in t.strip().splitlines()] + [""]


def wall(d):
    idx = read_index(d)
    tot = sum(num(r.get("wall_s")) or 0 for r in idx.values())
    return tot, len(idx)


def stage_md(stage, out):
    lines = []
    if stage == "a":
        d = os.path.join(out, "a")
        if not os.path.isdir(d):
            return ["## Stage a: not run"]
        lines.append("## Stage a: the #1485 gate cells (epoll)\n")
        lines += availability(out)
        t, n = wall(d)
        lines.append("%d cells, %.1f min of cell wall time.\n" % (n, t / 60))
        g, info = gate_tables(d, title="main / head / head-aw / main-skip / main-twin")
        lines += g
        lines.append("")
        if info:
            lines += decide(info["cells"], info["src"], info["base"], EXPECT_NS, info["idx"])
        lines.append("")
        lines.append(RULE)
    elif stage == "b":
        lines.append("## Stage b: paired first-request cells\n")
        d1, d2 = os.path.join(out, "b1"), os.path.join(out, "b2")
        if os.path.isdir(d1):
            lines += empty_tables(d1)
            lines.append("")
        if os.path.isdir(d2):
            lines += held_tables(d2, "main", "head", "1,000 connections held: main (A) against head (B), epoll, for #1485")
        t = wall(d1)[0] + wall(d2)[0]
        lines.append("\nCell wall time: %.1f min." % (t / 60))
    elif stage == "c":
        d = os.path.join(out, "c")
        if not os.path.isdir(d):
            return ["## Stage c: not run"]
        lines.append("## Stage c: io_uring, main against head, for the record\n")
        lines += availability(out)
        g, info = gate_tables(d, prefix="iouring-", title="io_uring, main / head")
        lines += g
        lines.append("\nFor the record only: the decision rule is for epoll (stage a).")
        t, n = wall(d)
        lines.append("\n%d cells, %.1f min of cell wall time." % (n, t / 60))
    return lines


def main(argv):
    if len(argv) >= 3 and argv[1] == "stage":
        print("\n".join(stage_md(argv[2], argv[3])))
    elif len(argv) >= 3 and argv[1] == "report":
        out = argv[2]
        print("# Idle cell report\n")
        print("## Host as found\n")
        print("\n".join(facts_summary(out)) + "\n")
        for s in ("a", "b", "c"):
            print("\n".join(stage_md(s, out)))
            print()
        fc = read(os.path.join(out, "FAILED-CELLS.txt"))
        print("Cells that did not end rc=0: %s" % (fc.split() if fc else "none"))
    elif len(argv) >= 3 and argv[1] == "gate":
        alias, prefix, only = {}, "", None
        for i, a in enumerate(argv):
            if a == "--alias":
                alias = dict(x.split("=") for x in argv[i + 1].split(","))
            if a == "--prefix":
                prefix = argv[i + 1]
            if a == "--n":
                only = [int(x) for x in argv[i + 1].split(",")]
        g, info = gate_tables(argv[2], alias, prefix, only)
        print("\n".join(g))
        expect = EXPECT_NS
        for i, a_ in enumerate(argv):
            if a_ == "--expect":
                expect = tuple(int(x) for x in argv[i + 1].split(","))
        if info:
            print("\n".join(decide(info["cells"], info["src"], info["base"], expect, info["idx"])))
    elif len(argv) >= 3 and argv[1] == "empty":
        print("\n".join(empty_tables(argv[2])))
    elif len(argv) >= 5 and argv[1] == "held":
        print("\n".join(held_tables(argv[2], argv[3], argv[4], "%s (A) against %s (B)" % (argv[3], argv[4]))))
    else:
        print(__doc__)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
