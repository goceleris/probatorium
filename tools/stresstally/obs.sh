#!/usr/bin/env bash
# The observation wrapper of the stress workflow's timing mode (cluster only).
#
# shard.sh runs `go test -exec=<this file> ...`, so go test builds and links
# the test binary as usual and then runs THIS script as `obs.sh BINARY ARGS...`
# in the package directory, with the binary's stdout and stderr going to go
# test as ever. go test re-links the binary on every invocation, so a timing
# taken around go test would include the linker; this script measures the
# binary alone: one run of it is one observation.
#
# It appends ONE line to $STRESS_OBS_FILE, which shard.sh copies into the
# shard's log before the trailer:
#
#   stress-obs: seq= case= block= binary_sha256= exit= wall_ns= user_s= sys_s=
#               load1_before= load1_after= perf= instructions_u= cycles_u= task_clock_ms=
#
# user_s and sys_s are the CPU time of the binary (and of perf, when it
# counts): the shell's children time from `times` (millisecond resolution)
# after the run, minus the same before it, so the sha256sum above is not in
# it. instructions_u, cycles_u and task_clock_ms come from `perf stat` when
# the host script found it usable (STRESS_PERF=ok); otherwise perf= says why
# not and they are empty.
# The binary's sha256 shows that every observation of an arm ran the same
# bytes. The exit status is the binary's, so go test judges it as always.
set -u

bin=${1:?usage: obs.sh TEST_BINARY [ARGS...]}
out=${STRESS_OBS_FILE:?}
perf_state=${STRESS_PERF:-off}
perf_out="${out}.perf.$$"

clean() { printf '%s' "$1" | tr -c 'A-Za-z0-9_.,:<>+-' '_'; }
secs() { # bash `times` prints NmS.SSSs
	local m=${1%%m*} s=${1#*m}
	s=${s%s}
	awk -v m="$m" -v s="$s" 'BEGIN { printf "%.3f", m * 60 + s }'
}

sha=$(sha256sum "$bin" 2>/dev/null | awk '{print $1}')
read -r l1b _ </proc/loadavg

# The binary runs in the background only so that a signal go test sends this
# script (SIGQUIT when the binary overruns -timeout by go test's grace) reaches
# the binary too; the loop waits until it has really exited.
if [ "$perf_state" = "ok" ]; then
	cmd=(perf stat "-x," -e "instructions:u,cycles:u,task-clock" -o "$perf_out" -- "$@")
else
	cmd=("$@")
fi
# The second line of `times` is the CPU time of this shell's children. It
# must run in THIS shell (a redirection, not a command substitution: a
# forked subshell starts its own counts at zero).
times_file="${out}.times.$$"
times >"$times_file"
{ read -r _ _ && read -r cu0 cs0; } <"$times_file"
t0=$(date +%s%N)
"${cmd[@]}" &
child=$!
trap 'kill -s QUIT "$child" 2>/dev/null' QUIT
trap 'kill -s TERM "$child" 2>/dev/null' TERM INT
rc=0
wait "$child" || rc=$?
while kill -0 "$child" 2>/dev/null; do
	rc=0
	wait "$child" || rc=$?
done
t1=$(date +%s%N)
read -r l1a _ </proc/loadavg

times >"$times_file"
{ read -r _ _ && read -r cu1 cs1; } <"$times_file"
rm -f "$times_file"
user_s=$(awk -v a="$(secs "$cu0")" -v b="$(secs "$cu1")" 'BEGIN { printf "%.3f", b - a }')
sys_s=$(awk -v a="$(secs "$cs0")" -v b="$(secs "$cs1")" 'BEGIN { printf "%.3f", b - a }')

# perf -x, prints one line per event and PMU: the count, the unit, the event.
# On a heterogeneous host (arm64 big.LITTLE) each core type has its own PMU
# and the event is qualified by it ("armv8_cortex_a720/instructions/u"); a
# PMU whose cores never ran the binary prints "<not counted>". The count is
# the sum over the PMUs that counted; none counted is empty.
perf_sum() {
	awk -F, -v ev="$1" '$3 ~ ev && $1 ~ /^[0-9]+$/ {s += $1; n++} END {if (n) printf "%.0f", s}' "$2"
}
ins="" cyc="" tc=""
if [ -f "$perf_out" ]; then
	ins=$(perf_sum instructions "$perf_out")
	cyc=$(perf_sum cycles "$perf_out")
	tc=$(awk -F, '$3 ~ /task-clock/ && $1 ~ /^[0-9.]+$/ {print $1; exit}' "$perf_out")
	rm -f "$perf_out"
fi

printf 'stress-obs: seq=%s case=%s block=%s binary_sha256=%s exit=%s wall_ns=%s user_s=%s sys_s=%s load1_before=%s load1_after=%s perf=%s instructions_u=%s cycles_u=%s task_clock_ms=%s\n' \
	"$(clean "${STRESS_OBS_SEQ:-}")" "$(clean "${STRESS_CASE:-}")" "$(clean "${STRESS_SHARD:-}")" "$(clean "$sha")" "$rc" \
	"$((t1 - t0))" "$user_s" "$sys_s" "$(clean "$l1b")" "$(clean "$l1a")" "$(clean "$perf_state")" \
	"$(clean "$ins")" "$(clean "$cyc")" "$(clean "$tc")" >>"$out"
exit "$rc"
