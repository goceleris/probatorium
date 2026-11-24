#!/usr/bin/env bash
# Tests .github/scripts/cpu-governor.sh (set / restore) end to end against per-host FAKE sysfs trees through a stand-in for the playbook (cpu_governor_fake_ansible.py, which runs the real
# cpu_power.py): the hosts of a run (target plus loadgen, every host on both), msr1 already on performance (zero writes), the restore verdict (RESTORED-EQUAL / NOT-RESTORED /
# NOT-EQUAL / NOTHING-RECORDED / KEPT-*), a failed set that still restores, a stale record, a bad governor, the results copy. SCRIPT=<path> runs another copy of the script (mutation test).
#   bash ansible/tests/test_cpu_governor.sh
set -u
here=$(cd "$(dirname "$0")/.." && pwd)
repo=$(cd "$here/.." && pwd)
script=${SCRIPT:-$repo/.github/scripts/cpu-governor.sh}
work=$(mktemp -d)
trap '[ -n "${KEEP:-}" ] || { chmod -R u+rw "${work:?}" 2> /dev/null; rm -rf "${work:?}"; }' EXIT
pass=0
fail=0
ok() { pass=$((pass + 1)); echo "ok   $1"; }
bad() { fail=$((fail + 1)); echo "FAIL $1" >&2; [ -f "$work/out.log" ] && tail -8 "$work/out.log" | cut -c1-240 >&2; }
check() { local n=$1; shift; if "$@"; then ok "$n"; else bad "$n"; fi; }
py=$(command -v python3)
export FAKE_SYS_ROOT=$work/sys FAKE_ANSIBLE_LOG=$work/ansible.log
export ANSIBLE=$here/tests/cpu_governor_fake_ansible.py PY=$py CPUGOV_ANSIBLE_DIR=$here

mkhost() { # mkhost <host> <ncpu> <governor> <driver> <epp|->
  local h=$1 n=$2 g=$3 drv=$4 epp=$5 c i d s
  rm -rf "${FAKE_SYS_ROOT:?}/$h"
  for ((c = 0; c < n; c++)); do
    d=$FAKE_SYS_ROOT/$h/cpu/cpu$c
    mkdir -p "$d/cpufreq" "$d/cpuidle"
    printf '%s\n' "$g" > "$d/cpufreq/scaling_governor"; printf '%s\n' "$drv" > "$d/cpufreq/scaling_driver"
    echo "performance powersave" > "$d/cpufreq/scaling_available_governors"
    [ "$epp" = - ] || printf '%s\n' "$epp" > "$d/cpufreq/energy_performance_preference"
    echo 400000 > "$d/cpufreq/scaling_min_freq"; echo 3500000 > "$d/cpufreq/scaling_max_freq"; echo 1600000 > "$d/cpufreq/scaling_cur_freq"
    i=0
    for s in POLL C1 C2 C3; do
      mkdir -p "$d/cpuidle/state$i"; echo "$s" > "$d/cpuidle/state$i/name"; echo 0 > "$d/cpuidle/state$i/disable"
      echo "$((i * 40))" > "$d/cpuidle/state$i/latency"; echo 0 > "$d/cpuidle/state$i/usage"; echo 0 > "$d/cpuidle/state$i/time"
      i=$((i + 1))
    done
  done
  mkdir -p "$FAKE_SYS_ROOT/$h/cpu/cpufreq" "$FAKE_SYS_ROOT/$h/cpu/cpuidle"
  echo 1 > "$FAKE_SYS_ROOT/$h/cpu/cpufreq/boost"; echo acpi_idle > "$FAKE_SYS_ROOT/$h/cpu/cpuidle/current_driver"; echo menu > "$FAKE_SYS_ROOT/$h/cpu/cpuidle/current_governor"
  printf 'processor\t: 0\ncpu MHz\t\t: 1600.000\n' > "$FAKE_SYS_ROOT/$h/cpuinfo"
}
reset() { # a fresh cluster: msa2-server and msa2-client on powersave, msr1 already on performance (as found 2026-11-05)
  rm -rf "${work:?}/sys" "${work:?}/rt" "${work:?}/res" "${work:?}/ansible.log"
  mkhost msa2-server 4 powersave amd-pstate-epp balance_performance
  mkhost msa2-client 6 powersave amd-pstate-epp balance_performance
  mkhost msr1 3 performance scmi -
  export CPUGOV_DIR=$work/rt/cpu-power CPUGOV_RESULTS=$work/res/cpu-power
  unset FAKE_RESTORE_NOOP BENCH_TARGET CPU_GOVERNOR
}
run() { bash "$script" "$@" > "$work/out.log" 2>&1; }
govs() { local h=$1 f; for f in "$FAKE_SYS_ROOT/$h"/cpu/cpu*/cpufreq/scaling_governor; do tr -d '\n' < "$f"; echo; done | sort -u | tr '\n' ' '; }
status() { cut -d' ' -f1 "$CPUGOV_RESULTS/restore-status.txt" 2> /dev/null; }
snapshot() { for f in "$FAKE_SYS_ROOT"/"$1"/cpu/cpu*/cpufreq/* "$FAKE_SYS_ROOT"/"$1"/cpu/cpu*/cpuidle/state*/disable; do echo "$f=$(cat "$f")"; done | sort; }

echo "--- target msa2-server: the hosts are the target and the loadgen, msr1 is not touched"
reset
export BENCH_TARGET=msa2-server CPU_GOVERNOR=performance
snapshot msr1 > "$work/msr1.before"
check "set performance" run set
check "  msa2-server on performance" test "$(govs msa2-server)" = "performance "
check "  msa2-client on performance" test "$(govs msa2-client)" = "performance "
check "  msr1 neither recorded nor changed" bash -c "[ ! -e '$CPUGOV_DIR/orig/msr1.json' ] && snapshot_ok=1"
check "  the playbook was limited to the target and the loadgen" grep -q 'limit=msa2-server,msa2-client' "$work/ansible.log"
check "  EPP written, idle states left as found" bash -c "grep -q performance '$FAKE_SYS_ROOT/msa2-server/cpu/cpu1/cpufreq/energy_performance_preference' && grep -qx 0 '$FAKE_SYS_ROOT/msa2-server/cpu/cpu1/cpuidle/state3/disable'"
check "  orig and applied records for both hosts" test -s "$CPUGOV_DIR/orig/msa2-server.json" -a -s "$CPUGOV_DIR/applied/msa2-client.json" -a -s "$CPUGOV_DIR/applied/msa2-server.apply.json"
check "  mode.txt names the governor and the limit" grep -q 'governor.performance' "$CPUGOV_DIR/mode.txt"
check "  summary.txt: one line per host and label with governor and idle states" grep -q 'msa2-server.orig.*governor=powersave=4.*C3(enabled)' "$CPUGOV_DIR/summary.txt"
check "  summary.txt applied line says performance" grep -q 'msa2-client.applied.*governor=performance=6' "$CPUGOV_DIR/summary.txt"
check "restore" run restore
check "  RESTORED-EQUAL in the results copy" test "$(status)" = RESTORED-EQUAL
check "  governors back to as found" test "$(govs msa2-server)$(govs msa2-client)" = "powersave powersave "
check "  EPP back" grep -q balance_performance "$FAKE_SYS_ROOT/msa2-client/cpu/cpu5/cpufreq/energy_performance_preference"
check "  the results copy carries orig applied final summary mode" test -s "$CPUGOV_RESULTS/orig/msa2-server.json" -a -s "$CPUGOV_RESULTS/applied/msa2-server.json" -a -s "$CPUGOV_RESULTS/final/msa2-client.json" -a -s "$CPUGOV_RESULTS/summary.txt" -a -s "$CPUGOV_RESULTS/mode.txt"
check "  summary.txt has the final lines" grep -q 'msa2-server.final.*governor=powersave=4' "$CPUGOV_RESULTS/summary.txt"
check "  msr1 byte-identical" bash -c "[ \"\$($(declare -f snapshot); FAKE_SYS_ROOT='$FAKE_SYS_ROOT'; snapshot msr1)\" = \"\$(cat '$work/msr1.before')\" ]"
check "a second restore is a no-op" run restore
check "  and stays RESTORED-EQUAL" test "$(status)" = RESTORED-EQUAL

echo "--- target both: msr1 already on performance gets no write"
reset
export BENCH_TARGET=both
check "set (both)" run set
check "  three hosts recorded" test -s "$CPUGOV_DIR/orig/msr1.json" -a -s "$CPUGOV_DIR/orig/msa2-server.json" -a -s "$CPUGOV_DIR/orig/msa2-client.json"
check "  no --limit" grep -q 'limit=-' "$work/ansible.log"
check "  msr1 apply made zero writes" grep -q '"changed_writes": 0' "$CPUGOV_DIR/applied/msr1.apply.json"
check "  msr1 summary says performance as found" grep -q 'msr1.orig.*governor=performance=3' "$CPUGOV_DIR/summary.txt"
check "restore (both)" run restore
check "  RESTORED-EQUAL" test "$(status)" = RESTORED-EQUAL
check "  msr1 still performance (as found)" test "$(govs msr1)" = "performance "

echo "--- keep: record only"
reset
export BENCH_TARGET=msa2-server CPU_GOVERNOR=keep
check "set keep" run set
check "  nothing changed" test "$(govs msa2-server)$(govs msa2-client)" = "powersave powersave "
check "  no applied record" test ! -e "$CPUGOV_DIR/applied"
check "restore keep" run restore
check "  KEPT-EQUAL" test "$(status)" = KEPT-EQUAL
reset
export BENCH_TARGET=msa2-server CPU_GOVERNOR=keep
run set
echo performance | tee "$FAKE_SYS_ROOT"/msa2-client/cpu/cpu2/cpufreq/scaling_governor > /dev/null  # someone else changed a governor during the run
check "keep: a governor changed during the run is KEPT-CHANGED and does not fail the step" run restore
check "  KEPT-CHANGED" test "$(status)" = KEPT-CHANGED

echo "--- a failed set"
if [ "$(id -u)" != 0 ]; then
  reset
  export BENCH_TARGET=msa2-server CPU_GOVERNOR=performance
  chmod 444 "$FAKE_SYS_ROOT/msa2-client/cpu/cpu3/cpufreq/scaling_governor"
  check "a refused governor write fails the set step (rc 4)" bash -c "bash '$script' set > '$work/out.log' 2>&1; [ \$? = 4 ]"
  check "  msa2-server was moved, msa2-client's other CPUs too" test "$(govs msa2-server)" = "performance "
  chmod 644 "$FAKE_SYS_ROOT/msa2-client/cpu/cpu3/cpufreq/scaling_governor"
  check "the restore after the failed set puts everything back" run restore
  check "  RESTORED-EQUAL" test "$(status)" = RESTORED-EQUAL
  check "  every governor as found" test "$(govs msa2-server)$(govs msa2-client)" = "powersave powersave "
  reset
  export BENCH_TARGET=msa2-server CPU_GOVERNOR=performance
  run set
  chmod 444 "$FAKE_SYS_ROOT/msa2-server/cpu/cpu1/cpufreq/scaling_governor"
  check "a restore whose write is refused fails the step" bash -c "bash '$script' restore > '$work/out.log' 2>&1; [ \$? = 1 ]"
  check "  NOT-RESTORED" test "$(status)" = NOT-RESTORED
  chmod 644 "$FAKE_SYS_ROOT/msa2-server/cpu/cpu1/cpufreq/scaling_governor"
fi
reset
export BENCH_TARGET=msa2-server CPU_GOVERNOR=performance
run set
export FAKE_RESTORE_NOOP=1
check "a restore that silently restored nothing is caught by the compare (rc 1)" bash -c "bash '$script' restore > '$work/out.log' 2>&1; [ \$? = 1 ]"
check "  NOT-EQUAL" test "$(status)" = NOT-EQUAL
check "  the differences are named" grep -q "cpu0.governor: 'powersave' -> 'performance'" "$work/out.log"
unset FAKE_RESTORE_NOOP

echo "--- guards"
reset
export BENCH_TARGET=msa2-server CPU_GOVERNOR=performance
check "restore with no set before it: NOTHING-RECORDED, rc 0" run restore
check "  verdict" test "$(status)" = NOTHING-RECORDED
check "  nothing was called on a host" test ! -s "$work/ansible.log"
reset
export BENCH_TARGET=msa2-server CPU_GOVERNOR=performance
mkdir -p "$CPUGOV_DIR/orig" && echo '{}' > "$CPUGOV_DIR/orig/msa2-server.json"
check "a stale record in the directory refuses the set (rc 2)" bash -c "bash '$script' set > '$work/out.log' 2>&1; [ \$? = 2 ]"
check "  and changed nothing" test "$(govs msa2-server)" = "powersave "
reset
export CPU_GOVERNOR=turbo
check "an unknown governor is refused (rc 2)" bash -c "bash '$script' set > '$work/out.log' 2>&1; [ \$? = 2 ]"
check "  also for restore" bash -c "bash '$script' restore > '$work/out.log' 2>&1; [ \$? = 2 ]"
export CPU_GOVERNOR=performance
check "no arguments is a usage error (rc 2)" bash -c "bash '$script' > '$work/out.log' 2>&1; [ \$? = 2 ]"

echo "$pass passed, $fail failed"
[ "$fail" = 0 ]
