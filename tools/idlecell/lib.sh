#!/usr/bin/env bash
# Shared pieces of the bare-metal idle cell (host-run.sh). Sourced, never run. NO removal anywhere, and nothing here
# changes a host setting: every file under /sys and /proc is only read, no sysctl is written, no package is installed,
# no governor, idle state or IRQ is touched. What a host does not have is RECORDED as missing, never provided.
#
# Environment (set by host-run.sh from the workflow's inputs):
#   IC_BIN      directory of the binaries the build job made (idlehold-server-<arm>, idleclient, idlecost-<arm>)
#   IC_OUT      output root; one directory per stage under it (a, b1, b2, c) and facts/
#   IC_MAXLOAD  load1 above which a cell waits (default 1.0); IC_LOAD_WAIT seconds it waits (60) before it runs anyway, forced=1
#   IC_WINDOW / IC_SETTLE   the cell's window and settle (20s / 12s: the phase-1 harness)
# From facts/env.sh (host-run.sh facts): SERVER_CPUS CLIENT_CPUS POWER_SPEC PERF_STAT PERF_ENERGY NOFILE_OK IOURING_OK

: "${IC_MAXLOAD:=1.0}" "${IC_LOAD_WAIT:=60}" "${IC_WINDOW:=20s}" "${IC_SETTLE:=12s}"
IC_ADDR=127.0.0.1:18080

die() { echo "::error::$*" >&2; echo "ABORT: $*" >>"${IC_OUT:-.}/ABORT.txt"; exit 3; }
load1() { cut -d' ' -f1 /proc/loadavg; }
over() { awk -v l="$1" -v m="$2" 'BEGIN{exit !(l>m)}'; }
secs() { local v=${1%s}; echo "${v%.*}"; }

# prep <tag>: poll load1 for at most IC_LOAD_WAIT s while it is above IC_MAXLOAD, then run anyway with forced=1 (the cell's
# INDEX line says so: it is recorded, never hidden). Sets waited, forced, t, hl. Refuses (exit 3) to start on top of a
# leftover tool process or a busy port: something else is on the host.
prep() {
	waited=0 forced=0
	while over "$(load1)" "$IC_MAXLOAD"; do
		if [ "$waited" -ge "$IC_LOAD_WAIT" ]; then forced=1; break; fi
		/bin/sleep 10
		waited=$((waited + 10))
	done
	if pgrep -f 'idlehold-server-|/idleclient|idlecost-' >/dev/null 2>&1; then
		ps -eo pid,etimes,args | grep -E 'idlehold-server-|idleclient|idlecost-' | grep -v grep >>"$IC_OUT/ABORT.txt" || true
		die "a tool process is already running on this host before $1: another run, or a leftover. Nothing may run on the host during a cell"
	fi
	if (ss -ltn 2>/dev/null || true) | grep -q ":${IC_ADDR##*:} "; then die "port ${IC_ADDR##*:} is busy before $1"; fi
	t=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	hl=$(cut -d' ' -f1-3 /proc/loadavg)
}

# index <file> <tag> <kind> <variant> <param> <rc> <wall seconds>
index() {
	printf '%s\t%s\t%s\t%s\t%s\thost_load=%s\twait=%s\tforced=%s\trc=%s\twall_s=%s\n' "$t" "$2" "$3" "$4" "$5" "$hl" "$waited" "$forced" "$6" "$7" >>"$1"
}

# cpu_sets: SERVER_CPUS = 4 CPUs on four distinct physical cores, the highest cpu_capacity first (a big.LITTLE part: the big cores);
# CLIENT_CPUS = up to 4 other CPUs, preferring cores the server does not use (so a hyperthread sibling of a server CPU is a last
# resort). A core is the set of CPUs that share topology/core_cpus_list (thread_siblings_list on older kernels), never core_id (it
# restarts per cluster on many arm64 parts). Both are overridable by the inputs. celeris plans its loops from the CPUs the process is
# allowed (the i-th loop on the i-th allowed CPU), so the taskset mask is what places them; the tool prints loop_cpus in the IDLE line.
cpu_sets() {
	local allowed cpu key cap prim_lines=() sib=() seen=" " prim=()
	allowed=$(taskset -pc $$ | sed 's/.*: //')
	for cpu in $(echo "$allowed" | tr ',' '\n' | awk -F- 'NF==2{for(i=$1;i<=$2;i++)print i;next}{print}'); do
		key=$(cat "/sys/devices/system/cpu/cpu$cpu/topology/core_cpus_list" 2>/dev/null || cat "/sys/devices/system/cpu/cpu$cpu/topology/thread_siblings_list" 2>/dev/null || echo "$cpu")
		cap=$(cat "/sys/devices/system/cpu/cpu$cpu/cpu_capacity" 2>/dev/null || echo 0)
		case "$seen" in *" $key "*) sib+=("$cpu") ;; *) seen="$seen$key "; prim_lines+=("$cap $cpu") ;; esac
	done
	mapfile -t prim < <(printf '%s\n' "${prim_lines[@]}" | sort -k1,1nr -s | awk '{print $2}')
	local all=$((${#prim[@]} + ${#sib[@]}))
	[ "$all" -ge 6 ] || die "only $all CPUs are allowed to this job ($allowed): the server needs 4 and the holder of the connections 2 more, on other cores"
	local s=() c=() i
	for i in "${!prim[@]}"; do if [ "$i" -lt 4 ]; then s+=("${prim[$i]}"); else c+=("${prim[$i]}"); fi; done
	if [ "${#s[@]}" -lt 4 ]; then # fewer than 4 physical cores: take siblings for the server too, and say so in the facts
		for i in "${!sib[@]}"; do if [ "${#s[@]}" -lt 4 ]; then s+=("${sib[$i]}"); else c+=("${sib[$i]}"); fi; done
	else
		c+=("${sib[@]}")
	fi
	SERVER_CPUS=$(IFS=,; echo "${s[*]}")
	CLIENT_CPUS=$(IFS=,; echo "${c[*]:0:4}")
	[ -n "${IC_SERVER_CPUS:-}" ] && SERVER_CPUS=$IC_SERVER_CPUS
	[ -n "${IC_CLIENT_CPUS:-}" ] && CLIENT_CPUS=$IC_CLIENT_CPUS
	[ -n "$SERVER_CPUS" ] && [ -n "$CLIENT_CPUS" ] || die "no CPU set for the server or the holder (allowed: $allowed)"
	return 0
}

# discover_power: the readable power and energy sources of this host, as the tool's -power list; the unreadable ones
# listed. Reading only. Sets POWER_SPEC, POWER_NOTES.
discover_power() {
	POWER_SPEC="" POWER_NOTES=""
	local d f name p
	for d in /sys/class/powercap/*/; do
		[ -e "${d}energy_uj" ] || continue
		name=$(cat "${d}name" 2>/dev/null || echo unnamed)
		if cat "${d}energy_uj" >/dev/null 2>&1; then
			POWER_SPEC+="powercap-$(basename "$d")-$name=counter:${d}energy_uj:${d}max_energy_range_uj,"
		else
			POWER_NOTES+="unreadable by this user: ${d}energy_uj ($name)"$'\n'
		fi
	done
	for d in /sys/class/hwmon/hwmon*/; do
		name=$(cat "${d}name" 2>/dev/null || echo unnamed)
		for f in "${d}"energy*_input; do
			[ -e "$f" ] || continue
			p="hwmon-$name-$(basename "$d")-$(basename "$f" _input)"
			if cat "$f" >/dev/null 2>&1; then POWER_SPEC+="$p=counter:$f,"; else POWER_NOTES+="unreadable by this user: $f"$'\n'; fi
		done
		for f in "${d}"power*_input "${d}"power*_average; do
			[ -e "$f" ] || continue
			p="hwmon-$name-$(basename "$d")-$(basename "$f" | sed 's/_input$//; s/_average$/_avg/')"
			if cat "$f" >/dev/null 2>&1; then POWER_SPEC+="$p=gauge:$f:0.000001,"; else POWER_NOTES+="unreadable by this user: $f"$'\n'; fi
		done
	done
	POWER_SPEC=${POWER_SPEC%,}
}

# probe_perf: does perf exist, and which of the events this run needs does it count for this user? Sets PERF_STAT and
# PERF_ENERGY (command prefixes for the tool, empty when not usable) and PERF_NOTES. perf is never installed.
probe_perf() {
	PERF_STAT="" PERF_ENERGY="" PERF_NOTES=""
	if ! command -v perf >/dev/null 2>&1; then PERF_NOTES="perf: not installed on this host (instructions, cycles and perf's energy: none)"; return; fi
	PERF_NOTES="perf: $(perf --version 2>&1 | head -1); kernel.perf_event_paranoid=$(cat /proc/sys/kernel/perf_event_paranoid 2>/dev/null || echo '?')"$'\n'
	local out ev
	for ev in "duration_time,instructions:u,instructions:k,cycles:u,cycles:k" "duration_time,instructions:u,cycles:u"; do
		out=$(perf stat -x, -e "$ev" -- true 2>&1 || true)
		PERF_NOTES+="probe [$ev]:"$'\n'"$out"$'\n'
		if ! grep -qE 'not supported|not counted|Permission|permission|Operation not|No such|Access to performance' <<<"$out" && grep -q instructions <<<"$out"; then
			PERF_STAT="taskset -c $CLIENT_CPUS perf stat -x, -e $ev"
			break
		fi
	done
	out=$(perf stat -a -x, -e duration_time,power/energy-pkg/ -- true 2>&1 || true)
	PERF_NOTES+="probe [power/energy-pkg/ system-wide]:"$'\n'"$out"$'\n'
	if ! grep -qE 'not supported|not counted|Permission|permission|Operation not|No such|Access to performance|invalid|Invalid|unknown' <<<"$out" && grep -q energy-pkg <<<"$out"; then
		PERF_ENERGY="taskset -c $CLIENT_CPUS perf stat -x, -a -e duration_time,power/energy-pkg/"
	fi
}

# run_idle_cell <dir> <tag> <variant> <engine> <N> [window] [settle]: ONE cell. The server is pinned to SERVER_CPUS, the
# holder of the N idle keep-alive connections to CLIENT_CPUS of the same host; no container. Writes <tag>.srv.txt (the
# tool's lines), .cli.txt, .perf.txt, .pe.txt, .srv.err, and an INDEX line. Never fails the script: rc is recorded.
run_idle_cell() {
	local dir=$1 tag=$2 v=$3 eng=$4 n=$5 window=${6:-$IC_WINDOW} settle=${7:-$IC_SETTLE} kind=${IC_KIND:-idle} rc cap w0
	mkdir -p "$dir"
	prep "$tag"
	if [ "$n" -ge 40000 ] && [ "${NOFILE_OK:-0}" != 1 ]; then
		index "$dir/INDEX.tsv" "$tag" "$kind" "$v" "N=$n,engine=$eng" SKIP-nofile 0
		echo "SKIP $tag: the open-files limit of this job is below what $n connections need (see facts)"
		return 0
	fi
	if [ "$eng" = iouring ] && [ "${IOURING_OK:-0}" != 1 ]; then
		index "$dir/INDEX.tsv" "$tag" "$kind" "$v" "N=$n,engine=$eng" SKIP-iouring 0
		echo "SKIP $tag: io_uring is not usable on this host (see facts)"
		return 0
	fi
	cap=$(($(secs "$window") + $(secs "$settle") + 240))
	w0=$(date +%s)
	timeout -k 10 "$cap" taskset -c "$SERVER_CPUS" "$IC_BIN/idlehold-server-$v" -engine "$eng" -workers 0 -addr "$IC_ADDR" \
		-window "$window" -settle "$settle" -conns "$n" -power "$POWER_SPEC" \
		-perf "$PERF_STAT" -perf-out "$dir/$tag.perf.txt" -perf-energy "$PERF_ENERGY" -perf-energy-out "$dir/$tag.pe.txt" \
		>"$dir/$tag.srv.txt" 2>"$dir/$tag.srv.err" &
	local srv=$!
	if [ "$n" != 0 ]; then
		timeout -k 10 "$cap" taskset -c "$CLIENT_CPUS" "$IC_BIN/idleclient" -addr "$IC_ADDR" -n "$n" >"$dir/$tag.cli.txt" 2>"$dir/$tag.cli.err" || true
	fi
	wait "$srv"
	rc=$?
	index "$dir/INDEX.tsv" "$tag" "$kind" "$v" "N=$n,engine=$eng" "$rc" "$(($(date +%s) - w0))"
	echo "cell $tag rc=$rc wall=$(($(date +%s) - w0))s load=$hl wait=$waited forced=$forced"
	[ "$rc" = 0 ] || echo "$tag" >>"$IC_OUT/FAILED-CELLS.txt"
	return 0
}

# run_baseline <dir> <tag>: the host with nothing served, the same settle and window, the same power and perf reads.
run_baseline() {
	local dir=$1 tag=$2
	IC_KIND=baseline run_idle_cell "$dir" "$tag" main none 0 "$IC_WINDOW" 3s
}
