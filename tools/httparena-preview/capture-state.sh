#!/usr/bin/env bash
# Record the state of the host so a run can be shown to leave nothing behind.
#   capture-state.sh before <dir>      (also prints the start time)
#   capture-state.sh after  <dir>      (writes <dir>/after, prints the diffs against <dir>/before)
# Everything is read-only. Run on msa2-server (or inside the workflow's host job).
set -u
phase=${1:?before|after}; D=${2:?dir}; mkdir -p "$D/$phase"; O="$D/$phase"
{ date -u +%FT%TZ; hostname; uname -r; nproc; } > "$O/when.txt"
docker ps -a --format '{{.ID}} {{.Names}} {{.Image}}' | sort > "$O/docker-ps-a.txt" 2>&1
docker images --format '{{.Repository}}:{{.Tag}} {{.ID}}' | sort > "$O/docker-images.txt" 2>&1
docker volume ls -q | sort > "$O/docker-volumes.txt" 2>&1
docker network ls --format '{{.Name}}' | sort > "$O/docker-networks.txt" 2>&1
docker system df > "$O/docker-system-df.txt" 2>&1
# command lines only (pids differ). The diff below fails only on processes a run can leave: load generators, servers, postgres.
ps -eo pid,ppid,user,comm,args --sort=pid | awk '{ $1=""; $2=""; print }' | grep -E 'gcannon|h2load|wrk |zrk|httparena|postgres|benchmark\.sh|run-preview|/server|docker run' | grep -v -E 'capture-state|grep' | sort > "$O/ps.txt"
# volatile counters are not tunables: drop them so a diff means a changed setting
sysctl -a 2>/dev/null | grep -v -E 'file-nr|dentry-state|inode-nr|inode-state|^kernel\.random\.|entropy|uuid|boot_id|nf_conntrack_count|ns_last_pid|pty\.nr|^vm\.stat|^fs\.aio-nr|^net\.netfilter\.nf_conntrack_count' | sort > "$O/sysctl-a.txt"
cat /proc/mounts | sort > "$O/mounts.txt"
( ss -lntup 2>/dev/null || netstat -lntup 2>/dev/null ) | awk '{print $1,$5,$7}' | sort > "$O/listening.txt"
ls -la /tmp | awk '{print $1,$3,$9}' | sort > "$O/tmp-ls.txt"
ls -d /tmp/* 2>/dev/null | sort > "$O/tmp-names.txt"
df -k / /tmp 2>/dev/null > "$O/df.txt"
cat /sys/devices/system/cpu/cpu0/cpufreq/scaling_governor > "$O/governor.txt" 2>/dev/null
cat /proc/sys/net/ipv4/ip_local_port_range > "$O/port-range.txt" 2>/dev/null   # ONE line: the workflow restores from it
ip -o link show 2>/dev/null | awk '{print $2,$3,$9}' | sort > "$O/links.txt"
free -b > "$O/free.txt"
if [ "$phase" = after ]; then
  rc=0
  for f in docker-ps-a docker-images docker-volumes docker-networks ps sysctl-a mounts listening tmp-names port-range links; do
    if ! diff -u "$D/before/$f.txt" "$O/$f.txt" > "$O/diff-$f.txt" 2>&1; then
      echo "DIFF in $f (see $O/diff-$f.txt):"; head -20 "$O/diff-$f.txt"; rc=1
    fi
  done
  [ $rc = 0 ] && echo "NOTHING LEFT BEHIND: docker, ps, sysctl, mounts, listening ports, /tmp, port range, links all equal to before"
  echo "images pulled/built by the run (remove with docker rmi, listed in diff-docker-images.txt):"; grep '^+' "$O/diff-docker-images.txt" | grep -v '^+++' || true
  exit $rc
fi
echo "state captured in $O (start $(cat "$O/when.txt" | head -1))"
