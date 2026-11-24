#!/usr/bin/env python3
"""Per-run latency table for the WebSocket profiles (and any gcannon profile), from the benchmark logs.

Prints, per entry/profile/conns and per run: mean, p50, p90, p99, p99.9, samples, overflow (>5 s) count and share, and
flags every run whose p99 is BELOW its mean with the reason. gcannon v0.8.4 does not print min or max (its histogram keeps
no min/max; --json has avg/p50/p90/p99/p999/samples only), so min is shown as n/a and max as '>=5.00 s' when the overflow
counter is non-zero (the tail is clamped at 5.00 s in every percentile), else 'n/a'.
usage: ws_hist.py LOG... (the logs run-preview.sh bench() writes, with or without its HH:MM:SS prefix, e.g. logs/engines.celeris.log logs/supp.fiber-websocket.log)
"""
import re, sys

TS = re.compile(r"^\d\d:\d\d:\d\d ")

def us(v):
    m = re.match(r"([\d.]+)\s*(us|ms|s)$", v)
    return float(m.group(1)) * {"us": 1, "ms": 1e3, "s": 1e6}[m.group(2)]

def fmt(x):
    return "%.0fus" % x if x < 1e3 else ("%.2fms" % (x / 1e3) if x < 1e6 else "%.2fs" % (x / 1e6))

def parse(path):
    blk, run, rows = None, None, []
    cur = {}
    def flush():
        nonlocal cur
        if blk and cur.get("avg") is not None:
            rows.append((blk, run, cur))
        cur = {}
    for ln in open(path, errors="replace"):
        ln = TS.sub("", ln.rstrip("\r\n")) + "\n"   # run-preview.sh's bench() prefixes every log line with "HH:MM:SS " (UTC); old logs have none
        m = re.match(r"=== (\S+) / (\S+) / (\d+)c", ln)
        if m:
            flush(); blk = (m.group(1), m.group(2), int(m.group(3))); run = None; continue
        m = re.match(r"\[run (\d+)/(\d+)\]", ln)
        if m:
            flush(); run = int(m.group(1)); continue
        m = re.match(r"\s+Latency\s+(\S+)\s+(\S+)\s+(\S+)\s+(\S+)\s+(\S+)\s*$", ln)
        if m:
            cur.update(avg=us(m.group(1)), p50=us(m.group(2)), p90=us(m.group(3)), p99=us(m.group(4)), p999=us(m.group(5)))
            continue
        m = re.match(r"\s+Latency samples: (\d+) /", ln)
        if m: cur["samples"] = int(m.group(1)); continue
        m = re.match(r"\s+Latency overflow \(>5s\): (\d+)", ln)
        if m: cur["overflow"] = int(m.group(1)); continue
        m = re.match(r"\s+Throughput: (\S+) req/s", ln)
        if m: cur["thr"] = m.group(1)
    flush()
    return rows

print("%-16s %-17s %6s %3s  %9s %8s %8s %8s %8s  %10s %9s %7s  %-6s %-8s %s" % (
    "entry", "profile", "conns", "run", "mean", "p50", "p90", "p99", "p99.9", "samples", "overflow", "ovf%", "min", "max", "flag"))
for path in sys.argv[1:]:
    for (e, p, c), run, d in parse(path):
        ovf = d.get("overflow", 0); n = d.get("samples", 0) or 1
        flag = ""
        if d["p99"] < d["avg"]:
            flag = "P99<MEAN: %d responses (%.2f%%) took >5 s (clamped at 5.00 s in the percentiles, counted in full in the mean); the other %.1f%% are fast (p50 %s): a bimodal tail, not a bug in the numbers" % (
                ovf, 100.0 * ovf / n, 100 - 100.0 * ovf / n, fmt(d["p50"])) if ovf else "P99<MEAN, no overflow: the mean is carried by the tail above p99 (p99.9 = %s, %.0fx the p99); fast mode p50 %s" % (fmt(d["p999"]), d["p999"] / max(d["p99"], 1), fmt(d["p50"]))
        print("%-16s %-17s %6d %3s  %9s %8s %8s %8s %8s  %10d %9d %6.2f%%  %-6s %-8s %s" % (
            e, p, c, run, fmt(d["avg"]), fmt(d["p50"]), fmt(d["p90"]), fmt(d["p99"]), fmt(d["p999"]), n, ovf, 100.0 * ovf / n,
            "n/a", ">=5.00s" if ovf else "n/a", flag))
