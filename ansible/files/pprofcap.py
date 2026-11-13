#!/usr/bin/env python3
"""pprofcap.py: the in-window collector of the v1.6.0 attribution campaign's PPROF-1 (harness change H7' of attribution-plan-20261104.md, Revision 2).

Runs on the SUT host next to ONE bench column whose SUT env carries PROBATORIUM_PPROF=dump|profile|both (servers/celeris/pprof_sidecar.go mounts net/http/pprof on the loopback
side listener PROBATORIUM_DEBUG_ADDR, nothing on the benched engine). ansible/tasks/run_bench_cell.yml starts it before the runner, only when that env is set, and everything it
writes lands in the cell directory, which ansible/bench.yml tars and fetches with the rest of the run (so it ends up in the benchmark-tier-results artifact).

  mode profile: a CPU profile (default 30 s) centred in the measurement window, then a binary goroutine profile  ->  cpu.pb.gz, goroutine.pb.gz      (celeris#857)
  mode dump:    a goroutine?debug=2 text dump after a SECOND WITH ZERO COMPLETIONS inside the window            ->  pprofcap.events.jsonl, goroutine-dump-<n>.txt   (celeris#854)
  mode both:    both. The CPU profile is in-process SIGPROF (its overhead is not measured and is not claimed anywhere); no kernel setting is touched, perf_event_paranoid stays as it is.

"Second with zero completions". The SUT cannot see the loadgen's completions, so the trigger reads the engine's own request counter from /debug/vars once a second
(celeris.engine_metrics.RequestCount) together with ActiveConnections. The loadgen is closed-loop (one request outstanding per connection): a connection sends its next request only
after the previous one completed, so a second in which RequestCount did not move although connections are open (ActiveConnections > 0 at both ends of the second) is a second in which
no request completed and none was issued. This is a PROXY for the loadgen's "no completed request" second (the counter counts requests the engine took up, not responses written; an
async engine counts at the feed), and the PREREG states it. The dump stops the world, so it is taken ONLY AFTER the zero-completion second has been counted and written to the events
file; the sampler then re-baselines (the dump itself must not be read as the next zero second). At most --max-dumps dumps, at least --min-gap seconds apart, each bounded by --dump-timeout.

The window: rendezvous on the first sample with ActiveConnections >= --min-conns (the same event the stall captures use, ssh-free); window = [t0 + warm, t0 + warm + dur].

Durable evidence (review r5 P4): pprofcap.meta.json is written atomically at the start (outcome "started"), after the rendezvous ("running") and at the end; SIGTERM (the teardown task
stops a slow collector after 45 s) is turned into an orderly exit that writes the meta with outcome "terminated: ...". An armed column without a meta whose outcome is "done" is
CAPTURE-MISSING (reported by the reader; it does not void the bench numbers).

Request/response guard (review r5 P3): the zero-second proxy reads the engine request counter, which only moves under request/response load. The trigger measures the counter over the
warmup (rendezvous to window start); below --min-warmup-requests (default 100) it writes outcome note "trigger_skipped" and never dumps, so a WebSocket or SSE window (a flat counter with
open connections) cannot collect dumps.

Nothing here fails the column: every error becomes a line in pprofcap.meta.json and the process exits 0. A SUT without the gate (404 on /debug/pprof/) or without a side listener (a non-Go
peer in the same dispatch) is recorded as "skipped".
usage: pprofcap.py --addr HOST:PORT --mode dump|profile|both --out DIR --warm S --dur S [--pid N] [--guard S] [--profile-seconds 30] [--min-conns 16] [--max-dumps 3] [--min-gap 15]
Tested by workflow_pprofcap_test.go (the real script against a scripted fake side listener).
WHICH WINDOW: exactly one, the FIRST measurement window after the rendezvous (the first sample with ActiveConnections >= --min-conns). The runner runs a column's scenarios in name order, so
a column watched by this script must run ONE scenario: the armed c1pp-* columns (servers/c1pp_arms.go) are dispatched with cells=<cell>/c1pp-<mode>-*, and servers/c1pp_arms_test.go proves
that this glob selects exactly one scenario per column. A rated sweep with several passes is watched in its FIRST pass only; a matched-rate profile (celeris#857) needs a single-pass
fixed-rate cell, fixed in the PREREG.
"""
import argparse
import json
import os
import signal
import sys
import threading
import time
import urllib.error
import urllib.request

MAX_DUMP_BYTES = 16 << 20


class Collector:
    def __init__(self, a):
        self.a = a
        self.base = "http://" + a.addr
        self.meta = {"mode": a.mode, "addr": a.addr, "outcome": "started", "files": [], "errors": [], "samples": 0, "zero_seconds": 0, "dumps": 0, "sample_errors": 0}
        self.lock = threading.Lock()
        self.meta_lock = threading.Lock()
        self.t_end = time.monotonic() + a.guard
        self.rc = []  # (epoch, RequestCount, ActiveConnections)
        self.rendezvous_count = None

    # ---- plumbing
    def get(self, path, timeout, limit=None):
        with urllib.request.urlopen(self.base + path, timeout=timeout) as r:
            body = r.read(limit + 1 if limit else -1)
            return r.status, body

    def err(self, msg):
        with self.lock:
            self.meta["errors"].append("%.3f %s" % (time.time(), msg))

    def flush_meta(self):
        """atomic write of the meta as it stands (called at every phase: a killed collector leaves its last state). One writer at a time (meta_lock) and a tmp name per
        thread, so two workers (mode both) or a SIGTERM during a save can neither tear the file nor write an older snapshot over a newer one."""
        try:
            with self.meta_lock:
                with self.lock:
                    data = json.dumps(self.meta, indent=1, sort_keys=True).encode()
                p = os.path.join(self.a.out, "pprofcap.meta.json")
                tmp = "%s.%d.%d.part" % (p, os.getpid(), threading.get_ident())
                with open(tmp, "wb") as f:
                    f.write(data)
                    f.flush()
                    os.fsync(f.fileno())
                os.replace(tmp, p)
        except Exception:  # noqa: BLE001
            pass

    def save(self, name, data):
        p = os.path.join(self.a.out, name)
        tmp = "%s.%d.%d.part" % (p, os.getpid(), threading.get_ident())
        with open(tmp, "wb") as f:
            f.write(data)
        os.replace(tmp, p)
        with self.lock:
            self.meta["files"].append({"name": name, "bytes": len(data)})
        self.flush_meta()

    def vars(self, timeout=2.0):
        """-> (RequestCount, ActiveConnections, pid) or None"""
        try:
            _, b = self.get("/debug/vars", timeout)
            d = json.loads(b)
            em = d.get("celeris.engine_metrics") or {}
            return int(em["RequestCount"]), int(em["ActiveConnections"]), d.get("pid")
        except Exception as e:  # noqa: BLE001 - the collector must never raise into the bench
            with self.lock:
                self.meta["sample_errors"] += 1
            self.last_err = repr(e)
            return None

    def sleep_until(self, t):
        d = min(t, self.t_end) - time.monotonic()
        if d > 0:
            time.sleep(d)
        return time.monotonic() < self.t_end

    # ---- phases
    def attach(self):
        """wait for the side listener and the pprof mount; -> True when the collector can work"""
        a = self.a
        deadline = time.monotonic() + a.attach_timeout
        v = None
        while time.monotonic() < deadline and time.monotonic() < self.t_end:
            v = self.vars()
            if v is not None:
                break
            time.sleep(0.5)
        if v is None:
            self.meta["outcome"] = "skipped: no /debug/vars on " + a.addr + " (no side listener: a non-Go peer, or the port was taken)"
            return False
        if a.pid and v[2] != a.pid:
            self.meta["outcome"] = "skipped: /debug/vars answers for pid %s, the SUT is %s" % (v[2], a.pid)
            return False
        try:
            code, _ = self.get("/debug/pprof/", 3.0)
        except urllib.error.HTTPError as e:
            code = e.code
        except Exception as e:  # noqa: BLE001
            self.meta["outcome"] = "skipped: /debug/pprof/ unreachable: %r" % (e,)
            return False
        if code != 200:
            self.meta["outcome"] = "skipped: /debug/pprof/ answers %s (PROBATORIUM_PPROF is not on in the SUT)" % code
            return False
        return True

    def rendezvous(self):
        a = self.a
        deadline = time.monotonic() + a.rendezvous_timeout
        while time.monotonic() < deadline and time.monotonic() < self.t_end:
            v = self.vars()
            if v is not None and v[1] >= a.min_conns:
                self.rendezvous_count = v[0]
                return time.monotonic()
            time.sleep(0.2)
        return None

    def profile(self, start):
        a = self.a
        secs = int(min(a.profile_seconds, a.dur - 2))
        if secs < 1:
            self.err("window of %ss is too short for a CPU profile" % a.dur)
            return
        t = start + max(0.5, (a.dur - secs - 1.5) / 2.0)  # centred; the goroutine profile follows inside the window
        if not self.sleep_until(t):
            return
        t0 = time.time()
        try:
            code, body = self.get("/debug/pprof/profile?seconds=%d" % secs, secs + 20.0)
            if code == 200 and len(body) > 10 and body[:2] == b"\x1f\x8b":
                self.save("cpu.pb.gz", body)
                with self.lock:
                    self.meta["cpu_profile"] = {"seconds": secs, "started_epoch": t0, "ended_epoch": time.time()}
            else:
                self.err("cpu profile: status %s, %d bytes" % (code, len(body)))
        except Exception as e:  # noqa: BLE001
            self.err("cpu profile: %r" % (e,))
        try:
            code, body = self.get("/debug/pprof/goroutine", 15.0)
            if code == 200 and body[:2] == b"\x1f\x8b":
                self.save("goroutine.pb.gz", body)
                with self.lock:
                    self.meta["goroutine_profile_epoch"] = time.time()
            else:
                self.err("goroutine profile: status %s, %d bytes" % (code, len(body)))
        except Exception as e:  # noqa: BLE001
            self.err("goroutine profile: %r" % (e,))

    def trigger(self, start, end):
        a = self.a
        if not self.sleep_until(start):
            return
        events = os.path.join(a.out, "pprofcap.events.jsonl")
        # request/response guard: the counter must have moved over the warmup, or this is not a load the proxy can read
        w = self.vars()
        if w is not None and self.rendezvous_count is not None and w[0] - self.rendezvous_count < a.min_warmup_requests:
            with self.lock:
                self.meta["trigger_skipped"] = "the request counter moved by %d over the warmup (floor %d): not a request/response load" % (w[0] - self.rendezvous_count, a.min_warmup_requests)
            self.flush_meta()
            return
        prev, prev_t = None, None
        last_dump = -1e9
        nxt = time.monotonic()
        while time.monotonic() < min(end, self.t_end):
            cur = self.vars()
            now_m, now_e = time.monotonic(), time.time()
            if cur is None:
                prev = None  # a failed sample is not a zero: re-baseline
            else:
                with self.lock:
                    self.meta["samples"] += 1
                    self.rc.append((now_e, cur[0], cur[1]))
                if prev is not None and cur[0] == prev[0] and cur[1] > 0 and prev[1] > 0:
                    with self.lock:
                        self.meta["zero_seconds"] += 1
                    ev = {"epoch": now_e, "second_start_epoch": prev_t, "request_count": cur[0], "active_connections": cur[1], "dump": None}
                    # the counted second is on disk BEFORE the world is stopped
                    self.append_event(events, ev)
                    if self.meta["dumps"] < a.max_dumps and now_m - last_dump >= a.min_gap:
                        n = self.meta["dumps"] + 1
                        ev2 = dict(ev)
                        try:
                            code, body = self.get("/debug/pprof/goroutine?debug=2", a.dump_timeout, MAX_DUMP_BYTES)
                            trunc = len(body) > MAX_DUMP_BYTES
                            body = body[:MAX_DUMP_BYTES]
                            if code == 200:
                                name = "goroutine-dump-%d.txt" % n
                                self.save(name, body)
                                ev2["dump"] = {"file": name, "bytes": len(body), "truncated": trunc, "taken_epoch": time.time()}
                                with self.lock:
                                    self.meta["dumps"] = n
                            else:
                                ev2["dump"] = {"error": "status %s" % code}
                        except Exception as e:  # noqa: BLE001
                            ev2["dump"] = {"error": repr(e)}
                        last_dump = time.monotonic()
                        self.append_event(events, {"dump_result_for": now_e, **ev2})
                        prev = None  # the dump stopped the world: do not read its own pause as the next zero second
                        nxt = time.monotonic() + 1.0
                        self.sleep_until(nxt)
                        continue
                prev, prev_t = cur, now_e
            nxt += 1.0
            if nxt < time.monotonic():  # fell behind (a slow sample): do not burst
                nxt = time.monotonic() + 1.0
            self.sleep_until(nxt)

    def append_event(self, path, ev):
        with open(path, "a") as f:
            f.write(json.dumps(ev, sort_keys=True) + "\n")
            f.flush()
            os.fsync(f.fileno())

    def run(self):
        a = self.a
        os.makedirs(a.out, exist_ok=True)
        try:
            with open(os.path.join(a.out, "pprofcap.pid"), "w") as f:
                f.write(str(os.getpid()))
            self.flush_meta()
            if not self.attach():
                return
            t0 = self.rendezvous()
            if t0 is None:
                self.meta["outcome"] = "skipped: no rendezvous (ActiveConnections never reached %d within %ss)" % (a.min_conns, a.rendezvous_timeout)
                return
            start = t0 + a.warm
            end = start + a.dur
            self.meta.update(rendezvous_epoch=time.time() - (time.monotonic() - t0), window_start_in_s=a.warm, window_s=a.dur, outcome="running")
            self.flush_meta()
            ths = []
            if a.mode in ("profile", "both"):
                ths.append(threading.Thread(target=self.profile, args=(start,), daemon=True))
            if a.mode in ("dump", "both"):
                ths.append(threading.Thread(target=self.trigger, args=(start, end), daemon=True))
            for t in ths:
                t.start()
            for t in ths:
                t.join(max(1.0, self.t_end - time.monotonic()))
            self.meta["outcome"] = "done"
        except BaseException as e:  # noqa: BLE001 - SystemExit from the SIGTERM handler lands here too
            self.meta["outcome"] = ("terminated: %s" % (e,)) if isinstance(e, SystemExit) else "error: %r" % (e,)
        finally:
            if self.meta["outcome"] in ("started", "running"):
                self.meta["outcome"] = "terminated: ended while " + self.meta["outcome"]
            try:
                with self.lock:
                    rc = list(self.rc)
                if rc:
                    self.save("pprofcap.requests.csv", ("epoch,request_count,active_connections\n" + "".join("%.3f,%d,%d\n" % r for r in rc)).encode())
                self.flush_meta()
            except Exception:  # noqa: BLE001
                pass


def parse(argv):
    p = argparse.ArgumentParser()
    p.add_argument("--addr", required=True)
    p.add_argument("--mode", required=True, choices=("dump", "profile", "both"))
    p.add_argument("--out", required=True)
    p.add_argument("--warm", type=float, required=True)
    p.add_argument("--dur", type=float, required=True)
    p.add_argument("--pid", type=int, default=0)
    p.add_argument("--guard", type=float, default=3600.0)
    p.add_argument("--profile-seconds", type=float, default=30.0)
    p.add_argument("--min-conns", type=int, default=16)
    p.add_argument("--max-dumps", type=int, default=3)
    p.add_argument("--min-gap", type=float, default=15.0)
    p.add_argument("--dump-timeout", type=float, default=20.0)
    p.add_argument("--min-warmup-requests", type=int, default=100)
    p.add_argument("--attach-timeout", type=float, default=30.0)
    p.add_argument("--rendezvous-timeout", type=float, default=240.0)
    return p.parse_args(argv)


def main(argv):
    a = parse(argv)

    def on_term(signum, _frame):  # the teardown task sends SIGTERM to a collector that is still attaching or waiting for the rendezvous
        raise SystemExit("signal %d" % signum)

    signal.signal(signal.SIGTERM, on_term)
    Collector(a).run()
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
