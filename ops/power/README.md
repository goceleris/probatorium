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
   `drain-acknowledged` plus `drained-remaining.json` (the cells that never
   ran) and marks the remainder interrupted.
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
`/tmp/actions-runner-<host>/`. That is correct: the 2026-08-17 post-mortem
found msa2-client's QLC NVMe killed by a 29:1 write:read ratio, and moving
bench artefacts off the disk is part of the fix.

But it means a power-loss reboot destroys a partially-completed run. The
snapshot is the narrow exception: per-cell JSON only, only on an actual power
event, single-digit MB against a drive rated in hundreds of TBW. It keeps five
snapshots and prunes the rest.

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
