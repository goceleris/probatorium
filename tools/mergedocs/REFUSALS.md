# mergedocs refusal list

This file is the frozen refusal list of `tools/mergedocs`. It is embedded in
the binary (go:embed) and its sha256 is written into every COMPOSITION.json
(`refusals_sha256`), so "the list that was frozen" is a hash, not a promise.
Every item has a stable ID, one mutation test in `refusals_test.go`
(`TestRefusalCases`), and a meta-test that fails if an ID below has no case.

A refusal prints `REFUSE Rn: ...` on stderr for every violated item (all of
them, not only the first), writes nothing, and exits with status 2. There is
no override flag for any item. Composition is "A = candidate + twin
dispatch, B = frameworks dispatch, both of ONE architecture".

General rule (R19): a value that is missing on EITHER side is a refusal, never
"equal". Empty == empty never passes a check.

## R1 architecture
`host_arch_pair` differs between the two documents, or the timeseries sidecar
`arch` label of either document is empty/different from the document's own arch
tag (x86_64 / arm64), or differs between the two sidecars (study case C4).

## R2 schema version
`schema_version` differs between the documents.

## R3 harness commit
The probatorium commit that ran the dispatch (gh run `headSha`, the `head=`
line of the read-wrapper params.txt, or `harness_commit` in a meta file)
differs between the two dispatches (an abbreviation of 7+ hex digits of the
other counts as equal). Two different harness commits are accepted only when
NO file that can change a measurement differs (bot ruling msg_48951bca24af,
which scopes the earlier msg_259b3b2617d1): readers, validators, the composer,
tools, tests and docs may differ between the two dispatches, so a reader fix
after the screen does not throw the frameworks dispatch away.

It is proved with `--harness-repo DIR` (a probatorium checkout) from
`git diff --name-only -z --no-renames A B` (a rename lists both paths, so a
deletion cannot hide behind its destination). Each changed path is classified
by the first list below that matches it, in this order:

1. `r3-pin`: ACCEPT. A celeris pin bump: `go.mod`/`go.sum` under
   `servers/celeris/` and `validation/refapp/` at any depth. Nothing else under
   those trees (Go files included) is a pin.
2. `r3-inert`: ACCEPT. Files that no dispatch executes: tests, test data, docs.
   This list wins over the measurement list, so a `_test.go` file inside an
   adapter directory is inert.
3. `r3-measurement`: REFUSE. Files that execute or configure a measurement or
   change host state: the runner (mage and `cmd/`), the ansible bench, deploy,
   cleanup and tuning tasks and their roles and files, the runner provisioning,
   the host sampler and observer, the cluster power handling, the server
   adapters, the loadgen pin
   (the root `go.mod`/`go.sum`), the profiles and budgets, the scenarios, the
   report schema, the dispatching workflows and cluster actions, and the cpu
   power setter and host recording.
4. `r3-non-measurement`: ACCEPT. Readers and validators (`validation/`,
   `mage_diff.go`, `mage_gate.go`, `mage_validate.go`, `mage_fault_control.go`:
   none of their symbols is used by the runner files; `mage_publish.go` IS used
   by BenchTier and so is a measurement path), the composer, stresstally and
   the `tools/*-read*` readers (any other path under `tools/` is unknown),
   lint/test/publish workflows, and the host
   operations whose outcome the composer checks itself (OS upgrade and reboot:
   kernel R6, boot id and command line R7).
5. A path that no list matches: REFUSE ("unknown paths are refused by default"),
   so the lists cannot silently miss a file. So are an identical tree, a path
   that is not a clean relative path (`..`, leading `/`), a missing
   `--harness-repo`, and a git failure.

Every refused path is named in the refusal. The globs are exact: `**` stands
for any number of directories (also none), `*` and `?` never cross a `/`, and
the match is case sensitive. This section is parsed by the composer (the four
fenced lists below ARE the rule, and their text is part of the frozen hash).
Adding a measurement-relevant path is a change of this file and so of the
sha256. The celeris pin of the documents is checked separately in R10, the
loadgen pin in R4.

```r3-pin
servers/celeris/**/go.mod
servers/celeris/**/go.sum
validation/refapp/**/go.mod
validation/refapp/**/go.sum
```

```r3-inert
**/*_test.go
ansible/tests/**
ops/**/test_*.py
tools/**/test_*.py
report/testdata/**
**/*.md
docs/**
LICENSE
```

```r3-measurement
# the runner: mage targets that Bench, BenchTier, Deploy and Cleanup run
magefile.go
mage_bench*.go
mage_tier.go
mage_cluster.go
mage_helpers.go
mage_sampler.go
# Publish: BenchTier runs publishPreflight before the bench and Publish after it
mage_publish.go
cmd/runner/**
cmd/loadgen/**
cmd/observer/**
# the skip list that decides which cells run
cmd/smoketest/**
# profiles, budgets, scenarios, services, interleave, the document schema
budget/**
scenarios/**
services/**
interleave/**
report/**
# the loadgen pin and the module graph of the runner; the mage build tool pin
go.mod
go.sum
.github/tools/**
# server adapters, their lock files, the baseline and twin modules, the
# reference apps and their arms (pin bumps are r3-pin and come first)
servers/**
validation/refapp/**
validation/refapparms/**
# ansible: what runs on the cluster hosts during a dispatch
ansible/ansible.cfg
ansible/inventory.yml
ansible/bench.yml
ansible/deploy.yml
ansible/cleanup.yml
ansible/host-sampler.yml
ansible/forensics.yml
ansible/rescue-results.yml
ansible/power-guard.yml
# provisioning that cluster-runner-up and cluster-runner-down run on every dispatch
ansible/runner-setup.yml
ansible/runner-teardown.yml
ansible/roles/**
ansible/files/nic_irq_pin.sh
ansible/tasks/run_bench_cell.yml
ansible/tasks/bench_tuning.yml
ansible/tasks/build_native_competitor.yml
ansible/tasks/nic_irq_placement.yml
ansible/tasks/nic_irq_placement_end.yml
# cpu power setter and host recording (they change host state)
# (? is one character: cpu_power, cpu-power, cpu.power; path.Match has no literal -)
ansible/tasks/cpu_power.yml
**/*cpu?power*
**/*cpupower*
**/cpu?power*/**
# host-side programs and units: sampler, forensics, rescue, power handling
ops/host-sampler/**
ops/forensics/**
ops/rescue/**
ops/power/**
**/stallcap*
**/stall?cap*
# workflows that dispatch measurements, and the cluster composite actions
.github/workflows/benchmark-tier.yml
.github/workflows/matrix-*.yml
.github/actions/**
# the script every dispatching workflow runs before its first ansible call
.github/scripts/refresh-cluster-known-hosts.sh
```

```r3-non-measurement
# the composer, stresstally and the readers (govab-read.py, c2lad-read...); any
# other path under tools/ is unknown and refused, so a setter that lands there
# cannot be accepted by a glob that was meant for readers
tools/mergedocs/**
tools/stresstally/**
tools/*-read*
tools/*-read*/**
# readers and validators (the pre-bench conformance gate and the validator commands)
validation/**
cmd/conformance/**
cmd/validator/**
cmd/validator-checker/**
cmd/validator-replay/**
mage_diff.go
mage_gate.go
mage_validate.go
mage_fault_control.go
ansible/validate.yml
ansible/validate-ssh.yml
ansible/fuzz.yml
ansible/load-check.sh
# host operations verified by R6 (kernel) and R7 (boot id, command line)
ansible/os-upgrade.yml
ansible/reboot.yml
ansible/files/os_upgrade.py
ansible/files/reboot_snapshot.sh
ansible/tasks/os_upgrade_*.yml
ansible/tasks/reboot_request.yml
ansible/vars/os_upgrade_judge.yml
ansible/vars/reboot_judge.yml
# lint, test, publish, stress and ops workflows, scripts, repository metadata
.github/workflows/lint.yml
.github/workflows/test.yml
.github/workflows/coverage.yml
.github/workflows/publish-results.yml
.github/workflows/cluster-os-upgrade.yml
.github/workflows/cluster-reboot.yml
.github/workflows/celeris-stress.yml
.github/workflows/celeris-stress-cluster.yml
.github/scripts/os-upgrade-*.sh
.github/scripts/reboot-summary.sh
.github/CODEOWNERS
.github/PULL_REQUEST_TEMPLATE.md
.github/actionlint.yaml
.github/dependabot.yml
.github/cover.png
.coderabbit.yaml
.gitignore
codecov.yml
```

## R4 loadgen version
`benchmark_config.loadgen_version` differs. (It is the merge host's go.mod
value, not the SUT's: a different value means a different harness checkout.)

## R5 Go toolchain
The Go rows (`language == "go"`) of one document carry more than one
`language_version`, or the single value differs between the two documents. The
value is the merge host's `runtime.Version()`, so it also catches a tool-cache
change between dispatches.

## R6 kernel
The SUT host's kernel release differs (cluster-forensics `uname`, or `kernel=`
of the meta file).

## R7 reboot between the dispatches
The SUT host's boot id differs, i.e. the host rebooted between the dispatches.
A reboot is accepted ONLY when all of these hold: the kernel is the same (R6),
the boot command line is the same in both (meta key `boot_cmdline`, the
`/proc/cmdline` of the survey; missing on a side is a refusal), and every
anchor's drift is within the 5 % floor of the tie margin on every scenario (an
anchor scenario measured in one dispatch only, or not ok, cannot be verified and
refuses too). When accepted, COMPOSITION.json (`reboot.between_dispatches`,
`reboot.note`) and the published `composition.json` say that a reboot lay
between the two halves. Schedule the two dispatches of one architecture as close
together as the gates allow: that is the only mitigation this tool has.

## R8 profile
The dispatch `profile` input differs. The profile is not stored in any
document or artifact: it is operator-asserted in the meta file and is required.

## R9 window
`benchmark_config.duration`, `warmup`, `runs` or `scenarios_filter` differ.

## R10 celeris pin
`benchmark_config.git_ref` or `celeris_version` differs between the documents
while the peers document also carries a celeris-category column (a celeris row
measured against another pin would be published under the candidate's pin).
If the peers document carries no celeris column the differing pin is only
recorded in COMPOSITION.json.

## R11 environment
`environment` differs in `fabric`, `fabric_line_rate_bits_per_sec`,
`loadgen_host`, `sut_env`, `kernel_sysctls_applied`, `arch_exclusion` or the
configuration of `nic_irq_placement` (host, interface, vector, binding, cpu).
The kernel readbacks (`irqs`, `at_end`, `changed`, `verified`) are not compared:
they are measurements, not configuration.

## R12 run identity and date
One published cell is ONE document under ONE date (the docs build silently
drops a same-date run-1 once a run-N exists, study case C5; it merges two dates
by averaging, C6). The composer refuses when the two inputs are the same run
(equal run id, or byte-identical results.json), or when their
[started_at, finished_at] windows overlap (one cluster cannot run two
dispatches at once, so an overlap means a copied or contaminated document).
The composer emits one results.json and publishes it under ONE date that the
operator passes with `--date` (required, no default: the README scripts read
`results/<version>/<date>/{x86_64,arm64}`, so both architectures of a release
must use the same date, and the two single-arch dispatch sequences start on
different days). A missing or malformed `--date` is a usage error (exit 64).

## R13 duplicate column across documents
A column name present in both documents that is not a declared drift anchor
(study cases C2 and C2b: a naive union keeps both copies in the summary and
silently loses half of the histograms).

## R14 duplicate column within a document
A column name appears twice inside one document's `benchmarks`.

## R15 drift anchor
A declared anchor (default gnet-h1, fiber-h1, stdhttp-h1, lithium, h2o, actix) is missing from either
document; or the two rows differ in identity (`framework`, `framework_version`,
`engine`, `compile_options`, `category`, `language`; the toolchain is R5); or the
anchor has no saturation scenario measured ok in both documents, so no drift
can be computed. An unpaired or non-ok anchor CELL is not a refusal: it is
recorded in COMPOSITION.json and warned about.

## R16 timeseries sidecar
A sidecar holds the same (scenario, server) twice; or a series names a server
that has no row in its document (it would be silently dropped); or exactly one
of the two documents has a sidecar (the merged sidecar would silently cover
only part of the columns); or an input sidecar does not decode.

## R17 unmergeable document sections
`validation_results` or `soak_summary` is present in either document (there is
no merge semantics for them; a bench dispatch does not produce them).

## R18 run-directory collision
Both run directories carry a `<TS>-bench-<host>` directory with the same name
(their column directories would merge in the output). The integrity audit of `mage Publish` reads those directories and `raw/*.json`.

## R19 missing metadata
A value needed by another item is missing on either side: run id, harness
commit, profile, SUT host, kernel, boot id, loadgen version, Go toolchain,
`schema_version`, `host_arch_pair`, started_at/finished_at; or a value given
both by the meta file and by the artifact (cluster-forensics) disagrees.

## R20 input does not round-trip
A results.json does not survive a decode into `report.Document` and an encode
back to the same bytes (the way mage wrote it). It means the composer checkout
and the run disagree on the schema, so keys would be silently dropped. Compose
from a checkout at the SHA that ran the dispatches.

## R21 expected columns
Only with `--expect-columns FILE`: a column listed in the file (one per line,
`#` comments allowed) is missing from the merged document.

## R22 composed input
An input directory already carries a COMPOSITION.json (composing a composed
output would hide the drift anchors of the first composition).
