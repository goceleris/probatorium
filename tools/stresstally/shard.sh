#!/usr/bin/env bash
# One shard of the celeris stress workflow (.github/workflows/celeris-stress.yml).
#
# Runs `go test -v` once in a celeris checkout and writes everything the
# summary needs into ONE log: a header (what ran, where, in which shape), the
# raw go test output, and a trailer carrying go test's exit status. The
# summary (`stresstally summarize`) judges the run from that log alone, so a
# log with no trailer is a shard that was cut short, never a pass.
#
# Every STRESS_* value was validated by `stresstally plan` before it reached
# this job. It is still only ever used as ONE argv element or ONE exported
# NAME=VALUE: nothing is eval'd and nothing is word-split by the shell except
# the space-separated token lists, which `read -r -a` splits without globbing.
#
# The exit status is 0 whenever go test ran and the log is complete, whatever
# go test decided: the summary job is the verdict. A non-zero exit here means
# the shard could not run in the shape it was asked for (memlock not applied,
# wrong celeris commit, wrong architecture).
set -euo pipefail

: "${STRESS_LOG:?}" "${STRESS_CASE:?}" "${STRESS_ARCH:?}" "${STRESS_SHARD:?}"
: "${STRESS_SHUFFLE:?}" "${STRESS_PACKAGES:?}" "${STRESS_COUNT:?}" "${STRESS_TIMEOUT:?}"
: "${STRESS_MEMLOCK:?}" "${STRESS_MEMLOCK_LIMIT:?}" "${STRESS_RACE:?}" "${STRESS_CELERIS_SHA:?}"
STRESS_RUN=${STRESS_RUN-}
STRESS_FLAGS=${STRESS_FLAGS-}
STRESS_ENV=${STRESS_ENV-}
celeris_dir=${STRESS_CELERIS_DIR:-celeris}

mkdir -p "$(dirname "$STRESS_LOG")"
: >"$STRESS_LOG"
hdr() { printf 'stress-header: %s=%s\n' "$1" "$2" >>"$STRESS_LOG"; }

# RLIMIT_MEMLOCK for this shell; go test and the test binary inherit it. This
# is how celeris's own ci.yml sets it (sudo prlimit on the step's shell). The
# 8m shape is set explicitly rather than trusted to be the runner default,
# because the default is not documented for the arm64 image.
if command -v sudo >/dev/null 2>&1; then
	sudo prlimit --pid "$$" --memlock="${STRESS_MEMLOCK_LIMIT}:${STRESS_MEMLOCK_LIMIT}"
else
	prlimit --pid "$$" --memlock="${STRESS_MEMLOCK_LIMIT}:${STRESS_MEMLOCK_LIMIT}"
fi
# Read back from a CHILD process, which is what the test binary will be.
in_force=$(awk '/^Max locked memory/ {print $4 ":" $5}' /proc/self/limits)

actual_sha=$(git -C "$celeris_dir" rev-parse HEAD)
machine=$(uname -m)

args=(test -v "-count=${STRESS_COUNT}" "-shuffle=${STRESS_SHUFFLE}" "-timeout=${STRESS_TIMEOUT}" "-run=${STRESS_RUN}")
if [ "$STRESS_RACE" = "true" ]; then
	args+=(-race)
fi
read -r -a flags <<<"$STRESS_FLAGS"
read -r -a pkgs <<<"$STRESS_PACKAGES"
read -r -a envs <<<"$STRESS_ENV"
args+=("${flags[@]}" "${pkgs[@]}")
for kv in "${envs[@]}"; do
	export "${kv?}"
done

hdr case "$STRESS_CASE"
hdr arch "$STRESS_ARCH"
hdr runner "${STRESS_RUNNER-}"
hdr shard "$STRESS_SHARD"
hdr shuffle "$STRESS_SHUFFLE"
hdr celeris_ref "${STRESS_CELERIS_REF-}"
hdr celeris_sha "$actual_sha"
hdr expected_sha "$STRESS_CELERIS_SHA"
hdr memlock "$STRESS_MEMLOCK"
hdr memlock_limit "$STRESS_MEMLOCK_LIMIT"
hdr memlock_in_force "$in_force"
hdr race "$STRESS_RACE"
hdr count "$STRESS_COUNT"
hdr timeout "$STRESS_TIMEOUT"
hdr run "$STRESS_RUN"
hdr packages "$STRESS_PACKAGES"
hdr flags "$STRESS_FLAGS"
hdr env "$STRESS_ENV"
hdr machine "$machine"
hdr kernel "$(uname -r)"
hdr uname "$(uname -a)"
hdr nproc "$(nproc)"
hdr image "${ImageOS-unknown} ${ImageVersion-unknown}"
hdr go_version "$(cd "$celeris_dir" && go version)"
hdr cmd "$(printf '%q ' go "${args[@]}")"
hdr started "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
printf 'stress-header-end\n' >>"$STRESS_LOG"

cat "$STRESS_LOG"

# The shape this shard was asked for. A mismatch is refused before go test
# runs: its verdicts would describe a different experiment.
refuse=""
case "$STRESS_MEMLOCK_LIMIT" in
unlimited) want_limit="unlimited:unlimited" ;;
*) want_limit="${STRESS_MEMLOCK_LIMIT}:${STRESS_MEMLOCK_LIMIT}" ;;
esac
[ "$in_force" = "$want_limit" ] || refuse="memlock in force is $in_force, want $want_limit"
[ "$actual_sha" = "$STRESS_CELERIS_SHA" ] || refuse="${refuse:+$refuse; }celeris HEAD is $actual_sha, want $STRESS_CELERIS_SHA"
case "$STRESS_ARCH/$machine" in
x86/x86_64 | arm64/aarch64) ;;
*) refuse="${refuse:+$refuse; }arch $STRESS_ARCH but the machine is $machine" ;;
esac
if [ -n "$refuse" ]; then
	printf 'stress-refused: %s\n' "$refuse" >>"$STRESS_LOG"
	printf 'stress-trailer: exit=refused elapsed_s=0\n' >>"$STRESS_LOG"
	echo "::error::shard refused to run: $refuse"
	exit 1
fi

start=$SECONDS
rc=0
(cd "$celeris_dir" && go "${args[@]}") >>"$STRESS_LOG" 2>&1 || rc=$?
elapsed=$((SECONDS - start))
printf 'stress-trailer: exit=%d elapsed_s=%d\n' "$rc" "$elapsed" >>"$STRESS_LOG"

# A short view for the job log; the whole log is the artifact.
pass=$(grep -cE '^[[:space:]]*--- PASS: ' "$STRESS_LOG" || true)
fail=$(grep -cE '^[[:space:]]*--- FAIL: ' "$STRESS_LOG" || true)
skip=$(grep -cE '^[[:space:]]*--- SKIP: ' "$STRESS_LOG" || true)
echo "go test exit=$rc after ${elapsed}s; verdict lines: PASS $pass, FAIL $fail, SKIP $skip (subtests included)"
echo "The summary job judges this shard from its log; this job only reports that it ran."
if [ "$fail" -gt 0 ]; then
	echo "::group::FAIL lines (first 50)"
	grep -m 50 -E '^[[:space:]]*--- FAIL: ' "$STRESS_LOG" || true
	echo "::endgroup::"
fi
echo "::group::last 80 lines of the log"
tail -n 80 "$STRESS_LOG"
echo "::endgroup::"
