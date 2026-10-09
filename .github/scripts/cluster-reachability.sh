#!/usr/bin/env bash
# Is every cluster host reachable? Retry with backoff for a BOUNDED time, so a
# short outage does not turn a teardown into a no-op.
#
#   cluster-reachability.sh wait    retry until all hosts answer or the budget
#                                   is spent; never fails; reports through
#                                   $GITHUB_OUTPUT (unreachable, attempts, waited)
#   cluster-reachability.sh probe   one attempt; exit 1 if any host is silent
#
# WHY (bench 37226284351, 2026-10-06). A site uplink outage hit all three hosts
# at once. The teardown ran into it, every task printed `UNREACHABLE ...
# ...ignoring`, the play recap read `unreachable=0 ... ignored=6`, and the job
# reported SUCCESS with three idle online runners and an orphan SUT left behind.
# `ignore_unreachable` is right for the reclamation plays (one dead host must not
# stop the others) but it also makes ansible's exit status useless as a signal.
# This script is the signal: it asks the hosts directly, using a probe that is
# NOT subject to ignore_unreachable, and waits for them.
#
# NORMAL RUNS PAY ONE PING: when every host answers on the first attempt there
# is no sleep and no retry (a few seconds for ansible to connect).
#
# BOUNDED: PROBE_BUDGET_SECONDS caps attempts plus sleeps (sleeps are counted by
# the delay requested, attempts by the clock), PROBE_ATTEMPT_TIMEOUT caps one
# attempt. The caller's job timeout stays the backstop.
#
# Env:
#   INVENTORY_PATH          ansible/inventory.yml
#   PROBE_BUDGET_SECONDS    180
#   PROBE_DELAYS            "5 10 20 30 45 60"  (the last one repeats)
#   PROBE_ATTEMPT_TIMEOUT   60
#   ANSIBLE_BIN, SLEEP_BIN  ansible, sleep (tests substitute fakes)
set -uo pipefail

mode=${1:-}
case "$mode" in wait | probe) ;; *)
	echo "usage: cluster-reachability.sh wait|probe" >&2
	exit 2
	;;
esac

inventory=${INVENTORY_PATH:-ansible/inventory.yml}
budget=${PROBE_BUDGET_SECONDS:-180}
delays=${PROBE_DELAYS:-5 10 20 30 45 60}
attempt_timeout=${PROBE_ATTEMPT_TIMEOUT:-60}
ansible_bin=${ANSIBLE_BIN:-ansible}
sleep_bin=${SLEEP_BIN:-sleep}

# Short, deliberate SSH bounds, as ansible/forensics.yml: the inventory's own
# tuning is patient on purpose (a saturated LIVE host must not be dropped) and
# would make every attempt against a dead host cost a minute.
ssh_args='-o ControlMaster=no -o ControlPath=none -o ConnectTimeout=10 -o ServerAliveInterval=5 -o ServerAliveCountMax=3 -o BatchMode=yes -o StrictHostKeyChecking=accept-new'
extra=$(printf '{"ansible_ssh_common_args": "%s", "ansible_ssh_retries": 0}' "$ssh_args")

all_hosts=$("$ansible_bin" cluster -i "$inventory" --list-hosts 2>/dev/null | awk 'NR > 1 && NF == 1 { print $1 }')
if [ -z "$all_hosts" ]; then
	echo "::error title=Cluster reachability::cannot list the hosts of group 'cluster' from $inventory"
	exit 2
fi

# attempt -> sets reached (space separated) and silent (comma separated)
attempt() {
	local out
	# `timeout` is coreutils: on a machine without it (macOS, running the tests)
	# the attempt is simply unbounded, as ssh's own ConnectTimeout still bounds it.
	local bound=()
	command -v timeout >/dev/null 2>&1 && bound=(timeout -k 5 "$attempt_timeout")
	out=$(${bound[@]+"${bound[@]}"} "$ansible_bin" cluster -i "$inventory" -m ansible.builtin.ping -o -e "$extra" 2>&1)
	reached=$(printf '%s\n' "$out" | awk -F' [|] ' '$2 ~ /^SUCCESS/ { print $1 }')
	silent=""
	local h
	for h in $all_hosts; do
		case " $(echo "$reached" | tr '\n' ' ') " in
		*" $h "*) ;;
		*) silent="${silent:+$silent,}$h" ;;
		esac
	done
}

emit() { # unreachable attempts waited
	echo "unreachable=$1"
	echo "attempts=$2"
	echo "waited=$3"
}

n=0
spent=0
di=0
read -r -a darr <<<"$delays"
dcount=${#darr[@]}
while :; do
	n=$((n + 1))
	t0=$SECONDS
	attempt
	spent=$((spent + SECONDS - t0))
	if [ -z "$silent" ]; then
		echo "cluster reachability: all hosts answered (attempt $n, ${spent}s)"
		[ -n "${GITHUB_OUTPUT:-}" ] && emit "" "$n" "$spent" >>"$GITHUB_OUTPUT"
		exit 0
	fi
	if [ "$mode" = probe ]; then
		echo "cluster reachability: no answer from $silent"
		exit 1
	fi
	di=$((di + 1))
	[ "$di" -gt "$dcount" ] && di=$dcount
	delay=${darr[$((di - 1))]}
	if [ $((spent + delay)) -gt "$budget" ]; then
		echo "::warning title=Cluster hosts unreachable::no answer from $silent after $n attempt(s) over ${spent}s (budget ${budget}s). Teardown continues on the hosts that answer; the final verdict step will fail the job."
		[ -n "${GITHUB_OUTPUT:-}" ] && emit "$silent" "$n" "$spent" >>"$GITHUB_OUTPUT"
		exit 0
	fi
	echo "::notice title=Cluster hosts unreachable::attempt $n: no answer from $silent; retrying in ${delay}s (${spent}s of ${budget}s spent)"
	"$sleep_bin" "$delay"
	spent=$((spent + delay))
done
