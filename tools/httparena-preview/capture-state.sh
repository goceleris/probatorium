#!/usr/bin/env bash
# Record the state of the host so a run can be shown to leave nothing behind.
#   capture-state.sh before <dir>                       (records; prints the start time)
#   capture-state.sh after  <dir> [images-baseline.ids]  (records; diffs against <dir>/before; exit 1 on a difference)
# Everything is read-only. Run on msa2-server (or inside the workflow's host job).
#
# What is compared (and what is deliberately not):
#   compared   containers, processes a run can leave (load generators, servers, postgres), listening sockets, volumes,
#              networks, /tmp names, links, mounts (except Docker's own netns mounts), the REGISTERED sysctl knobs below,
#              the per-CPU governor / scaling driver / EPP (cpufreq.txt), loopback MTU and THP: the settings HttpArena's tuner or this run could touch.
#   images     compared against a fixed BASELINE of image IDs (images-baseline.ids, committed beside this script), not against
#              the before-capture: a leftover from an earlier run must not become the next run's "original".
#   not compared  the full `sysctl -a` (kept in sysctl-a.txt for the record: it holds volatile counters such as fs.quota.*),
#              free/df, the build cache (the legacy builder keeps none: docker-system-df.txt shows it).
set -u
phase=${1:?before|after}; D=${2:?dir}; BASE=${3:-}; mkdir -p "$D/$phase"; O="$D/$phase"
KNOBS="net.ipv4.ip_local_port_range net.ipv4.ip_local_reserved_ports net.ipv4.tcp_tw_reuse net.ipv4.tcp_fin_timeout net.ipv4.tcp_max_syn_backlog net.ipv4.tcp_syncookies net.core.somaxconn net.core.netdev_max_backlog net.core.rmem_max net.core.wmem_max net.core.rmem_default net.core.wmem_default net.netfilter.nf_conntrack_max fs.nr_open fs.file-max vm.max_map_count vm.swappiness vm.overcommit_memory kernel.pid_max kernel.threads-max"
{ date -u +%FT%TZ; hostname; uname -r; nproc; } > "$O/when.txt"
docker ps -a --format '{{.ID}} {{.Names}} {{.Image}}' | sort > "$O/docker-ps-a.txt" 2>&1
docker images --format '{{.Repository}}:{{.Tag}} {{.ID}}' | sort > "$O/docker-images.txt" 2>&1
docker images -q | sort -u > "$O/docker-image-ids.txt" 2>&1
docker volume ls -q | sort > "$O/docker-volumes.txt" 2>&1
docker network ls --format '{{.Name}}' | sort > "$O/docker-networks.txt" 2>&1
docker system df > "$O/docker-system-df.txt" 2>&1
# command lines only (pids differ). The diff below fails only on processes a run can leave: load generators, servers, postgres.
ps -eo pid,ppid,user,comm,args --sort=pid | awk '{ $1=""; $2=""; print }' | grep -E 'gcannon|h2load|wrk |zrk|httparena|postgres|benchmark\.sh|run-preview|/server|docker run' | grep -v -E 'capture-state|grep' | sort > "$O/ps.txt"
sysctl -a 2>/dev/null | sort > "$O/sysctl-a.txt"          # the record; NOT diffed (volatile counters)
{ for k in $KNOBS; do printf '%s = %s\n' "$k" "$(sysctl -n "$k" 2>/dev/null | tr -s '[:space:]' ' ')"; done
  printf 'governors = %s\n' "$(cat /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor 2>/dev/null | sort | uniq -c | tr -s '[:space:]' ' ')"
  printf 'lo_mtu = %s\n' "$(cat /sys/class/net/lo/mtu 2>/dev/null)"
  printf 'thp = %s\n' "$(cat /sys/kernel/mm/transparent_hugepage/enabled 2>/dev/null)"; } > "$O/sysctl-knobs.txt"
grep -v -E '^nsfs /run/docker/netns/' /proc/mounts | sort > "$O/mounts.txt"      # Docker's own netns mounts appear after any container ran
( ss -lntup 2>/dev/null || netstat -lntup 2>/dev/null ) | awk '{print $1,$5,$7}' | sort > "$O/listening.txt"
ls -la /tmp | awk '{print $1,$3,$9}' | sort > "$O/tmp-ls.txt"
ls -d /tmp/* 2>/dev/null | sort > "$O/tmp-names.txt"
df -k / /tmp 2>/dev/null > "$O/df.txt"
cat /proc/sys/net/ipv4/ip_local_port_range > "$O/port-range.txt" 2>/dev/null   # ONE line: the workflow restores from it
ip -o link show 2>/dev/null | awk '{print $2,$3,$9}' | sort > "$O/links.txt"
free -b > "$O/free.txt"
# per-CPU governor, scaling_driver and EPP (+ pstate mode, boost): registered knobs, compared after the run (governor.sh record is read-only)
bash "$(dirname "$0")/governor.sh" record "$O/cpufreq.txt" 2>/dev/null || : > "$O/cpufreq.txt"
if [ "$phase" = after ]; then
  rc=0
  for f in docker-ps-a docker-volumes docker-networks ps sysctl-knobs cpufreq mounts listening tmp-names port-range links; do
    if ! diff -u "$D/before/$f.txt" "$O/$f.txt" > "$O/diff-$f.txt" 2>&1; then
      echo "DIFF in $f (see $O/diff-$f.txt):"; head -20 "$O/diff-$f.txt"; rc=1
    fi
  done
  if [ -n "$BASE" ] && [ -f "$BASE" ]; then
    grep -v -E '^\s*(#|$)' "$BASE" | awk '{print $1}' | sort -u > "$O/images-baseline.sorted"
    if ! diff -u "$O/images-baseline.sorted" "$O/docker-image-ids.txt" > "$O/diff-docker-images.txt" 2>&1; then
      echo "DIFF in docker images vs the baseline (see $O/diff-docker-images.txt):"; head -20 "$O/diff-docker-images.txt"; rc=1
    fi
  fi
  [ $rc = 0 ] && echo "NOTHING LEFT BEHIND: containers, processes, listening ports, volumes, networks, /tmp, mounts, sysctl knobs, governor, port range, links, images (vs baseline)"
  exit $rc
fi
echo "state captured in $O (start $(head -1 "$O/when.txt"))"
