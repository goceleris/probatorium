# Power-loss guard

Keeps a bench or validation run recoverable when mains fails, and keeps the
cluster from being cut mid-write.

## Hardware

APC **Back-UPS BE1050G2** (1050 VA / **600 W nominal**). The data cable is
RJ45-at-the-UPS to USB-at-the-host, plugged into **msa2-client**.

> The RJ45 socket on the UPS is **not Ethernet**. It carries APC's serial
> signalling; plugging it into a switch or a NIC can damage the port.

## Runtime budget — measure it, do not assume it

Measured 2026-09-09: `TIMELEFT 46.2 min` at `LOADPCT 15%` (~90 W) — that is
the cluster **idle**, and implies only **~69 Wh usable**.

| draw | projected | after Peukert derating |
|---|---|---|
| 90 W (idle) | 46 min | — measured |
| 200 W | ~21 min | ~16–18 min |
| 300 W | ~14 min | ~11–12 min |
| 400 W | ~10 min | ~8 min |

**Under a real benchmark expect ~8–17 minutes, not 40.** Validation cells run
~150 s and bench cells ~5 min, so draining the current cell is affordable; a
"finish anything under 20 minutes" rule is not.

In **rated** mode one cell is a saturation pass *plus* one pass per
`RatedFractions` entry (`runRatedSweep`), so a drained cell waits for all of
them. That is correct — a drain landing between passes would leave a
half-populated `RatedSamples` set — and it is why the sentinel is checked at
the top of the schedule loop rather than inside `executeCell`.

`cluster-power-event` records `LOADPCT`/`TIMELEFT` into
`/var/lib/celeris-power/power-events.jsonl` on every `onbattery`. That is the
only place the loaded figure is ever observed — read it after the first real
outage and re-tune the thresholds below.

## apcupsd topology

| host | role | thresholds |
|---|---|---|
| msa2-client (192.168.50.195) | master, owns the USB cable, serves NIS on 3551 | `BATTERYLEVEL 20`, `MINUTES 4` |
| msr1 | NIS slave | `BATTERYLEVEL 30`, `MINUTES 6` |
| msa2-server | NIS slave | `BATTERYLEVEL 30`, `MINUTES 6` |

Slaves shut down **first** by design — the host holding the cable must be last
or it cannot tell the others to go. `ONBATTERYDELAY 30` rides through blips.

Two apcupsd gotchas, both of which cost debugging time:

1. The config's **first line must be literally `## apcupsd.conf v1.1 ##`**, or
   the daemon warns and half-ignores it.
2. `NETSERVER off` on a slave makes local `apcaccess` return **nothing** (it
   queries `127.0.0.1:3551`). The slave still tracks the master fine; we set
   `NETSERVER on` + `NISIP 127.0.0.1` on slaves purely so `apcaccess` works
   for diagnostics.

## What happens on mains loss

Every host observes `ONBATT` independently — no SSH fan-out, which would add a
network dependency exactly when the network may be going away.

1. `onbattery` → `cluster-power-event` writes `/run/celeris/drain-requested`
   and appends the load snapshot to `power-events.jsonl`.
2. The runner checks that sentinel **at the top of each cell iteration**, so
   the cell that was running completes and flushes first. It then writes
   `drain-acknowledged` plus `drained-remaining.json` — which carries both the
   unrun cells and a ready-to-paste `cells_glob` in `BENCH_CELLS` form
   (`<scenario>/<server>`, comma separated) — and marks the remainder
   interrupted.

   Because the runner exits *cleanly*, ansible proceeds to its results
   collection play. That play is `any_errors_fatal` and only runs at the end
   of a grid, so a run killed by a power cut fetches **nothing at all** —
   which is precisely the difference a graceful drain buys.
3. If mains returns before the runner has acknowledged, `offbattery` cancels
   the drain. Once acknowledged, the drain is allowed to finish — half-resuming
   is more surprising than stopping.
4. apcupsd's own thresholds are the **backstop**, not the mechanism: if the
   runner is absent or ignores the sentinel, the shutdown still happens.
5. `doshutdown` → `cluster-power-snapshot` copies the per-cell results off
   tmpfs into `/var/lib/celeris-power/partial/<stamp>/` before the reboot
   destroys them.

### Why the snapshot exists

`ansible/inventory.yml` puts all transient bench state on tmpfs on purpose —
`results_root: /tmp/celeris-results`, and the Actions runner lives under
`/tmp/actions-runner-<host>/`. **`/tmp` is RAM-backed on all three hosts, so a
power cut destroys in-flight results instantly — there is no post-hoc
recovery.** That is why the snapshot runs from `doshutdown`, while the machine
is still up on battery. That is correct: the 2026-08-17 post-mortem
found msa2-client's QLC NVMe killed by a 29:1 write:read ratio, and moving
bench artefacts off the disk is part of the fix.

But it means a power-loss reboot destroys a partially-completed run. The
snapshot is the narrow exception: per-cell JSON only, only on an actual power
event, single-digit MB against a drive rated in hundreds of TBW. It keeps five
snapshots and prunes the rest.

## Reclaiming the space

A snapshot exists only to bridge a reboot. Once its contents are back on tmpfs
— or explicitly abandoned — keeping the copy on `/var/lib` is pure
accumulation, on the same disk the tmpfs policy exists to protect.

```
cluster-power-restore --list      # what is held, changes nothing
cluster-power-restore             # restore the newest snapshot, then DELETE it
cluster-power-restore --discard   # delete everything without restoring
```

Both restore and discard free the space; there is no path that consumes a
snapshot and leaves it behind. A restore that fails part-way **keeps** the
snapshot and exits non-zero — deleting the source of a move that did not
finish would destroy the evidence it failed to move.

Restore reads `snapshot-manifest.tsv` (stored-name → original-path) written at
snapshot time, rather than trying to un-mangle a flattened directory name, so
the mapping stays explicit if the naming scheme ever changes.

Backstops, because "meant to be consumed" is not a guarantee:

- at most **3** snapshots retained, oldest pruned first
- anything older than **30 days** dropped regardless of count
- `/var/log/cluster-power-event.log` and `power-events.jsonl` are rotated by
  `/etc/logrotate.d/celeris-power` (6 and 12 months respectively) — both are
  append-only and nothing else prunes them

A storm week nobody follows up on therefore costs a bounded, small amount of
disk instead of an unbounded pile.

## Resuming an interrupted run

Two halves: the runner/validator can *finish* a partial matrix, and the host
decides *when* it is safe to ask for that.

### Finishing a partial matrix

```
probatorium-runner  -resume-from <dir>        # bench   (pass the same path as -out)
validator           -matrix-resume-from <dir> # nightly/soak
```

Both drop cells that already reached a final verdict and run the rest. The
completeness test is status-aware, which is the crux: an interrupted run
writes a per-cell record for cells that never ran, so "a record exists" would
skip exactly the cells a resume must run. Only real verdicts count —
`ok`/`suspect`/`not_applicable` for the bench, evidence of requests or a
property verdict for validation. `dnf`, empty and unrecognised all re-run,
because a wasted measurement window costs minutes while a wrongly skipped cell
hands the absolute gate a matrix with a hole in it.

The `matrix-nightly-tier` and `matrix-weekend-tier` workflows take a
`resume_from` input. `auto` restores the newest rescued snapshot on the
cluster host and continues it; a path resumes from that directory.

### Deciding when to resume

`cluster-power-resume`, on a 5-minute timer, on the UPS master only (three
hosts racing three dispatches for one run would be worse than not resuming).

| interlock | default | why |
|---|---|---|
| mains back | `ONLINE` | obvious |
| mains stable | 15 min | a flapping supply resets the clock, so it can never accumulate credit |
| battery | ≥ 80 % | enough headroom to survive the *next* cut, not just start |
| attempts | ≤ 3 | a run that cannot survive three tries needs a person, not a fourth |

```
cluster-power-resume --status    # what it sees
cluster-power-resume --dry-run   # evaluate, dispatch nothing
```

Dispatch needs a token with `actions:write` at
`/etc/celeris-power/github-token` (0600 root). **Without it the interlocks
still run and the script prints the exact command** — it degrades to telling a
human rather than failing shut.

The pending marker is cleared by `cluster-power-restore` when the resumed run
actually restores the snapshot, not at dispatch time: clearing it early would
lose the rescued state if the dispatch never produced a run.

## On the way back up

`celeris-resume-check.service` runs once at boot and **reports**; it does not
restart anything. During a storm, mains can cut repeatedly, and an
unconditional auto-resume turns that into a thrash loop that starts a 74-hour
benchmark, dies twenty minutes in, and starts again. Resuming wants a
stable-power precondition and a deliberate decision.

```
ls -lt /var/lib/celeris-power/partial/     # rescued results
cat    /var/lib/celeris-power/resume-pending.json
cat    /var/lib/celeris-power/power-events.jsonl
```

## Deploying

```
ansible-playbook -i ansible/inventory.yml ansible/power-guard.yml
```

Persistent host provisioning, unlike `deploy.yml`/`bench.yml` whose state is
meant to vanish on reboot. Re-run whenever `ops/power/*` changes.

## Testing it

Pull the UPS mains plug for ~10 s with the cluster idle. `STATUS` should flip
to `ONBATT` and back to `ONLINE`; the 30 s `ONBATTERYDELAY` means nothing
else triggers. Confirm with `apcaccess status` and
`/var/log/cluster-power-event.log`.
