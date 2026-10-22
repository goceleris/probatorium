<p align="center">
  <img src=".github/cover.png" alt="goceleris / probatorium cover: probatorium, benchmark and production-readiness proving ground (bench, matrix-validate, race, checkptr, cluster), beside a grid of check marks">
</p>

<p align="center">
  <a href="https://github.com/goceleris/probatorium/actions/workflows/test.yml?query=branch%3Amain"><img src="https://github.com/goceleris/probatorium/actions/workflows/test.yml/badge.svg?branch=main" alt="Test status on main"></a>
  <a href="https://github.com/goceleris/probatorium/actions/workflows/lint.yml?query=branch%3Amain"><img src="https://github.com/goceleris/probatorium/actions/workflows/lint.yml/badge.svg?branch=main" alt="Lint status on main"></a>
  <a href="https://codecov.io/gh/goceleris/probatorium"><img src="https://codecov.io/gh/goceleris/probatorium/graph/badge.svg" alt="Codecov test coverage"></a>
  <a href="go.mod"><img src="https://img.shields.io/github/go-mod/go-version/goceleris/probatorium" alt="Go version from go.mod"></a>
  <a href="LICENSE"><img src="https://img.shields.io/github/license/goceleris/probatorium" alt="License: Apache-2.0"></a>
</p>

<p align="center">
  <strong>The benchmark and validation harness for <a href="https://github.com/goceleris/celeris">celeris</a>.</strong><br>
  It drives a real three-host cluster, x86 and arm64, and asks every celeris build two questions:
  how fast is it, and does it stay correct under stress?
</p>

<p align="center">
  <a href="#where-the-latest-results-live">Latest results</a> ·
  <a href="https://goceleris.dev/benchmarks/">Benchmark dashboard</a> ·
  <a href="https://goceleris.dev/methodology/">Methodology</a> ·
  <a href="#reproduce-a-run">Reproduce a run</a>
</p>

## What it proves

probatorium runs two kinds of tier on the same cluster: bench tiers measure, validation tiers judge.

- **Speed (the bench tier).** celeris runs side by side with a field of 29 other frameworks in 10
  languages and the Go `net/http` baseline: 52 server columns across up to 29 scenarios, 813 cells. The
  load comes from [loadgen](https://github.com/goceleris/loadgen). Besides saturation throughput, the
  headline metric is **`latency_at_slo`**: the highest request rate whose P99 stays within each latency
  budget. Rated runs are published to [goceleris.dev](https://goceleris.dev/benchmarks/).
- **Correctness (the validation tiers).** Every reference app (8 of them, covering celeris's middleware,
  streaming and database drivers) runs on every engine (`iouring`, `epoll`, `std`, `adaptive`) on both
  architectures: 64 cells per full matrix. Each cell is hit with session-shaped traffic, malformed
  HTTP/1.1, h2c-upgrade churn, WebSocket frame torture, killed SSE streams and a replayed fault
  schedule, while a per-second property loop checks invariants such as connection accounting, heap,
  goroutine and RSS growth, and unexpected panics. `mage ValidateGate` fails the run on any violation,
  dead cell or unexercised oracle. `mage ValidateDiff` fails it when one engine or one architecture
  behaves differently from the others.
- **The same checks under instrumented builds.** Race Validation rebuilds the reference apps with
  `-race`; Checkptr Validation rebuilds them with the pointer checker, which also compiles in celeris's
  own io_uring SQE checks.
- **Flake rates, off the cluster.** Celeris Stress runs celeris's own tests many times on GitHub-hosted
  x86 and arm64 runners and reports, per test and architecture, the per-process failure rate with an
  exact 95 % interval ([`docs/STRESS.md`](docs/STRESS.md)).

## Where the latest results live

Every cluster tier is a GitHub Actions workflow in this repository. Its run page is the latest result:
the gate steps (`mage ValidateGate`, `mage ValidateDiff`) give the verdict, and the full result tree is
attached to the run as an artifact.

> [!NOTE]
> **The tiers are dispatched by hand.** Their schedules have been paused since
> [#380](https://github.com/goceleris/probatorium/pull/380) (2026-09-15) to keep the cluster free for
> the v1.6.0 release validation; [`workflow_schedule_paused_test.go`](workflow_schedule_paused_test.go) pins that state. The run lists
> below show the most recent manual runs, not a nightly or weekly cadence.

| Tier (links to its runs) | What it runs | Result artifact on each run (kept for) |
| --- | --- | --- |
| [Nightly Validation](https://github.com/goceleris/probatorium/actions/workflows/matrix-nightly-tier.yml) | The full 64-cell matrix on both architectures | `matrix-nightly-results-<run id>` (30 days) |
| [Weekend Soak](https://github.com/goceleris/probatorium/actions/workflows/matrix-weekend-tier.yml) | The same matrix over 24 h, long enough to judge slow leaks | `matrix-weekend-results-<run id>` (90 days) |
| [Race Validation](https://github.com/goceleris/probatorium/actions/workflows/matrix-race-tier.yml) | The matrix with `-race` reference apps; it gates liveness and records the latency of a slow 64 KiB echo ([tier semantics](#race-tier-semantics-liveness-is-gated-latency-is-recorded)) | `matrix-race-results-<run id>` (30 days) |
| [Checkptr Validation](https://github.com/goceleris/probatorium/actions/workflows/matrix-checkptr-tier.yml) | The matrix with pointer-checker reference apps | `matrix-checkptr-results-<run id>` (30 days) |
| [PR Validation](https://github.com/goceleris/probatorium/actions/workflows/matrix-pr-tier.yml) | A 10-minute matrix on the amd64 host, for pull requests a maintainer labels `cluster-ok` | `matrix-pr-results-<run id>` (7 days) |
| [Benchmark Tier](https://github.com/goceleris/probatorium/actions/workflows/benchmark-tier.yml) | `mage BenchTier` over the whole grid | `benchmark-tier-results-<run id>` (90 days); rated runs are also published to the [benchmark dashboard](https://goceleris.dev/benchmarks/) |

Validation results are not published anywhere else: they are not in
[goceleris/docs](https://github.com/goceleris/docs) or on goceleris.dev. Every cluster run also keeps
a `cluster-forensics-*` and a `cluster-rescued-results-*` artifact for 90 days. How the benchmark
numbers are made is described on the [methodology page](https://goceleris.dev/methodology/).

## Reproduce a run

**Without the cluster.** Two parts of the harness are deterministic and run on any machine with Go:

```sh
# The bench grid: every (scenario, server) cell a full run schedules (813 of them), without running any.
go run ./cmd/runner -dry-run -cells '*/*' -runs 1

# A validation seed: a found bug is the tuple (seed, celeris commit, host arch), and the seed expands
# into the same workload and fault schedule every time.
go run ./cmd/validator-replay -seed=0x1 -commit="$(git rev-parse HEAD)" -target=msa2-server -dry-run
```

**On the cluster.** Everything else drives the three hosts through [mage](https://magefile.org) targets
and Ansible, so it needs Linux or macOS with `mage` and `ansible-playbook`, and SSH access to the
cluster in [`ansible/inventory.yml`](ansible/inventory.yml). `CLUSTER_USE_LAN=1` routes Ansible's SSH
over the 20G LACP fabric (`192.168.50.0/24`) instead of the Tailscale overlay and records that fabric
in the results; bench load always targets the hosts' LAN addresses.

```sh
# Cluster reachability and manifest state (read-only).
mage Status

# Cross-compile and ship the binaries. DEPLOY_COMPETITORS=go-only skips the native toolchains;
# DEPLOY_NEEDS_DBSERVICES=1 pulls the postgres/redis/memcached images the driver_* apps need.
CLUSTER_USE_LAN=1 DEPLOY_COMPETITORS=go-only DEPLOY_NEEDS_DBSERVICES=1 mage Deploy

# Smoke bench: two cells on the amd64 host.
CLUSTER_USE_LAN=1 \
  BENCH_TARGET=msa2-server \
  BENCH_CELLS='get-json/stdhttp-h1,get-json/gin-h1' \
  BENCH_DURATION=15s BENCH_WARMUP=3s \
  mage Bench

# Full validation matrix on the amd64 host: every reference app on every engine.
CLUSTER_USE_LAN=1 \
  VALIDATE_TARGET=msa2-server VALIDATE_DURATION=10m \
  VALIDATE_MATRIX=1 VALIDATE_DBSERVICES=1 \
  mage Validate

# Judge it the way the tiers do.
mage ValidateGate

# Return every host to its pristine state.
CLUSTER_USE_LAN=1 mage Cleanup
```

A maintainer can also start any tier from its workflow page (**Run workflow**) or with
`gh workflow run <workflow file> --repo goceleris/probatorium`. The tiers share one concurrency group,
so a new run queues behind the one on the cluster.

## The cluster

Three hosts, defined in [`ansible/inventory.yml`](ansible/inventory.yml):

| Host | Arch | Role |
| --- | --- | --- |
| msa2-client | amd64 | Load generator (loadgen) for the bench tier |
| msa2-server | amd64 | Server under test; in validation it runs the validator and the reference app |
| msr1 | arm64 | Server under test; in validation it runs the validator and the reference app |

The validator runs on the bench target itself, next to the reference app it drives
([`ansible/validate.yml`](ansible/validate.yml)); msa2-client holds no validation state.

### msr1 NIC interrupt placement

msr1 has two bonded Realtek r8127 ports (enp1s0, enp49s0) with one RX and one TX queue each and no RSS.
The bench pins the TX-completion vector of enp1s0 (interrupt name `enp1s0-8`; its IRQ number changes per boot)
to CPU 1 before every bench run on msr1. This is a host-tuning choice made by the maintainer on 2026-10-17
([goceleris/celeris#1036](https://github.com/goceleris/celeris/issues/1036)), not a measured fact about the
hardware:

- **What the evidence is.** It is exploratory and post hoc. X2: an extra loop on CPU 4 (where the vector sat)
  cleared a whole-port TX stall of about 200 ms in 8 of 8 runs. X3 (run 39060669852): 21 of 21 columns with the
  vector on a loop CPU were clean and 3 of 3 with it on CPU 4 stalled. But the pre-registered gate of that X3
  run failed and its row was INCONCLUSIVE, and the h-irq4to1 arm always ran last, so order is not separated
  from placement. The mechanism (a TX-completion vector serviced by a CPU without a loop stalls the port) is
  the working explanation, not an established one. A +35% rps seen with the vector on CPU 1 is relative to
  CPU 8 and is unexplained; do not quote it as an effect of this placement.
- **What CPU 1 is.** A loop CPU only in celeris's #1049 (C1) layout. The non-celeris columns have no loops
  there, so for them CPU 1 is simply the CPU the vector was put on, and the placement does not mean the same
  thing across engines.

- **Where.** `nic_irq_placement` on the msr1 entry of [`ansible/inventory.yml`](ansible/inventory.yml)
  (`iface`, `vector`, `cpu`), applied by [`ansible/tasks/nic_irq_placement.yml`](ansible/tasks/nic_irq_placement.yml)
  through [`ansible/files/nic_irq_pin.sh`](ansible/files/nic_irq_pin.sh) in the stage play of `bench.yml`.
  Empty or unset means do nothing: msa2-server and msa2-client never have it. A one-off run without it:
  `-e '{"nic_irq_placement": {}}'`. enp49s0's vector is not touched (the maintainer excluded it from the placement).
- **How.** The vector is found by name in `/proc/interrupts`, an explicit single CPU is written to
  `/proc/irq/N/smp_affinity_list`, and both `smp_affinity_list` and `effective_affinity_list` are read
  back. IFACE is bound to the vector: it is accepted when either (a) each IRQ found by that name is listed
  under `/sys/class/net/<iface>/device/msi_irqs/` or (b) the vector name starts with `<iface>-`, and refused
  (exit 2, nothing written) only when both fail; the record says which check passed (`binding`: `msi_irqs` or
  `name`). Either alone suffices because an out-of-tree driver may expose one of them differently. A readback that differs from the
  CPU fails the play before the first cell. It is a pin, not a
  restore: writing the old mask (0-11) back does not return the vector to its previous CPU, the kernel
  picks one from the mask. Applying it again is a no-op. Nothing is undone at teardown.
- **Record.** Every bench run on a host with the placement writes `nic-irq-placement.json` into its run
  directory (interface, vector, binding, irq, cpu, the affinity found before and read back after, and a second
  readback taken after the last cell) and prints the same in the log. `mage Bench` folds it into
  `environment.nic_irq_placement` of the results document and so into `env.json` (schema 5.21); a
  run without a placement has no such key.
- **Where it applies.** Only `bench.yml` pins. `mage Validate` (PR, nightly and checkptr tiers) and the stress
  playbooks do not, so on msr1 their vector placement depends on history: pinned if a bench ran since the
  boot, else the boot default. It is not recorded for them. irqbalance is not handled: nothing stops or
  configures it, so it can move the vector between the pin and the end of a run (the end-of-run readback
  below only detects that).
- **Invariant.** Every bench run records its placement, and a changed placement (another vector, another
  CPU, none) changes comparability: runs are compared only across the same `nic_irq_placement`. The docs
  site does not flag `nic_irq_placement` changes, so the first arm64 publish after this change crosses a
  placement change unflagged. An end-of-run readback that differs from the pin (something moved the vector
  mid-run) or that could not run at all (a become or transport failure) is logged as
  `WARNING: NIC IRQ placement DRIFTED` and recorded as `at_end.verified: false`; treat that run as unplaced.

Tests: `bash ansible/tests/test_nic_irq_pin.sh` (the shell logic against a fake `/proc` and `/sys`; also
runs in a plain container) and `bash ansible/tests/test_nic_irq_placement_play.sh` (the real tasks files
under ansible, run by the ansible CI job; `PROBATORIUM_ANSIBLE_TESTS=1 go test` runs it too). `go test` runs the first.

### Rebooting a cluster host

A host is rebooted by one sanctioned path and no other: the [`Cluster Reboot`](.github/workflows/cluster-reboot.yml)
workflow, which runs [`ansible/reboot.yml`](ansible/reboot.yml) from a GitHub-hosted machine on the tailnet (no
runner on the host, so none to strand). Nobody ssh-es in. Why you would: the since-boot interrupt counters (celeris's
rule D reads them; msr1's IRQ 135 is a 32-bit counter near its wrap) and leftover `/tmp` state. The maintainer's
standing approval (2026-10-20) covers reboots of msr1 needed for validation; report each one.

```
gh workflow run cluster-reboot.yml --repo goceleris/probatorium --ref main \
  -f host=msr1 -f confirm=REBOOT-msr1 -f reason="rule D counters before the final-sha bench-full"
```

- **Inputs.** `host` is `msr1` or `msa2-server` (`msa2-client` is not offered: it drives both targets), `confirm`
  must be exactly `REBOOT-<host>`, `reason` is one line recorded in the summary, and `allow_results_loss` (default
  false) lets a reboot go ahead although `results_root` holds entries. Dispatch from `main` only; it never runs on a schedule.
- **It refuses rather than queues.** A `cluster-guard` job outside the group runs `stresstally guard -idle`: anything
  running or waiting in `matrix-tier-cluster` (a tier, a stress run, another reboot) refuses the run, and the
  reboot job, which holds the group, also refuses an idle verdict older than ten minutes. The group keeps one running
  and one pending run and a run queued into it cancels the pending one, so a reboot must never wait there. The
  orchestrator's `HOLD-CLUSTER` marker lives outside this repository: the workflow cannot see it, so whoever
  dispatches honours it. Add `cluster-reboot.yml` to any dispatcher's list of cluster workflows.
- **It refuses, on the host, before touching anything:** any of the three hosts unreachable; a bench manifest on any
  of them; on the target a runner job executing, a power-loss resume pending, entries in `results_root` (tmpfs: the
  reboot destroys them), an r8127 NIC without the shutdown quiesce hook or without the module for the running
  kernel, tailscaled running but not enabled at boot (the host would come back off the tailnet), or a newer kernel
  installed than the one running (a decision for the maintainer: it changes comparability). The reasons are in the step
  summary.
- **It records, then reboots, then verifies.** Before: boot id, boot time, kernel, the NIC vectors found by name with
  their since-boot totals and IRQ affinity, bonds, crash dumps. Then `ansible.builtin.reboot` (bounded at 15 minutes),
  then a snapshot polled for up to 3 minutes. It FAILS unless the boot id changed, the kernel is the same, no new
  crash dump appeared, each power-guard prerequisite that held before holds again, the fabric address is configured
  and the loadgen answers over it, the bond has not lost a slave, `enp1s0-8` (what `bench.yml` pins) exists and the
  since-boot totals are small. The reconnect goes over the tailnet, so it also proves tailscale came back. The step
  summary and the `cluster-reboot-<host>-<run id>` artifact hold the before/after tables; the run is red unless the
  host was rebooted and verified.
- **What it does not do.** It restores nothing by hand: the IRQ affinity is recorded as the kernel left it. Only
  `bench.yml` re-applies the msr1 `nic_irq_placement` (enp1s0-8 to CPU 1) per run and reads it back; a soak, race or
  stress run on msr1 right after the reboot sees the kernel's placement. Runners are ephemeral and registered per
  run, so none is stranded, and the power guard is persistent (checked, not re-applied). The `/tmp` tool cache is
  gone: the next bootstrap is cold (the 25-minute timeout covers it).
- **If the host does not return.** msr1 has a documented shutdown panic in the out-of-tree r8127 driver
  ([`ops/power/quiesce-r8127.shutdown`](ops/power/quiesce-r8127.shutdown) works around it and is required before a
  reboot); a host that hangs needs a physical power-cycle. The run fails after 15 minutes with the phase reached
  (`rebooting`) in the summary. Do not dispatch anything on that host until it is back.

Tests: `go test -run 'ClusterReboot|RebootSnapshot|RebootJudge|RebootSummary|RebootIsDocumented'` (the workflow's safety
properties as text, the snapshot script against a fake `/proc`/`/sys`, the summary), `go test ./tools/stresstally`
(the idle guard), and `bash ansible/tests/test_reboot_judge.sh` (the real Jinja judgement under ansible) and `bash ansible/tests/test_reboot_play.sh` (the whole playbook on local
connections with only the restart faked), both run by the ansible CI job.

### Upgrading a cluster host's OS

The kernel, the OS packages and the firmware of a cluster host are changed by one sanctioned path and no other: the
[`Cluster OS Upgrade`](.github/workflows/cluster-os-upgrade.yml) workflow, which runs
[`ansible/os-upgrade.yml`](ansible/os-upgrade.yml) from a GitHub-hosted machine on the tailnet. Every command on a host
is a subcommand of [`ansible/files/os_upgrade.py`](ansible/files/os_upgrade.py) (the playbook has no shell task); its
header lists them. Nobody ssh-es in. Decision (maintainer, 2026-10-21): bring all three hosts to the latest versions
before the final release evidence; comparability with older releases does not matter.

```
# 1. read-only, all three hosts, no confirmation
gh workflow run cluster-os-upgrade.yml --repo goceleris/probatorium --ref main \
  -f mode=survey -f reason="platform survey before the OS upgrade"
# 2. ONE host per dispatch, servers first, the load generator last
gh workflow run cluster-os-upgrade.yml --repo goceleris/probatorium --ref main \
  -f mode=upgrade -f host=msa2-server -f confirm=UPGRADE-msa2-server -f reason="platform upgrade before the final evidence"
```

- **Inputs.** `mode` is `survey` or `upgrade`; an upgrade names ONE `host` and `confirm` must be exactly
  `UPGRADE-<host>` (`DISARM-<host>` for `mode=disarm`, which only clears a pending grub one-shot); `reason` is one line. `allow_results_loss` (default false) lets an upgrade go ahead although
  `results_root` holds entries (the reboot destroys the tmpfs). `refresh_lists` (survey only, default false) runs
  `apt-get update` first so the simulation is not stale: it WRITES `/var/lib/apt/lists` and the apt caches
  (`/var/cache/apt/*.bin`) and runs the apt update post-invoke hooks, so it is the one thing a survey changes, and only when asked. `blacklist_r8169`
  (upgrade only, default false) is explained below. Dispatch from `main` only; it never runs on a schedule.
- **Five modes.** `survey` (read-only, all hosts; its summary opens with a checklist of every fact to read before a host-changing
  dispatch, each with a PASS / FAIL / NOTE mark and the decision it informs); `rehearse` (ONE host, `REHEARSE-<host>`: pins the
  running kernel, arms a one-shot to a boot of the SAME kernel, reboots, and requires a new boot id, the same kernel, an EMPTY
  `next_entry` (GRUB read and cleared the environment block Linux writes) and the usual verification; no package or kernel changes;
  an `upgrade` is refused until the host carries a rehearsal record for the kernel it runs. **It is not free of kernel risk**: if
  GRUB does not read the environment block, the host boots GRUB's FIRST menu entry, on that boot and every power-cycle after it;
  that is the running kernel only when nothing newer is installed (msr1 today: -38 is installed and is entry 0, running is -31).
  The survey shows `boot.entry0_kernel`, and a rehearsal is refused while it differs from the running kernel unless the
  confirmation is `REHEARSE-<host>-CONSOLE`, which says someone is at the rack with a console); `upgrade` (ONE host, the sequence below); `disarm` (ONE host, `DISARM-<host>`:
  clears a pending grub one-shot, nothing else; the playbook also does it by itself on any failure between arming and the new
  kernel's boot while the host still answers); `repair` (ONE host, `REPAIR-<host>`, `repair_action` `configure`,
  `abort-new-kernel` or `release-holds`). Why `repair`: a DKMS module that does not build for the new kernel (r8127 on msr1) most likely fails the
  `linux-headers` postinst, so `apt-get` exits non-zero and leaves dpkg half-configured, and every later dispatch refuses on
  `dpkg --audit`. `configure` runs `dpkg --configure -a` (detached, same `policy-rc.d` discipline as the upgrade).
  `abort-new-kernel` first purges the kernel versions newer than the running one that the failed upgrade left half-installed
  (a kernel that was installed and working before is never removed), so the host stays on the old kernel. A repair never reboots
  and never touches grub's saved default, and refuses while a one-shot is pending. `abort-new-kernel` simulates first (`apt-get -s`):
  it stops on anything beyond the kernel metapackages, and when apt removes them with the image it says so (later full-upgrades
  install no kernel until they are installed again). It also removes a kernel that installed cleanly but that an earlier run armed
  and never verified (below); it forgets that record only when the purge exited 0 and dpkg shows the version gone (a failed purge keeps
  the record, so `retry_kernel` stays required). `release-holds` releases the access-path holds that a dead run left (below) and
  nothing else.
- **It refuses rather than queues**, exactly like the reboot: a `cluster-guard` job runs `stresstally guard -idle`, the
  job holds `matrix-tier-cluster`, an idle verdict older than ten minutes is refused, and the orchestrator's
  `HOLD-CLUSTER` marker lives outside this repository, so whoever dispatches honours it. The survey goes through the
  same gate: it runs commands on the hosts.
- **A kernel that stranded a host is not armed twice.** `arm-oneshot` leaves `/var/lib/celeris-os-upgrade/armed-<kernel>.json`; a
  verified boot removes it. After msr1's documented failure (the new kernel without a NIC, a power-cycle back to the old kernel) the
  record is still there: the next `upgrade` refuses to arm that kernel again unless dispatched with `retry_kernel=<kernel>`, and
  `repair` with `abort-new-kernel` accepts it for removal. A disarm that finds the host on the same boot (the reboot was never
  issued) drops the record, because that kernel never booted. `cluster-reboot` no longer refuses a host forever for "a newer kernel is
  installed", but accepts it only with proof: the drop-in exists, a rehearsal record exists for the running kernel with the grub and
  shim versions installed now, grub.cfg says `set default="${saved_entry}"`, the saved entry is the running kernel's normal (not
  recovery) Advanced entry and exists in grub.cfg, and no one-shot is pending. A host that was never rehearsed stays refused.
- **The access path can be held.** `hold_access_packages` (`auto`, `on`, `off`; `auto` = on for a host with an r8127 NIC) holds
  `grub-*`, `shim*`, `openssh-server` and `tailscale*` BEFORE the plan is simulated (so the protected-removal judgement describes the plan that runs) and for the length of the apt run,
  and releases only the holds this run put (a refusal releases them at once). If the run dies with the holds on, the survey lists them
  (`packages.tool_holds`), every upgrade refuses naming the repair, and `mode=repair repair_action=release-holds` releases them. Both
  kernels share those packages, so no fallback helps if one breaks: upgrade them in a separate dispatch (`hold_access_packages=off`)
  with a person at the rack and a console.
- **Order: servers first, the load generator last.** `msa2-client` is refused until `msa2-server` and `msr1` each carry
  a record of a verified upgrade (`/var/lib/celeris-os-upgrade/last-upgrade.json`, written only after verification)
  that still matches the kernel they run. Why: every server is verified against the client (fabric ping), so the
  client stays on the known-good platform while the servers are proven; the client's own verification then uses the
  upgraded servers. The two servers have no order between them; `msa2-server` (no out-of-tree driver) is the rehearsal
  to do before `msr1`.
- **Survey** (read-only; its commands are on an allow-list the process enforces) records per host: os-release, running
  and installed kernels, which kernel grub boots after a power-cycle, `GRUB_DEFAULT`, EFI and Secure Boot, what
  `apt-get -s full-upgrade` would install, remove and keep back, phased updates, held packages, third-party
  repositories and pins, unattended-upgrades, reboot-required, `dkms status`, the NIC drivers, PCI ids and link speeds,
  linux-firmware, disk, time sync, crash dumps, the run state, and the packages of the compilers and services. For the
  r8127 NIC it also records the out-of-tree module (version, where it came from), and, per installed kernel, whether the
  in-tree `r8169` lists the NIC's PCI id, so the summary shows whether the in-tree driver is already an option. A
  separate step compares the toolchains the roles install (pinned or floating) with upstream.
- **It refuses, on the host, before touching anything:** any of the three hosts unreachable; a bench manifest on any of
  them; on the target a runner job executing, a power-loss resume pending, entries in `results_root`, the r8127
  shutdown quiesce hook missing, the r8127 module not available for the running kernel, tailscaled not enabled at boot,
  too little disk on `/`, `/boot` and `/boot/efi`, dpkg or apt busy (an unattended upgrade) or half-configured
  packages, held kernel/boot/DKMS packages, an existing `policy-rc.d`, no usable GRUB, GRUB not PROVEN to have booted the host (`BOOT_IMAGE=` on the kernel command line naming the running kernel; on EFI the firmware's `BootCurrent` loader is `shim`/`grub` under `\EFI\<vendor>`; the ESP stub's `search`/`prefix` names the filesystem of `/boot/grub`; `grubenv` is a 1024-byte file: anything unknown counts as not proven), `GRUB_SAVEDEFAULT=true`, a
  `/boot/grub` on btrfs/zfs/lvm/mdraid (grub cannot make the boot one-shot there), no grub entry for the running kernel,
  a one-shot grub entry still pending, a saved default that differs from the running kernel after an earlier
  run (a second dispatch would move the fallback to an unverified kernel), `blacklist_r8169` with a NIC bound to
  `r8169`, a wrong confirmation, and the order rule. Each is a message in `vars/os_upgrade_judge.yml` with a test case.
- **Then, in this order.** (1) The *running* kernel becomes grub's saved default (`GRUB_DEFAULT=saved` in a drop-in, the
  old `/etc/default/grub` backed up, `update-grub`, `grub-set-default`) and its packages are marked manual, BEFORE a new
  kernel is installed: from then on a power-cycle at any moment boots the old kernel. (2) `apt-get update`, a simulation,
  and a refusal if apt would remove the kernel, the boot chain, DKMS, ssh, tailscale, the power guard or docker.
  (3) `apt-get full-upgrade`, detached (a dropped ssh connection cannot half-configure dpkg), non-interactive,
  `--force-confold`, with a temporary `policy-rc.d` so no service is restarted behind the run and `needrestart` in
  list-only mode; **never `autoremove`**, so the old kernel stays installed. (4) The DKMS modules are built for the new
  kernel (`dkms autoinstall -k`, make log kept) and checked: headers, initrd, grub entry, the r8127 module found by
  `modinfo -k`, its PCI aliases, its signature when Secure Boot is on, the shutdown hook, the fallback still intact.
  **If anything fails here the host is not rebooted**: the old kernel is the default and still runs. (5)
  `grub-reboot <new kernel>` (the next boot only; `saved_entry` stays the old kernel), `ansible.builtin.reboot` bounded
  at 15 minutes. (6) Verification, like the reboot plus: running kernel == the intended one (a fall-back to the old one is
  reported as such), each NIC's driver and link speed, time sync, no new failed unit, a C compiler that builds and runs,
  TLS to github.com. (7) **Only then** `grub-set-default <new kernel>` and the record. On any failure the new kernel is
  NOT made the default.
- **r8127 and the in-tree driver.** The upgrade keeps the out-of-tree r8127 behaviour. If the new kernel's `r8169` lists the
  same PCI id as r8127 and `r8169` is not blacklisted (in the new kernel's initramfs too: it must have been rebuilt after the blacklist), which driver binds the NIC would be a race, and the run refuses
  to reboot (whatever the running kernel's `r8169` lists). `blacklist_r8169=true` writes `/etc/modprobe.d/celeris-r8127.conf` (`blacklist r8169`) and rebuilds the
  initramfs of the new kernel, then goes on. Moving to the in-tree driver is a separate decision (PLAN.md of the
  upgrade lane lists what to test first); this workflow never makes it.
- **If the host does not return.** The summary names the phase and says whether to do a **physical power-cycle**: only when the
  run is red in phase `rebooting` and the host stays silent. What the power-cycle boots depends on why it is silent: (1) the
  NEW kernel booted without a NIC: grub already consumed the one-shot, so it boots the saved default, the OLD kernel; (2) the OLD
  kernel hung or panicked while shutting down (msr1's documented r8127 power-off panic) or the reboot was never issued: the
  one-shot is **still armed**, so the next boot is the NEW kernel, once, and a second power-cycle is what returns to the old
  one; (3) the host is only slow (a run killed by its timeout reads the same): survey first. `mode=disarm` clears a one-shot
  that is still armed. If the host still answers, the playbook disarms by itself on any failure between arming and the new
  kernel's boot. **A power-cycle restores the choice of kernel only**: if the upgrade broke something both kernels share
  (bootloader, ssh, tailscale, netplan, systemd, firmware), it restores nothing and the host needs its console. Every other phase
  says "not needed". A host that came back but failed verification keeps the old kernel as its default: reboot it back with the
  cluster-reboot workflow, which ends red with "kernel changed" by design (and refuses outright when the OLD kernel runs with a
  newer one installed, which needs no recovery). The upgrade itself refuses to run again while the host runs a kernel other than
  grub's saved default, or while a one-shot is pending. The apt run is detached: if a poll is lost, the run fails with apt possibly
  still running; survey before anything else.
- **What this does not prove.** The tests run the real playbook and script against stub `grub`, `apt`, `dkms` and
  `modinfo` commands and a fake `/proc`/`/sys`: they prove the order, the refusals and the verdicts, not that a real GRUB
  on msr1 honours `grub-reboot`, that r8127 builds for a real kernel, or that the NIC comes up. The first real
  evidence is the survey, then `msa2-server`.

#### Enabling a host change

No host-changing mode (`upgrade`, `disarm`, `repair`) can be dispatched until the maintainer enables it. The code enforces one control
and supports a second; this repository does not create either (that is settings, not code).

Be plain about what these controls are: settings that a person with the maintainer's token can also change (`gh variable set`,
approving a pending deployment). They stop accidents and anyone without that token; they do not stop an agent that holds it.
Rules of use: (a) empty the variable the moment a window ends; a "Re-run all jobs" of an old host-changing run re-executes it with
its old typed confirmation for as long as the host is listed; (b) **approve within ten minutes of the dispatch**: the idle verdict
of the cluster guard is refused after ten minutes, and the approval wait counts against it (the run then says so and must be
dispatched again).

1. **The switch (enforced by the `plan` job).** Settings, Secrets and variables, Actions, Variables: create the repository variable
   `OS_UPGRADE_ALLOWED_HOSTS`, a comma or space separated list of hosts, for the window: `msa2-server` for the first step. Add `msr1`
   only after the survey has answered the questions of the lane PLAN (d.2 does it boot through GRUB, d.3 the in-tree r8169, d.7 Secure
   Boot) and a person is at the rack; `msa2-client` last. Empty or missing (the default) means every host-changing dispatch is refused
   in `plan`, before any approval is asked and before any host is touched. The `plan` job also reads the environment of step 2 through the API and
   refuses unless it exists WITH a required reviewer and a deployment branch policy (if the workflow token may not read environments, it
   refuses and says so: give it a token with Environments: read in the secret `ENVIRONMENTS_READ_TOKEN`). Empty the variable when the campaign is over.
2. **The approval (GitHub environment `cluster-host-change`, used by host-changing modes only; a survey runs in `cluster-read` and
   never waits).** Settings, Environments, New environment `cluster-host-change`: Required reviewers = the maintainer (leave
   "Prevent self-review" off if the maintainer both dispatches and approves, or add a second reviewer); Deployment branches and
   tags = Selected branches and tags: `main` only. The `plan` job refuses a host-changing mode until this environment exists with a reviewer (GitHub would otherwise create it unprotected on first use).
3. **The secrets.** Environment secrets shadow repository secrets of the same name for a job in that environment. In
   `cluster-host-change` (and in `cluster-read`, which needs the same access for the survey) create `CLUSTER_SSH_KEY`,
   `TS_OAUTH_CLIENT_ID` and `TS_OAUTH_SECRET`; their values cannot be read back from the repository, so re-enter them from where the
   maintainer keeps them (a separate, less privileged key for `cluster-read` is better). **Be aware:** copying them does not remove
   the repository-level secrets, and the tier workflows, `cluster-reboot` and the runner bootstrap read those. Deleting them would
   break those workflows unless they adopt environments too. So until that separate decision, a workflow pushed on a branch by
   someone with write access can still read the repository secrets; the variable and the reviewer protect this workflow, not the key.
4. **Verify.** (a) With the variable empty, `gh workflow run cluster-os-upgrade.yml --ref main -f mode=disarm -f host=msr1
   -f confirm=DISARM-msr1 -f reason=test` fails in the `plan` job with "not in the repository variable OS_UPGRADE_ALLOWED_HOSTS" and
   touches nothing. (b) `gh variable list` shows the list; `gh api repos/goceleris/probatorium/environments/cluster-host-change
   --jq '.protection_rules, .deployment_branch_policy'` shows the required reviewer and the `main`-only policy. (c) With
   `msa2-server` listed, an upgrade of `msa2-server` stops at the `os-upgrade` job with a "Waiting for review" state until approved,
   while a survey does not wait. If (c) does not wait, the environment has no reviewer yet.

Tests: `go test -run 'ClusterOSUpgrade|OSUpgrade|FreeText'`, and `python3 -I ansible/tests/test_os_upgrade.py` (the script against a fake
host), `python3 -I ansible/tests/test_os_upgrade_judge.py` (the real Jinja judgement under ansible, every refusal) and
`python3 -I ansible/tests/test_os_upgrade_play.py` (the whole playbook on local connections, Linux only); the last two run
in the ansible CI job.

## Bench tier

### The field

The grid is capability-gated: 52 server columns from `servers.Registry` (the single source of truth,
in [`servers/servers.go`](servers/servers.go)) × 32 registered scenarios, of which 29 schedule cells.
The three `tls-*` scenarios need a TLS terminator that no run configures today.

- **celeris** contributes **9 columns** that pick out an engine, a wire mode and a handler dispatch
  mode: `iouring` (`h1-sync`, `h1-async`, `auto+upg-async`), `epoll` (`h1-sync`, `h1-async`,
  `auto+upg-async`), `adaptive` (`h1-async`, `auto+upg-async`) and `std-h1`. This is how the grid
  separates, for example, an io_uring regression from a wire-mode one.
- **`stdhttp`** is the Go `net/http` baseline (`h1`, `h2`, `hybrid`).
- **29 competitor frameworks across 10 languages** make up the rest:

| Language | Frameworks |
| --- | --- |
| Go (10) | chi, echo, fasthttp, fiber, gin, gnet, gorilla, hertz, iris, nbio |
| Rust (4) | actix-web, axum, hyper, ntex |
| Bun (3) | bunraw, elysia, hono |
| Node / JS (3) | express, fastify, uWebSockets.js |
| C++ (2) | drogon, lithium |
| Java (2) | netty, vertx |
| Python (2) | fastapi, starlette |
| C (1) | h2o |
| C# (1) | aspnet |
| Zig (1) | httpzig |

Count from the registry, not from adapter directories: a directory can exist without being registered.
Adding a framework takes a registry entry and an adapter directory, plus a `nativeBuildSpecs` entry in
[`mage_cluster.go`](mage_cluster.go) for a non-Go adapter.

### Capability gating

Not every scenario runs against every server. Each adapter declares its `Capabilities`, which the runner
projects into a `servers.FeatureSet`, and each scenario implements `Applicable(servers.FeatureSet) bool`.
The scheduler skips the mismatches **before** dialing loadgen, so a framework with no native Postgres
path never shows up as a spurious zero in the driver table. That is why a full run has 813 cells rather
than the raw 52 × 32 product.

### Profiles

`mage BenchTier` runs a **profile** from `budget.ForProfile` ([`budget/`](budget/)). Every profile
covers the same full grid in exactly one pass; they differ in the per-cell window and in whether the
rated sweep runs.

| Profile | Per-cell window | Rated sweep | Published by the Benchmark Tier workflow |
| --- | --- | --- | --- |
| **fast** (the `ForProfile` default) | 35 s active / 10 s warmup | off: saturation only | never (the workflow forces `BENCH_PUBLISH=0`) |
| **headline** | 40 s / 12 s | on | yes, unless dispatched with `publish=false` |
| **full** | 90 s / 20 s | on | yes, unless dispatched with `publish=false` |

The workflow publishes only rated profiles, because saturation-only data would publish datasets with no
`latency_at_slo`. `mage BenchTier` run by hand publishes unless `BENCH_PUBLISH=0`. The workflow sets
`BENCH_BUDGET` from a table of profile, baseline switch and arch policy (single arch, no baseline: 24 h, 36 h
and 60 h; the policies of a both-arch run and the baseline columns have their own entries, see below). The
budget model projects about 30 h for headline and 51 h for full on one arch. A preset `cells` or `rated_cells`
scope is projected from the runner's own dry run (the realized cell and rated cell counts, baseline columns
included), not from those pins: `rated_cells='*'` rates every cell, about twice the model's 401, and a full
both-arch dispatch with it no longer passes the gate at 101.9 h. A scope the dry run cannot resolve is refused. Runs before [#418](https://github.com/goceleris/probatorium/issues/418) measured about 53 h
and 69 h, because they rated every clean cell.

**Rated data is not publishable yet.** [#441](https://github.com/goceleris/probatorium/issues/441) gates
any rated publish, and [#442](https://github.com/goceleris/probatorium/issues/442) does until it is settled.
The validation run of 2026-10-21 showed the arch lock holds on the scoped rows (P1b, P2, P3, P5) and that the
exemption of light cells did not (P6 fails on arm64), so the default policy is now `all`
([`docs/ARCH-EXCLUSION-VALIDATION.md`](docs/ARCH-EXCLUSION-VALIDATION.md), "Verdict"). #442 stays open for the
three rows only a full both-arch run reads (get-simple-1024c, get-json-h2, post-4k-h2), so dispatch headline and
full with `publish=false` until both are settled. The runner keeps no achieved rate per pass, so no rated number
can tell a slow server from load that loadgen never sent (#441).

### A both-arch run: the two arches share one client (#442)

`BENCH_TARGET=both` (the default) runs the x86 pass and the arm64 pass at the same time, and both are
driven from the one loadgen host, msa2-client (#168 asked for a loadgen per arch; as landed they share
it). One arch alone leaves msa2-client far from saturation, but two arches loading it together did not
leave the heavy rows alone: in the 0829 run, x86 ws-large-echo passes that overlapped the arm64 run of
the same row read a backlog clock of 1 s or more in 6 of 10 passes, against 0 of 42 for the rest, and in
run 36441790519 the saturation numbers of the bandwidth rows moved with the other arch's load (the 2x10G
bond carries both arches). A run made before the fix marks nothing of this.

**What the harness does now.** The default policy is `all` (since the validation read of 2026-10-21): the
arches take turns on the client and no pass of one arch overlaps any load of the other. The bullets below
describe the lock; under `all` every cell takes it, under `heavy` (the previous default, still selectable)
only the heavy ones.

- Every scenario has a client-load class, `heavy` or `light` ([`scenarios/clientload.go`](scenarios/clientload.go),
  the table, its demand figures and the rule). Light is a row whose p90 stays at or below 150k requests/s
  and 4 Gbit/s on both arches in the published 0829 run: the 11 driver rows, churn-close, get-simple-1c,
  ws-hub-broadcast-128 and sse-fanout-128 (369 of the 813 cells). Everything else, and any scenario the
  table does not name, is heavy (a test fails until a new scenario is classified).
- The runner holds a host-wide `flock` on msa2-client (`/tmp/probatorium-client.lock`) around every heavy
  cell, saturation pass, rated sweep and warmups included ([`cmd/runner/clientlock.go`](cmd/runner/clientlock.go)).
  A heavy cell of one arch therefore waits for the other arch's heavy cell. The wait polls once a second,
  ends with the run context, and the kernel drops the lock when a runner dies, so a runner killed by its hang
  guard frees the other. Neither arch can stall the other for more than a cell: the holder releases the lock
  itself after the longest a cell can run when its own contexts work (its saturation pass plus every rated pass
  at their timeouts, plus a minute: a wedged cell, loadgen workers blocked in reads, has driven nothing since
  its context fired), and a waiter gives up after that plus two minutes and fails the cell as
  `client-lock: ...` (a DNF with its wait recorded, not an "interrupted" cell). The checker still reads the
  passes' own timestamps, so a cell that did overlap its peer is reported. `started_at` is stamped after a
  wait, so the idle wait never lands in the per-scenario CPU window (`scenario_resources`); the wait is
  recorded as `client_lock_wait_s` and is exactly 0 for a lock that was free at the first attempt. After a wait
  of 5 s or more the runner probes the SUT again before it drives load. The column's hang guard grows by
  `lockWaitAllowance` so a queued but healthy runner is not killed. The lock file is opened without following
  symlinks, its times are refreshed at every acquire (a host that age-cleans `/tmp` must not unlink it
  mid-run), and a lock taken on an inode the path no longer names is dropped and retaken.
  Two things the wait does to a measurement. The SUT idles through it (minutes, 6.7 h in total per arch in the
  model) where the only gap between cells used to be the 5 s cooldown; whether the first cell after a long
  wait reads differently (CPU frequency, an adaptive engine's settled state) is not measured, and the
  validation run has an item for it. And the column-wide `resources` mean (every scenario of the column,
  not the window) now includes that idle time, diluting it by an amount that differs per column: read
  `scenario_resources` for per-cell CPU. The lock is released when a cell's load ends, before the 5 s
  cooldown, so the peer's heavy cell can start while the previous cell's sockets drain on the client; they
  are connections to the other arch's SUT, so they share no 4-tuple with the next cell's, and the planner
  models the release at the same point.
- The arm64 pass walks each column's cells **in reverse** (`-cell-order reverse`). The registry order runs
  churn-close and the driver rows (light) first and the static, h2 and WebSocket/SSE rows (mostly heavy)
  after them; reversed on one arch, the two arches' heavy cells fall in different halves of a column and
  the lock rarely makes one wait (it matters: 57.6 h against 65.9 h in the model below). The order is recorded
  (`environment.arch_exclusion.cell_order`). It is a comparability change for arm64 only: its heavy rows now
  run first on a freshly started SUT and its light rows last, where every earlier arm64 run did the opposite.
  x86, the arch #442 contaminated and the one the single-arch references are for, keeps the registry order.
  `all` reverses arm64 too: its wall-clock does not depend on the order (99.7 h either way in the model), and
  it keeps every cell's position in its column the same under both policies, so a comparison of the two
  (the validation's run A against run B) differs in what the arches shared, not in where a cell sat.
- `BENCH_ARCH_EXCLUSIVE=all|heavy|off` (workflow input `arch_exclusive`) picks the policy. `all` is the
  default for `target=both`: it locks every cell, so the arches take turns (about twice one arch's
  wall-clock). `heavy` locks the heavy cells only (+14%, the previous default). `off` is the pre-fix
  behaviour, kept as a control arm: the publish gate refuses its documents. A single-arch target has no
  peer and ignores the knob.

**What it costs.** `interleave.SimulateArchPlan` replays the two passes on the real 813-cell, 52-column
grid at the `full` profile's window (`go test ./cmd/runner -run 'FullGridArchPlan|DriftSweep' -v`; the output
of this change is in the PR). One arch alone is 50.9 h in the model (bench-full 38431303162 measured 47.6 h
with both arches, so the model runs about 7% high). The first table starts both arches together with equal
column set-up and walks the same order, which is the lockstep 36441790519 showed (the same cell +61 s apart);
38431303162 was not in lockstep (median -247 s, same-row overlap fraction 0.06, #518), so "444 of 444" is the
lockstep case, not every past run.

| both-arch schedule (lockstep model) | model | vs one arch | heavy cells overlapping a heavy cell of the other arch |
| --- | --- | --- | --- |
| today: no lock | 50.9 h | +0% | 444 of 444 |
| lock on heavy cells, both arches forward | 65.9 h | +30% | 0 |
| lock on heavy cells, arm64 cells reversed (`heavy`, the previous default) | 57.6 h | +13% | 0 |
| lock on heavy cells, arm64 columns reversed | 58.6 h | +15% | 0 |
| **lock on every cell (`all`, the default)** | **99.7 h** | **+96%** | 0 |
| option, not implemented: heavy cells and every rated sweep, arm64 cells reversed | 75.2 h | +48% | 0 |

The lock's cost depends on how far apart the arches run. The drift sweep moves the arm64 start by 0 to 3 h
and the column set-up time of each arch by 0-120 s (20 points):

| makespan / one arch alone | min | mean | max |
| --- | --- | --- | --- |
| arm64 reversed, arches within 10 min (both full runs) | 1.12 | 1.14 | 1.17 |
| arm64 reversed, arm64 1-3 h later | 1.16 | 1.22 | 1.25 |
| both forward, arches within 10 min | 1.15 | 1.24 | 1.30 |
| both forward, arm64 1-3 h later | 1.11 | 1.14 | 1.21 |

So the reversal pays while the arches stay together, as in both full runs, and costs a few points if one
falls hours behind the other. A `full` target=both run goes from the measured 47.6 h to about 54 h (the
model's 1.14 mean times 47.6 h), 53-60 h across the sweep; `budget.FitWithin` projects 58 h
(`ArchLockOverheadHeavy` = 14%, pinned to the lockstep planner by a test) and the workflow budgets
(40 h headline, 69 h full) cover the sweep's 1.30 worst case. `headline` goes from about 30 h to about 34 h.
`all`, the default, is about twice, and the workflow budgets it separately (the budget is a table of profile,
baseline switch and policy and the job timeout a table of policy, `workflow_arch_budget_test.go`): 34 h fast,
72 h headline, 108 h full under a 7080 min (118 h) job timeout, which sits under the 5-day (7200 min) limit. A
default `full` dispatch therefore projects 101.9 h, so its budget has a 6% margin where every other entry has
18% or more; the model ran 7% high against the one measured run, so about 95 h is the expectation, but it is
the thinnest margin of any dispatch, and the weekly headline run, when its schedule is restored, projects 59.7 h
under the default instead of 34 h. Projected hours of `profile=full` with the opt-in baseline columns
(`baseline=true`, +222 saturation and +120 rated cells per arch), from the budget model and from the planner
(`go test ./budget ./cmd/runner -run 'FullBaselineUnderAll|BaselineGridArchPlan' -v`):

| `profile=full`, `baseline=true` | model | planner | job limit 120 h |
| --- | --- | --- | --- |
| one arch (`target=msa2-server` or `msr1`) | 65.5 h | 65.4 h | fits |
| both arches, `heavy` | 74.6 h | 72.1 h | fits (budget 90 h) |
| both arches, `all` (the default) | **130.9 h** | **128.0 h** | **does not fit** |

`full` + `baseline=true` + `target=both` under `all` cannot be held by any job: the workflow refuses it at the
start of the `setup` job, before any runner is provisioned and before Deploy (the run has already entered the
workflow's cluster concurrency group, so it may have queued behind a running one; the step is guarded by exactly
that dispatch and tested against the model). Dispatch it as two single-arch runs, `target=msa2-server` and then
`target=msr1`. `arch_exclusive=heavy` is not an alternative for a published run: its documents are refused at
publish (policy `heavy` failed P6), and the `setup` job refuses a `heavy` dispatch that would publish; `heavy` stays
available with `publish=false`. These are a model of fixed window lengths and the lock's first-come order, not a
cluster run: how often the lock really queues a runner is the first thing the validation run measures
(`lock_wait_seconds`).

**What `heavy` does not do** (the reason the default moved). Under `heavy` a heavy cell still shares the client with the other arch's light
cells, and a light cell's own measurement runs under the other arch's heavy cells. Neither is measured.

- *Heavy under light, x86 side.* The coefficient that chose the rule (an x86 cell lost 0.18-0.31 requests/s per
  concurrent arm64 request, partial r -0.92 to -0.97, on static rows of 128 and 256 connections over arm64
  demand of 174-321k requests/s; from run 36441790519, `evidence/bench-36441790519/rank/skeptic/
  out_s18_cpu_contention2.txt`) was never measured on the light rows. At the arm64 light rows' demand
  (p90 over columns, `out/demand_by_row.out` of the lane evidence): ws-hub-broadcast-128 122.5k frames/s and
  sse-fanout-128 125.7k events/s cost an x86 heavy cell 22-39k requests/s, 3.5-6.0% of a 650k requests/s cell
  (2.3-3.9% of a 1M one); every other light row stays at or below 30k, under 1.5%.
- *Heavy under light, arm64 side.* The x86 light rows reach 120-140k (churn-close 140k, driver-mc-set 130k,
  ws-hub-broadcast-128 127k). The coefficient was measured only as x86 loss per arm64 request. If it were
  symmetric an arm64 heavy cell of about 400k requests/s would lose 25-43k, 6-11%. This direction is
  unquantified.
- *Light under heavy.* 12 of the 15 `RatedScenarios` rows are light (churn-close and the 11 driver rows,
  ranked by latency at load). In the default order, 173 of each arch's rated light cells run their rated sweep
  while a heavy cell of the other arch does (346 cells in the model, 15.7 h in total;
  `go test ./cmd/runner -run FullGridArchPlan -v`). That is #442's own mechanism (rated passes under the other
  arch's heavy load) on most of the rated rows. It is exposure, not measured contamination.

`BENCH_ARCH_EXCLUSIVE=all` removes all three at about twice the wall-clock (99.7 h in the model, against
57.6 h). A third option, not implemented, locks every rated sweep as well as every heavy cell: the planner
prices it at 75.2 h (+48%) and it removes the third exposure only. The default order itself is a trade: with
both arches forward there are 230 heavy+light cell overlaps at 65.9 h; reversing arm64 makes it 793 at 57.6 h.
`all` removes that trade too. The validation run measured them
([`docs/ARCH-EXCLUSION-VALIDATION.md`](docs/ARCH-EXCLUSION-VALIDATION.md), "Verdict"): P5 held (light rows'
rated p99, A against B) and P6 failed on arm64 (a heavy cell under the x86 light rows, celeris-iouring post-4k,
minimum rps ratio 0.827 against a 0.92 floor, median 1.001), so by the pre-registered rule the default is `all`.

**The check on a finished run.** Every saturation and every rated pass records the interval it drove load
in, on msa2-client's own clock (the same clock for both arches), kept or skipped
(`cell.pass_windows` in `raw/<host>.json`). `Bench` folds them into `load_windows` of each
`results-<arch>.json` and, since each arch is published alone, stamps the cross-arch verdict into both:
`environment.arch_exclusion` (policy, peer arch, cell order, windows checked, `heavy_heavy_overlaps`,
`heavy_light_overlaps`, `light_light_overlaps`, `violations`, `missing_timestamps`, `lock_wait_seconds`;
schema 5.22). `report.CheckArchOverlap` is the check, with a fixture self-test (a clean pair, a pair with
one overlapping heavy pass, the policies, windows with no usable timestamps). It runs three times:

- in `Bench`, which prints the report after the merge;
- in the publish gate (`checkDataCompleteness`): a document whose verdict shows a violation, a window that
  could not be checked, or policy `off` is refused (`BENCH_PUBLISH_FORCE=1` overrides, as for the other
  completeness checks);
- on demand, `mage BenchArchOverlap` (`BENCH_RESULTS_DIR`, default the latest run; `BENCH_ARCH_EXCLUSIVE`
  to check against another policy), which reads the two `results-<arch>.json` and fails on a forbidden
  overlap, a window without timestamps, or documents with no `load_windows` at all.

`load_windows` stays in `results-<arch>.json` (the run artifact); the published `summary.json` drops it,
and `env.json` keeps the verdict.

**Results from before the fix.** No earlier field says a document came from a both-arch run, so the mark is
a rule, not a flag. A `results-<arch>.json` or published cell with **no `environment.arch_exclusion` and
`schema_version` below 5.22** that came from a `target=both` run ran the arches concurrently with nothing
excluded. Such a run is recognisable by its two arch documents carrying the same `benchmark_config.started_at`
(Bench stamps one start for both; checked on the results of runs 33256654220 and 37091941051, where
`started_at` is identical to the nanosecond and `finished_at` differs by under 3 s) and by the workflow's
dispatch input `target`. The runs this applies to
include 33256654220 (0829), 36441790519, 37091941051 (schema 5.18, identical `started_at`, checked) and
38431303162 (bench-full). A post-fix both-arch document always
carries `arch_exclusion`, whatever the policy, so absence can no longer be read two ways. The docs site
does not flag it yet; that is for goceleris/docs.

**Validation owed.** Nothing above has run on the cluster. The two scoped both-arch runs that check it (the
default policy and `all`, about 3.6 h of bench together, criteria pre-registered, including the three
exposures above) are in [`docs/ARCH-EXCLUSION-VALIDATION.md`](docs/ARCH-EXCLUSION-VALIDATION.md).

### Result: latency at SLO

In the rated sweep, each cell gets four constant-rate passes at 25, 50, 75 and 90 % of that cell's
saturation RPS, with loadgen's coordinated-omission correction. `latency_at_slo` maps each budget in
`{10, 50, 100, 500, 1000}` ms to the highest pass target whose P99 (the median across runs) stayed within
it. Bigger is better, and it ranks the field where saturation ceilings collapse together, such as the
store-bound driver rows. Only the 401 cells of `budget.RatedScenarios` (every rated scenario on every
server that can serve it) get the sweep: `BenchTier` passes their glob to the runner's `-rated-cells`
(`BENCH_RATED_CELLS`), with the profile's pass window in `BENCH_RATED_DURATION`. The rated rows are the
driver rows, `get-json`, `post-4k`, `churn-close` and `ws-echo`. The other scenarios (`get-simple` and its
concurrency sweep, `get-json-1k`, the H2 rows, `ws-large-echo`, and the WS hub and SSE fan-out rows)
publish saturation data only; [`budget/profiles.go`](budget/profiles.go) gives the measured reason for
each. Datasets published before [#418](https://github.com/goceleris/probatorium/issues/418) also carry
rated data for those scenarios.

## Validation tier

Each validation cell runs Tier 1 and Tier 3 against one reference app on one engine; Tier 2 is still
scaffolding.

### Tier 1: always-on property stress

Five traffic slices fan out over `Concurrency` walker goroutines. The budget activates progressively, so
small smoke runs don't pay for the expensive slices:

| Slice | Share of walkers | Active at concurrency ≥ | What it does |
| --- | --- | --- | --- |
| Markov | ~60 % | 1 | Session-shaped traffic over the reference app's routes, with transitions weighted by `validation/markov/<refapp>.yaml` |
| Adversarial | ~20 % | 1 | Raw-TCP malformed HTTP/1.1: bad chunks, oversized headers, NUL in a header, CRLF injection, slowloris, double Content-Length |
| h2c upgrade churn | ~10 % | 10 | Valid h2c upgrade preambles, then an RST at three different stages. Only `kitchen_sink` runs on `Protocol: celeris.Auto`, so only it completes an upgrade; the other seven serve HTTP/1.1 and decline by design |
| WS frame torture | ~5 % | 4 | A real RFC 6455 handshake, then one of: fragmented reserved opcode, oversize payload, unmasked client frame, ping flood, continuation without start, invalid UTF-8 |
| SSE kill-mid-stream | ~5 % | 4 | Open an SSE stream, hold it 50 to 1500 ms, then RST; the broker must clean up the client slot |

From concurrency 4, two more walkers run outside that budget. One is an RFC conformance scraper that
reads the wire (`I-RFC-1`, `I-RFC-2`). The other is a 64 KiB WebSocket echo (`I-WS-ECHO`), which runs
where `/ws` is routed. The echo holds no connection for the first 18 s of every 54 s: a native engine
keeps each echo's backlog on the Go heap, and these gaps give every `I-MEM-1` trough bucket samples
without one ([`validation/ws_echo_quiet.go`](validation/ws_echo_quiet.go), probatorium#478).

In a stall-capture run (the `stall_capture` dispatch input), a 64 KiB echo fire that times out is held
open: the sender stops, the walker keeps reading, and `ss -tmi` of both ends, the echo and byte
counters, the refapp's per-thread CPU masks and states, its `GOMAXPROCS` and its engine counters are
taken at t0 and t0 + 3 s. A record-only `I-WS-ECHO-STALL` dossier carries `ws-echo-diag.txt`, whose first
line is `WS_ECHO_VERDICT=SLOW_CLASS` (the server application read bytes or wrote more than a control frame
in the window, from the exact counters `bytes_received` less Recv-Q and `bytes_acked` plus Send-Q: slow),
`WEDGE_CLASS` (neither moved while bytes sat unread in the server's receive queue) or `INDETERMINATE`
(missing or contradictory data is never a wedge).
The timeout is counted after the second instant. `mage` and the race tier also take two off-by-default
experiment arms for msa2-server only, `refapp_cpus` (a `taskset -c` list of at least 2 CPUs) and
`refapp_workload_hint` (`high` starts the adaptive engine on io_uring); each cell records the arm it
actually ran under in `refapp_sched.txt` ([`validation/ws_echo_diag.go`](validation/ws_echo_diag.go),
[`validation/refapparms`](validation/refapparms), probatorium#481).

#### Race tier semantics: liveness is gated, latency is recorded

The race tier's adaptive cell is deliberately small (`adaptiveCellWorkers` = 2 workers, a floor of
`adaptiveCellConcurrencyFloor` = 110 walkers, to force the epoll-to-io_uring promotion) and runs under
`-race` instrumentation, so a late 64 KiB echo there is a capacity and latency effect before it is a
liveness defect. Its amd64 adaptive cell timed out in every run, and every timeout the two-instant
capture classified was `SLOW_CLASS`: the server read and wrote, the echoes were only slower than the
fire's deadline. The race tier therefore judges liveness and records latency:

| In the race tier | Result |
| --- | --- |
| A `ws_echo` timeout classed `SLOW_CLASS` | not a failure: counted in `ws_echo_timeout_slow` (exported in `validate-results.json` under `tier_1.ws_echo`, printed by `mage ValidateGate`, which lists every cell that recorded one), and a record-only `I-WS-ECHO-SLOW` dossier whose `ws-echo-diag.txt` keeps `WS_ECHO_VERDICT` as its first line and adds `WS_ECHO_DISPOSITION=TOLERATED_SLOW` as its second |
| `WEDGE_CLASS`, `INDETERMINATE` (no capture included) | `ws_echo_timeout` as before: hard `I-WS-ECHO`, the cell stops, the gate fails |
| More than N = 3 `SLOW_CLASS` timeouts in one cell | the 4th is `ws_echo_timeout` (`WS_ECHO_DISPOSITION=FAIL reason=over_n`) |
| A `SLOW_CLASS` timeout after the first with no fire in the cell having drained cleanly since the first | `ws_echo_timeout` (`reason=no_clean_fire_since_first_slow`): every fire after the first timed out, which is a server delivering nothing, not one that is late now and then |
| Every other violation | unchanged |

The tolerance is explicit and scoped to this tier. The validator takes `-race-tier`, which
`ansible/validate.yml` passes only when `validate_race` is `1` (the `-race` refapp set), and
`mage ValidateGate` takes `VALIDATE_GATE_TOLERATE_WS_ECHO_SLOW=1`, which only `matrix-race-tier.yml`
sets. The gate repeats the rule on the results: it tolerates `ws_echo_timeout_slow` only with that
variable, only in a cell that declared `I-RACE` (a `-race` build), and only up to 3. Nightly, soak,
weekend, checkptr and PR runs have neither switch, so a `SLOW_CLASS` ws_echo timeout in them stays
`ws_echo_timeout` and fails: their timing budget is real. In the race tier the capture is armed by
`-race-tier` itself, so `stall_capture` is not needed for the tolerance. `ValidateDiff` does not list
`ws_echo_timeout_slow`, so an amd64-only slow echo is not a cross-arch divergence.

`ws_echo_timeout_slow` is a measurement, not an exemption: watch its count per run. Before this change
the first timeout ended its cell, so "one per run" was a censored count; the per-cell rate is now
measured by the race tier itself.

What a race run records about itself (schema 5.23, record only, nothing new is gated):

- **Every diagnosed SLOW fire is counted** (the capture budget is N+1 = 4 per cell; a later timeout is counted in `ws_echo_timeout` alone). A `SLOW_CLASS` fire the guard refuses is still the failing
  `ws_echo_timeout`; it is also counted by reason in `ws_echo_slow_over_n` and
  `ws_echo_slow_no_clean_fire` (subsets of `ws_echo_timeout`). `mage ValidateGate` prints each cell's total
  (tolerated plus refused) and lists the refused ones in a block of their own, marked FAILING, so the
  failure reads as slow-class latency past N and not as a wedge. A document without these keys prints as
  before.
- **`h2c_hang` and `ws_handshake_fail` have an instant.** `tier_1.h2c_hang_span` and
  `tier_1.ws_handshake_fail_span` hold the first and last event of the cell: UTC `ts`, `since_ready_ms`,
  `cause` (eof, reset, timeout, status, other), the verbatim `err` and the addresses (`since_ready_ms` is omitted when 0, i.e. when the refapp's ready instant is unknown or the event is within the same millisecond). The slow ring keeps
  only reads over one second, so a hang that closed in milliseconds had no timestamp anywhere. `ts` is UTC,
  like `ready_at`, the slow ring and the epoch seconds of `properties_series.csv` (whose
  `adaptive_switches` column steps at the switch), so the four join on one timeline; the refapp's own
  stderr lines carry host-local time with no zone and join through `since_ready_ms`.
- **The refapp's CPU placement is in the cell document in every run:** `tier_1.refapp_sched` has the
  per-thread `Cpus_allowed_list` counts (`thread_masks`) read from `/proc` at ready, and the leader's mask
  separately, which is not the process's mask. `gomaxprocs` is recorded only when the stall-capture side
  listener answered (never under the ssh driver); otherwise it is 0 with a note. Recording it in every run needs the refapp to publish
  it (`validation/refapp/internal/debugvars`), a change to the refapp build that is not made here.
- **The race tier runs the host sampler** (`HostSamplerStart` / `HostSamplerStop`, tag
  `race-<run id>`, lifetime fixed at 150m inside the job's 180 minute limit, each step bounded to 5 minutes), as the weekend soak does.

**The race tier shapes the load of ONE cell (no schema bump).** The adaptive cell is sized 2 workers x
110 walkers so the controller's promotion fires, and under the race detector's slowdown the closed-loop
walker fleet of the `auth_session_ratelimit` adaptive cell saturated its two engine loops (a fourth
`SLOW_CLASS` echo, now and then a reset-type counter). With `-race-tier`, in that cell only (both arches),
each Markov walker now starts a request at most once every 6.5 ms (`raceAdaptiveMarkovMinPeriod` in
`cmd/validator/matrix.go`): open loop, so the fleet offers at most 67 / 6.5 ms = ~10.3k requests a second
whatever the response time, against the ~16.6k the same walkers sent when the cell saturated. The worker,
walker and connection counts are unchanged, no threshold, N or timeout moves, and every other cell is
closed-loop exactly as before: the other seven refapps' adaptive cells (green at full load; they run 77 Markov
walkers, and a 6.5 ms period would have kept only 21-46% of their request volume on amd64), every pure engine
and every non-race tier. Shaping costs race-detector coverage, so another refapp's adaptive cell joins only if
a run shows a SLOW or reset-type counter in that cell with the loops busy, never pre-emptively. The cell
says so: `tier_1.load_shaping` has the reason, `walkers`, `markov_walkers` and `markov_min_period_us`, and is
omitted everywhere else. The field is additive in schema 5.23 and does not bump it, so the documents of
every other tier are byte-identical to before. Promotion is still gated in every default race run
(`VALIDATE_GATE_EXPECT_ADAPTIVE_SWITCH`); a shaped cell that misses it says it was shaped, and a shaped cell
without a `-race` build (`I-RACE`) fails the gate.

The two streaming slices are **routed**. Once per cell, the tier probes `/ws` and `/events` with the
exact request its walker sends, and skips the slice when the reference app answers 404; only
`auth_session_ratelimit` routes them today. The verdict is recorded per cell (`ws_route_probed` /
`ws_route_present` and the `sse_` pair), so a zero is attributable. A matrix run also writes a
per-app coverage table to `streaming-coverage.txt`.

The first time one of these signals appears, the orchestrator acts on it mid-run
([`validation/runner.go`](validation/runner.go)):

| Signal | Predicate | Meaning | On first hit |
| --- | --- | --- | --- |
| adversarial wrong-accepted > 0 | `I-ADV-ACCEPTED` | The server accepted malformed bytes: an RFC violation | hard fail: forensics first, then the cell stops |
| h2c crashed > 0 | `I-H2C-CRASHED` | The engine answered an upgrade with non-HTTP bytes | hard fail: forensics first, then the cell stops |
| WS accepted bad frame > 0 | `I-WS-ACCEPTED` | The server accepted an RFC 6455 violation | hard fail: forensics first, then the cell stops |
| WS hang (no close) > 0 | `I-WS-HANG` | A WebSocket connection hung past the close timeout | hard fail: forensics first, then the cell stops |
| 64 KiB echo corrupt / reordered / missing / timed out | `I-WS-ECHO` | WebSocket echoes were not byte-intact and in order | hard fail: forensics first, then the cell stops. In the race tier only, a timeout that the two-instant capture classed `SLOW_CLASS` is not this row: see the next one |
| 64 KiB echo timed out, `SLOW_CLASS` (race tier only) | `I-WS-ECHO-SLOW` | The server read and wrote in the window; the echoes were only later than the fire's deadline | record-only dossier, counted in `ws_echo_timeout_slow`, the cell runs on; the gate fails past 3 per cell |
| process died | `I-LIVENESS` | The reference app crashed mid-run | hard fail: forensics first, then the cell stops |
| process alive but wedged | `I-HANG` | The reference app stopped responding | hard fail: forensics first, then the cell stops |
| h2c hang > 0 | `I-H2C-HANG` | An upgrade was neither answered nor declined within 20 s | dossier without a core dump; the cell runs on |
| WS handshake fail > 0 | `I-WS-HANDSHAKE` | A WebSocket upgrade did not complete within 2 s | dossier without a core dump; the cell runs on |

A zero only means health when the oracle ran. `mage ValidateGate` therefore also fails a `kitchen_sink`
cell whose `h2c_upgraded` is 0 after sending preambles: every churn mode would then have degenerated
into a declined GET, and the h2c oracles would have judged a path the engine never entered. The same
rule covers dead cells (`requests_sent == 0`) and properties that were never evaluated.

### Tier 2: stateful API fuzzing (not implemented)

The intent is RESTler-style producer/consumer inference from `validation/spec/<refapp>.openapi.yaml`, to
catch API-level bugs Tier 1 misses. **The tier is scaffolding today:** `runTierRESTler` parks on the run
context and sends nothing, and every `plan.json` marks the tier `DISABLED` with that reason. Only
`auth_session_ratelimit` ships a spec.

### Tier 3: deterministic seed replay

This is a real kernel with no mocking. A seed expands into a workload plus a fault schedule
(`iptables` drops, `tc netem` loss and delay), so a bug is the tuple `(seed, git_commit, host_arch)` and
reproduces with `validator-replay` (see [Reproduce a run](#reproduce-a-run)). The corpus is the 100
hand-authored seeds in [`validation/corpus/seeds_initial.go`](validation/corpus/seeds_initial.go). Each
cell replays seeds for its whole window at 15 s per seed, looping over the corpus.

### Invariants

Beyond the Tier 1 signals, the orchestrator runs a **per-second property loop** inside every cell
([`validation/propertyloop.go`](validation/propertyloop.go), with the shared evaluator in
[`validation/checker`](validation/checker)). Once the reference app announces its address, the loop:

- polls its `/debug/vars`, which every reference app serves through
  `validation/refapp/internal/debugvars`, alongside `/debug/pprof` for forensics;
- samples its RSS from `/proc`;
- evaluates the registered `I-*` predicates against a rolling one-hour history.

Every violation is counted into `tier_1.property_violations` / `property_violation_ids`, and
`mage ValidateGate` fails on any of them. What the first violation does to the *cell* is a switch:

- **Record-only** (the default): an incident dossier with live forensics (`/proc`, the `/debug/pprof`
  profiles, gcore, dmesg) is written while the reference app is still running. The cell then continues to
  its budget, so a leak's whole trajectory is on record.
- **Hard fail** (the validator's `-property-hard-fail`, or `VALIDATE_PROPERTY_HARD_FAIL=1` in its
  environment, which `ansible/validate.yml` sets from the `validate_property_hard_fail` extra-var): the
  same dossier is captured *before* the tiers are torn down, then the cell is cancelled and the validator
  exits non-zero.

Each cell reports `properties_passed`, `properties_failed`, `properties_not_instrumented` and
`properties_not_judged`. A predicate counts as passed only if it reached a **verdict** at least once and
never failed. One that only ever skipped (its window never became judgeable) is `not_judged`; one with no
data source is `not_instrumented`. By default the gate also fails a cell whose loop never evaluated
anything (`/debug/vars` unreachable), unless `tier_1.property_loop_skipped` names a reason.

Judged in every cell:

- `I-LIVENESS` / `I-HANG`: the reference app stayed up and responsive (judged from the walkers, not the
  snapshot loop).
- `I-PANIC`: no *unexpected* panic. This is `celeris.panic_count` from `/debug/vars` minus the panics
  the traffic corpus designs in, and it must persist for 3 samples.
- `I-CONN-2`: `accepted − closed − active == 0`, judged only when the drift keeps the same sign for 30
  samples.
- `I-MEM-1`: the `heap_inuse` trough slope is at most 1 KB/s over the trailing min(1 h, elapsed), after a
  5-minute warm-up, from 10 minutes of samples.
- `I-MEM-3`: the goroutine-count trough slope is at most 0.2/s over the trailing 10 minutes after
  warm-up.
- `I-MEM-4`: the RSS trough slope is at most 64 KB/s over the trailing min(1 h, elapsed) after warm-up
  (Linux, local driver).

The three slope oracles share a false-positive guard:
- The fit runs over per-150 s bucket **minima**, so a GC sawtooth cannot pass for a slope.
- A slope above budget counts only if the rise across the window also clears a floor, so one legitimate
  step (a cache filling once, a standby engine spinning up) cannot fire it. The floor is the larger of
  budget × 10 minutes, a fraction of the series' level (3 % heap, 5 % goroutines and RSS) and, for the
  heap, 8× its sampling noise and the rise the cell's own trough scatter can explain: the fitted slope
  must be significant (one-sided 0.001, a critical value per trough count) against the scatter of the
  window's troughs, estimated from their successive differences. **That is not free.** Measured against
  the rule without the term on the weekend soaks' 45 minute cells (25 noisy observability cells, trough
  sd 0.34-1.4 MB; 162 quiet cells; detected/sustained counts; all tables and the case definitions are in
  probatorium#520):
  - Noisy cells, straight leak from the end of warm-up: 1 KB/s is called in 9 of 25 cells (24 without
    the term), 2 KB/s in 24 (25), and a 3 KB/s leak about 11 minutes later (median 17.5 -> 28.7 min).
  - Noisy cells, a leak that starts late: from minute 20, 3 KB/s 25/25 -> 22/19; from minute 25, 3 KB/s
    21/10 -> 12/3, 4 KB/s 25/22 -> 21/10, 6 KB/s 25/25 -> 24/21.
  - Noisy cells, a stepped leak of 3 KB/s on average: 11 minutes later with a step every 15 minutes
    (median 23.7 -> 35.0), sustained 21 -> 9 with a step every 30.
  - Quiet cells, straight leak: 3 KB/s is called 3.7 minutes later at the median and 6.2 at most;
    1 KB/s in 146 of 162 cells instead of 150. Late-start and stepped leaks: up to 2.5-5 minutes later.
  - **Quiet cells, a leak that stops.** A leak that starts at the end of warm-up, runs 7.5 minutes and
    then plateaus is lost: 3 KB/s 154/153 -> 64/18 (90 quiet cells lost), 4 KB/s 162/156 -> 125/75,
    6 KB/s 162/162 -> 156/153; 3 KB/s for 10 minutes 162/162 -> 157/149. The estimator, not a floor
    that is too high, is the cause: an instant 2 MB step is still called in 156 cells, as without the
    term. After the leak stops, the shifted successive differences sit in windows of 6-8 troughs where
    the 75 % trim drops only one difference, which inflates the scatter estimate. The other floors do
    not catch it.
  - A single low trough (a 2-4 MB dip at minute 10-15) delays a 3-5 KB/s call by a median of 7.5-11.2
    minutes (max 11.2-13.7) on quiet cells, beyond the 10 minute maximum the straight-leak tests allow.
  - A single 1 MB step is called in 139 of 150 quiet cells that the rule without the term calls.
  - Cells that scatter heavily though quiet (1-2 MB of added trough scatter): 3 KB/s is lost in 19
    (1 MB) to 123 (2 MB) of 162 cells, and a 4 MB sawtooth loses 161. In the same cells with no leak
    the rule without the term trips falsely in 59 of 162 and this rule in none.
  - Short cells: the scatter term needs the load phase to last long enough for windows of 8 or more
    troughs. With the load phase cut to 20 minutes the quiet cells lose 99 of 162 at 3 KB/s, 75 at
    5 KB/s and 32 at 10 KB/s (noisy cells 22-25 of 25); at 25 minutes none are lost.
  - Windows of 5-8 troughs judge only a leak that is large against the scatter.
  - Nothing the floor absolves is hidden. The validator runs I-MEM-1 as it was before the floor
    beside the gated rule, on the same series, and records it per cell as `property_informational`
    (schema 5.20) with its violation count, the evaluations the gated rule absolved and a flag for a
    cell it absolved entirely. `mage ValidateGate` prints it on every run under "informational, NOT
    gating (the pre-scatter-floor rule of I-MEM-1 ...)". The gate never reads it: the verdict is the
    gated rule's alone. A reader of a soak sees every trip the floor absolved and can judge it.
  - The maintainer accepted these costs on 2026-10-17 after five pre-registered attempts (probatorium#520;
    the two later estimators that did not work, and why, are in its description).
- A verdict must persist for 150 consecutive evaluations before it is declared.

These oracles need cells of 15 minutes or more to judge at all, and I-MEM-1 needs about 25 minutes of
sustained load to keep its sensitivity: a shorter load phase loses leaks (see the cost list above). They
judge in the weekend soak's cells (about 45 minutes each: 24 h over 32 cells per architecture, which is
unaffected), and are `not_judged` in the short nightly cells.

Judged only where their data source exists, and reported as `not_instrumented` everywhere else:

- `I-MW-*`: only the apps that install the middleware.
- `I-DRV`: the three driver apps, which read every write back and publish the hit/miss tally.
- `I-RFC-1` / `I-RFC-2`: the conformance scraper, at concurrency ≥ 4.
- `I-CONN-1`: the per-connection last-byte table.
- `I-CHECKPTR`: Checkptr Validation cells; `I-ENG-IOURING`: that tier's io_uring cells.
- `I-RACE`: Race Validation cells, which count `WARNING: DATA RACE` reports on stderr.
- `I-ENG-ADAPTIVE`: adaptive cells.
- `I-MEM-2`: cells of 20 minutes or more, which idle the app twice, after a 60 s burst and at the end.

### Cross-engine and cross-arch divergence

A matrix run's `validate-results.json` holds one `Cells[]` entry per `(refapp, engine, arch)`.
`mage ValidateDiff` compares the two latest matrix documents and reports:

- **cross-engine** divergence: a counter non-zero on one engine (say `iouring`) but zero on another for
  the same `(refapp, arch)`, which is typically an engine-specific bug;
- **cross-arch** divergence: the same shape, comparing amd64 with arm64.

It exits non-zero on HIGH severity, and on MEDIUM too with `VALIDATE_DIFF_STRICT=1` (which every
validation tier sets). It writes `validate-diff/diff.{txt,json}`.

## Reference apps

Each reference app is a **separate Go module** under [`validation/refapp/`](validation/refapp/), so the
validator never pulls in every middleware's dependency graph. The matrix runner discovers them at
runtime.

| Slug | Coverage |
| --- | --- |
| `auth_session_ratelimit` | Session cookie, rate limit, and the WebSocket / SSE detach paths (the only app that routes `/ws` and `/events`) |
| `auth_jwt_csrf` | JWT (HS256), CSRF synchronizer token, keyauth |
| `kitchen_sink` | 17 stateless middlewares (basicauth, bodylimit, cache, circuitbreaker, cors, etag, healthcheck, idempotency, methodoverride, ratelimit, recovery, redirect, requestid, rewrite, secure, singleflight, timeout). The only app on `Protocol: celeris.Auto`, so it is where the h1→h2c upgrade path is exercised |
| `driver_postgres` | celeris's native Postgres driver, `session/postgresstore`, and the `I-DRV` read-after-write tally |
| `driver_redis` | The native Redis driver, `session/redisstore` and a Redis-backed rate limiter |
| `driver_memcached` | The native memcached driver, `session/memcachedstore` and a memcached-backed rate limiter |
| `observability` | logger, metrics and otel middleware |
| `static_swagger_proxy` | static (`embed.FS`), swagger (OpenAPI 3.0) and proxy (X-Forwarded-For trust) |

Each follows the same shape: its own `go.mod`, `resolveEngine("auto")` (io_uring on Linux, std elsewhere),
signal-driven graceful shutdown, and a `ready addr=<bind-addr>` startup line.

## Mage targets

| Target | What it does | Main knobs |
| --- | --- | --- |
| `Status` | Cluster reachability and manifest state | |
| `Deploy` | Cross-compile and stage every binary | `CLUSTER_USE_LAN`, `DEPLOY_COMPETITORS=all\|go-only\|none\|<list>`, `DEPLOY_NEEDS_DBSERVICES` |
| `Cleanup` | Undo what a run installed, per the host manifests | `CLEANUP_HOSTS=all\|<list>` |
| `Bench` | One bench run | `BENCH_TARGET` (default `both`), `BENCH_ARCH_EXCLUSIVE` (`both` only: `all` default, `heavy`, `off`), `BENCH_CELLS`, `BENCH_COMPETITORS`, `BENCH_DURATION`, `BENCH_WARMUP`, `BENCH_RATED`, `BENCH_RATED_DURATION`, `BENCH_RATED_CELLS` (default: the rated cells), `BENCH_SUT_ENV`, `CELERIS_VERSION` |
| `BenchTier` | A profile-driven run over the whole grid; publishes it unless `BENCH_PUBLISH=0` | `BENCH_PROFILE=fast\|headline\|full`, `BENCH_BUDGET`, `BENCH_TARGET`, `BENCH_SKIP_RATED`, `BENCH_PUBLISH`, `BENCH_BASELINE=1` (see [Same-run celeris baseline](#same-run-celeris-baseline)) |
| `BenchArchOverlap` | Check a finished both-arch run for overlap between the arches' load windows (#442); fails on a forbidden overlap | `BENCH_RESULTS_DIR` (default the latest run), `BENCH_ARCH_EXCLUSIVE` |
| `BenchSince` | Compare against a baseline version | `BASELINE_VERSION` (default `v1.4.2`), `REGRESSION_THRESHOLD` (default `0.05`) |
| `Validate` | A validation run, one cell or the matrix | `VALIDATE_TARGET` (default `both`), `VALIDATE_DURATION` (default `6h`), `VALIDATE_MATRIX=1`, `VALIDATE_MATRIX_REFAPPS`, `VALIDATE_MATRIX_ENGINES`, `VALIDATE_PARALLEL=1`, `VALIDATE_DBSERVICES=1`, `VALIDATE_CONCURRENCY`, `VALIDATE_RESUME_FROM`, `CELERIS_VERSION`, `PROBATORIUM_VALIDATE_DRIVER=ssh` |
| `Soak` | A long validation run | `SOAK_DURATION` (default `24h`), plus the `Validate` knobs |
| `ValidateGate` | The absolute gate over the most recent run's `validate-results.json` on every host | `VALIDATE_GATE_EXPECT_CELLS`, `VALIDATE_GATE_EXPECT_ADAPTIVE_SWITCH`, `VALIDATE_GATE_TOLERATE_WS_ECHO_SLOW` (race tier only) |
| `ValidateDiff` | The cross-engine and cross-arch gate | `VALIDATE_DIFF_STRICT=1` (MEDIUM fails too), `VALIDATE_DIFF_HOSTS=a,b` |
| `BuildRaceRefapps` | Cross-build the `-race` reference apps (the race runtime needs cgo) | |
| `HostSamplerStart` / `HostSamplerStop` | Start and stop the read-only host sampler on every cluster host | `SAMPLER_DURATION`, `SAMPLER_INTERVAL`, `SAMPLER_TAG` |
| `Publish` | Write the newest bench run into goceleris/docs and fire the `benchmark-published` pointer | `PUBLISH_VERSION`, `PUBLISH_VIA=git\|contents`, `PUBLISH_DRYRUN=1`, `DOCS_REPO_DIR`, `DOCS_TOKEN`, `BENCH_PUBLISH_FORCE` |
| `PublishValidate` | Kept for callers; it runs `Publish` | |
| `BenchAndValidate` | `Validate` → `ValidateDiff` (when `VALIDATE_TARGET=both`) → `Bench` → `Publish` | the knobs of each |
| `Fuzz` | Hostile-input fuzzing through `ansible/fuzz.yml`. **Not working today:** the playbook passes validator flags (`-mode`, `-url`) that `cmd/validator` does not define | `FUZZ_DURATION`, `FUZZ_CORPUS` |

`VALIDATE_PARALLEL=1` on a two-arch run drives the two targets concurrently, halving the wall-clock of
long soaks.

### Matrix mode (`VALIDATE_MATRIX=1`)

Matrix mode iterates the `(refapp × engine)` cells and runs a fresh orchestrator per cell with a budget
of `total_duration / len(cells)`. It emits one `validate-results.json` with `Cells[]` populated. Filter
the matrix with:

- `VALIDATE_MATRIX_REFAPPS=driver_postgres,driver_redis` to limit the reference apps;
- `VALIDATE_MATRIX_ENGINES=iouring,epoll` to limit the engines. The default is the OS production set:
  iouring, epoll, std and adaptive on Linux, std elsewhere.

**A failing cell does not stop the run**, because cluster time is the scarce resource. Every cell carries
a `status`:
- `ok`;
- `failed`: it ran, then something failed it;
- `not_run`: it produced no requests and no property verdicts, because its app never started.

Each cell also carries the verbatim `failure_reason`. For a cell that never came up, that is the driver's
own account of why. The ledger names the cell's evidence (`refapp_stderr_tail.txt`, else the incident
dossier) only when that evidence has content. The end of the run prints the ledger and leaves it in
`cell-failures.txt`. The exit status is still non-zero for any cell that did not pass: this is a gate,
not a report.

Three conditions stop the run immediately, because none of them leaves a later cell able to measure
anything:
- the remote driver cannot be built;
- the reference-app staging directory is unreadable;
- a write fails with `ENOSPC`.

Two caps bound the damage when the matrix is failing wholesale:
- **8 consecutive** failures;
- **half the plan** failed in total.

Override them with `PROBATORIUM_MATRIX_MAX_CONSECUTIVE_FAILURES` / `PROBATORIUM_MATRIX_MAX_FAILED_CELLS`
(negative disables); `mage Validate` carries both across the SSH boundary. The weekend soak sets the
total cap to 8. Whatever stops a run early, the summary names the cells it never attempted and prints the
`-matrix-resume-from` line that finishes them.

## Result layout

The key files a run leaves under `results/`:

```text
results/<ts>-bench-<version>/
  results.json                           # the run's roll-up (results-amd64.json + results-arm64.json with BENCH_TARGET=both)
  <TS>-bench-<bench_target>/<RR>-<competitor>/
    run0/<scenario>/<server>.json        # per-cell runner result: the ingested data
    server.log, cpu.*.log

results/<ts>-validate-<bench_target>-<refapp>/   # fetched back from the bench target
  validate-results.json
  cell-failures.txt                      # matrix runs: every cell that failed or never ran, with its reason
  streaming-coverage.txt                 # matrix runs: per-app ws/sse routing table
  cell-<NN>-<refapp>-<engine>/
    validate-results.json                # the cell's own document
    refapp_stderr_tail.txt               # the app's last words (why a not_run cell never started)
    incidents/<ts>-<predicate>/          # forensics dossier per violation

<parent of the two compared runs>/validate-diff/
  diff.txt, diff.json                    # ValidateDiff findings
```

The document types and the current `SchemaVersion` live in [`report/schema.go`](report/schema.go). A
single-cell run leaves `Cells[]` empty and fills `tier_1` / `tier_3` at the top level. Sub-tallies are
`map[string]int64`, so the schema does not re-version when the validator grows a counter.

## Continuous integration

| Workflow | Trigger today | Runs on | Role |
| --- | --- | --- | --- |
| `test.yml` (Test) | push to `main`, pull requests | GitHub-hosted | `go test -race` over the root module, every adapter module, the reference apps, the mage-tagged files and the field guard, plus govulncheck. The `ci-ok` fan-in is a required check |
| `lint.yml` (Lint) | push to `main`, pull requests | GitHub-hosted | `go vet`, golangci-lint, gofmt and actionlint (required) |
| `coverage.yml` (Coverage) | push to `main`, pull requests | GitHub-hosted | Coverage upload to Codecov |
| `matrix-pr-tier.yml` (PR Validation) | pull requests labeled `cluster-ok` (path-filtered), manual | cluster | 10-minute matrix on msa2-server, with `ValidateDiff` |
| `matrix-nightly-tier.yml` (Nightly Validation) | manual (schedule paused) | cluster | Full matrix, both architectures, `ValidateGate` + `ValidateDiff` |
| `matrix-weekend-tier.yml` (Weekend Soak) | manual (schedule paused) | cluster | The same over 24 h by default |
| `matrix-race-tier.yml` (Race Validation) | manual | cluster; the `-race` apps are built on a GitHub-hosted runner | Full matrix with `-race` apps |
| `matrix-checkptr-tier.yml` (Checkptr Validation) | manual | cluster | Full matrix with pointer-checker apps |
| `benchmark-tier.yml` (Benchmark Tier) | manual (schedule paused) | cluster | `mage BenchTier`; publishes rated profiles |
| `publish-results.yml` | manual; also logs `benchmark-published` dispatches | GitHub-hosted | Re-send the pointer for a cell already pushed to goceleris/docs |
| `celeris-stress.yml` (Celeris Stress) | manual; pull requests that change it or `tools/stresstally` (self-test) | GitHub-hosted x86 and arm64 | Run celeris tests many times to measure a flake and its rate; see [`docs/STRESS.md`](docs/STRESS.md) |

The cluster tiers share `concurrency: matrix-tier-cluster` with `cancel-in-progress: false`, so they never
overlap on the cluster. The group keeps only one pending run, so a newer queued run replaces an older
one. Each tier provisions runners, runs on the cluster, then tears the runners down.

### Same-run celeris baseline

`servers/celeris_v1511` is the bench server built against the released celeris v1.5.11 instead of the
pinned main, registered as eight opt-in columns `baseline-celeris-v1511-<engine>` (epoll, iouring and adaptive in
every protocol flavour the `celeris-*` columns have, without `celeris-std-h1`). They exist so one dispatch
holds the same-run interleaved comparison against the v1.5.x release. They are left out of every "all"
expansion (the `*/*` grid, the default competitor set, `DEPLOY_COMPETITORS=all`, the budget pins) unless
`BENCH_BASELINE=1`, which `benchmark-tier.yml` sets from its `baseline` input (default `false`). Switched on it adds
222 saturation and 120 rated cells per architecture (about +14.5 h projected on `full`, +8.5 h on
`headline`) and the workflow raises `BENCH_BUDGET` for it, for every arch policy. With `target=both` under the default
policy `all`, `profile=full` with the baseline projects 130.9 h and is refused by the `setup` job (above the
7200 min job limit; see "A both-arch run"): dispatch it as two single-arch runs (`target=msa2-server`, then
`target=msr1`; 65.5 h each). Columns run as blocks in list order, so
`servers.InterleaveBaseline` places each baseline column directly beside its `celeris-*` twin (about 1.8 h
apart, alternating which runs first) instead of the sorted `baseline-*` block fifteen hours earlier. Rows publish with framework `celeris-baseline`,
framework_version `v1.5.11`. The module is a frozen fork of `servers/celeris` (go.mod pinned, not in
Dependabot); `servers/baseline_test.go` fails when the two drift. Dry-run the extra cells with
`BENCH_BASELINE=1 go run ./cmd/runner -dry-run -runs 1 -cells '*/*'`.

### Self-hosted runner bootstrap

Cluster runners are provisioned per run and torn down at the end:

- [`.github/actions/cluster-runner-up/`](.github/actions/cluster-runner-up/) joins the tailnet with a
  SHA-pinned `tailscale/github-action` (an ephemeral `tag:ci` node), mints a registration token, runs
  [`ansible/runner-setup.yml`](ansible/runner-setup.yml) and checks that the runners came online.
- [`.github/actions/cluster-runner-down/`](.github/actions/cluster-runner-down/) collects forensics,
  rescues results to `/var/lib/celeris-rescue`, cleans the hosts, writes a "Resume candidates" job
  summary, mints a removal token, runs [`ansible/runner-teardown.yml`](ansible/runner-teardown.yml) and
  sweeps orphaned registrations.
- Runners live under `/tmp/actions-runner-<host>/`, with no systemd unit and no package install.

The one-time operator setup (four repository secrets and a Tailscale ACL rule) is documented in
[`ansible/RUNNER_BOOTSTRAP.md`](ansible/RUNNER_BOOTSTRAP.md). Resuming an interrupted matrix after a power
event is covered in [`ops/power/README.md`](ops/power/README.md).

## Related projects

| Project | What it is |
| --- | --- |
| [celeris](https://github.com/goceleris/celeris) | The HTTP engine for Go that probatorium benchmarks and validates |
| [loadgen](https://github.com/goceleris/loadgen) | The HTTP/1.1 and HTTP/2 load generator the bench tier drives, as a library |
| [docs](https://github.com/goceleris/docs) | The source of [goceleris.dev](https://goceleris.dev), where rated bench runs are published |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for setup, the checks CI runs and the `cluster-ok` label, and
[SECURITY.md](SECURITY.md) to report a vulnerability.

## License

Apache-2.0. See [LICENSE](LICENSE).
