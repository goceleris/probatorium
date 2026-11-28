#!/usr/bin/env bash
# The host half of idle-cell-baremetal.yml: runs ON the measured host (its self-hosted runner), one subcommand per
# workflow step, so that each stage's artifact is uploaded when that stage ends.
#
#   host-run.sh preflight    every tool the job uses is on the host (named when not), before anything else: refuses a host that lacks one
#   host-run.sh restore      put perf_event_paranoid back if this run lowered it (idempotent; the workflow runs it with if: always())
#   host-run.sh facts        what the host is, as FOUND (nothing is changed); the CPU sets, power sources and perf probes
#   host-run.sh stage a      the #1485 gate cells: main, head, head-aw, main-skip at 100/1,000/10,000/50,000 idle
#                            keep-alive connections, epoll, IC_PASSES passes (pass 2 reversed), plus the idle baseline
#   host-run.sh stage b      the paired first-request cells (2 s gap, ABBA, IC_PAIRS pairs): b1 an empty server, the OLD 4 ms
#                            build against main, for epoll, io_uring and adaptive; b2 1,000 connections held, main against head
#   host-run.sh stage c      io_uring, main against head, at 10,000 and 50,000, for the record (IC_PASSES passes)
#   host-run.sh finish       the state again as found, and the verdict on the cells that failed
#
# Environment: IC_BIN IC_OUT IC_HOST IC_ARCH IC_PASSES IC_PAIRS IC_DRY (true|false) IC_MAXLOAD IC_LOAD_WAIT IC_WINDOW IC_SETTLE
# IC_SERVER_CPUS IC_CLIENT_CPUS (optional overrides). The host runs nothing else while this does: the runner agent itself,
# and no other cluster workflow (they all hold matrix-tier-cluster, and the guard refused to start while one ran).
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
: "${IC_BIN:?}" "${IC_OUT:?}" "${IC_PASSES:=3}" "${IC_PAIRS:=10}" "${IC_DRY:=false}"
mkdir -p "$IC_OUT/facts"
# shellcheck source=lib.sh
. "$here/lib.sh"

if [ "$IC_DRY" = true ]; then
	PASSES=1 PAIRS=1 NS="1000" VARIANTS="main" B1_ENGINES="epoll" B1_COUNT=3 DRY_STAGE_B_ONLY_B1=1
else
	PASSES=$IC_PASSES PAIRS=$IC_PAIRS NS="${IC_NS:-100 1000 10000 50000}" VARIANTS="main head head-aw main-skip main-twin" B1_ENGINES="epoll iouring adaptive" B1_COUNT=8 DRY_STAGE_B_ONLY_B1=0
fi
C_NS="${IC_C_NS:-10000 50000}" # IC_NS and IC_C_NS exist for the plumbing test in a container only; the workflow never sets them
GAP=2

# A cancel or a timeout signals this script alone: stop the tool processes it started (and only those, by name), so that
# nothing of ours is left running on the host after the job.
stop_tools() {
	# shellcheck disable=SC2046
	kill $(jobs -p) 2>/dev/null
	pkill -f 'idlehold-server-|/idleclient|idlecost-' 2>/dev/null
}
trap 'stop_tools; cmd_restore; echo "host-run.sh: stopped by a signal" >&2; exit 143' TERM INT HUP

# the state a run must leave as it found it: governor, driver, boost, idle states, kernel. Read, never written.
state_snapshot() {
	echo "kernel: $(uname -srvm)"
	local f
	for f in scaling_governor scaling_driver energy_performance_preference scaling_min_freq scaling_max_freq; do
		echo "cpufreq/$f: $(cat /sys/devices/system/cpu/cpu*/cpufreq/$f 2>/dev/null | sort | uniq -c | tr '\n' ';')"
	done
	echo "intel_pstate/no_turbo: $(cat /sys/devices/system/cpu/intel_pstate/no_turbo 2>/dev/null || echo n/a)"
	echo "cpufreq/boost: $(cat /sys/devices/system/cpu/cpufreq/boost 2>/dev/null || echo n/a)"
	echo "cpuidle driver: $(cat /sys/devices/system/cpu/cpuidle/current_driver 2>/dev/null || echo n/a) governor: $(cat /sys/devices/system/cpu/cpuidle/current_governor 2>/dev/null || echo n/a)"
	echo "cpuidle states (cpu0: name disable latency_us):"
	for f in /sys/devices/system/cpu/cpu0/cpuidle/state*; do
		[ -d "$f" ] && echo "  $(basename "$f") $(cat "$f/name" 2>/dev/null) disable=$(cat "$f/disable" 2>/dev/null) latency=$(cat "$f/latency" 2>/dev/null)"
	done
	echo "cpuidle disable flags (all cpus, count of 1): $(cat /sys/devices/system/cpu/cpu*/cpuidle/state*/disable 2>/dev/null | grep -c 1)"
	echo "smt: $(cat /sys/devices/system/cpu/smt/control 2>/dev/null || echo n/a)"
	echo "thp: $(cat /sys/kernel/mm/transparent_hugepage/enabled 2>/dev/null || echo n/a)"
	echo "perf_event_paranoid: $(cat /proc/sys/kernel/perf_event_paranoid 2>/dev/null || echo n/a)"
}

cmd_preflight() {
	preflight_tools
}

# restore: put kernel.perf_event_paranoid back to the value found, if (and only if) this run lowered it. Idempotent; run by the workflow's
# always() step, by finish, and by the trap of every stage on a signal. Failing to restore is loud.
cmd_restore() {
	local prevf=$IC_OUT/facts/paranoid-prev out=$IC_OUT/facts/paranoid.txt prev method cur
	[ -f "$prevf" ] || { echo "restore: this run did not change perf_event_paranoid"; return 0; }
	[ ! -f "$IC_OUT/facts/paranoid-restored" ] || { echo "restore: perf_event_paranoid was already restored"; return 0; }
	read -r prev method <"$prevf"
	cur=$(cat "$PARANOID" 2>/dev/null || echo n/a)
	[ "$cur" = "$prev" ] || paranoid_write "$prev" "$method" || true
	cur=$(cat "$PARANOID" 2>/dev/null || echo n/a)
	if [ "$cur" = "$prev" ]; then
		echo "perf_event_paranoid: restored to $prev (verified: it reads $cur) at $(date -u +%Y-%m-%dT%H:%M:%SZ)" | tee -a "$out"
		: >"$IC_OUT/facts/paranoid-restored"
	else
		echo "::error::perf_event_paranoid could NOT be restored: found $prev, it reads $cur now. Restore it by hand on ${IC_HOST:-the host}" | tee -a "$out"
		return 1
	fi
}

cmd_facts() {
	local f=$IC_OUT/facts/host-facts.txt
	{
		echo "== idle cell host facts (as FOUND; nothing was changed) $(date -u +%Y-%m-%dT%H:%M:%SZ)"
		echo "host label: ${IC_HOST:-?} arch: ${IC_ARCH:-?} hostname: $(hostname) user: $(id)"
		echo "kernel: $(uname -a)"
		grep -E '^(PRETTY_NAME|VERSION)=' /etc/os-release 2>/dev/null
		echo "loadavg: $(cat /proc/loadavg)  uptime: $(uptime -p 2>/dev/null)"
		echo "== CPU"
		lscpu 2>/dev/null | grep -E 'Model name|Architecture|CPU\(s\)|Thread|Core|Socket|MHz|max|min|L[123]|Vendor|Flags' | grep -v Flags
		grep -m1 -E 'model name|Model|Hardware|CPU part' /proc/cpuinfo
		echo "frequency now (kHz, first 16 cpus): $(cat /sys/devices/system/cpu/cpu{0..15}/cpufreq/scaling_cur_freq 2>/dev/null | tr '\n' ' ')"
		echo "cpu_capacity (heterogeneous cores show different values): $(cat /sys/devices/system/cpu/cpu{0..15}/cpu_capacity 2>/dev/null | tr '\n' ' ')"
		echo "== state (governor, driver, boost, idle states, kernel)"
		state_snapshot
		echo "== limits of this job"
		echo "ulimit -n soft $(ulimit -Sn) hard $(ulimit -Hn); -l soft $(ulimit -Sl) hard $(ulimit -Hl); -u $(ulimit -u)"
		echo "io_uring_disabled=$(cat /proc/sys/kernel/io_uring_disabled 2>/dev/null || echo n/a) nofile-max=$(cat /proc/sys/fs/file-max 2>/dev/null) nf_conntrack_max=$(cat /proc/sys/net/netfilter/nf_conntrack_max 2>/dev/null || echo n/a)"
		echo "tools: taskset=$(command -v taskset || echo MISSING) timeout=$(command -v timeout || echo MISSING) perf=$(command -v perf || echo MISSING) python3=$(command -v python3 || echo MISSING) ss=$(command -v ss || echo MISSING)"
		echo "== what else runs (ps by current cpu, vmstat 1 s x 4)"
		ps -eo pid,pcpu,etimes,comm --sort=-pcpu | head -16
		vmstat 1 4 2>/dev/null | tail -5
		echo "runner agent processes: $(pgrep -fc 'Runner.Listener|Runner.Worker' 2>/dev/null)"
		echo "timers: $(systemctl list-timers --no-pager 2>/dev/null | head -12 | tr '\n' '|')"
		echo "== network"
		(ip -br addr 2>/dev/null | head -8) || true
	} >"$f" 2>&1
	cpu_sets
	discover_power
	probe_perf
	nofile_ok=0
	local hard
	ulimit -n "$(ulimit -Hn)" 2>/dev/null || true
	hard=$(ulimit -Sn)
	{ [ "$hard" = unlimited ] || [ "$hard" -ge 120000 ]; } && nofile_ok=1
	iou=0
	[ "$(cat /proc/sys/kernel/io_uring_disabled 2>/dev/null || echo 0)" = 0 ] && iou=1
	{
		echo "== CPU sets chosen: server (4 physical cores) $SERVER_CPUS ; holder of the idle connections (other cores, same host) $CLIENT_CPUS"
		echo "== power sources the tool reads (RAPL powercap, hwmon): ${POWER_SPEC:-none}"
		[ -n "$POWER_NOTES" ] && printf '%s' "$POWER_NOTES"
		echo "== perf: ${PERF_STAT:-no usable instructions/cycles counting} | energy via perf: ${PERF_ENERGY:-none}"
		printf '%s\n' "$PERF_NOTES"
		echo "== open-files limit after raising the soft limit to the hard: $hard (cells at 40,000+ connections run: $nofile_ok); io_uring usable: $iou"
		echo "== powercap tree:"
		ls -l /sys/class/powercap/ 2>&1 | head -12
		echo "== hwmon names:"
		for d in /sys/class/hwmon/hwmon*; do echo "$(basename "$d"): $(cat "$d/name" 2>/dev/null)"; done 2>&1 | head -20
	} >>"$f" 2>&1
	write_env
	state_snapshot >"$IC_OUT/facts/state-before.txt"
	cat "$f"
	# a host that is not quiet is not measured: refuse with the numbers, before any cell (and the load stays on record)
	local waited=0
	while over "$(load1)" "$IC_MAXLOAD"; do
		if [ "$waited" -ge 180 ]; then
			echo "load: $(cat /proc/loadavg)" >>"$f"
			die "the host is not quiet: load1 $(load1) is above the threshold $IC_MAXLOAD after 180 s of waiting. Something else is running on ${IC_HOST:-the host} (see facts/host-facts.txt: ps by cpu). No cell was run. Dispatch again when it is quiet, or raise max_load knowingly"
		fi
		/bin/sleep 10
		waited=$((waited + 10))
	done
	echo "quiet check passed: load1 $(load1) <= $IC_MAXLOAD (waited ${waited}s)"
	(cd "$IC_BIN" && sha256sum -- *) >"$IC_OUT/facts/binary-sha256.txt" 2>&1 || true
	lower_paranoid_if_needed
	write_availability
	cat "$IC_OUT/facts/availability.txt"
}

write_env() {
	printf 'export SERVER_CPUS=%q CLIENT_CPUS=%q POWER_SPEC=%q PERF_STAT=%q PERF_ENERGY=%q NOFILE_OK=%q IOURING_OK=%q\n' \
		"$SERVER_CPUS" "$CLIENT_CPUS" "$POWER_SPEC" "$PERF_STAT" "$PERF_ENERGY" "$nofile_ok" "$iou" >"$IC_OUT/facts/env.sh"
}

# The one conditional change (see lib.sh): lower perf_event_paranoid for the job when perf is refused ONLY by it and this user may change it.
lower_paranoid_if_needed() {
	local out=$IC_OUT/facts/paranoid.txt prev method t need_kernel need_energy had_kernel=$PERF_KERNEL had_energy=0 improved=0
	prev=$(cat "$PARANOID" 2>/dev/null || echo n/a)
	echo "perf_event_paranoid as found: $prev" >"$out"
	[ -z "$PERF_ENERGY" ] || had_energy=1
	if ! command -v perf >/dev/null 2>&1; then echo "perf is not installed on this host: nothing to lower, the run is CPU-only" >>"$out"; return 0; fi
	[ "$prev" != n/a ] || { echo "perf_event_paranoid is not readable: not changed" >>"$out"; return 0; }
	want() { # sets need_kernel / need_energy from the current probe
		need_kernel=0 need_energy=0
		[ "$PERF_KERNEL" = 1 ] || need_kernel=1
		if [ -z "$PERF_ENERGY" ] && ! grep -q 'counter:' <<<"$POWER_SPEC"; then need_energy=1; fi
	}
	want
	if [ "$need_kernel" = 0 ] && [ "$need_energy" = 0 ]; then echo "perf already counts user and kernel instructions and an energy source is readable: not changed" >>"$out"; return 0; fi
	if ! grep -qiE 'paranoid|permission|access to performance|not permitted' <<<"$PERF_ERR"; then
		echo "perf is not refused by perf_event_paranoid (its output: $(printf '%s' "$PERF_ERR" | tr '\n' '|' | cut -c1-300)): not changed" >>"$out"
		return 0
	fi
	method=$(paranoid_method)
	if [ "$method" = no ]; then
		echo "perf_event_paranoid=$prev refuses perf and the runner user may not change it (the file is not writable and there is no passwordless root): NOT changed. Where perf is refused the run is CPU-only" >>"$out"
		return 0
	fi
	printf '%s %s\n' "$prev" "$method" >"$IC_OUT/facts/paranoid-prev" # the previous value is on record BEFORE the change
	for t in 1 0; do
		[ "$prev" -gt "$t" ] || continue
		[ "$t" = 1 ] || [ "$need_energy" = 1 ] || break
		paranoid_write "$t" "$method" || { echo "writing $t by $method failed" >>"$out"; break; }
		echo "perf_event_paranoid lowered from $prev to $t by $method (for this job only; the always() restore step puts $prev back)" >>"$out"
		probe_perf
		want
		[ "$need_kernel" = 1 ] || [ "$had_kernel" = 1 ] || improved=1
		[ -z "$PERF_ENERGY" ] || [ "$had_energy" = 1 ] || improved=1
		{ [ "$need_kernel" = 0 ] && [ "$need_energy" = 0 ]; } && break
	done
	if [ "$improved" = 0 ]; then
		echo "lowering did not make perf count more: restoring now" >>"$out"
		cmd_restore
		probe_perf
	fi
	write_env
}

write_availability() {
	local f=$IC_OUT/facts/availability.txt instr energy
	if [ -n "$PERF_STAT" ]; then
		if [ "$PERF_KERNEL" = 1 ]; then instr="instructions and cycles: counted for the server process, user AND kernel"
		else instr="instructions and cycles: counted in USER MODE ONLY (kernel-side counting refused): kernel work is not seen"; fi
	else
		instr="instructions and cycles: NOT MEASURED ($(command -v perf >/dev/null 2>&1 && echo "perf refused: $(printf '%s' "$PERF_ERR" | tr '\n' '|' | cut -c1-160)" || echo 'perf is not installed'))"
	fi
	if [ -n "$POWER_SPEC" ]; then energy="energy: read from ${POWER_SPEC%%=*} (and the other readable sources of facts/host-facts.txt)"
	elif [ -n "$PERF_ENERGY" ]; then energy="energy: perf power/energy-pkg (system-wide)"
	else energy="energy: NOT MEASURED (no RAPL or hwmon source readable by this user, perf's energy event refused or absent)"; fi
	{
		echo "$instr"
		echo "$energy"
		if [ -z "$PERF_STAT" ] && [ -z "$POWER_SPEC" ] && [ -z "$PERF_ENERGY" ]; then
			echo "THIS HOST RUNS CPU-ONLY: instructions and energy are NOT MEASURED; the verdicts can only be 'NO LOSS ON CPU ONLY' or UNDECIDED (or INCOMPLETE)"
		fi
	} >"$f"
}

load_env() {
	# shellcheck disable=SC1091
	. "$IC_OUT/facts/env.sh" || die "facts/env.sh is missing: run 'host-run.sh facts' first"
	ulimit -n "$(ulimit -Hn)" 2>/dev/null || true
	ulimit -l unlimited 2>/dev/null || true
	for b in idlehold-server-main idleclient idlecost-main; do [ -x "$IC_BIN/$b" ] || chmod +x "$IC_BIN"/* 2>/dev/null; done
	[ -x "$IC_BIN/idlehold-server-main" ] || die "no binary in $IC_BIN: the build artifact was not downloaded"
}

# render the stage's table with the reader, if python3 exists on the host; the collect job renders it again from every
# stage's artifact, so a host without python3 loses nothing but the early table.
render() { # stage
	if command -v python3 >/dev/null 2>&1; then
		python3 -I "$here/reader.py" stage "$1" "$IC_OUT" >"$IC_OUT/TABLE-$1.md" 2>"$IC_OUT/TABLE-$1.err" || echo "the reader failed on stage $1: see TABLE-$1.err" >>"$IC_OUT/TABLE-$1.md"
	else
		echo "python3 is missing on the host: the table is rendered by the collect job from this artifact" >"$IC_OUT/TABLE-$1.md"
	fi
	cat "$IC_OUT/TABLE-$1.md"
	[ -z "${GITHUB_STEP_SUMMARY:-}" ] || cat "$IC_OUT/TABLE-$1.md" >>"$GITHUB_STEP_SUMMARY"
}

stage_end() { # stage
	{
		echo "stage $1 ended $(date -u +%Y-%m-%dT%H:%M:%SZ); loadavg $(cat /proc/loadavg)"
		ps -eo pid,pcpu,etimes,comm --sort=-pcpu | head -8
	} >"$IC_OUT/facts/stage-$1-end.txt" 2>&1
	date -u +%Y-%m-%dT%H:%M:%SZ >"$IC_OUT/STAGE-$1.done"
	render "$1"
}

cmd_stage_a() {
	local out=$IC_OUT/a pass ns vs n v
	mkdir -p "$out"
	for pass in $(seq 1 "$PASSES"); do
		if [ "$pass" = 2 ]; then
			ns=$(echo "$NS" | tr ' ' '\n' | sort -rn | tr '\n' ' ')
			vs=$(echo "$VARIANTS" | awk '{for(i=NF;i>0;i--) printf "%s ", $i}') # the later builds first
		else
			ns=$NS vs=$VARIANTS
		fi
		run_baseline "$out" "baseline-n0-pass$pass"
		for n in $ns; do
			for v in $vs; do
				run_idle_cell "$out" "$v-n$n-pass$pass" "$v" epoll "$n"
			done
		done
	done
	[ "$IC_DRY" = true ] || run_baseline "$out" "baseline-n0-pass$((PASSES + 1))"
	if [ "$IC_DRY" = true ]; then
		# the dry-run starts every other build too (head, head-aw, main-skip: one cell each at N=100, 5 s window), so that a broken
		# isolation build fails the DRY RUN, not the real run; their sha256 were verified against the build job's record by the workflow
		local sv
		for sv in head head-aw main-skip; do
			run_idle_cell "$IC_OUT/smoke" "$sv-n100-pass1" "$sv" epoll 100 5s 3s
			if ! grep -q '^IDLE ' "$IC_OUT/smoke/$sv-n100-pass1.srv.txt" 2>/dev/null; then
				echo "smoke $sv: no IDLE line: this build does not run"
				echo "smoke-$sv" >>"$IC_OUT/FAILED-CELLS.txt"
			fi
		done
	fi
	stage_end a
}

# b1 and b2 run the same ABBA design: pair k runs A then B when k is odd, B then A when k is even.
run_empty() { # engine variant pair
	local dir=$IC_OUT/b1 eng=$1 v=$2 k=$3 tag w0 rc
	tag="$eng-p$k-$v"
	mkdir -p "$dir"
	prep "$tag"
	if [ "$eng" = iouring ] && [ "$IOURING_OK" != 1 ]; then index "$dir/INDEX.tsv" "$tag" firstreq-empty "$v" "engine=$eng,pair=$k" SKIP-iouring 0; return 0; fi
	w0=$(date +%s)
	timeout -k 10 180 taskset -c "$SERVER_CPUS" "$IC_BIN/idlecost-$v" -engine "$eng" -workers 0 -window 10s -gaps "${GAP}s:$B1_COUNT" >"$dir/$tag.txt" 2>&1
	rc=$?
	index "$dir/INDEX.tsv" "$tag" firstreq-empty "$v" "engine=$eng,pair=$k" "$rc" "$(($(date +%s) - w0))"
	grep -hE '^(FIRST|SHUTDOWN)' "$dir/$tag.txt" | cut -c1-150
	[ "$rc" = 0 ] || echo "$tag" >>"$IC_OUT/FAILED-CELLS.txt"
	return 0
}

run_held() { # variant pair
	local dir=$IC_OUT/b2 v=$1 k=$2 tag w0 rc win
	tag="p$k-$v"
	win=$((8 * 2 * GAP + 8))
	mkdir -p "$dir"
	prep "$tag"
	w0=$(date +%s)
	timeout -k 10 $((win + 240)) taskset -c "$SERVER_CPUS" "$IC_BIN/idlehold-server-$v" -engine epoll -workers 0 -addr "$IC_ADDR" \
		-conns 1000 -settle 12s -window "${win}s" -power "$POWER_SPEC" >"$dir/$tag.srv.txt" 2>"$dir/$tag.srv.err" &
	local srv=$!
	timeout -k 10 $((win + 240)) taskset -c "$CLIENT_CPUS" "$IC_BIN/idleclient" -addr "$IC_ADDR" -n 1000 -probe-gap "${GAP}s" -probe-count 8 -probe-after 13s \
		>"$dir/$tag.cli.txt" 2>"$dir/$tag.cli.err" || true
	wait "$srv"
	rc=$?
	index "$dir/INDEX.tsv" "$tag" firstreq-held "$v" "N=1000,pair=$k" "$rc" "$(($(date +%s) - w0))"
	grep -h '^PROBE' "$dir/$tag.cli.txt" | cut -c1-120
	[ "$rc" = 0 ] || echo "$tag" >>"$IC_OUT/FAILED-CELLS.txt"
	return 0
}

cmd_stage_b() {
	local eng k a b
	for eng in $B1_ENGINES; do
		for k in $(seq 1 "$PAIRS"); do
			if [ $((k % 2)) = 1 ]; then a=old b=main; else a=main b=old; fi
			echo "== b1 $eng pair $k: $a then $b"
			run_empty "$eng" "$a" "$k"
			run_empty "$eng" "$b" "$k"
		done
	done
	if [ "$DRY_STAGE_B_ONLY_B1" != 1 ]; then
		for k in $(seq 1 "$PAIRS"); do
			if [ $((k % 2)) = 1 ]; then a=main b=head; else a=head b=main; fi
			echo "== b2 1,000 held, pair $k: $a then $b"
			run_held "$a" "$k"
			run_held "$b" "$k"
		done
	fi
	stage_end b
}

cmd_stage_c() {
	local out=$IC_OUT/c pass ns vs n v
	mkdir -p "$out"
	for pass in $(seq 1 "$PASSES"); do
		if [ "$pass" = 2 ]; then ns=$(echo "$C_NS" | tr ' ' '\n' | sort -rn | tr '\n' ' ') vs="head main"; else ns=$C_NS vs="main head"; fi
		[ "$pass" = 1 ] && run_baseline "$out" "baseline-n0-pass1"
		for n in $ns; do
			for v in $vs; do
				run_idle_cell "$out" "iouring-$v-n$n-pass$pass" "$v" iouring "$n"
			done
		done
	done
	run_baseline "$out" "baseline-n0-pass2"
	stage_end c
}

cmd_finish() {
	cmd_restore || true # idempotent: the workflow's always() step has normally done it already
	state_snapshot >"$IC_OUT/facts/state-after.txt"
	if diff -u "$IC_OUT/facts/state-before.txt" "$IC_OUT/facts/state-after.txt" >"$IC_OUT/facts/state-diff.txt"; then
		echo "state (governor, driver, boost, idle states, kernel): identical before and after"
	else
		echo "state DIFFERS before and after (this workflow changes none of it; something else did): see facts/state-diff.txt"
		cat "$IC_OUT/facts/state-diff.txt"
	fi
	if [ -s "$IC_OUT/FAILED-CELLS.txt" ]; then
		echo "::error::cells that did not end rc=0: $(tr '\n' ' ' <"$IC_OUT/FAILED-CELLS.txt")"
		exit 1
	fi
	echo "every cell ended rc=0"
}

case "${1:-}" in
preflight) cmd_preflight ;;
restore) cmd_restore ;;
facts) cmd_facts ;;
stage)
	load_env
	case "${2:-}" in
	a) cmd_stage_a ;;
	b) cmd_stage_b ;;
	c) cmd_stage_c ;;
	*) echo "usage: host-run.sh stage a|b|c" >&2; exit 2 ;;
	esac
	;;
finish) load_env; cmd_finish ;;
*) echo "usage: host-run.sh facts | stage a|b|c | finish" >&2; exit 2 ;;
esac
