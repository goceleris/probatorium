#!/usr/bin/env bash
# irqplace.sh - THROWAWAY (branch ab/irqplace-v1, celeris#1036 IRQPLACE run, not for merge).
# Moves the enp1s0-8 TX-completion vector to a probe CPU for ONE bench column and ALWAYS puts it back on the explicit
# standing CPU, every move through nic_irq_pin.sh (the one writer) and verified by effective_affinity_list readback.
#
#   irqplace.sh apply   IFACE VECTOR CPU STANDING WATCHDOG_SECONDS RUNDIR
#       CPU == STANDING: pin STANDING with readback (no probe, nothing armed).
#       CPU != STANDING: create the armed marker, (re)start the watchdog, pin CPU with readback.
#   irqplace.sh restore WHO IFACE VECTOR STANDING [SLEEP_SECONDS] [RUNDIR]
#       WHO is cell | always | cleanup | watchdog. cell restores unconditionally; the others only while the armed
#       marker exists. Pins STANDING with readback. On success removes the marker and kills the watchdog
#       (unless WHO is watchdog). On failure keeps the marker and exits non-zero. SLEEP_SECONDS first sleeps (watchdog).
#
# Files (IRQPLACE_DIR, default /tmp/celeris-irqplace): nic_irq_pin.sh and this script are staged by ansible;
# armed (marker), watchdog.pid, log (every APPLY/RESTORE line). RUNDIR receives the same lines in irqplace.txt.
# IRQPLACE_PIN overrides the pin script path and NIC_IRQ_ROOT is nic_irq_pin.sh's own test root (both for tests).
set -u
D=${IRQPLACE_DIR:-/tmp/celeris-irqplace}
PIN=${IRQPLACE_PIN:-$D/nic_irq_pin.sh}
self=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")

num() { [[ ${1:-} =~ ^(0|[1-9][0-9]*)$ ]]; }
record() { # LINE RUNDIR
	mkdir -p "$D"
	printf '%s\n' "$1" >> "$D/log"
	if [ -n "${2:-}" ] && [ -d "$2" ]; then printf '%s\n' "$1" >> "$2/irqplace.txt"; fi
}
irqbalance() { systemctl is-active irqbalance 2> /dev/null | head -1; }
# pin CPU: runs nic_irq_pin.sh, leaves the JSON in $json and the status in $rc
pin() { # IFACE VECTOR CPU
	json=$(bash "$PIN" pin "$1" "$2" "$3" 2> "$D/pin.err")
	rc=$?
	cat "$D/pin.err" >&2
}
compact() { printf '%s' "$json" | tr -d '\n'; }

kill_watchdog() {
	if [ -f "$D/watchdog.pid" ]; then
		wp=$(cat "$D/watchdog.pid" 2> /dev/null)
		# the watchdog is its own session leader (setsid), so signalling the group takes its sleep with it
		[ -n "$wp" ] && kill -TERM -- "-$wp" 2> /dev/null
		[ -n "$wp" ] && kill -TERM "$wp" 2> /dev/null
		rm -f "$D/watchdog.pid"
	fi
}

mode=${1:-}
case "$mode" in
apply)
	[ $# -eq 7 ] || { echo "usage: irqplace.sh apply IFACE VECTOR CPU STANDING WATCHDOG_SECONDS RUNDIR" >&2; exit 2; }
	iface=$2 vec=$3 cpu=$4 standing=$5 wd=$6 rundir=$7
	num "$cpu" && num "$standing" && num "$wd" || { echo "irqplace: CPU, STANDING and WATCHDOG_SECONDS must be plain integers" >&2; exit 2; }
	# only the two CPUs of this experiment are allowed
	case "$cpu" in 1 | 4) ;; *) echo "irqplace: CPU $cpu is not in the allowlist {1,4}" >&2; exit 2 ;; esac
	mkdir -p "$D"
	if [ "$cpu" != "$standing" ]; then
		kill_watchdog
		: > "$D/armed"
		# the watchdog restores the standing CPU even when ansible, the job and the cleanup are all gone (cancelled run, lost runner)
		SETSID=$(command -v setsid || true)
		$SETSID nohup bash "$self" restore watchdog "$iface" "$vec" "$standing" "$wd" "$rundir" > "$D/watchdog.out" 2>&1 < /dev/null &
		echo $! > "$D/watchdog.pid"
	fi
	pin "$iface" "$vec" "$cpu"
	record "APPLY $(date +%s) cpu=$cpu standing=$standing rc=$rc irqbalance=$(irqbalance) $(compact)" "$rundir"
	exit $rc
	;;
restore)
	[ $# -ge 5 ] || { echo "usage: irqplace.sh restore WHO IFACE VECTOR STANDING [SLEEP_SECONDS] [RUNDIR]" >&2; exit 2; }
	who=$2 iface=$3 vec=$4 standing=$5 sl=${6:-0} rundir=${7:-}
	case "$who" in cell | always | cleanup | watchdog) ;; *) echo "irqplace: unknown WHO $who" >&2; exit 2 ;; esac
	num "$standing" && num "$sl" || { echo "irqplace: STANDING and SLEEP_SECONDS must be plain integers" >&2; exit 2; }
	[ "$sl" -gt 0 ] && sleep "$sl"
	if [ "$who" != cell ] && [ ! -f "$D/armed" ]; then
		exit 0 # nothing was moved, or an earlier layer already restored and verified it
	fi
	pin "$iface" "$vec" "$standing"
	if [ "$rc" = 0 ] && printf '%s' "$json" | grep -q '"verified":true'; then
		rm -f "$D/armed"
		[ "$who" = watchdog ] || kill_watchdog
		record "RESTORE $(date +%s) who=$who cpu=$standing VERIFIED $(compact)" "$rundir"
		exit 0
	fi
	record "RESTORE $(date +%s) who=$who cpu=$standing FAILED rc=$rc $(compact)" "$rundir"
	exit "${rc:-1}"
	;;
*)
	echo "usage: irqplace.sh apply|restore ..." >&2
	exit 2
	;;
esac
