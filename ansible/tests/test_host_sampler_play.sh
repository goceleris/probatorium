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
statf() { # statf <pid> <field no. of proc(5)>: fields counted as proman does, comm may hold spaces
  sed 's/^.*) //' "/proc/$1/stat" 2> /dev/null | awk -v n="$2" '{print $(n - 2)}'
}
zhead_has() { # zhead_has <file.z> <text>: the first 200 bytes of the decompressed stream hold <text>
  python3 -c 'import sys,zlib; print(zlib.decompress(open(sys.argv[1], "rb").read()).decode()[:200])' "$1" | grep -q "$2"
}
gone() { # gone <pid>: up to 20 s
  local n=0
  while [ -d "/proc/$1" ] && [ "$n" -lt 40 ]; do sleep 0.5; n=$((n + 1)); done
  [ ! -d "/proc/$1" ]
}

echo "--- a start without a fine cadence (the race and weekend tiers)"
check "start without a fine cadence succeeds" play sampler_action=start sampler_interval=2 sampler_duration_seconds=300 sampler_tag=race-1
check "  the 60 s sampler is alive" alive "$run/host-sampler.pid"
sp=$(cat "$run/host-sampler.pid" 2> /dev/null)
check "  and runs at the priority it was started with (policy 0, nice 0)" test "$(statf "$sp" 41)/$(statf "$sp" 19)" = "0/0"
check "  no fine sampler was started" test ! -e "$run/host-sampler-fine.pid"
check "  no fine sampler was staged" test ! -e "$run/celeris-host-sampler-fine"
check "  no fine directory was written" test -z "$(ls -A "$fine")"
check "stop succeeds" play sampler_action=stop "sampler_fetch_dir=$fetch"
check "  the 60 s series was fetched" test -s "$fetch/localhost/samples.jsonl"
check "  nothing was fetched into fine/" test ! -e "$fetch/localhost/fine"
sp=$(cat "$run/host-sampler.pid" 2> /dev/null)
[ -z "$sp" ] || gone "$sp"

echo "--- a start with a fine cadence (the bench tier)"
check "start with sampler_fine_interval=0.1 and low priority succeeds" play sampler_action=start sampler_interval=2 sampler_fine_interval=0.1 sampler_low_priority=true sampler_duration_seconds=300 sampler_tag=bench-1
check "  the report says it is alive" grep -q "fine sampler pid=[0-9]* alive" "$work/last.log"
check "  the fine sampler is alive" alive "$run/host-sampler-fine.pid"
fp=$(finepid)
sp=$(cat "$run/host-sampler.pid" 2> /dev/null)
check "  the fine sampler says which scheduling class it took" grep -q "sched=[a-z0-9+-]*" "$fine/fine-run.txt"
if grep -q "sched=idle" "$fine/fine-run.txt"; then
  check "  and it is SCHED_IDLE (policy 5)" test "$(statf "$fp" 41)" = 5
else
  echo "note: SCHED_IDLE was refused here ($(cat "$fine/fine-run.txt")); policy $(statf "$fp" 41)"
fi
if command -v chrt > /dev/null; then
  check "  the 60 s sampler is SCHED_IDLE too (policy 5, bench tier only)" test "$(statf "$sp" 41)" = 5
else
  check "  the 60 s sampler is at nice 19 (no chrt here)" test "$(statf "$sp" 19)" = 19
fi
check "  its header carries the run's tag, cadence and class" grep -q "^# fine-sampler format=1 .* tag=bench-1 interval_s=0.1 .* sched=" "$fine/fine.log"
check "  its run file carries the tag" grep -q "^tag=bench-1 " "$fine/fine-run.txt"
# the series is buffered and flushed every 5 s
n=0
while [ "$(wc -l < "$fine/fine.log")" -lt 10 ] && [ "$n" -lt 30 ]; do sleep 0.5; n=$((n + 1)); done
check "  it is writing ticks" test "$(wc -l < "$fine/fine.log")" -ge 10
check "stop with another run's tag succeeds" play sampler_action=stop "sampler_fetch_dir=$fetch/other" sampler_fetch_tag=bench-OTHER
check "  and fetches nothing of the fine series" test ! -e "$fetch/other/localhost/fine"
check "  the fine sampler exited" gone "$fp"
check "stop without a tag succeeds" play sampler_action=stop "sampler_fetch_dir=$fetch/notag"
check "  and fetches nothing of the fine series (race and weekend stops)" test ! -e "$fetch/notag/localhost/fine"
check "stop naming this run succeeds" play sampler_action=stop "sampler_fetch_dir=$fetch" sampler_fetch_tag=bench-1
check "  the stats say it was asked to stop" grep -q "final=1 .*exit_reason=stop_requested" "$fine/fine.stats"
check "  the open generation was compressed at the end" test ! -e "$fine/fine.log"
check "  a compressed generation was fetched" test -s "$fetch/localhost/fine/fine.00000.log.z"
check "  and it holds the series (zlib, header with the tag)" zhead_has "$fetch/localhost/fine/fine.00000.log.z" "tag=bench-1"
check "  fine-minute.log was fetched" test -s "$fetch/localhost/fine/fine-minute.log"
check "  fine-events.log was fetched" test -s "$fetch/localhost/fine/fine-events.log"
check "  fine.stats was fetched" test -s "$fetch/localhost/fine/fine.stats"
check "  fine-run.txt was fetched" test -s "$fetch/localhost/fine/fine-run.txt"
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

echo "--- the 60 s sampler finds its pids without pgrep / ps (SCHED_IDLE hazard: they open every process's cmdline)"
stub=$work/stub
mkdir -p "$stub" "$work/sample-out" "$work/sample-run"
for tool in pgrep ps pidof; do
  printf '#!/bin/sh\necho %s >> "%s/tool-called"\nexit 1\n' "$tool" "$work" > "$stub/$tool"
  chmod +x "$stub/$tool"
done
cp "$(command -v sleep)" "$work/validator" # a process whose comm is validator
"$work/validator" 60 &
vpid=$!
SAMPLER_OUTDIR=$work/sample-out SAMPLER_RUNDIR=$work/sample-run PATH="$stub:$PATH" \
  "$here/../ops/host-sampler/celeris-host-sampler" --sample > /dev/null 2>&1
check "  a sample was written" test -s "$work/sample-out/samples.jsonl"
check "  it found the validator by comm" grep -q "\"validator_pid\":$vpid," "$work/sample-out/samples.jsonl"
check "  and called neither pgrep, ps nor pidof" test ! -e "$work/tool-called"
kill "$vpid" 2> /dev/null
wait "$vpid" 2> /dev/null

echo "$pass passed, $fail failed"
[ "$fail" = 0 ]
