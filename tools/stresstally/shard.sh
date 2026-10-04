#!/usr/bin/env bash
# One shard of the celeris stress workflow (.github/workflows/celeris-stress.yml).
# On GitHub it is the whole shard job; on the cluster (STRESS_TARGET=cluster)
# tools/stresstally/cluster-host.sh calls it once per shard, or per
# observation of a timing (STRESS_MODE=timing).
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
target=${STRESS_TARGET:-github}
mode=${STRESS_MODE:-stress}
celeris_dir=${STRESS_CELERIS_DIR:-celeris}
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=tools/stresstally/pkgpaths.sh
. "$here/pkgpaths.sh"

mkdir -p "$(dirname "$STRESS_LOG")"
: >"$STRESS_LOG"
hdr() { printf 'stress-header: %s=%s\n' "$1" "$2" >>"$STRESS_LOG"; }

# What the runner gave us before any change, recorded for the record: the
# arm64 image's default is not documented.
memlock_default=$(awk '/^Max locked memory/ {print $4 ":" $5}' /proc/self/limits)

# RLIMIT_MEMLOCK for this shell; go test and the test binary inherit it. This
# is how celeris's own ci.yml sets it (sudo prlimit on the step's shell). The
# 8m shape is set explicitly rather than trusted to be the runner default,
# because the default is not documented for the arm64 image.
# If prlimit fails, carry on: the shape check below then refuses the shard
# with the memlock actually in force, in the log, instead of dying silently.
#
# On the cluster the runner is a long-lived process of the host's own user,
# so no prompt may ever be reached: plain prlimit first (lowering, or keeping
# an unlimited limit, needs no privilege), then `sudo -n`, which fails at once
# instead of asking for a password. It changes this shell's limit only.
set_memlock() {
	local want="${STRESS_MEMLOCK_LIMIT}:${STRESS_MEMLOCK_LIMIT}"
	if [ "$target" = "cluster" ]; then
		prlimit --pid "$$" --memlock="$want" 2>/dev/null || sudo -n prlimit --pid "$$" --memlock="$want"
	elif command -v sudo >/dev/null 2>&1; then
		sudo prlimit --pid "$$" --memlock="$want"
	else
		prlimit --pid "$$" --memlock="$want"
	fi
}
set_memlock || echo "::warning::prlimit could not set memlock to ${STRESS_MEMLOCK_LIMIT}"
# Read back from a CHILD process, which is what the test binary will be.
in_force=$(awk '/^Max locked memory/ {print $4 ":" $5}' /proc/self/limits)

actual_sha=$(git -C "$celeris_dir" rev-parse HEAD)
machine=$(uname -m)
host=$(hostname -s 2>/dev/null || hostname)

# fact prints what a probe printed, or n/a: host facts are for the record and
# never stop a shard.
fact() {
	local v
	v=$("$@" 2>/dev/null) || v=""
	[ -n "$v" ] || v="n/a"
	printf '%s' "$v"
}
cpu_model() {
	lscpu 2>/dev/null | awk -F: '/^Model name/ {sub(/^[ \t]+/, "", $2); print $2; exit}'
}
pmu_devices() {
	local d out=""
	for d in /sys/bus/event_source/devices/*; do out="${out:+$out,}${d##*/}"; done
	printf '%s' "$out"
}
loadavg() { awk '{print $1 "," $2 "," $3}' /proc/loadavg; }
if [ "$target" = "cluster" ]; then
	# The OS release, so the summary's cross-arch image warning compares the
	# two hosts' releases as it compares the two runner images on GitHub.
	os_release() (
		# shellcheck source=/dev/null
		. /etc/os-release && printf '%s%s baremetal' "$ID" "$VERSION_ID"
	)
	image=$(fact os_release)
else
	image="${ImageOS-unknown} ${ImageVersion-unknown}"
fi

args=(test -v "-count=${STRESS_COUNT}" "-shuffle=${STRESS_SHUFFLE}" "-timeout=${STRESS_TIMEOUT}" "-run=${STRESS_RUN}")
if [ "$STRESS_RACE" = "true" ]; then
	args+=(-race)
fi
# Timing: go test re-links the test binary on every run, so the observation
# is taken INSIDE go test's -exec: obs.sh runs the binary once and measures
# only it. The whole go test is pinned to the host's chosen CPUs (taskset),
# so the binary inherits them and GOMAXPROCS follows. -trimpath keeps each
# arm's checkout directory out of the binary (and out of cmd/go's action
# IDs), so two arms of one commit build the same bytes: the A/A witness the
# summary checks.
prefix=()
if [ "$mode" = "timing" ]; then
	args+=(-trimpath "-exec=${STRESS_OBS_WRAPPER:?}")
	prefix=(taskset -c "${STRESS_CPUSET:-}")
	STRESS_OBS_FILE="${TMPDIR:-/tmp}/stress-obs.$$"
	: >"$STRESS_OBS_FILE"
	export STRESS_OBS_FILE STRESS_OBS_SEQ="${STRESS_OBS_SEQ:-0}" STRESS_PERF="${STRESS_PERF:-off}"
fi
read -r -a flags <<<"$STRESS_FLAGS"
read -r -a asked <<<"$STRESS_PACKAGES"
# Each pattern runs where this celeris commit keeps the package (pkgpaths.sh):
# celeris#443 moves packages under internal/, and one row must work for a
# commit on either side of the move. The header keeps the patterns as asked
# (the summary checks them against the plan) and records what ran in
# packages_resolved.
pkgs=()
for p in "${asked[@]}"; do
	resolve_pkg "$celeris_dir" "$p"
	pkgs+=("$pkg_out")
	[ "$pkg_out" = "$p" ] || echo "package $p runs as $pkg_out ($pkg_why)"
done
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
hdr memlock_default "$memlock_default"
hdr memlock_in_force "$in_force"
hdr race "$STRESS_RACE"
hdr count "$STRESS_COUNT"
# go test's -timeout bounds each test binary (one per package), not the
# whole command; the job limit allows one timeout per package.
hdr timeout "$STRESS_TIMEOUT"
hdr job_timeout_minutes "${STRESS_JOB_TIMEOUT-}"
hdr run "$STRESS_RUN"
hdr packages "$STRESS_PACKAGES"
hdr packages_resolved "${pkgs[*]}"
hdr flags "$STRESS_FLAGS"
hdr env "$STRESS_ENV"
hdr machine "$machine"
hdr kernel "$(uname -r)"
hdr uname "$(uname -a)"
hdr nproc "$(nproc)"
hdr image "$image"
hdr go_version "$(cd "$celeris_dir" && go version)"
hdr cmd "$(printf '%q ' ${prefix[@]+"${prefix[@]}"} go "${args[@]}")"
hdr started "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
# Where and on what the shard ran: best effort, never a reason to stop.
hdr target "$target"
hdr mode "$mode"
hdr host "$host"
hdr cpu_model "$(fact cpu_model)"
hdr cpus_online "$(fact getconf _NPROCESSORS_ONLN)"
hdr smt_active "$(fact cat /sys/devices/system/cpu/smt/active)"
hdr governor "$(fact cat /sys/devices/system/cpu/cpu0/cpufreq/scaling_governor)"
hdr boost "$(fact cat /sys/devices/system/cpu/cpufreq/boost)"
hdr perf_event_paranoid "$(fact cat /proc/sys/kernel/perf_event_paranoid)"
hdr pmu_devices "$(fact pmu_devices)"
hdr loadavg_before "$(fact loadavg)"
if [ "$mode" = "timing" ]; then
	hdr cpus_asked "${STRESS_CPUS-}"
	hdr pmu_asked "${STRESS_PMU-}"
	hdr cpuset "${STRESS_CPUSET-}"
	hdr perf "${STRESS_PERF-}"
	hdr quiet "${STRESS_QUIET-}"
	hdr obs_seq "${STRESS_OBS_SEQ-}"
fi
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
if [ "$target" = "cluster" ] && [ "$host" != "${STRESS_HOST-}" ]; then
	refuse="${refuse:+$refuse; }host is $host, want ${STRESS_HOST-}"
fi
# A reason the host script found before any shard ran (cluster-host.sh).
if [ -n "${STRESS_PRE_REFUSE-}" ]; then
	refuse="${refuse:+$refuse; }${STRESS_PRE_REFUSE}"
fi
if [ "$mode" = "timing" ]; then
	[ -n "${STRESS_CPUSET-}" ] || refuse="${refuse:+$refuse; }timing without a CPU set"
	if [ "${STRESS_PMU-}" = "required" ] && [ "${STRESS_PERF-}" != "ok" ]; then
		refuse="${refuse:+$refuse; }pmu required but perf is ${STRESS_PERF-unprobed}"
	fi
fi
if [ -n "$refuse" ]; then
	[ "$mode" != "timing" ] || rm -f "$STRESS_OBS_FILE"
	printf 'stress-refused: %s\n' "$refuse" >>"$STRESS_LOG"
	printf 'stress-trailer: exit=refused elapsed_s=0\n' >>"$STRESS_LOG"
	echo "::error::shard refused to run: $refuse"
	exit 1
fi

start=$SECONDS
rc=0
(cd "$celeris_dir" && ${prefix[@]+"${prefix[@]}"} go "${args[@]}") >>"$STRESS_LOG" 2>&1 || rc=$?
elapsed=$((SECONDS - start))
if [ "$mode" = "timing" ]; then
	# obs.sh's line(s): exactly one when the test binary ran once.
	cat "$STRESS_OBS_FILE" >>"$STRESS_LOG"
	rm -f "$STRESS_OBS_FILE"
fi
printf 'stress-after: loadavg=%s\n' "$(fact loadavg)" >>"$STRESS_LOG"
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
