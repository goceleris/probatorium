#!/usr/bin/env bash
# gov-ab-arms.sh: the GOVAB arm loop of the benchmark job (THROWAWAY ab/gov-ab-20261122, not for merge). ONE step of benchmark-tier.yml runs it in place of a bare `mage BenchTier`.
#
#   gov-ab-arms.sh [run]     the benchmark step: a competitors value that does not start with `gab` runs ONE plain `mage BenchTier` (exec, as before this branch); a `gab...` value is looked up
#                            EXACTLY in .github/gov-ab-arms.tsv (no entry = the job fails before anything is changed), then
#                              record the ORIGINAL state of msa2-server and msa2-client (write-once, results/cpu-power/orig/<host>.json),
#                              for each arm: move both hosts to the arm FROM THE ORIGINAL RECORD and read every value back (a value that did not take fails the job BEFORE the benchmark),
#                              `mage BenchTier` (own timeout), a full record after it, one row in results/cpu-power/arms.tsv,
#                              stop rule: when the original msa2-server record shows ANY CPU on the performance governor, an as-found arm may still run but the first arm that changes the state is NOT run (exit 3, STOP-RULE.txt): the contrast would be empty,
#                              finally restore from the original record, read it back, record `final`, and COMPARE it with the original: any difference fails the job.
#                            A trap (EXIT, TERM, INT) runs the same restore on every exit path.
#   gov-ab-arms.sh restore  the `if: always()` step after the benchmark: the same restore + compare; a no-op when no original record exists (nothing was changed) or it already succeeded.
#
# Files (all under results/cpu-power, uploaded with the results): arms-plan.txt, arms.tsv (k, arm, start, end, BenchTier rc, results dir, pre record, post record: the reader maps arms from THIS file,
# never from timestamps), orig/ pre-<k>/ post-<k>/ final/ (<host>.json = the record, <host>.apply.json = what the apply did and read back), STOP-RULE.txt, restore-status.txt.
# Test hooks: MAGE, ANSIBLE, PY, TIMEOUT_CMD, GOVAB_TABLE, GOVAB_RESULTS, GOVAB_ANSIBLE_DIR, GOVAB_SETTLE (seconds slept after an arm took, default 0).
# shellcheck disable=SC2329,SC2012  # on_exit runs from the trap; a listing of results/ names only directories this script knows
set -u
MODE=${1:-run}
MAGE=${MAGE:-mage}
ANSIBLE=${ANSIBLE:-ansible-playbook}
PY=${PY:-python3}
TIMEOUT_CMD=${TIMEOUT_CMD:-timeout}
TABLE=${GOVAB_TABLE:-.github/gov-ab-arms.tsv}
RES=${GOVAB_RESULTS:-$PWD/results}
ADIR=${GOVAB_ANSIBLE_DIR:-$PWD/ansible}
CPPY=${GOVAB_CPPY:-$ADIR/files/cpu_power.py}
CP=$RES/cpu-power
TAG=${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-1}
COMP=${BENCH_COMPETITORS:-}

say() { echo "gov-ab: $*"; }
die() { say "FAIL: $*"; exit "${2:-1}"; }

ap() { # ap <action> <label> [arm]: the cpu-power playbook on msa2-server and msa2-client
  local extra=()
  [ -n "${3:-}" ] && extra=(-e "cpu_power_arm=$3")
  [ "${CLUSTER_USE_LAN:-}" = 1 ] && extra+=(-e use_lan=true)
  (cd "$ADIR" && "$ANSIBLE" -i inventory.yml cpu-power.yml -e "cpu_power_action=$1" -e "cpu_power_label=$2" -e "cpu_power_dir=$CP" -e "cpu_power_tag=$TAG" ${extra[@]+"${extra[@]}"})
}

restore_all() { # the restore every path ends in: as-found from the original record, read back, record `final`, COMPARE; never throws
  [ -f "$CP/restore-status.txt" ] && grep -q '^RESTORED-EQUAL' "$CP/restore-status.txt" && { say "already restored and equal"; return 0; }
  if [ ! -d "$CP/orig" ] || [ -z "$(ls -A "$CP/orig" 2> /dev/null)" ]; then
    say "no original record: nothing was changed, nothing to restore"
    return 0
  fi
  local rc=0 h f
  ap restore final || rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "NOT-RESTORED: the restore play failed (rc=$rc); a host may be left changed: read final/ and the next survey" > "$CP/restore-status.txt"
    say "NOT RESTORED (rc=$rc)"
    ap unstage x > /dev/null 2>&1 || true
    return 1
  fi
  for f in "$CP"/orig/*.json; do
    h=$(basename "$f" .json)
    "$PY" -I -B "$CPPY" compare --a "$f" --b "$CP/final/$h.json" || rc=1
  done
  if [ "$rc" -ne 0 ]; then
    echo "NOT-EQUAL: a host differs from its original record after the restore (see the lines above)" > "$CP/restore-status.txt"
    say "LEFT BEHIND: the final record differs from the original"
    ap unstage x > /dev/null 2>&1 || true
    return 1
  fi
  echo "RESTORED-EQUAL $(date -u +%FT%TZ)" > "$CP/restore-status.txt"
  ap unstage x || say "warning: unstage failed (the script stays on tmpfs; harmless)"
  say "restored: every host equals its original record"
  return 0
}

if [ "$MODE" = restore ]; then
  restore_all
  exit $?
fi
[ "$MODE" = run ] || die "unknown mode $MODE" 2

case "$COMP" in
  gab*) ;;
  *) exec "$MAGE" BenchTier ;;
esac

# ---- a gab dispatch: the exact table entry or nothing
entry=$(awk -F'\t' -v c="$COMP" '/^#/ || NF < 4 {next} $1 == c {print; exit}' "$TABLE")
[ -n "$entry" ] || die "competitors=$COMP is not an exact line of $TABLE: no arms, nothing changed" 2
ARMS=$(printf '%s' "$entry" | cut -f2)
ARM_TMO=$(printf '%s' "$entry" | cut -f3)
NAME=$(printf '%s' "$entry" | cut -f4)
[ "${BENCH_TARGET:-}" = msa2-server ] || die "BENCH_TARGET must be msa2-server for the arms (got '${BENCH_TARGET:-}')" 2
case "$ARM_TMO" in ''|*[!0-9]*) die "bad per-arm timeout '$ARM_TMO' in $TABLE" 2 ;; esac
for a in $(printf '%s' "$ARMS" | tr ',' ' '); do
  case "$a" in as-found|performance|performance-c2off) ;; *) die "unknown arm '$a' in $TABLE" 2 ;; esac
done
if [ -e "$CP" ] && [ -n "$(ls -A "$CP" 2> /dev/null)" ]; then die "$CP is not empty: a stale record would be taken for this run's original" 2; fi
mkdir -p "$CP"
{
  echo "name	$NAME"
  echo "competitors	$COMP"
  echo "arms	$ARMS"
  echo "per_arm_timeout_min	$ARM_TMO"
  echo "table_sha256	$( (shasum -a 256 "$TABLE" 2> /dev/null || sha256sum "$TABLE") | awk '{print $1}')"
  echo "tag	$TAG"
} > "$CP/arms-plan.txt"
printf 'k\tarm\tstart\tend\tbenchtier_rc\tresults_dir\tpre\tpost\n' > "$CP/arms.tsv"

on_exit() {
  local rc=$?
  trap - EXIT
  say "exit path (rc=$rc): restoring from the original record"
  restore_all || { [ "$rc" -eq 0 ] && rc=1; }
  exit "$rc"
}
trap on_exit EXIT
trap 'exit 143' TERM
trap 'exit 130' INT

say "$NAME: arms $ARMS, ${ARM_TMO} min per BenchTier, tag $TAG"
ap record orig || die "cannot record the original state: nothing was changed"
STOPRULE=0
"$PY" -I -B "$CPPY" stop-rule --orig "$CP/orig/msa2-server.json" || STOPRULE=1
[ "$STOPRULE" = 1 ] && say "stop rule armed: an as-found arm may run, no arm that changes the state will"

k=0
STOPPED=0
narms=$(printf '%s' "$ARMS" | tr ',' '\n' | grep -c .)
for arm in $(printf '%s' "$ARMS" | tr ',' ' '); do
  k=$((k + 1))
  if [ "$STOPRULE" = 1 ] && [ "$arm" != as-found ]; then
    echo "STOP RULE: the original msa2-server record shows a performance governor: arm $k ($arm) and the rest are not run (the contrast would be empty)" > "$CP/STOP-RULE.txt"
    say "STOP RULE: stopping before arm $k ($arm)"
    STOPPED=1
    break
  fi
  say "arm $k/$narms: $arm"
  ap apply "pre-$k" "$arm" || die "arm $k ($arm) did not take on a host: no benchmark ran in this arm" 4
  [ "${GOVAB_SETTLE:-0}" -gt 0 ] && sleep "$GOVAB_SETTLE"
  before=$(ls -1 "$RES" 2> /dev/null | sort)
  start=$(date -u +%FT%TZ)
  "$TIMEOUT_CMD" --kill-after=120 --signal=TERM "${ARM_TMO}m" "$MAGE" BenchTier
  brc=$?
  end=$(date -u +%FT%TZ)
  ap record "post-$k" || say "warning: the record after arm $k failed"
  after=$(ls -1 "$RES" 2> /dev/null | sort)
  newdirs=$(comm -13 <(printf '%s\n' "$before") <(printf '%s\n' "$after") | grep -- '-bench-' || true)
  nnew=$(printf '%s' "$newdirs" | grep -c . || true)
  [ "$nnew" = 1 ] || say "warning: arm $k produced $nnew results directories ($newdirs): the reader will VOID this arm"
  printf '%s\t%s\t%s\t%s\t%s\t%s\tpre-%s\tpost-%s\n' "$k" "$arm" "$start" "$end" "$brc" "$(printf '%s' "$newdirs" | tr '\n' ',')" "$k" "$k" >> "$CP/arms.tsv"
  [ "$brc" -eq 0 ] || die "BenchTier of arm $k ($arm) exited $brc" "$brc"
done
[ "$STOPPED" = 1 ] && exit 3
exit 0
