#!/usr/bin/env bash
# Runs the REAL ansible/cpu-power.yml + tasks/cpu_power.yml on a local connection (no become; the run directory and the file owner moved by the playbook's own test hooks) against a FAKE
# sysfs tree (CPU_SYSFS, which the staged script reads): the original record is write-once, performance is applied from it and read back, a refused write fails the play and still fetches
# the result, the restore ends equal to the original, a restore with no original is a no-op, unstage removes the script. Not tested here: real sysfs, real sudo (the smoke dispatch).
#   bash ansible/tests/test_cpu_power_play.sh     (needs ansible-playbook; skips with exit 0 without)
set -u
here=$(cd "$(dirname "$0")/.." && pwd)
command -v ansible-playbook > /dev/null || { echo "SKIP: ansible-playbook not installed"; exit 0; }
work=$(mktemp -d)
trap '[ -n "${KEEP:-}" ] || { chmod -R u+rw "${work:?}" 2> /dev/null; rm -rf "${work:?}"; }' EXIT
py=$(command -v python3)
# the interpreter ansible's own modules run under: the one in ansible-playbook's shebang when it is a python (a newer python3 on PATH can be one ansible's module payload does not load under)
sb=$(head -1 "$(command -v ansible-playbook)" | sed 's/^#! *//' | awk '{print $1}')
case "$sb" in */python*) [ -x "$sb" ] && apy=$sb ;; esac
apy=${apy:-$py}
cat > "$work/inventory.ini" << EOT
[cluster]
msa2-server ansible_connection=local ansible_become=false ansible_python_interpreter=$apy
EOT
export ANSIBLE_CONFIG=$here/ansible.cfg ANSIBLE_LOCALHOST_WARNING=False ANSIBLE_INVENTORY_UNPARSED_WARNING=False
export CPU_SYSFS=$work/sys CPUINFO=$work/cpuinfo
pass=0
fail=0
ok() { pass=$((pass + 1)); echo "ok   $1"; }
bad() { fail=$((fail + 1)); echo "FAIL $1" >&2; [ -f "$work/last.log" ] && tail -12 "$work/last.log" | cut -c1-300 >&2; }
check() { local n=$1; shift; if "$@"; then ok "$n"; else bad "$n"; fi; }

for c in 0 1 2 3; do
  d=$CPU_SYSFS/cpu$c
  mkdir -p "$d/cpufreq" "$d/cpuidle"
  echo powersave > "$d/cpufreq/scaling_governor"; echo amd-pstate-epp > "$d/cpufreq/scaling_driver"
  echo "performance powersave" > "$d/cpufreq/scaling_available_governors"
  echo balance_performance > "$d/cpufreq/energy_performance_preference"; echo 1600000 > "$d/cpufreq/scaling_cur_freq"
  i=0
  for n in POLL C1 C2 C3; do
    mkdir -p "$d/cpuidle/state$i"; echo "$n" > "$d/cpuidle/state$i/name"; echo 0 > "$d/cpuidle/state$i/disable"
    echo $((i * 40)) > "$d/cpuidle/state$i/latency"; echo 0 > "$d/cpuidle/state$i/usage"; echo 0 > "$d/cpuidle/state$i/time"
    i=$((i + 1))
  done
done
mkdir -p "$CPU_SYSFS/cpufreq" "$CPU_SYSFS/cpuidle"; echo 1 > "$CPU_SYSFS/cpufreq/boost"
echo acpi_idle > "$CPU_SYSFS/cpuidle/current_driver"; echo menu > "$CPU_SYSFS/cpuidle/current_governor"
printf 'processor\t: 0\ncpu MHz\t\t: 1600.000\n' > "$CPUINFO"

res=$work/results/cpu-power run=$work/run
play() { # play <action> <label> [arm]; the log of the last run is $work/last.log
  local extra=()
  [ -n "${3:-}" ] && extra=(-e "cpu_power_arm=$3")
  (cd "$here" && ansible-playbook -i "$work/inventory.ini" cpu-power.yml -e "cpu_power_action=$1" -e "cpu_power_label=$2" -e "cpu_power_dir=$res" -e cpu_power_tag=t1 \
    -e "cpu_power_run_dir=$run" -e "cpu_power_file_owner=$(id -un)" -e "cpu_power_file_group=$(id -gn)" ${extra[@]+"${extra[@]}"}) > "$work/last.log" 2>&1
}
refused() { ! play "$@" && ! grep -q "YAML parsing failed\|ERROR! \|syntax" "$work/last.log"; }
vals() { local f; for f in "$@"; do tr -d '\n' < "$f"; echo; done | sort -u | tr '\n' ' '; }  # sysfs writes carry no newline
govs() { vals "$CPU_SYSFS"/cpu*/cpufreq/scaling_governor; }
dis() { vals "$CPU_SYSFS"/cpu*/cpuidle/state"$1"/disable; }
cp_py() { "$py" -I -B "$here/files/cpu_power.py" "$@"; }

echo "--- record: the original is write-once"
check "record orig" play record orig
check "  the original is on the controller" test -s "$res/orig/msa2-server.json"
check "  the script was staged on the run dir" test -x "$run/cpu_power.py"
cp "$res/orig/msa2-server.json" "$work/orig.keep"
check "a second record orig is refused" refused record orig
check "  and the original is untouched" cmp -s "$res/orig/msa2-server.json" "$work/orig.keep"

echo "--- the change"
check "apply performance" play apply applied performance
check "  governors" test "$(govs)" = "performance "
check "  idle states left as found" test "$(dis 0)$(dis 1)$(dis 2)$(dis 3)" = "0 0 0 0 "
check "  result and record fetched" test -s "$res/applied/msa2-server.apply.json" -a -s "$res/applied/msa2-server.json"
check "  the result says ok" grep -q '"ok": true' "$res/applied/msa2-server.apply.json"
check "apply with no original record is refused" bash -c "! (cd '$here' && ansible-playbook -i '$work/inventory.ini' cpu-power.yml -e cpu_power_action=apply -e cpu_power_label=applied -e cpu_power_arm=performance -e cpu_power_dir='$work/none' -e cpu_power_run_dir='$run' -e cpu_power_file_owner=$(id -un) -e cpu_power_file_group=$(id -gn)) > '$work/last.log' 2>&1"

if [ "$(id -u)" != 0 ]; then
  echo "--- a refused write"
  play restore back > /dev/null
  chmod 444 "$CPU_SYSFS/cpu2/cpufreq/scaling_governor"
  check "apply performance with a refused governor write fails the play" refused apply applied2 performance
  check "  and still fetched the result, which names the CPU" grep -q cpu2 "$res/applied2/msa2-server.apply.json"
  chmod 644 "$CPU_SYSFS/cpu2/cpufreq/scaling_governor"
fi

echo "--- the final restore"
check "restore" play restore final
check "  every value equals the original" cp_py compare --a "$res/orig/msa2-server.json" --b "$res/final/msa2-server.json"
check "  the summary of the original says powersave" bash -c "'$py' -I -B '$here/files/cpu_power.py' summary --rec '$res/orig/msa2-server.json' | grep -q governor=powersave=4"
check "unstage after this run's own original removes the script" play unstage x
check "  the script is gone" test ! -e "$run/cpu_power.py"
check "  and the host-side records" test ! -e "$run/records"
rm -rf "${res:?}"
check "a restore with no original record is a no-op" play restore final2

echo "--- the latch: the host-side original is removed only by the run that recorded it"
mkdir -p "$run/records"
touch "$run/records/orig.json"
check "a record orig on a host that still has the previous run's original is REFUSED" refused record orig
check "  nothing was fetched" test ! -e "$res/orig/msa2-server.json"
check "unstage for a host this run did not record leaves the latch" play unstage x
check "  the latch is still there" test -e "$run/records/orig.json"
rm -f "${run:?}/records/orig.json"
check "after the latch is cleared (reboot) the host records its original again" play record orig
check "unstage after it removes the latch" play unstage x
check "  gone" test ! -e "$run/records"

echo "$pass passed, $fail failed"
[ "$fail" = 0 ]
