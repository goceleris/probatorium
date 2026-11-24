#!/usr/bin/env python3
"""Per-family HttpArena composite for a PRIVATE preview run (celeris + references on one box).

Formula (HttpArena site/content/docs/scoring/composite-score.md, gen_leaderboard_data.py badge_composite):
  per profile  avgRps over the profile's scored connection counts; score = avgRps / best_avgRps_in_league * 1000
  json-comp    avgRps is first multiplied by (minBpr / myBpr)^2, bpr = avg bandwidth / avg rps
  fixed-rate   latency-1m, latency-10k, 8gbit, connections-1m(-tls): latency_score.score_rows on the run's row,
               then score / best_score_in_league * 1000
  family       = SUM of its scored profiles (no weights); a profile an entry lacks adds 0
  Overall      = H/1.1 + H/2 + WebSocket (H/3 and gRPC are not served by celeris); Database is its own board
  completeness multiplier x(1 - 0.025 * number of false axes) on the family sum (celeris declares all true: x1.00)
The league here is ONLY the entries in --results (celeris + the references run on the same box), NOT HttpArena's
250-entry field: every 1000 means "best of this private group". Never quote it as a board position.

Usage:
  score_preview.py --results HttpArena/results --hepa HttpArena --entries celeris,gin,fiber,echo,go-fasthttp,fiber-websocket,go-stdlib --rates latency-1m=125000,8gbit=6250 --tls-profiles json-tls,8gbit,baseline-h2,static-h2 --merge fiber-websocket=fiber
  score_preview.py --published HttpArena/site/data/results --entries gin,fiber,echo,go-fasthttp      (self-test on published rows)
"""
import argparse, json, math, re, sys
from pathlib import Path

FAMILIES = {
 "H/1.1": [("baseline",[4096]),("limited-conn",[4096]),("async",[32000]),("latency-10k",[1024]),("latency-1m",[1024]),
           ("connections-1m",[1000000]),("connections-1m-tls",[1000000]),("json-comp",[4096,16384]),("json-tls",[4096]),("8gbit",[512])],
 "H/2":   [("baseline-h2",[256,1024]),("static-h2",[256,1024]),("baseline-h2c",[256,1024,4096]),("json-h2c",[1024,4096])],
 "WebSocket": [("echo-ws",[512,4096,16384]),("echo-ws-pipeline",[512,4096,16384]),("echo-ws-limited",[512,4096])],
 "Database":  [("async-db",[1024]),("fortunes",[1024])],
}
OVERALL = ["H/1.1", "H/2", "WebSocket"]
FIXED = {"latency-1m","latency-10k","8gbit","connections-1m","connections-1m-tls"}

def unit(v, table):
    m = re.match(r"\s*([\d.]+)\s*([A-Za-z/]*)", str(v or ""))
    return float(m.group(1)) * table.get(m.group(2), 1.0) if m else 0.0
BW = {"B/s":1,"KB/s":1e3,"MB/s":1e6,"GB/s":1e9}
def to_us(v):
    if isinstance(v,(int,float)): return float(v)
    return unit(v, {"us":1.0,"ms":1e3,"s":1e6})

def load_layout(results, entries):
    """-> rows[(entry, profile, conns)] = json row"""
    rows = {}
    for e in entries:
        for p in Path(results).glob(f"*/*/{e}.json"):
            rows[(e, p.parts[-3], int(p.parts[-2]))] = json.load(open(p))
    return rows

def load_published(d, entries):
    rows = {}
    for e in entries:
        f = Path(d)/f"{e}.json"
        if not f.exists(): continue
        for k, r in json.load(open(f))["results"].items():
            m = re.match(r"(.+)-(\d+)$", k)
            if m: rows[(e, m.group(1), int(m.group(2)))] = r
    return rows

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--results"); ap.add_argument("--published"); ap.add_argument("--hepa")
    ap.add_argument("--entries", default=""); ap.add_argument("--entry", default="celeris")
    ap.add_argument("--rates", default="", help="offered rates actually used, e.g. latency-1m=125000,8gbit=6250,latency-10k=10000,connections-1m=250000 (HttpArena defaults 1000000/10000/50000/250000); FULL_RATE = 95%% of each")
    ap.add_argument("--tls-profiles", default="json-tls,8gbit,baseline-h2,static-h2,connections-1m-tls")
    ap.add_argument("--merge", default="", help="fold a profile-limited supplement into its entry, e.g. fiber-websocket=fiber (HttpArena merges by display_name)")
    ap.add_argument("--json", action="store_true")
    ap.add_argument("--engines", default="", help="ENGINE-VARIANT section after the three views: the Adaptive entry first (the reference for the switch log), then the others, e.g. celeris,celeris-default,celeris-epoll,celeris-iouring (their rows do not enter the three views)")
    ap.add_argument("--container-logs", default="", help="HttpArena/site/static/logs: the harness's per-profile container logs (the Adaptive engine's own switch lines)")
    ap.add_argument("--bench-logs", default="", help="comma list of the timestamped benchmark logs (logs/engines.<entry>.log, logs/engines-async.<entry>.log) to place the runs in time")
    a = ap.parse_args()
    engine_names = [e for e in a.engines.split(",") if e]
    hepa = Path(a.hepa or (Path(a.results).parent if a.results else ".")).resolve()
    sys.path.insert(0, str(hepa/"scripts"))
    import latency_score as L
    for kv in filter(None, a.rates.split(",")):
        k, v = kv.split("="); L.FULL_RATE[k] = 0.95 * float(v)
    entries = [e for e in a.entries.split(",") if e] or None
    if a.published:
        entries = entries or sorted(p.stem for p in Path(a.published).glob("*.json"))
        rows = load_published(a.published, entries)
    else:
        entries = entries or sorted({p.stem for p in Path(a.results).glob("*/*/*.json")})
        rows = load_layout(a.results, entries)
        if engine_names: rows.update(load_layout(a.results, engine_names))
    for kv in filter(None, a.merge.split(",")):
        src, dst = kv.split("=")
        for (e, p, c) in [k for k in rows if k[0] == src]:
            rows.setdefault((dst, p, c), rows.pop((e, p, c)))
        entries = [e for e in entries if e != src]
    tls = set(a.tls_profiles.split(","))

    # per (profile, entry): effective avg rps or fixed-rate score
    eff = {}
    for fam, profs in FAMILIES.items():
        for pid, conns in profs:
            have = {e: [rows[(e,pid,c)] for c in conns if (e,pid,c) in rows] for e in entries}
            have = {e: r for e, r in have.items() if r}
            if pid in FIXED:
                for c in conns:
                    rs = [{"entry": e, "rps": rows[(e,pid,c)]["rps"], "cpu": rows[(e,pid,c)].get("cpu_per_req_us"),
                           "p99": to_us(rows[(e,pid,c)].get("p99_latency")), "mean": to_us(rows[(e,pid,c)].get("avg_latency"))}
                          for e in entries if (e,pid,c) in rows]
                    L.score_rows(rs, pid)
                    for r in rs: eff[(pid, r["entry"])] = r["score"]
            else:
                avg = {e: (sum(r["rps"] for r in rs)/len(rs), sum(unit(r.get("bandwidth"), BW) for r in rs)/len(rs)) for e, rs in have.items()}
                if pid == "json-comp" and avg:
                    bpr = {e: (bw/rps if rps else 0) for e,(rps,bw) in avg.items()}
                    best = min(v for v in bpr.values() if v > 0)
                    for e,(rps,bw) in avg.items(): eff[(pid,e)] = rps*(best/bpr[e])**2 if bpr[e] > 0 else 0
                else:
                    for e,(rps,bw) in avg.items(): eff[(pid,e)] = rps
    # Three pictures. eff holds one number per (profile, entry); a view drops some pairs and re-normalizes
    # against the best that is left, exactly as the board would if those rows did not exist.
    def view(drop):
        res = {}
        for e in entries:
            fam_tot = {}; detail = {}
            for fam, profs in FAMILIES.items():
                t = 0.0
                for pid, _ in profs:
                    vals = [v for (p, en), v in eff.items() if p == pid and (p, en) not in drop]
                    best = max(vals, default=0)
                    s_ = (eff.get((pid, e), 0)/best*1000) if best > 0 and (pid, e) not in drop else 0
                    detail[pid] = round(s_, 1); t += s_
                fam_tot[fam] = round(t, 1)
            res[e] = (fam_tot, detail)
        return res
    V_ALL = view(set())
    V_CLR = view({(p, a.entry) for p in tls})                    # celeris ships cleartext-only: the references keep their TLS rows
    V_SUB = view({(p, e) for p in tls for e in entries})         # profile subset: the TLS profiles removed for everyone
    out = {}
    for e in entries:
        out[e] = {"profiles": V_ALL[e][1],
                  "family_with_tls": V_ALL[e][0], "family_celeris_cleartext_only": V_CLR[e][0], "family_cleartext_profiles_only": V_SUB[e][0]}
        for k, v in (("with_tls", V_ALL), ("celeris_cleartext_only", V_CLR), ("cleartext_profiles_only", V_SUB)):
            out[e]["overall_" + k] = round(sum(v[e][0][f] for f in OVERALL), 1)
    if a.json: print(json.dumps(out, indent=1)); return
    print("League = only these entries (private group, NOT the HttpArena field): " + ", ".join(entries))
    print("Views: A = all rows (celeris with its TLS side); B = %s ships cleartext-only (its TLS rows 0, the others keep theirs: the board picture);" % a.entry)
    print("       C = the TLS profiles (%s) removed for everyone (a profile subset)." % ", ".join(sorted(tls)))
    for fam in FAMILIES:
        print(f"\n[{fam}]  profile score 0-1000 in view A (best of the group = 1000); '-' = profile not run")
        pids = [p for p,_ in FAMILIES[fam]]
        print("%-14s" % "entry" + "".join("%-18s" % p for p in pids) + "SUM A    SUM B    SUM C")
        for e in entries:
            cells = "".join("%-18s" % (out[e]["profiles"][p] if (p, e) in eff else "-") for p in pids)
            print("%-14s%s%-8s %-8s %s" % (e, cells, out[e]["family_with_tls"][fam], out[e]["family_celeris_cleartext_only"][fam], out[e]["family_cleartext_profiles_only"][fam]))
    print("\nOverall (H/1.1 + H/2 + WebSocket):")
    for e in entries:
        print("  %-14s A %8s   B %8s   C %8s" % (e, out[e]["overall_with_tls"], out[e]["overall_celeris_cleartext_only"], out[e]["overall_cleartext_profiles_only"]))
    if engine_names: engine_section(rows, engine_names, L, a.container_logs, a.bench_logs.split(","))

def compute_eff(rows, entries, L):
    """Same arithmetic as the three views' eff table, for any subset of entries (used by the engine section only)."""
    eff = {}
    for fam, profs in FAMILIES.items():
        for pid, conns in profs:
            have = {e: [rows[(e,pid,c)] for c in conns if (e,pid,c) in rows] for e in entries}
            have = {e: r for e, r in have.items() if r}
            if pid in FIXED:
                for c in conns:
                    rs = [{"entry": e, "rps": rows[(e,pid,c)]["rps"], "cpu": rows[(e,pid,c)].get("cpu_per_req_us"),
                           "p99": to_us(rows[(e,pid,c)].get("p99_latency")), "mean": to_us(rows[(e,pid,c)].get("avg_latency"))}
                          for e in entries if (e,pid,c) in rows]
                    L.score_rows(rs, pid)
                    for r in rs: eff[(pid, r["entry"])] = r["score"]
            else:
                avg = {e: (sum(r["rps"] for r in rs)/len(rs), sum(unit(r.get("bandwidth"), BW) for r in rs)/len(rs)) for e, rs in have.items()}
                if pid == "json-comp" and avg:
                    bpr = {e: (bw/rps if rps else 0) for e,(rps,bw) in avg.items()}
                    best = min((v for v in bpr.values() if v > 0), default=0)
                    for e,(rps,bw) in avg.items(): eff[(pid,e)] = rps*(best/bpr[e])**2 if bpr[e] > 0 and best > 0 else 0
                else:
                    for e,(rps,bw) in avg.items(): eff[(pid,e)] = rps
    return eff

DUR = {"latency-1m": 20, "latency-10k": 20, "async": 10, "async-db": 10}   # seconds of one measured run (the rest: 5 s); +1 s grace (the cool-down between runs is 2 s; timestamps have 1 s resolution)

def secs(hms): h, m, s_ = (int(x) for x in hms.split(":")); return h*3600 + m*60 + s_

def bench_windows(paths):
    """{(profile, conns): [run start seconds-of-day UTC]} from the timestamped benchmark logs (HH:MM:SS prefix)."""
    out = {}; cur = None
    for p in paths:
        try: lines = open(p, errors="replace").read().splitlines()
        except OSError: continue
        for l in lines:
            m = re.match(r"(\d\d:\d\d:\d\d) === \S+ / (\S+) / (\d+)c ", l)
            if m: cur = (m.group(2), int(m.group(3))); out.setdefault(cur, []); continue
            m = re.match(r"(\d\d:\d\d:\d\d) \[run (\d+)/(\d+)\]", l)
            if m and cur: out[cur].append(secs(m.group(1)))
    return out

H2C_PROFILES = {"baseline-h2c", "json-h2c"}   # served on 8082; every other profile of the entry is served on 8080

def switch_summary(entry, logs_dir, windows):
    """Per profile window of the Adaptive entry: the engine the LOADED server started on and every 'engine switch completed' line it
    logged, placed in the runs. The entry tags each server's logger with srv=<port> (8080 h1/ws, 8082 h2c): the other server's lines
    (idle in that window) are counted on their own and never credited to the loaded one."""
    lines = ["", f"[ADAPTIVE SWITCH LOG] {entry}: engine at start of the loaded server (8080; 8082 for the h2c profiles) and every switch it logged (celeris 'engine switch completed'), per profile window",
             "profile / conns          start engine  switches  where  [other server's switches]"]
    tot_sw = tot_in = nprof = other_tot = 0
    for logf in sorted(Path(logs_dir).glob(f"*/*/{entry}.log")):
        prof, conns = logf.parts[-3], int(logf.parts[-2])
        want = "8082" if prof in H2C_PROFILES else "8080"
        text = logf.read_text(errors="replace").splitlines()
        start = None; sw = []; other = 0; tagged = False
        for l in text:
            m = re.match(r"\d{4}/\d\d/\d\d (\d\d:\d\d:\d\d) INFO (.*)", l)
            if not m: continue
            t, msg = m.group(1), m.group(2)
            sm = re.search(r"\bsrv=(\d+)", msg); tagged = tagged or bool(sm)
            srv = sm.group(1) if sm else want                      # untagged line (older entry): treated as the loaded server
            if "adaptive engine listening" in msg and srv == want and start is None:
                a_ = re.search(r"active=(\S+)", msg); start = a_.group(1) if a_ else "?"
            if msg.startswith("engine switch completed"):
                if srv != want: other += 1; continue
                a_ = re.search(r"now_active=(\S+)", msg); sw.append((t, a_.group(1) if a_ else "?"))
        runs = windows.get((prof, conns), []); dur = DUR.get(prof, 5)
        where = []
        for t, to in sw:
            ts = secs(t); k = next((i+1 for i, r0 in enumerate(runs) if r0 - 1 <= ts <= r0 + dur + 1), None)
            where.append(f"{t} -> {to} " + (f"(inside run {k})" if k else ("(before the first run)" if runs and ts < runs[0] else "(between/after runs)" if runs else "(runs not placed)")))
            tot_in += 1 if k else 0
        tot_sw += len(sw); nprof += 1 if sw else 0; other_tot += other
        lines.append("%-24s %-13s %-9d %s%s%s" % (f"{prof} / {conns}", start or "?", len(sw), "; ".join(where) or "-",
                                                   f"  [other server: {other}]" if other else "", "" if tagged else "  [UNTAGGED log: server not identifiable]"))
    lines.append(f"profile windows with a switch on the loaded server: {nprof}; switches in total: {tot_sw}; inside a measured run: {tot_in}; switches on the idle server: {other_tot}")
    return lines

def engine_section(rows, engine_names, L, logs_dir, bench_logs):
    ad = engine_names[0]; ents = engine_names
    eff = compute_eff(rows, ents, L)
    print("\n[ENGINE VARIANTS] %s = Adaptive (Engine set explicitly), then the other entries (celeris-default = Engine unset, the control; epoll/io_uring pinned). Per profile: score 0-1000 against the best of these entries (1000 = best), best named; '-' = not run" % ad)
    print("%-18s" % "profile" + "".join("%-18s" % e for e in ents) + "best             %s/best" % ad)
    sums = {e: 0.0 for e in ents}; wins = {e: 0 for e in ents}
    for fam, profs in FAMILIES.items():
        for pid, _ in profs:
            vals = {e: eff.get((pid, e)) for e in ents if eff.get((pid, e))}
            if not vals: continue
            best = max(vals.values()); win = max(vals, key=vals.get)
            wins[win] += 1
            cells = ""
            for e in ents:
                v = vals.get(e); sc = (v/best*1000) if v else None
                if sc is not None: sums[e] += sc
                cells += "%-18s" % ("-" if sc is None else f"{sc:.1f}")
            print("%-18s%s%-16s %s" % (pid, cells, win, ("%.3f" % (vals[ad]/best)) if ad in vals else "-"))
    print("%-18s" % "SUM over profiles" + "".join("%-18s" % f"{sums[e]:.1f}" for e in ents) + "wins: " + ", ".join(f"{e} {wins[e]}" for e in ents))
    if logs_dir:
        for l in switch_summary(ad, logs_dir, bench_windows([b for b in bench_logs if b])): print(l)

main()
