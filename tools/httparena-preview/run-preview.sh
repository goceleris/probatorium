#!/usr/bin/env bash
# The measured part. Run from the HttpArena clone root (host_adapt.py + stage-images.sh done, preview.env written,
# this preview/ directory copied into the clone):   cd HttpArena && bash preview/run-preview.sh <phase>
#   smoke   one short celeris pass (baseline, h2c, ws, async-db), ~10 min: proves cpusets and the load generators work
#   clear   PHASE 1: cleartext profiles, celeris then the four Go references            (no TLS row can start here)
#   async   PHASE 1b: the 32,000-connection profile; runs when the host's ip_local_port_range is already 1024-65535 (no sysctl), else only with PORTRANGE_OK=1 (one recorded, restored sysctl), else skipped
#   engines ENGINE-VARIANT PASS: celeris (default Adaptive) vs celeris-epoll vs celeris-iouring on the cleartext + WebSocket profiles (+ async by the same port-range rule); no TLS
#   supp    PHASE 2: profile-limited Go supplements (go-stdlib h2c, fiber-websocket ws)
#   tls     PHASE 3: the TLS side (json-tls, 8gbit; celeris and go-stdlib also baseline-h2, static-h2)
#   governor-set      set the CPU governor to performance (+EPP where present), record the original in logs/cpufreq-original.tsv,
#                     read back; exit 1 if it did not take (nothing may be benchmarked then). Default for the full preview; smoke keeps the host as is.
#   governor-restore  write the recorded governor/EPP back and verify (run it in an EXIT trap / an always() step)
#   score   score tables (both pictures) from results/
# Every call logs start/stop times to logs/<phase>.log and one log per entry under logs/.
set -uo pipefail
HA=${HA:-$PWD}; cd "$HA" || exit 2; source ./preview.env; mkdir -p logs
export ZRK_RATE_LATENCY_1M=${ZRK_RATE_LATENCY_1M:-125000}   # 1M req/s x (8 server CPUs / 64): a 16-CPU host cannot offer 1M; same for every entry
export ZRK_RATE_8GBIT=${ZRK_RATE_8GBIT:-6250}              # 50K req/s x 1/8
export ZRK_RATE_LATENCY_10K=${ZRK_RATE_LATENCY_10K:-10000} # near-idle profile: the default
CLEAR=baseline,pipelined,limited-conn,latency-1m,latency-10k,json-comp,async-db,fortunes,baseline-h2c,json-h2c,echo-ws,echo-ws-pipeline,echo-ws-limited
REFS=${REFS:-"gin fiber echo go-fasthttp"}
export TZ=UTC
# Every line of an entry's log gets an HH:MM:SS UTC prefix, so the harness's "[run k/N]" markers can be lined up with the
# container's own log (engine switches) afterwards.
bench() { local fw=$1 list=$2 log=$3 rc
  echo "$(date -u +%FT%TZ) START $fw $list" | tee -a "logs/$log.log"
  ./scripts/benchmark.sh "$fw" "$list" --save 2>&1 | { while IFS= read -r l; do printf '%(%H:%M:%S)T %s\n' -1 "$l"; done; } >> "logs/$log.$fw.log"
  rc=${PIPESTATUS[0]}
  echo "$(date -u +%FT%TZ) STOP  $fw rc=$rc" | tee -a "logs/$log.log"; }
# 32,000 connections from one source address need a wide ephemeral range. Returns 0 when async may run: the range is already
# 1024-65535 (nothing is touched, whatever PORTRANGE_OK says), or PORTRANGE_OK=1 and the one sysctl took (recorded, restored by
# the EXIT trap, shown restored by capture-state.sh). Returns 1 (skip) otherwise.
portrange_prepare() {
  local old now lo hi
  old=$(cat /proc/sys/net/ipv4/ip_local_port_range); echo "$(date -u +%FT%TZ) port range was: $old" | tee -a logs/async.log
  now=$(echo "$old" | tr -s '[:space:]' ' ' | sed 's/ $//'); read -r lo hi <<< "$now"
  if [ "${lo:-99999}" -le 1024 ] && [ "${hi:-0}" -ge 65535 ]; then echo "port range already wide: no sysctl touched" | tee -a logs/async.log; return 0; fi
  if [ "${PORTRANGE_OK:-0}" != 1 ]; then echo "async skipped: port range is '$now' and PORTRANGE_OK is not 1" | tee -a logs/async.log; return 1; fi
  PORT_OLD="$old"
  trap 'sudo -n sysctl -w net.ipv4.ip_local_port_range="$PORT_OLD" >/dev/null; echo "$(date -u +%FT%TZ) port range restored: $(cat /proc/sys/net/ipv4/ip_local_port_range)" | tee -a logs/async.log' EXIT
  sudo -n sysctl -w net.ipv4.ip_local_port_range='1024 65535' >/dev/null 2>&1
  now=$(tr -s '[:space:]' ' ' < /proc/sys/net/ipv4/ip_local_port_range | sed 's/ $//')
  if [ "$now" != "1024 65535" ]; then echo "async skipped: port range is '$now' after sudo -n sysctl (no passwordless sudo?)" | tee -a logs/async.log; return 1; fi
}
ENGINES=${ENGINES:-"celeris celeris-epoll celeris-iouring"}
phase=${1:?phase}
case "$phase" in
  smoke) RUNS=1 DURATION=3s bench celeris baseline,pipelined,json-comp,latency-10k,baseline-h2c,echo-ws,async-db,fortunes,json-tls,8gbit,baseline-h2,static-h2 smoke   # covers every load generator: gcannon, h2load, wrk (json-tls), zrk (latency-10k, 8gbit)
         # what the cpusets actually were (benchmark.sh logs the DB split; the profile table sets the server's)
         grep -h -E "cpus: server|cpuset|available CPUs|pinned" logs/smoke.celeris.log | tee -a logs/smoke.log ;;
  clear) bench celeris "$CLEAR" clear; for fw in $REFS; do bench "$fw" "$CLEAR" clear; done ;;
  async) if portrange_prepare; then bench celeris async async; for fw in $REFS; do bench "$fw" async async; done; fi ;;
  engines) for fw in $ENGINES; do bench "$fw" "$CLEAR" engines; done
         if portrange_prepare; then for fw in $ENGINES; do bench "$fw" async engines-async; done; fi ;;
  supp)  bench go-stdlib baseline-h2c,json-h2c supp; bench fiber-websocket echo-ws,echo-ws-pipeline,echo-ws-limited supp ;;
  tls)   bench celeris json-tls,8gbit,baseline-h2,static-h2 tls; for fw in $REFS; do bench "$fw" json-tls,8gbit tls; done
         bench go-stdlib baseline-h2,static-h2 tls ;;
  score) ENGARGS=""
         if [ -d results/baseline/4096 ] && [ -f results/baseline/4096/celeris-epoll.json ]; then   # an engine-variant pass ran: add its section (the three views are unchanged)
           ENGARGS="--engines celeris,celeris-epoll,celeris-iouring --container-logs site/static/logs --bench-logs logs/engines.celeris.log,logs/engines-async.celeris.log"; fi
         python3 -I -B preview/score_preview.py --results results --hepa . $ENGARGS \
           --entries celeris,gin,fiber,echo,go-fasthttp,fiber-websocket,go-stdlib --merge fiber-websocket=fiber \
           --rates latency-1m=$ZRK_RATE_LATENCY_1M,latency-10k=$ZRK_RATE_LATENCY_10K,8gbit=$ZRK_RATE_8GBIT \
           --tls-profiles json-tls,8gbit,baseline-h2,static-h2 | tee logs/score.txt ;;
  governor-set)     bash "$(dirname "$0")/governor.sh" set "$HA/logs/cpufreq-original.tsv" ;;
  governor-restore) bash "$(dirname "$0")/governor.sh" restore "$HA/logs/cpufreq-original.tsv" ;;
  *) echo "unknown phase $phase"; exit 2 ;;
esac
