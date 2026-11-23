#!/usr/bin/env bash
# .github/scripts/gov-ab-arms.sh against stubs: a fake `mage` that records the CPU governor and idle flags the benchmark ran under, and a fake `ansible-playbook` that runs the REAL
# files/cpu_power.py against one fake sysfs tree per host (the playbook itself has its own test, test_cpu_power_play.sh). Checks: a plain dispatch is one BenchTier and nothing else; the
# proof and the full arm tables; each BenchTier ran under its arm's state; the left-behind compare; a failed set (no benchmark in that arm, restore, red); a failed BenchTier (restore, red);
# the stop rule; a competitors value that starts with gab but is no table line (fails closed); a TERM mid-arm restores; the always() restore mode; a restore that cannot put a host back is red.
#   bash ansible/tests/test_gov_ab_arms.sh
set -u
root=$(cd "$(dirname "$0")/../.." && pwd)
SCRIPT=$root/.github/scripts/gov-ab-arms.sh
CPPY=$root/ansible/files/cpu_power.py
work=$(mktemp -d)
trap '[ -n "${KEEP:-}" ] || { chmod -R u+rw "$work" 2> /dev/null; rm -rf "$work"; }' EXIT
pass=0
fail=0
ok() { pass=$((pass + 1)); echo "ok   $1"; }
bad() { fail=$((fail + 1)); echo "FAIL $1" >&2; }
check() { local n=$1; shift; if "$@"; then ok "$n"; else bad "$n"; fi; }
py=$(command -v python3)

mktree() { # mktree <dir>: 4 CPUs, powersave, EPP, POLL C1 C2 C3
  local d c i n
  for c in 0 1 2 3; do
    d=$1/cpu$c
    mkdir -p "$d/cpufreq" "$d/cpuidle"
    echo powersave > "$d/cpufreq/scaling_governor"; echo amd-pstate-epp > "$d/cpufreq/scaling_driver"
    echo "performance powersave" > "$d/cpufreq/scaling_available_governors"
    echo balance_performance > "$d/cpufreq/energy_performance_preference"; echo 1600000 > "$d/cpufreq/scaling_cur_freq"
    i=0
    for n in POLL C1 C2 C3; do
      mkdir -p "$d/cpuidle/state$i"; echo "$n" > "$d/cpuidle/state$i/name"; echo 0 > "$d/cpuidle/state$i/disable"
      echo 0 > "$d/cpuidle/state$i/latency"; echo 0 > "$d/cpuidle/state$i/usage"; echo 0 > "$d/cpuidle/state$i/time"; i=$((i + 1))
    done
  done
  mkdir -p "$1/cpufreq"; echo 1 > "$1/cpufreq/boost"
}

# ---- the stubs
mkdir -p "$work/bin"
cat > "$work/bin/mage" << 'EOT'
#!/usr/bin/env bash
# fake mage BenchTier: logs what the SERVER host looked like while the benchmark ran, makes results/<ts>-bench-v/
[ "$1" = BenchTier ] || exit 9
n=$(( $(wc -l < "$GOVAB_TEST_LOG" 2> /dev/null || echo 0) + 1 ))
g=$(tr -d '\n' < "$GOVAB_TEST_SYS/msa2-server/cpu0/cpufreq/scaling_governor")
gc=$(tr -d '\n' < "$GOVAB_TEST_SYS/msa2-client/cpu0/cpufreq/scaling_governor")
c2=$(tr -d '\n' < "$GOVAB_TEST_SYS/msa2-server/cpu3/cpuidle/state2/disable")
echo "bench$n server=$g client=$gc c2disable=$c2" >> "$GOVAB_TEST_LOG"
mkdir -p "$GOVAB_RESULTS/2026112200000$n-bench-v1"
[ "${GOVAB_TEST_FAIL_BENCH:-}" = "$n" ] && exit 1
[ "${GOVAB_TEST_HANG_BENCH:-}" = "$n" ] && { sleep 30; }
exit 0
EOT
cat > "$work/bin/timeout" << 'EOT'
#!/usr/bin/env bash
while [ $# -gt 0 ]; do case "$1" in --*) shift ;; *) break ;; esac; done
shift   # the duration
exec "$@"
EOT
cat > "$work/bin/ansible-playbook" << 'EOT'
#!/usr/bin/env bash
# fake ansible-playbook for cpu-power.yml: the same record/apply/restore/unstage flow over fake per-host trees, with the real cpu_power.py
act="" label="" arm="" dir=""
while [ $# -gt 0 ]; do
  case "$1" in
    -e) shift
      case "$1" in cpu_power_action=*) act=${1#*=};; cpu_power_label=*) label=${1#*=};; cpu_power_arm=*) arm=${1#*=};; cpu_power_dir=*) dir=${1#*=};; esac ;;
  esac
  shift
done
echo "ap $act $label $arm" >> "$GOVAB_TEST_AP"
rc=0
for h in msa2-server msa2-client; do
  export CPU_SYSFS=$GOVAB_TEST_SYS/$h CPUINFO=/nonexistent
  case "$act" in
    record)
      [ "$label" = orig ] && [ -e "$dir/orig/$h.json" ] && { echo "orig is write-once"; rc=2; continue; }
      mkdir -p "$dir/$label"; "$GOVAB_PY" -I -B "$GOVAB_CPPY" record --out "$dir/$label/$h.json" --label "$label" --samples 1 || rc=2 ;;
    apply|restore)
      [ -e "$dir/orig/$h.json" ] || { echo "$h: no original record: nothing to restore"; continue; }
      a=$arm; [ "$act" = restore ] && a=as-found
      mkdir -p "$dir/$label"
      if [ "${GOVAB_TEST_REFUSE:-}" = "$h:$a" ] || { [ "${GOVAB_TEST_REFUSE_RESTORE:-}" = "$h" ] && [ "$act" = restore ]; }; then
        echo "$h: arm $a did not take (refused)"; echo '{"ok": false}' > "$dir/$label/$h.apply.json"; rc=2; continue
      fi
      "$GOVAB_PY" -I -B "$GOVAB_CPPY" apply --arm "$a" --orig "$dir/orig/$h.json" --out "$dir/$label/$h.apply.json" || { rc=2; continue; }
      "$GOVAB_PY" -I -B "$GOVAB_CPPY" record --out "$dir/$label/$h.json" --label "$label" --samples 1 || rc=2 ;;
    unstage) echo "unstage $h" ;;
  esac
done
exit $rc
EOT
chmod +x "$work/bin/"*

export GOVAB_PY=$py GOVAB_CPPY=$CPPY PY=$py MAGE=$work/bin/mage ANSIBLE=$work/bin/ansible-playbook TIMEOUT_CMD=$work/bin/timeout
export GOVAB_ANSIBLE_DIR=$work/ansible
mkdir -p "$work/ansible"
cat > "$work/table.tsv" << 'EOT'
# test table
gabw-std-h1	performance,performance-c2off	30	PROOF
gabw-std-h1,gabl-std-h1	as-found,performance,performance-c2off,as-found	60	FULL
EOT
export GOVAB_TABLE=$work/table.tsv

fresh() { # fresh <case>: a clean results dir and clean fake hosts; exports the test paths
  rm -rf "$work/c-$1"; mkdir -p "$work/c-$1/results"
  export GOVAB_RESULTS=$work/c-$1/results GOVAB_TEST_LOG=$work/c-$1/bench.log GOVAB_TEST_AP=$work/c-$1/ap.log GOVAB_TEST_SYS=$work/c-$1/sys
  mkdir -p "$GOVAB_TEST_SYS/msa2-server" "$GOVAB_TEST_SYS/msa2-client"
  mktree "$GOVAB_TEST_SYS/msa2-server"; mktree "$GOVAB_TEST_SYS/msa2-client"
  : > "$GOVAB_TEST_LOG"; : > "$GOVAB_TEST_AP"
  unset GOVAB_TEST_FAIL_BENCH GOVAB_TEST_REFUSE GOVAB_TEST_REFUSE_RESTORE GOVAB_TEST_HANG_BENCH
}
govs() { tr -d '\n' < "$GOVAB_TEST_SYS/$1/cpu0/cpufreq/scaling_governor"; }
runs() { BENCH_TARGET=msa2-server BENCH_COMPETITORS=$1 bash "$SCRIPT" "${2:-run}" > "$work/out.txt" 2>&1; echo $? > "$work/rc.txt"; }
rc() { cat "$work/rc.txt"; }

echo "--- a plain dispatch: one BenchTier, no arms, no change"
fresh plain
runs celeris-std-h1
check "plain: rc 0" test "$(rc)" = 0
check "plain: exactly one BenchTier" test "$(wc -l < "$GOVAB_TEST_LOG" | tr -d ' ')" = 1
check "plain: the playbook was never called" test ! -s "$GOVAB_TEST_AP"
check "plain: no cpu-power directory" test ! -e "$GOVAB_RESULTS/cpu-power"

echo "--- the proof table entry"
fresh proof
runs gabw-std-h1
check "proof: rc 0" test "$(rc)" = 0
check "proof: bench1 ran performance on both hosts, C2 on" grep -q '^bench1 server=performance client=performance c2disable=0$' "$GOVAB_TEST_LOG"
check "proof: bench2 ran performance, C2 off" grep -q '^bench2 server=performance client=performance c2disable=1$' "$GOVAB_TEST_LOG"
check "proof: both hosts ended powersave, C2 on" test "$(govs msa2-server)$(govs msa2-client)" = powersavepowersave
check "proof: arms.tsv has two rows, rc 0, one results dir each" test "$(awk -F'\t' 'NR>1 && $5==0 && $6 ~ /-bench-v1$/' "$GOVAB_RESULTS/cpu-power/arms.tsv" | wc -l | tr -d ' ')" = 2
check "proof: the status says RESTORED-EQUAL" grep -q '^RESTORED-EQUAL' "$GOVAB_RESULTS/cpu-power/restore-status.txt"
check "proof: arms-plan names the table line" grep -q '^arms	performance,performance-c2off$' "$GOVAB_RESULTS/cpu-power/arms-plan.txt"
check "proof: the always() restore after success is a no-op" bash -c "BENCH_TARGET=msa2-server BENCH_COMPETITORS=gabw-std-h1 bash '$SCRIPT' restore | grep -q 'already restored'"

echo "--- the full table: A1, P, PC, A2"
fresh full
runs gabw-std-h1,gabl-std-h1
check "full: rc 0" test "$(rc)" = 0
check "full: A1 as found" grep -q '^bench1 server=powersave client=powersave c2disable=0$' "$GOVAB_TEST_LOG"
check "full: P" grep -q '^bench2 server=performance client=performance c2disable=0$' "$GOVAB_TEST_LOG"
check "full: PC" grep -q '^bench3 server=performance client=performance c2disable=1$' "$GOVAB_TEST_LOG"
check "full: A2 as found again" grep -q '^bench4 server=powersave client=powersave c2disable=0$' "$GOVAB_TEST_LOG"
check "full: four arms rows" test "$(wc -l < "$GOVAB_RESULTS/cpu-power/arms.tsv" | tr -d ' ')" = 5
check "full: original recorded once, before the first apply" test "$(grep -n 'ap record orig' "$GOVAB_TEST_AP" | head -1 | cut -d: -f1)" = 1
check "full: final compared equal" grep -q '^RESTORED-EQUAL' "$GOVAB_RESULTS/cpu-power/restore-status.txt"

echo "--- an arm that did not take: no benchmark in that arm, everything restored, red"
fresh refuse
GOVAB_TEST_REFUSE=msa2-client:performance runs gabw-std-h1,gabl-std-h1
check "refused set: red (rc 4)" test "$(rc)" = 4
check "refused set: only arm 1 ran a benchmark" test "$(wc -l < "$GOVAB_TEST_LOG" | tr -d ' ')" = 1
check "refused set: hosts restored" test "$(govs msa2-server)$(govs msa2-client)" = powersavepowersave
check "refused set: restored equal" grep -q '^RESTORED-EQUAL' "$GOVAB_RESULTS/cpu-power/restore-status.txt"

echo "--- a BenchTier that fails: restored, red with its rc"
fresh failbench
GOVAB_TEST_FAIL_BENCH=2 runs gabw-std-h1,gabl-std-h1
check "failed bench: rc 1" test "$(rc)" = 1
check "failed bench: arms 3 and 4 did not run" test "$(wc -l < "$GOVAB_TEST_LOG" | tr -d ' ')" = 2
check "failed bench: hosts restored" test "$(govs msa2-server)$(govs msa2-client)" = powersavepowersave
check "failed bench: the row says rc 1" grep -q '^2	performance	[^	]*	[^	]*	1	' "$GOVAB_RESULTS/cpu-power/arms.tsv"

echo "--- the stop rule: a host already on performance"
fresh stop
for c in 0 1 2 3; do echo performance > "$GOVAB_TEST_SYS/msa2-server/cpu$c/cpufreq/scaling_governor"; done
runs gabw-std-h1,gabl-std-h1
check "stop rule: rc 3" test "$(rc)" = 3
check "stop rule: A1 (as found) ran, P did not" test "$(wc -l < "$GOVAB_TEST_LOG" | tr -d ' ')" = 1
check "stop rule: marker written" test -s "$GOVAB_RESULTS/cpu-power/STOP-RULE.txt"
check "stop rule: the host is as it was (performance)" test "$(govs msa2-server)" = performance
fresh stopproof
for c in 0 1 2 3; do echo performance > "$GOVAB_TEST_SYS/msa2-server/cpu$c/cpufreq/scaling_governor"; done
runs gabw-std-h1
check "stop rule on the proof (no as-found arm): rc 3 and no benchmark" test "$(rc)$(wc -l < "$GOVAB_TEST_LOG" | tr -d ' ')" = 30

echo "--- a gab value that is no table line fails closed"
fresh closed
runs gabw-std-h1,gabx-std-h1
check "unknown gab value: rc 2" test "$(rc)" = 2
check "  nothing was called" test ! -s "$GOVAB_TEST_AP" -a ! -s "$GOVAB_TEST_LOG"
fresh wrongtarget
BENCH_TARGET=both BENCH_COMPETITORS=gabw-std-h1 bash "$SCRIPT" > "$work/out.txt" 2>&1; echo $? > "$work/rc.txt"
check "target both with arms: rc 2" test "$(rc)" = 2
fresh stale
mkdir -p "$GOVAB_RESULTS/cpu-power/orig"; echo '{}' > "$GOVAB_RESULTS/cpu-power/orig/msa2-server.json"
runs gabw-std-h1
check "a stale cpu-power directory is refused" test "$(rc)" = 2

echo "--- a restore that cannot put a host back is red and says so"
fresh norestore
GOVAB_TEST_REFUSE_RESTORE=msa2-client runs gabw-std-h1,gabl-std-h1
check "failed restore: red" test "$(rc)" != 0
check "failed restore: NOT-RESTORED written" grep -q '^NOT-RESTORED' "$GOVAB_RESULTS/cpu-power/restore-status.txt"

echo "--- a left-behind change the restore did not see: compare catches it"
fresh leftbehind
# a stub whose restore leaves cpu2 C3 disabled although it reported success: the compare of the final record with the original must fail the job
cat > "$work/bin/ap-leave" << 'EOT'
#!/usr/bin/env bash
# the fake playbook, except that its restore silently skips the idle flags
"$GOVAB_REAL_AP" "$@"; rc=$?
case " $* " in *cpu_power_action=restore*) echo 1 > "$GOVAB_TEST_SYS/msa2-server/cpu2/cpuidle/state3/disable"; CPU_SYSFS=$GOVAB_TEST_SYS/msa2-server CPUINFO=/nonexistent "$GOVAB_PY" -I -B "$GOVAB_CPPY" record --out "$GOVAB_RESULTS/cpu-power/final/msa2-server.json" --samples 1 ;; esac
exit $rc
EOT
chmod +x "$work/bin/ap-leave"
GOVAB_REAL_AP=$work/bin/ansible-playbook ANSIBLE=$work/bin/ap-leave runs gabw-std-h1,gabl-std-h1
check "left behind: red" test "$(rc)" != 0
check "left behind: NOT-EQUAL written" grep -q '^NOT-EQUAL' "$GOVAB_RESULTS/cpu-power/restore-status.txt"

echo "--- a TERM during an arm restores (the trap) and the always() step finds nothing to do"
fresh term
( GOVAB_TEST_HANG_BENCH=2 BENCH_TARGET=msa2-server BENCH_COMPETITORS=gabw-std-h1 bash "$SCRIPT" > "$work/out.txt" 2>&1; echo $? > "$work/rc.txt" ) &
bg=$!
n=0; while [ "$(wc -l < "$GOVAB_TEST_LOG" | tr -d ' ')" -lt 2 ] && [ $n -lt 100 ]; do sleep 0.2; n=$((n + 1)); done
pkill -TERM -f "gov-ab-arms.sh" 2> /dev/null; pkill -TERM -f "bin/mage" 2> /dev/null
wait "$bg"
check "TERM: hosts restored" test "$(govs msa2-server)$(govs msa2-client)" = powersavepowersave
check "TERM: restored equal" grep -q '^RESTORED-EQUAL' "$GOVAB_RESULTS/cpu-power/restore-status.txt"

echo "$pass passed, $fail failed"
[ "$fail" = 0 ]
