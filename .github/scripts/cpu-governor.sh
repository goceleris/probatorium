#!/usr/bin/env bash
# cpu-governor.sh: put the CPU frequency governor of the hosts of a release run on `performance` before the run starts, RECORD it, and put it back afterwards.
# Two steps of a cluster workflow call it (benchmark-tier.yml, matrix-race-tier.yml); the measured steps (mage BenchTier, mage Validate ...) are NOT touched or wrapped.
#
#   cpu-governor.sh set      after `mage Deploy`, before the sampler and the bench. No `continue-on-error`: a host whose governor did not take (read back, every CPU) fails the job
#                            BEFORE anything is measured. Records the ORIGINAL state of every host of the run (write-once, orig/<host>.json: scaling driver, governor, EPP, limits,
#                            frequency, boost, cpuidle driver / governor and every idle state with its disable flag), then, for CPU_GOVERNOR=performance, writes `performance` on every
#                            CPU that has a governor (a CPU already on it is not written) and records the result (applied/<host>.json, applied/<host>.apply.json).
#                            CPU_GOVERNOR=keep records only and changes nothing. Idle states are never written (left AS FOUND, recorded).
#   cpu-governor.sh restore  `if: always()` after the measured step, before the sampler stop, the upload and `mage Cleanup`: writes every recorded value back (governor first, then EPP,
#                            limits, idle flags), reads it back, records final/<host>.json and COMPARES it with orig/<host>.json: any difference fails the job (restore-status.txt =
#                            NOT-RESTORED or NOT-EQUAL). A no-op when nothing was recorded (the set step never ran) or it already restored. keep: records final, restore-status.txt =
#                            KEPT-EQUAL | KEPT-CHANGED | KEPT-UNVERIFIED (never fails: nothing was changed by this run).
#
# The hosts are the run's own: BENCH_TARGET msa2-server|msr1 = that host plus the loadgen msa2-client (the ansible --limit of mage Deploy, clusterLimitForTarget), both / unset = every
# cluster host. Records and verdict are collected in $RUNNER_TEMP/cpu-power and copied to results/cpu-power (uploaded with the results) by the restore step:
#   orig/ applied/ final/ (<host>.json = the record, <host>.apply.json = what the change did and read back), mode.txt, summary.txt (one line per host and label: the governor / driver /
#   idle setting the run was measured with), restore-status.txt (RESTORED-EQUAL | KEPT-EQUAL | ...). Test hooks: ANSIBLE, PY, CPUGOV_DIR, CPUGOV_RESULTS, CPUGOV_ANSIBLE_DIR.
# shellcheck disable=SC2310  # a function's status is read on purpose in `||` lists
set -u
MODE=${1:-}
GOV=${CPU_GOVERNOR:-performance}
ANSIBLE=${ANSIBLE:-ansible-playbook}
PY=${PY:-python3}
ADIR=${CPUGOV_ANSIBLE_DIR:-$PWD/ansible}
CPPY=$ADIR/files/cpu_power.py
DIR=${CPUGOV_DIR:-${RUNNER_TEMP:-/tmp}/cpu-power}
RES=${CPUGOV_RESULTS:-$PWD/results/cpu-power}
TAG=${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-1}

say() { echo "cpu-governor: $*"; }
die() { say "FAIL: $*"; exit "${2:-1}"; }

case "$GOV" in performance | keep) ;; *) die "CPU_GOVERNOR must be performance or keep, got '$GOV'" 2 ;; esac

limit() { # the ansible --limit of this run: the bench target plus the loadgen, or nothing (every cluster host)
  case "${BENCH_TARGET:-}" in
    "" | both | all) echo "" ;;
    *) echo "${BENCH_TARGET},msa2-client" ;;
  esac
}

ap() { # ap <action> <label> [arm]: the cpu-power playbook on the hosts of this run
  local extra=() lim
  [ -n "${3:-}" ] && extra=(-e "cpu_power_arm=$3")
  [ "${CLUSTER_USE_LAN:-}" = 1 ] && extra+=(-e use_lan=true)
  lim=$(limit)
  [ -n "$lim" ] && extra+=(--limit "$lim")
  (cd "$ADIR" && "$ANSIBLE" -i inventory.yml cpu-power.yml -e "cpu_power_action=$1" -e "cpu_power_label=$2" -e "cpu_power_dir=$DIR" -e "cpu_power_tag=$TAG" ${extra[@]+"${extra[@]}"})
}

summarize() { # one line per host and label in summary.txt: what each host ran on
  local label=$1 f h
  for f in "$DIR/$label"/*.json; do
    [ -e "$f" ] || continue
    case "$f" in *.apply.json) continue ;; esac
    h=$(basename "$f" .json)
    printf '%s\t%s\t%s\n' "$h" "$label" "$("$PY" -I -B "$CPPY" summary --rec "$f" 2> /dev/null || echo unreadable)" >> "$DIR/summary.txt"
  done
}

collect() { # copy the records to the results directory the job uploads; never fails the step
  mkdir -p "$RES" 2> /dev/null && cp -R "$DIR/." "$RES/" 2> /dev/null || say "warning: could not copy $DIR to $RES"
}

verdict() { echo "$1" > "$DIR/restore-status.txt"; }

if [ "$MODE" = set ]; then
  if [ -e "$DIR" ] && [ -n "$(ls -A "$DIR" 2> /dev/null)" ]; then die "$DIR is not empty: a stale record would be taken for this run's original" 2; fi
  mkdir -p "$DIR"
  {
    echo "governor	$GOV"
    echo "target	${BENCH_TARGET:-both}"
    echo "limit	$(limit)"
    echo "tag	$TAG"
    echo "started	$(date -u +%FT%TZ)"
  } > "$DIR/mode.txt"
  say "mode $GOV, hosts: ${BENCH_TARGET:-every cluster host}$([ -n "$(limit)" ] && echo " (limit $(limit))"), tag $TAG"
  ap record orig || die "cannot record the original state: nothing was changed"
  summarize orig
  if [ "$GOV" = performance ]; then
    ap apply applied performance || {
      summarize applied
      die "the performance governor did not take on a host (read back): nothing was measured; the restore step puts every recorded value back" 4
    }
    summarize applied
  fi
  cat "$DIR/summary.txt"
  exit 0
fi

if [ "$MODE" = restore ]; then
  mkdir -p "$DIR"
  if [ -f "$DIR/restore-status.txt" ] && grep -qE '^(RESTORED-EQUAL|KEPT-)' "$DIR/restore-status.txt"; then
    say "already done: $(cat "$DIR/restore-status.txt")"
    collect
    exit 0
  fi
  if [ ! -d "$DIR/orig" ] || [ -z "$(ls -A "$DIR/orig" 2> /dev/null)" ]; then
    verdict "NOTHING-RECORDED $(date -u +%FT%TZ): no original record, so nothing was changed by this run (the set step did not run or failed before recording)"
    say "no original record: nothing was changed, nothing to restore"
    collect
    exit 0
  fi
  mode=$(awk -F'\t' '$1 == "governor" {print $2; exit}' "$DIR/mode.txt" 2> /dev/null)
  rc=0
  if [ "$mode" = keep ]; then
    ap record final || rc=$?
  else
    ap restore final || rc=$?
  fi
  summarize final
  cmp_rc=0
  if [ "$rc" -eq 0 ]; then
    for f in "$DIR"/orig/*.json; do
      h=$(basename "$f" .json)
      "$PY" -I -B "$CPPY" compare --a "$f" --b "$DIR/final/$h.json" || cmp_rc=1
    done
  fi
  stamp=$(date -u +%FT%TZ)
  if [ "$mode" = keep ]; then
    if [ "$rc" -ne 0 ]; then
      verdict "KEPT-UNVERIFIED $stamp: the final record could not be taken (rc=$rc); this run changed nothing"
    elif [ "$cmp_rc" -ne 0 ]; then
      verdict "KEPT-CHANGED $stamp: a host differs from its original record at the end (this run changed nothing; read orig/ and final/)"
      echo "::warning::cpu-governor keep: a host's governor/idle state changed during the run (see results/cpu-power)"
    else
      verdict "KEPT-EQUAL $stamp"
    fi
    ap unstage x > /dev/null 2>&1 || true
    collect
    exit 0
  fi
  if [ "$rc" -ne 0 ]; then
    verdict "NOT-RESTORED $stamp: the restore play failed (rc=$rc); a host may be left changed: read final/ and the next survey"
    say "NOT RESTORED (rc=$rc)"
    ap unstage x > /dev/null 2>&1 || true
    collect
    exit 1
  fi
  if [ "$cmp_rc" -ne 0 ]; then
    verdict "NOT-EQUAL $stamp: a host differs from its original record after the restore (see the lines above)"
    say "LEFT BEHIND: the final record differs from the original"
    ap unstage x > /dev/null 2>&1 || true
    collect
    exit 1
  fi
  verdict "RESTORED-EQUAL $stamp"
  ap unstage x > /dev/null 2>&1 || say "warning: unstage failed (the script stays on tmpfs; harmless)"
  say "restored: every host equals its original record"
  cat "$DIR/summary.txt"
  collect
  exit 0
fi

die "usage: cpu-governor.sh set|restore" 2
