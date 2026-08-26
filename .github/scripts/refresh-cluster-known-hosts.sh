#!/usr/bin/env bash
# Build a PER-RUN SSH known_hosts for the bench cluster.
#
# Why this exists (2026-08-19): msa2-client was rebuilt onto a new SSD and so
# presents a new host key. The inventory sets StrictHostKeyChecking=accept-new,
# which accepts UNKNOWN hosts but correctly REFUSES CHANGED ones — so every run
# after the rebuild died with:
#
#     fatal: [msa2-client]: UNREACHABLE!
#     Host key verification failed.
#
# while the other two hosts passed. The symptom is badly misleading: a perfectly
# healthy machine looks dead. The stale entry lives in the persistent
# ~/.ssh/known_hosts of whichever host ran the job, and because Ubuntu hashes
# known_hosts by default `grep` finds nothing — only `ssh-keygen -F` reveals it.
#
# Rather than mutate the operator's persistent known_hosts, scan the cluster
# fresh into a file under the runner root (on /tmp, i.e. tmpfs, discarded on
# reboot) and point ansible at that. A rebuilt node is picked up automatically
# and host-key checking still applies WITHIN the run.
#
# Host list comes from ansible/inventory.yml — one source of truth for topology.
#
# Portability: no `mapfile`, no nested heredocs. macOS ships bash 3.2 and this
# script must stay testable on the dev machine, not just on the Linux runners.
set -euo pipefail

INVENTORY="${1:-ansible/inventory.yml}"
KH="${2:?usage: refresh-cluster-known-hosts.sh <inventory> <known_hosts path>}"

TARGETS=$(python3 -c '
import sys, yaml
doc = yaml.safe_load(open(sys.argv[1]))
out = []
def walk(node):
    for name, v in (node.get("hosts") or {}).items():
        out.append(name)
        if v and v.get("lan_ip"):
            out.append(str(v["lan_ip"]))
    for child in (node.get("children") or {}).values():
        walk(child or {})
walk(doc["all"])
seen = set()
for t in out:
    if t not in seen:
        seen.add(t)
        print(t)
' "$INVENTORY")

if [ -z "$TARGETS" ]; then
  echo "refresh-cluster-known-hosts: no hosts parsed from $INVENTORY" >&2
  exit 1
fi

mkdir -p "$(dirname "$KH")"
: > "$KH"
chmod 600 "$KH"

echo "refresh-cluster-known-hosts: scanning $(echo "$TARGETS" | wc -w | tr -d ' ') target(s) -> $KH"
for t in $TARGETS; do
  # -T 5 bounds an offline host to 5s. A host that is genuinely down is NOT
  # fatal here: ansible then reports a real UNREACHABLE with a real reason,
  # which is far more diagnosable than a host-key mismatch.
  if ssh-keyscan -T 5 "$t" 2>/dev/null >> "$KH"; then
    printf '  %-16s ok\n' "$t"
  else
    printf '  %-16s no response (will surface as a real UNREACHABLE, not a key mismatch)\n' "$t"
  fi
done

echo "refresh-cluster-known-hosts: $(wc -l < "$KH" | tr -d ' ') key line(s) written"
