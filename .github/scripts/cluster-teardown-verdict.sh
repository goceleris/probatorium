#!/usr/bin/env bash
# The teardown's verdict: did it actually tear the cluster down? FAIL LOUDLY if not.
#
# WHY. Bench 37226284351's teardown job reported success while every host was
# UNREACHABLE: the reclamation plays use ignore_unreachable (so one dead host
# cannot stop the others), which turns an unreachable host into `ignored` and the
# play's exit status into 0. Nothing downstream looked at the hosts, so three
# runners stayed online and idle for days and the orphan SUT stayed on :8080.
#
# This reads what the steps left behind and fails the job, with an annotation
# that names the hosts, when ANY of these holds:
#
#   1. a host was still silent after the bounded retry at the start of the
#      teardown (UNREACHABLE_AT_START, from cluster-reachability.sh wait);
#   2. a reclamation play logged `UNREACHABLE!` for a host mid-teardown
#      (every *.log under LOG_DIR; the plays tee their output there);
#   3. the cancel-path SUT stop (stop-sut.log) FAILED on a host: a process
#      survived SIGKILL, or the host dropped mid-play;
#   4. no removal token could be minted, so no runner could be de-registered;
#   5. GitHub still lists an ONLINE runner with the cluster label. Ephemeral
#      runners and `config.sh remove` take a moment to disappear, so this is
#      retried a few times before it counts. This one does not depend on any
#      host answering, and is the actual symptom left by 37226284351.
#
# A normal run passes all four without a single retry or sleep: one `gh api`
# call (~1 s) is its whole cost.
#
# Env:
#   UNREACHABLE_AT_START   csv, from the wait step           ("")
#   LOG_DIR                directory of tee'd ansible logs   (teardown-logs)
#   REMOVAL_TOKEN_MINTED   "true"; anything else, empty included, fails (false)
#   GH_BIN, GH_TOKEN, REPO the gh CLI, a PAT, owner/name
#   RUNNER_LABEL           celeris-cluster
#   ONLINE_RETRIES         4         ONLINE_DELAY  5 (seconds)
#   SLEEP_BIN              sleep
set -uo pipefail

log_dir=${LOG_DIR:-teardown-logs}
label=${RUNNER_LABEL:-celeris-cluster}
gh_bin=${GH_BIN:-gh}
sleep_bin=${SLEEP_BIN:-sleep}
retries=${ONLINE_RETRIES:-4}
delay=${ONLINE_DELAY:-5}

problems=()
add_unique() { # csv-accumulator helper for host names
	case ",$hosts," in *",$1,"*) ;; *) hosts="${hosts:+$hosts,}$1" ;; esac
}

hosts=""
start_silent=${UNREACHABLE_AT_START:-}
if [ -n "$start_silent" ]; then
	IFS=',' read -r -a arr <<<"$start_silent"
	for h in "${arr[@]}"; do add_unique "$h"; done
fi
if [ -d "$log_dir" ]; then
	while IFS= read -r h; do
		[ -n "$h" ] && add_unique "$h"
	done < <(cat "$log_dir"/*.log 2>/dev/null | sed -n -E 's/.*fatal: \[([^]]+)\]: UNREACHABLE!.*/\1/p' | sort -u)
fi
if [ -n "$hosts" ]; then
	problems+=("UNREACHABLE host(s): $hosts. Their runners were not de-registered, their SUT processes were not stopped and their state was not reclaimed")
fi

if [ -f "$log_dir/stop-sut.log" ]; then
	stuck=$(sed -n -E 's/.*fatal: \[([^]]+)\]: FAILED!.*/\1/p' "$log_dir/stop-sut.log" | sort -u | paste -sd, -)
	if [ -n "$stuck" ]; then
		problems+=("the bench SUT could not be stopped on: $stuck (see stop-sut.log in the job output); a stale process there will break the next tier")
	fi
fi

if [ "${REMOVAL_TOKEN_MINTED:-false}" != "true" ]; then
	problems+=("no runner removal token could be minted, so no runner was de-registered")
fi

online=""
check_online() {
	local out
	if ! out=$("$gh_bin" api --paginate "/repos/${REPO:?REPO}/actions/runners" \
		--jq ".runners[] | select(.status == \"online\") | select(.labels | map(.name) | index(\"$label\")) | .name" 2>&1); then
		online="?"
		api_error=$out
		return 1
	fi
	online=$(printf '%s\n' "$out" | sed '/^$/d' | sort -u | paste -sd, -)
	return 0
}
api_error=""
i=0
while :; do
	check_online
	[ -z "$online" ] && break
	[ "$online" = "?" ] && break
	i=$((i + 1))
	[ "$i" -ge "$retries" ] && break
	echo "runners still online after teardown: $online; checking again in ${delay}s ($i/$retries)"
	"$sleep_bin" "$delay"
done
if [ "$online" = "?" ]; then
	echo "::warning title=Teardown verdict::could not list the repository's runners, so runner de-registration is unverified: $(printf '%s' "$api_error" | head -c 300)"
elif [ -n "$online" ]; then
	problems+=("runner(s) still ONLINE with label $label: $online. They are idle and will take the next job")
fi

if [ "${#problems[@]}" -eq 0 ]; then
	echo "teardown verdict: clean (all hosts answered, runners de-registered, none online)"
	exit 0
fi

msg=""
for p in "${problems[@]}"; do msg="${msg:+$msg; }$p"; done
msg="$msg. Fix by hand before the next tier: re-run this workflow's teardown, or run ansible/stop-sut.yml, ansible/cleanup.yml and ansible/runner-teardown.yml against the named hosts."
echo "::error title=Cluster teardown incomplete::$msg"
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
	{
		echo "### Cluster teardown INCOMPLETE"
		echo
		for p in "${problems[@]}"; do echo "- $p"; done
		echo
		echo "The job is red on purpose: a teardown that could not reach its hosts used to report success (bench 37226284351)."
	} >>"$GITHUB_STEP_SUMMARY"
fi
exit 1
