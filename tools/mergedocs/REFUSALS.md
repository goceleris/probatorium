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
other counts as equal). The one accepted difference is a celeris pin bump:
every differing file must be `servers/celeris/go.mod` or `servers/celeris/go.sum`
(the pin lives there, and only the candidate dispatch carries celeris columns, so
such a bump cannot touch what the frameworks dispatch measured), proved with
`--harness-repo DIR` (a probatorium checkout) by `git diff --name-only A B`.
Any other file (adapter code in `servers/celeris/`, `servers/celeris_v1511/`,
`validation/`, mage code) is refused, as are identical trees, a missing
`--harness-repo`, and a git failure. The
celeris pin of the documents is checked separately in R10.

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
