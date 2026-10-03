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
#   - builds every arm's test binary once (prebuild_arms), so the build
#     cache holds every compile before the quiet check;
#   - waits up to 10 minutes for a quiet host: host-wide busy CPU under 5%
#     over 5 s AND a 1-minute load under 1.0, polled every 15 s; a host that
#     never gets quiet refuses every observation (no data is taken, so none
#     is ever dropped after the fact);
#   - chooses the CPUs every observation is pinned to: STRESS_CPUS primary
#     SMT threads of the fastest core class (cpu_capacity; on arm64 also the
#     core type and cluster frequency), never cpu0's core; their SMT
#     siblings stay idle. Too few such CPUs refuses;
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
# Each shard, and each prebuild, runs in a session of its own (setsid), and
# none may outlive the host job's step: the job's always() steps, the Go-state
# wipe among them, run as soon as the step has ended. On SIGINT, SIGTERM or
# SIGHUP (a cancel or a timeout: the runner signals the step's process alone,
# which is this script, because the workflow's host step execs it, through
# env --default-signal, since the cluster's runner starts its steps with
# SIGINT ignored and bash cannot trap that) it kills that session before it
# exits (on_signal). A watchdog in a session of its own kills it when this
# script is gone (SIGKILL runs no trap) or the runner dir is (a lost runner's
# teardown) (watch_runner). It kills nothing else.
#
# Run, it runs main; run as `cluster-host.sh watch SIDFILE PID`, the watchdog
# main starts. Sourced (the tests do), it only defines its functions; the
# tests point sysfs and procfs at fixtures (STRESS_SYSFS, STRESS_PROCFS) and
# shorten or lengthen the waits (STRESS_BUSY_SECONDS, STRESS_QUIET_SECONDS,
# STRESS_QUIET_POLL, STRESS_WATCHDOG_SECONDS). The workflow sets none of these.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=tools/stresstally/gostate.sh
. "$here/gostate.sh"
sysfs=${STRESS_SYSFS:-/sys/devices/system/cpu}
procfs=${STRESS_PROCFS:-/proc}
# The running shard's session (see in_session), and the watchdog.
shard_sidfile="" shard_pid="" watchdog_pid=""

note() { printf '%s\n' "$*" | tee -a "$STRESS_FACTS"; }

# sigmask PID FIELD: FIELD (SigIgn, SigBlk or SigCgt) of /proc/PID/status, a
# hex mask whose bit N-1 is signal N (1 HUP, 2 INT, 4 QUIT, 4000 TERM), or
# "-" when it cannot be read.
sigmask() {
	local m
	m=$(awk -v f="$2:" '$1 == f {print $2}' "/proc/$1/status" 2>/dev/null) || true
	echo "${m:--}"
}
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
		# Whether a cancel's SIGINT can reach on_signal (see main), and what
		# the runner's worker, this script's parent, was started with.
		echo "signals of this script (pid $$): SigIgn $(sigmask $$ SigIgn) SigBlk $(sigmask $$ SigBlk) SigCgt $(sigmask $$ SigCgt)"
		echo "signals of its parent (pid $PPID, $(cat "/proc/$PPID/comm" 2>/dev/null || echo '?')): SigIgn $(sigmask "$PPID" SigIgn) SigBlk $(sigmask "$PPID" SigBlk) SigCgt $(sigmask "$PPID" SigCgt)"
		echo "cpu  capacity  siblings  governor  cur_khz  max_khz  cpuinfo_max_khz  midr"
		local d
		for d in "$sysfs"/cpu[0-9]*; do
			printf '%s  %s  %s  %s  %s  %s  %s  %s\n' "${d##*cpu}" "$(cat "$d/cpu_capacity" 2>/dev/null || echo -)" \
				"$(cat "$d/topology/thread_siblings_list" 2>/dev/null || echo -)" "$(cat "$d/cpufreq/scaling_governor" 2>/dev/null || echo -)" \
				"$(cat "$d/cpufreq/scaling_cur_freq" 2>/dev/null || echo -)" "$(cat "$d/cpufreq/scaling_max_freq" 2>/dev/null || echo -)" \
				"$(cat "$d/cpufreq/cpuinfo_max_freq" 2>/dev/null || echo -)" "$(cat "$d/regs/identification/midr_el1" 2>/dev/null || echo -)"
		done
	} >>"$STRESS_FACTS" 2>&1
}

# pick_cpus N: the N highest-numbered primary SMT threads of the fastest core
# class, cpu0's core excluded. A class is the cores the kernel reports as the
# same: equal cpu_capacity, and on arm64 (where a kernel may report every
# core of a big.LITTLE SoC at capacity 1024) also the same core type (MIDR)
# and the same cluster maximum frequency. The fastest class has the highest
# capacity, then the highest maximum frequency. Prints the list on stdout
# and the classes found on stderr, or a reason on stderr and fails.
pick_cpus() {
	local n=$1 d c first sib cap midr khz
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
		midr=- khz=0
		if [ -r "$d/regs/identification/midr_el1" ]; then
			midr=$(cat "$d/regs/identification/midr_el1")
			khz=$(cat "$d/cpufreq/cpuinfo_max_freq" 2>/dev/null || echo 0)
		fi
		cand+=("$cap $khz $midr $c")
	done
	if [ "${#cand[@]}" -eq 0 ]; then
		echo "no primary CPU besides cpu0's core; asked for $n" >&2
		return 1
	fi
	local classes best bcap bkhz bmidr class x
	classes=$(printf '%s\n' "${cand[@]}" | awk '{k = "capacity " $1; if ($3 != "-") k = k " max " $2 " kHz midr " $3; n[k]++}
		END {for (k in n) print k " x" n[k]}' | sort | paste -sd';' -)
	best=$(printf '%s\n' "${cand[@]}" | awk 'NR == 1 || $1 > bc || ($1 == bc && $2 > bk) {bc = $1; bk = $2; bl = $0} END {print bl}')
	read -r bcap bkhz bmidr _ <<<"$best"
	class="capacity $bcap"
	[ "$bmidr" = - ] || class="$class, max $bkhz kHz, midr $bmidr"
	if printf '%s\n' "${cand[@]}" | awk -v c="$bcap" -v k="$bkhz" -v m="$bmidr" '$1 == c && $2 == k && $3 != m {x = 1} END {exit !x}'; then
		echo "two core types share the fastest capacity and frequency ($class); cannot choose one class (classes: $classes); asked for $n" >&2
		return 1
	fi
	local -a pick=()
	for x in "${cand[@]}"; do
		read -r cap khz midr c <<<"$x"
		[ "$cap $khz $midr" != "$bcap $bkhz $bmidr" ] || pick+=("$c")
	done
	if [ "${#pick[@]}" -lt "$n" ]; then
		echo "only ${#pick[@]} primary CPU(s) of the fastest class ($class) besides cpu0's core; asked for $n (classes: $classes)" >&2
		return 1
	fi
	echo "fastest class: $class; classes: $classes" >&2
	printf '%s\n' "${pick[@]}" | sort -n | tail -n "$n" | paste -sd, -
}

# perf_probe sets perf_state: ok when `perf stat -e instructions:u` counts,
# absent without perf, else unusable (with the paranoid level; perf's own
# output is left in $TMPDIR/probe.perf and probe.err for the host facts).
# On a heterogeneous host perf prints one line per core type's PMU, the
# event qualified by the PMU ("armv8_cortex_a720/instructions/u"): any PMU
# that counted is enough (obs.sh sums them).
perf_probe() {
	if ! command -v perf >/dev/null; then
		perf_state="absent"
	elif perf stat "-x," -e instructions:u -o "$TMPDIR/probe.perf" -- true >/dev/null 2>"$TMPDIR/probe.err" &&
		awk -F, '$3 ~ /instructions/ && $1 ~ /^[0-9]+$/ {ok = 1} END {exit !ok}' "$TMPDIR/probe.perf"; then
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

# prebuild_arms (timing): build every arm's test binary once, with the
# observations' build flags (-trimpath, and -race and -tags when asked), so
# the build cache holds every compile before the quiet check and no
# observation directly follows a compile. Records each binary's sha256 (with
# -trimpath, two arms of one commit share it). A failed build is recorded;
# its observations then report the build failure themselves.
prebuild_arms() {
	local kv arm out rc f
	local -a build=(-trimpath) flags=() pkgs=()
	[ "${STRESS_RACE:-false}" != "true" ] || build+=(-race)
	read -r -a flags <<<"${STRESS_FLAGS-}"
	for f in ${flags[@]+"${flags[@]}"}; do
		case "$f" in -tags=*) build+=("$f") ;; esac
	done
	read -r -a pkgs <<<"$STRESS_PACKAGES"
	for kv in $STRESS_CASE_SHAS; do
		arm=${kv%%=*}
		out="$TMPDIR/prebuild-$arm.test"
		rc=0
		# shellcheck disable=SC2016 # $1 and $@ are the inner shell's
		in_session bash -c 'cd "$1" && shift && exec go test -c "$@"' _ "celeris-$arm" -o "$out" "${build[@]}" "${pkgs[@]}" \
			>"$TMPDIR/prebuild-$arm.log" 2>&1 || rc=$?
		if [ "$rc" = 0 ]; then
			note "prebuild $arm: binary $(sha256sum "$out" | awk '{print $1}') (go test -c ${build[*]})"
		else
			note "prebuild $arm: go test -c exited $rc; its observations will report the build failure"
			tail -n 30 "$TMPDIR/prebuild-$arm.log" >>"$STRESS_FACTS" 2>/dev/null || true
		fi
		rm -f "$out"
	done
}

# running PID: PID is a live process (a zombie is not). Always the real
# /proc: a test's STRESS_PROCFS holds no processes.
running() {
	local st=""
	read -r st 2>/dev/null <"/proc/$1/stat" || return 1
	st=${st##*) }
	[ "${st%% *}" != Z ]
}

# kill_session SIDFILE [PID]: SIGKILL every process of the session whose id
# SIDFILE holds (else PID's: a child caught before it wrote SIDFILE), in
# whatever process group, and wait until none of them runs, 2 s at most.
# Fails when there was no session to kill.
kill_session() {
	local sid="" f st
	local -a pids fld
	read -r sid 2>/dev/null <"$1" || true
	[[ $sid =~ ^[0-9]+$ ]] || sid=${2-}
	[[ $sid =~ ^[0-9]+$ ]] || return 1
	kill -KILL -- "-$sid" 2>/dev/null || kill -KILL "$sid" 2>/dev/null || true
	for _ in {1..20}; do
		pids=()
		for f in /proc/[0-9]*/stat; do
			read -r st 2>/dev/null <"$f" || continue
			read -r -a fld <<<"${st##*) }" # state ppid pgrp session ...
			if [ "${fld[0]}" != Z ] && [ "${fld[3]}" = "$sid" ]; then pids+=("${f//[!0-9]/}"); fi
		done
		[ "${#pids[@]}" -gt 0 ] || return 0
		kill -KILL "${pids[@]}" 2>/dev/null || true
		sleep 0.1
	done
}

# in_session CMD...: run CMD to its end in a session of its own (setsid),
# whose id is in $shard_sidfile while it runs, so that on_signal and the
# watchdog can kill all of it: shard.sh, go, the test binary and anything
# they start. It runs asynchronously and is waited for, because bash runs no
# trap until a foreground command has ended, and a shard can run for hours.
# Bash starts an asynchronous command with SIGINT and SIGQUIT ignored, and a
# signal ignored across exec stays ignored. Bash 5.2's exec builtin restored
# the dispositions the shell was started with first; bash 5.3's (Ubuntu
# 26.04, the cluster hosts) does not. So env resets the two, and CMD starts
# with a foreground command's signal dispositions (and stdin), as a shard
# does on GitHub.
in_session() {
	local rc=0
	# shellcheck disable=SC2016 # $$, $1 and $@ are the inner shell's
	(exec env --default-signal=INT,QUIT setsid bash -c 'printf %s "$$" >"$1" && shift && exec "$@"' _ "$shard_sidfile" "$@") <&0 &
	shard_pid=$!
	wait "$shard_pid" || rc=$?
	shard_pid=""
	rm -f "$shard_sidfile"
	return "$rc"
}

stop_watchdog() {
	[ -z "$watchdog_pid" ] || kill -TERM -- "-$watchdog_pid" 2>/dev/null || true
	watchdog_pid=""
}

# on_signal SIG: a cancel or a timeout. The runner sends SIGINT to the step's
# process alone (this script: the workflow's host step execs it), SIGTERM
# 7.5 s later and SIGKILL 2.5 s after that, and then runs the job's always()
# steps. So kill the running shard's session (or prebuild's) now, then the
# watchdog, and exit as the signal would have.
on_signal() {
	trap '' INT TERM HUP
	local what="no shard was running"
	if kill_session "$shard_sidfile" "$shard_pid"; then what="killed the running shard's session"; fi
	stop_watchdog
	echo "::warning::cluster-host.sh got SIG$1 (a cancel or a timeout): $what" >&2 || true
	printf '\n=== SIG%s (%s): %s\n' "$1" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$what" >>"$STRESS_FACTS" 2>/dev/null || true
	exit $((128 + $(kill -l "$1")))
}

# watch_runner SIDFILE PID: the watchdog. main starts it in a session of its
# own, so nothing sent to this script's process group reaches it. Every
# STRESS_WATCHDOG_SECONDS (default 2) it checks that PID (this script) still
# runs and that the runner dir ($RUNNER_TEMP) is still there. PID can die of
# SIGKILL, which runs no trap (the runner's last word on a cancel, 10 s after
# its SIGINT). A runner GitHub gave up on can keep running its job on the
# host while the teardown kills the listener and deletes that dir. Either way
# nothing else would stop the running shard, which could run on into the next
# cluster run for up to its -timeout (5h30m at most). So once either is gone,
# it kills the session in SIDFILE (which lives in TMPDIR, outside the runner
# dir), then PID if it still runs, and exits.
watch_runner() {
	local sidfile=$1 pid=$2 why
	while :; do
		if ! running "$pid"; then
			why="the host script (pid $pid) is gone"
			break
		fi
		if [ ! -d "$RUNNER_TEMP" ]; then
			why="the runner dir $RUNNER_TEMP is gone (teardown ran while this job was still running)"
			break
		fi
		sleep "${STRESS_WATCHDOG_SECONDS:-2}"
	done
	if kill_session "$sidfile"; then
		echo "::error::$why: killed the running shard's session" >&2 || true
	fi
	if running "$pid"; then
		kill -TERM "$pid" 2>/dev/null || true
	fi
}

main() {
	: "${STRESS_ARCH:?}" "${STRESS_HOST:?}" "${STRESS_MODE:?}" "${STRESS_SEQUENCE:?}" "${STRESS_CASE_SHAS:?}"
	: "${STRESS_LOG_DIR:?}" "${STRESS_FACTS:?}" "${RUNNER_TEMP:?}"
	gostate_init
	mkdir -p "$STRESS_LOG_DIR" "$(dirname "$STRESS_FACTS")"
	: >"$STRESS_FACTS"

	# From here on nothing long-running this script starts may outlive it:
	# see in_session, on_signal and watch_runner.
	shard_sidfile="$TMPDIR/stress-shard.sid"
	rm -f "$shard_sidfile"
	trap 'on_signal INT' INT
	trap 'on_signal TERM' TERM
	trap 'on_signal HUP' HUP
	# Bash cannot trap a signal that was ignored when it started: the INT trap
	# above is then void, silently, and a cancel reaches on_signal only on the
	# runner's SIGTERM, 7.5 s after its SIGINT. The cluster's runner starts
	# every step with SIGINT ignored, so the workflow's host step resets it
	# (env --default-signal); say so if that ever stops working.
	local ign
	ign=$(sigmask $$ SigIgn)
	if [[ $ign =~ ^[0-9a-f]{1,16}$ ]] && ((16#$ign & 2)); then
		echo "::warning::cluster-host.sh started with SIGINT ignored (SigIgn $ign): a cancel reaches it only on the runner's SIGTERM, 7.5 s after its SIGINT. Start it as the workflow's host step does: exec env --default-signal=INT,QUIT,HUP bash cluster-host.sh" >&2
	fi
	setsid bash "$here/${BASH_SOURCE[0]##*/}" watch "$shard_sidfile" "$$" </dev/null &
	watchdog_pid=$!

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
		if cpuset=$(pick_cpus "$STRESS_CPUS" 2>"$TMPDIR/pick.err"); then
			note "cpu classes: $(cat "$TMPDIR/pick.err")"
		else
			cpuset=""
			pre_refuse="${pre_refuse:+$pre_refuse; }$(cat "$TMPDIR/pick.err")"
		fi
		note "cpuset: ${cpuset:-none}"
		perf_probe
		note "perf: $perf_state"
		case "$perf_state" in
		# perf may have written neither file: never let that end the job.
		unusable*) { echo "perf's output:"; cat "$TMPDIR/probe.perf" "$TMPDIR/probe.err" 2>/dev/null || true; } >>"$STRESS_FACTS" ;;
		esac
		# Decided here once, so a refused timing builds nothing and waits for
		# nothing (shard.sh keeps the same rule on its own).
		if [ "$STRESS_PMU" = "required" ] && [ "$perf_state" != "ok" ]; then
			pre_refuse="${pre_refuse:+$pre_refuse; }pmu required but perf is $perf_state"
		fi
		if [ -z "$pre_refuse" ]; then
			prebuild_arms
			quiet_wait
			note "quiet check: $quiet"
			snapshot "after the quiet check"
		else
			note "no prebuild and no quiet check: every observation is refused ($pre_refuse)"
		fi
	fi

	export STRESS_TARGET=cluster STRESS_HOST STRESS_RUNNER="$STRESS_HOST" STRESS_MODE STRESS_CPUS STRESS_PMU
	export STRESS_CPUSET="$cpuset" STRESS_PERF="$perf_state" STRESS_QUIET="$quiet" STRESS_PRE_REFUSE="$pre_refuse"
	export STRESS_OBS_WRAPPER="$here/obs.sh"

	local -a seq
	local refused=0 i=0 tok c shard shuffle rc
	read -r -a seq <<<"$STRESS_SEQUENCE"
	for tok in "${seq[@]}"; do
		i=$((i + 1))
		if ! [[ $tok =~ ^([A-Za-z0-9]+):([0-9]+):(off|[0-9]+)$ ]]; then
			note "sequence token $tok is not CASE:SHARD:SHUFFLE; refusing the host job"
			stop_watchdog
			exit 1
		fi
		c=${BASH_REMATCH[1]} shard=${BASH_REMATCH[2]} shuffle=${BASH_REMATCH[3]}
		echo "=== [$i/${#seq[@]}] case $c shard $shard (-shuffle=$shuffle)"
		rc=0
		STRESS_CASE=$c STRESS_SHARD=$shard STRESS_SHUFFLE=$shuffle STRESS_OBS_SEQ=$i \
			STRESS_CELERIS_SHA=${sha_of[$c]:?no commit for case $c} STRESS_CELERIS_REF=${ref_of[$c]-} \
			STRESS_CELERIS_DIR="celeris-$c" STRESS_LOG="$STRESS_LOG_DIR/${c}__${STRESS_ARCH}__${shard}.log" \
			in_session bash "$here/shard.sh" || rc=$?
		[ "$rc" = 0 ] || refused=$((refused + 1))
		note "$(date -u +%H:%M:%SZ) [$i/${#seq[@]}] $c shard $shard: $(tail -n 1 "$STRESS_LOG_DIR/${c}__${STRESS_ARCH}__${shard}.log" 2>/dev/null || echo no log)"
	done
	stop_watchdog

	snapshot "after the last shard"
	if [ "$refused" -gt 0 ]; then
		echo "::error::$refused of ${#seq[@]} shard(s) refused to run in the shape asked for on $STRESS_HOST; see their logs and the host facts"
		exit 1
	fi
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
	if [ "${1-}" = watch ]; then
		shift
		watch_runner "$@"
	else
		main "$@"
	fi
fi
