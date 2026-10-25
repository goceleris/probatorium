#!/usr/bin/env bash
# Runs the REAL ansible/host-sampler.yml on a local connection (no become, the run / output / fetch directories
# moved into a temp dir by the playbook's own variables) and checks what only a real run shows: that a start
# WITHOUT a fine cadence (the race and weekend tiers) starts no fine sampler and fetches nothing new; that a start
# WITH one launches the python sampler, which writes its series, lowers its own priority and ends on the stop
# sentinel; that the stop fetches the fine files into <fetch dir>/<host>/fine/; that a start without a fine
# cadence retires a fine sampler an earlier run left; and that a cadence outside 0.05-10 s is refused before
# anything starts.
#   bash ansible/tests/test_host_sampler_play.sh     (needs ansible-playbook and a Linux /proc; skips with exit 0 without)
set -u
here=$(cd "$(dirname "$0")/.." && pwd)
command -v ansible-playbook > /dev/null || { echo "SKIP: ansible-playbook not installed"; exit 0; }
[ -r /proc/stat ] && [ -r /proc/softirqs ] || { echo "SKIP: needs a Linux /proc"; exit 0; }
# the playbook creates root-owned files (it runs with become on the cluster): as root directly, otherwise through
# passwordless sudo, as on a hosted runner
if [ "$(id -u)" = 0 ]; then
  become=false
elif sudo -n true 2> /dev/null; then
  become=true
else
  echo "SKIP: needs root or passwordless sudo"
  exit 0
fi

work=$(mktemp -d)
run=$work/run out=$work/out fine=$work/fine fetch=$work/fetch
mkdir -p "$run" "$out" "$fine"
# (root-owned files in directories the playbook chowned: remove them with the privilege that made them)
asroot=""
[ "$become" = true ] && asroot="sudo -n"
trap '$asroot touch "$run/host-sampler-stop" "$run/host-sampler-fine-stop" 2> /dev/null; sleep 3; $asroot rm -rf "$work"' EXIT

py=$(command -v python3)
cat > "$work/inventory.ini" << EOT
[cluster]
localhost ansible_connection=local ansible_become=$become ansible_python_interpreter=$py
EOT
export ANSIBLE_CONFIG=$here/ansible.cfg ANSIBLE_LOCALHOST_WARNING=False ANSIBLE_INVENTORY_UNPARSED_WARNING=False

pass=0
fail=0
ok() { pass=$((pass + 1)); echo "ok   $1"; }
bad() { fail=$((fail + 1)); echo "FAIL $1" >&2; }
check() { local n=$1; shift; if "$@"; then ok "$n"; else bad "$n"; fi; }

play() { # play <extra-vars...>; the log of the last run is $work/last.log
  local a=()
  for v in "$@"; do a+=(-e "$v"); done
  (cd "$here" && ansible-playbook -i "$work/inventory.ini" host-sampler.yml \
    -e "sampler_rundir=$run" -e "sampler_outdir=$out" -e "sampler_fine_dir=$fine" "${a[@]}") > "$work/last.log" 2>&1
}
refused() { ! play "$@"; }
alive() { local p; p=$(cat "$1" 2> /dev/null) && [ -n "$p" ] && [ -d "/proc/$p" ]; }
finepid() { cat "$run/host-sampler-fine.pid" 2> /dev/null; }
gone() { # gone <pid>: up to 20 s
  local n=0
  while [ -d "/proc/$1" ] && [ "$n" -lt 40 ]; do sleep 0.5; n=$((n + 1)); done
  [ ! -d "/proc/$1" ]
}

echo "--- a start without a fine cadence (the race and weekend tiers)"
check "start without a fine cadence succeeds" play sampler_action=start sampler_interval=2 sampler_duration_seconds=300 sampler_tag=race-1
check "  the 60 s sampler is alive" alive "$run/host-sampler.pid"
check "  no fine sampler was started" test ! -e "$run/host-sampler-fine.pid"
check "  no fine sampler was staged" test ! -e "$run/celeris-host-sampler-fine"
check "  no fine directory was written" test -z "$(ls -A "$fine")"
check "stop succeeds" play sampler_action=stop "sampler_fetch_dir=$fetch"
check "  the 60 s series was fetched" test -s "$fetch/localhost/samples.jsonl"
check "  nothing was fetched into fine/" test ! -e "$fetch/localhost/fine"
sp=$(cat "$run/host-sampler.pid" 2> /dev/null)
[ -z "$sp" ] || gone "$sp"

echo "--- a start with a fine cadence (the bench tier)"
check "start with sampler_fine_interval=0.1 succeeds" play sampler_action=start sampler_interval=2 sampler_fine_interval=0.1 sampler_duration_seconds=300 sampler_tag=bench-1
check "  the report says it is alive" grep -q "fine sampler pid=[0-9]* alive" "$work/last.log"
check "  the fine sampler is alive" alive "$run/host-sampler-fine.pid"
fp=$(finepid)
check "  it runs at a lower priority (nice 10)" test "$(awk '{print $19}' "/proc/$fp/stat" 2> /dev/null)" = 10
check "  its header carries the run's tag and cadence" grep -q "^# fine-sampler format=1 .* tag=bench-1 interval_s=0.1 " "$fine/fine.log"
# the series is buffered and flushed every 5 s
n=0
while [ "$(wc -l < "$fine/fine.log")" -lt 10 ] && [ "$n" -lt 30 ]; do sleep 0.5; n=$((n + 1)); done
check "  it is writing ticks" test "$(wc -l < "$fine/fine.log")" -ge 10
check "stop succeeds with a fine sampler" play sampler_action=stop "sampler_fetch_dir=$fetch"
check "  the fine sampler exited" gone "$fp"
check "  the stats say it was asked to stop" grep -q "final=1 exit_reason=stop_requested" "$fine/fine.stats"
check "  fine.log was fetched into fine/" test -s "$fetch/localhost/fine/fine.log"
check "  fine-events.log was fetched" test -s "$fetch/localhost/fine/fine-events.log"
check "  fine.stats was fetched" test -s "$fetch/localhost/fine/fine.stats"
sp=$(cat "$run/host-sampler.pid" 2> /dev/null)
[ -z "$sp" ] || gone "$sp"

echo "--- a start without a fine cadence retires a stale fine sampler"
check "start with sampler_fine_interval=0.5 succeeds" play sampler_action=start sampler_interval=2 sampler_fine_interval=0.5 sampler_duration_seconds=300 sampler_tag=bench-2
fp=$(finepid)
check "  the fine sampler is alive" alive "$run/host-sampler-fine.pid"
check "start without it succeeds" play sampler_action=start sampler_interval=2 sampler_duration_seconds=300 sampler_tag=race-2
check "  the stale fine sampler exited" gone "$fp"
check "  the stats say it was asked to stop" grep -q "exit_reason=stop_requested" "$fine/fine.stats"
# the 60 s sampler of this start takes over from the one of the previous start (up to 5 s) and then
# deletes the sentinel: a stop issued inside that window would be lost (a minutes-long gap in a real run)
n=0
while ! grep -q '"event":"sampler_start".*"tag":"race-2"' "$out/samples.jsonl" && [ "$n" -lt 40 ]; do sleep 0.5; n=$((n + 1)); done
check "stop" play sampler_action=stop "sampler_fetch_dir=$fetch"
sp=$(cat "$run/host-sampler.pid" 2> /dev/null)
[ -z "$sp" ] || gone "$sp"

echo "--- cadences outside the bounds are refused before anything starts"
rm -f "$run/host-sampler-fine.pid" "$run/host-sampler.pid"
for bad in 0.01 0.049 10.5 11 60 abc -1 1.2345; do
  check "sampler_fine_interval=$bad is refused" refused sampler_action=start "sampler_fine_interval=$bad"
  check "  and nothing started" test ! -e "$run/host-sampler-fine.pid"
done
for bad in 0 3601 1.5 abc; do
  check "sampler_interval=$bad is refused" refused sampler_action=start "sampler_interval=$bad"
done
check "  and nothing started (60 s sampler)" test ! -e "$run/host-sampler.pid"
check "a bad action is refused" refused sampler_action=restart

echo "$pass passed, $fail failed"
[ "$fail" = 0 ]
