#!/usr/bin/env bash
# One host job of the celeris stress workflow's cluster target
# (.github/workflows/celeris-stress-cluster.yml): every shard of one arch, or
# every observation of a timing, run back to back on that arch's bare-metal
# host, one go test process each, through tools/stresstally/shard.sh, which
# writes each shard's log exactly as on GitHub.
#
# Before the first shard it records the host (host-facts file: CPU, cores and
# their classes, governor, boost, perf, load, the busiest processes), and in
# timing mode it
#   - waits up to 10 minutes for a quiet host: host-wide busy CPU under 5%
#     over 5 s AND a 1-minute load under 1.0, polled every 15 s; a host that
#     never gets quiet refuses every observation (no data is taken, so none
#     is ever dropped after the fact);
#   - chooses the CPUs every observation is pinned to: STRESS_CPUS primary
#     SMT threads of the fastest core class (cpu_capacity), never cpu0's
#     core; their SMT siblings stay idle. Too few such CPUs refuses;
#   - probes `perf stat -e instructions:u` (needs perf installed and
#     kernel.perf_event_paranoid <= 2, or privilege). pmu=required refuses
#     every observation without it.
# It changes no host setting: governor, boost, paranoid and isolation are
# recorded, never set. It kills nothing but its own shards. Go's caches and
# modules live under $RUNNER_TEMP and its temporary files in /tmp/cstress
# (tools/stresstally/gostate.sh: all writable, and wiped by the job's last
# step), never in the host's shared ~/go or ~/.cache/go-build that the tiers
# build from.
#
# Inputs (validated by `stresstally plan`): STRESS_ARCH, STRESS_HOST,
# STRESS_MODE (stress|timing), STRESS_SEQUENCE (CASE:SHARD:SHUFFLE tokens in
# run order), STRESS_CASE_SHAS and STRESS_CASE_REFS (CASE=VALUE tokens),
# STRESS_LOG_DIR, STRESS_FACTS, STRESS_CPUS, STRESS_PMU, and the shared
# go test configuration shard.sh reads (STRESS_PACKAGES ... STRESS_ENV).
# Exit status: 0 when every shard ran (the summary judges them), 1 when any
# shard refused to run in the shape asked for.
#
# Run, it runs main. Sourced (the tests do), it only defines its functions;
# the tests point sysfs and procfs at fixtures (STRESS_SYSFS, STRESS_PROCFS)
# and shorten the waits (STRESS_BUSY_SECONDS, STRESS_QUIET_SECONDS,
# STRESS_QUIET_POLL). The workflow sets none of these.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=tools/stresstally/gostate.sh
. "$here/gostate.sh"
sysfs=${STRESS_SYSFS:-/sys/devices/system/cpu}
procfs=${STRESS_PROCFS:-/proc}

note() { printf '%s\n' "$*" | tee -a "$STRESS_FACTS"; }
section() { printf '\n=== %s (%s)\n' "$1" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >>"$STRESS_FACTS"; }

# busy prints host-wide busy CPU percent over 5 s (/proc/stat: everything
# but idle and iowait).
busy() {
	local a b
	read -r -a a <"$procfs/stat"
	sleep "${STRESS_BUSY_SECONDS:-5}"
	read -r -a b <"$procfs/stat"
	awk -v a="${a[*]}" -v b="${b[*]}" 'BEGIN {
		n = split(a, x, " "); split(b, y, " "); t = 0
		# user nice system idle iowait irq softirq steal; guest time is
		# already inside user and nice.
		for (i = 2; i <= 9 && i <= n; i++) t += y[i] - x[i]
		idle = (y[5] - x[5]) + (y[6] - x[6])
		if (t <= 0) { print "0.0"; exit }
		printf "%.1f", 100 * (t - idle) / t
	}'
}

snapshot() {
	section "$1"
	{
		echo "loadavg: $(cat "$procfs/loadavg")"
		echo "busy_pct_5s: $(busy)"
		echo "top processes by CPU:"
		ps -eo pid,user,pcpu,etimes,comm --sort=-pcpu 2>/dev/null | head -n 12 || true
		echo "docker containers: $( (docker ps -q 2>/dev/null || true) | wc -l)"
	} >>"$STRESS_FACTS" 2>&1
}

host_facts() {
	section "host"
	{
		echo "host: $(hostname -s) (planned $STRESS_HOST), arch $STRESS_ARCH, mode $STRESS_MODE"
		echo "kernel: $(uname -a)"
		grep -E '^(PRETTY_NAME|VERSION_ID)=' /etc/os-release 2>/dev/null || true
		lscpu 2>/dev/null || true
		echo "smt active: $(cat "$sysfs/smt/active" 2>/dev/null || echo n/a)"
		echo "boost: $(cat "$sysfs/cpufreq/boost" 2>/dev/null || echo n/a)"
		echo "amd_pstate: $(cat "$sysfs/amd_pstate/status" 2>/dev/null || echo n/a)"
		echo "perf_event_paranoid: $(cat /proc/sys/kernel/perf_event_paranoid 2>/dev/null || echo n/a)"
		echo "pmu devices: $(cd /sys/bus/event_source/devices 2>/dev/null && printf '%s ' *)"
		echo "perf: $(command -v perf || echo absent)"
		echo "taskset: $(command -v taskset || echo absent)"
		echo "memlock of this job: $(awk '/^Max locked memory/ {print $4 ":" $5}' /proc/self/limits)"
		echo "io_uring_disabled: $(cat /proc/sys/kernel/io_uring_disabled 2>/dev/null || echo n/a)"
		echo "cpu  capacity  siblings  governor  cur_khz  max_khz"
		local d
		for d in "$sysfs"/cpu[0-9]*; do
			printf '%s  %s  %s  %s  %s  %s\n' "${d##*cpu}" "$(cat "$d/cpu_capacity" 2>/dev/null || echo -)" \
				"$(cat "$d/topology/thread_siblings_list" 2>/dev/null || echo -)" "$(cat "$d/cpufreq/scaling_governor" 2>/dev/null || echo -)" \
				"$(cat "$d/cpufreq/scaling_cur_freq" 2>/dev/null || echo -)" "$(cat "$d/cpufreq/scaling_max_freq" 2>/dev/null || echo -)"
		done
	} >>"$STRESS_FACTS" 2>&1
}

# pick_cpus N: the N highest-numbered primary SMT threads of the fastest core
# class, cpu0's core excluded. Prints the list, or a reason on stderr.
pick_cpus() {
	local n=$1 d c first sib cap best=0
	local -a cand=()
	for d in "$sysfs"/cpu[0-9]*; do
		c=${d##*cpu}
		[ "$c" != 0 ] || continue
		if [ -r "$d/online" ] && [ "$(cat "$d/online")" != 1 ]; then continue; fi
		sib=$(cat "$d/topology/thread_siblings_list" 2>/dev/null || echo "$c")
		first=${sib%%[,-]*}
		[ "$first" = "$c" ] || continue
		case "$sib" in 0 | 0,* | 0-*) continue ;; esac
		cap=$(cat "$d/cpu_capacity" 2>/dev/null || echo 1024)
		cand+=("$cap:$c")
		[ "$cap" -le "$best" ] || best=$cap
	done
	local -a pick=()
	local x
	for x in "${cand[@]}"; do
		[ "${x%%:*}" = "$best" ] && pick+=("${x#*:}")
	done
	if [ "${#pick[@]}" -lt "$n" ]; then
		echo "only ${#pick[@]} primary CPU(s) of the fastest class (capacity $best) besides cpu0's core; asked for $n" >&2
		return 1
	fi
	printf '%s\n' "${pick[@]}" | sort -n | tail -n "$n" | paste -sd, -
}

# perf_probe sets perf_state: ok when `perf stat -e instructions:u` counts,
# absent without perf, else unusable (with the paranoid level).
perf_probe() {
	if ! command -v perf >/dev/null; then
		perf_state="absent"
	elif perf stat "-x," -e instructions:u -o "$TMPDIR/probe.perf" -- true >/dev/null 2>&1 &&
		awk -F, '$3 ~ /^instructions/ && $1 ~ /^[0-9]+$/ {ok = 1} END {exit !ok}' "$TMPDIR/probe.perf"; then
		perf_state="ok"
	else
		perf_state="unusable:paranoid=$(cat /proc/sys/kernel/perf_event_paranoid 2>/dev/null || echo n/a)"
	fi
}

# quiet_wait sets quiet, and adds to pre_refuse when the host never got quiet.
quiet_wait() {
	local deadline load1 busy_pct
	deadline=$((SECONDS + ${STRESS_QUIET_SECONDS:-600}))
	while :; do
		load1=$(awk '{print $1}' "$procfs/loadavg")
		busy_pct=$(busy)
		if awk -v l="$load1" -v b="$busy_pct" 'BEGIN {exit !(l < 1.0 && b < 5.0)}'; then
			quiet="quiet:load1=$load1,busy_pct=$busy_pct"
			break
		fi
		if [ "$SECONDS" -ge "$deadline" ]; then
			quiet="loud:load1=$load1,busy_pct=$busy_pct"
			pre_refuse="${pre_refuse:+$pre_refuse; }the host did not get quiet in 10 minutes (load1 $load1, busy $busy_pct%)"
			break
		fi
		sleep "${STRESS_QUIET_POLL:-10}"
	done
}

main() {
	: "${STRESS_ARCH:?}" "${STRESS_HOST:?}" "${STRESS_MODE:?}" "${STRESS_SEQUENCE:?}" "${STRESS_CASE_SHAS:?}"
	: "${STRESS_LOG_DIR:?}" "${STRESS_FACTS:?}" "${RUNNER_TEMP:?}"
	gostate_init
	mkdir -p "$STRESS_LOG_DIR" "$(dirname "$STRESS_FACTS")"
	: >"$STRESS_FACTS"

	local -A sha_of ref_of
	local kv
	for kv in $STRESS_CASE_SHAS; do sha_of[${kv%%=*}]=${kv#*=}; done
	for kv in ${STRESS_CASE_REFS-}; do ref_of[${kv%%=*}]=${kv#*=}; done

	host_facts
	snapshot "before the first shard"

	cpuset="" perf_state="off" quiet="" pre_refuse=""
	if [ "$STRESS_MODE" = "timing" ]; then
		: "${STRESS_CPUS:?}" "${STRESS_PMU:?}"
		command -v taskset >/dev/null || pre_refuse="taskset is not installed"
		if ! cpuset=$(pick_cpus "$STRESS_CPUS" 2>"$TMPDIR/pick.err"); then
			cpuset=""
			pre_refuse="${pre_refuse:+$pre_refuse; }$(cat "$TMPDIR/pick.err")"
		fi
		note "cpuset: ${cpuset:-none}"
		perf_probe
		note "perf: $perf_state"
		quiet_wait
		note "quiet check: $quiet"
		snapshot "after the quiet check"
	fi

	export STRESS_TARGET=cluster STRESS_HOST STRESS_RUNNER="$STRESS_HOST" STRESS_MODE STRESS_CPUS STRESS_PMU
	export STRESS_CPUSET="$cpuset" STRESS_PERF="$perf_state" STRESS_QUIET="$quiet" STRESS_PRE_REFUSE="$pre_refuse"
	export STRESS_OBS_WRAPPER="$here/obs.sh"

	local -a seq
	local refused=0 i=0 tok c shard shuffle
	read -r -a seq <<<"$STRESS_SEQUENCE"
	for tok in "${seq[@]}"; do
		i=$((i + 1))
		if ! [[ $tok =~ ^([A-Za-z0-9]+):([0-9]+):(off|[0-9]+)$ ]]; then
			note "sequence token $tok is not CASE:SHARD:SHUFFLE; refusing the host job"
			exit 1
		fi
		c=${BASH_REMATCH[1]} shard=${BASH_REMATCH[2]} shuffle=${BASH_REMATCH[3]}
		echo "=== [$i/${#seq[@]}] case $c shard $shard (-shuffle=$shuffle)"
		STRESS_CASE=$c STRESS_SHARD=$shard STRESS_SHUFFLE=$shuffle STRESS_OBS_SEQ=$i \
			STRESS_CELERIS_SHA=${sha_of[$c]:?no commit for case $c} STRESS_CELERIS_REF=${ref_of[$c]-} \
			STRESS_CELERIS_DIR="celeris-$c" STRESS_LOG="$STRESS_LOG_DIR/${c}__${STRESS_ARCH}__${shard}.log" \
			bash "$here/shard.sh" || refused=$((refused + 1))
		note "$(date -u +%H:%M:%SZ) [$i/${#seq[@]}] $c shard $shard: $(tail -n 1 "$STRESS_LOG_DIR/${c}__${STRESS_ARCH}__${shard}.log" 2>/dev/null || echo no log)"
	done

	snapshot "after the last shard"
	if [ "$refused" -gt 0 ]; then
		echo "::error::$refused of ${#seq[@]} shard(s) refused to run in the shape asked for on $STRESS_HOST; see their logs and the host facts"
		exit 1
	fi
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
	main "$@"
fi
