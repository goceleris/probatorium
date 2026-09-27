#!/usr/bin/env bash
# The cluster bootstrap's last step (celeris-stress-cluster.yml). Each host
# job runs on [self-hosted, celeris-cluster, <host>]: exactly ONE runner can
# take it. cluster-runner-up only warns when fewer than three runners are
# online, and a host job whose runner is missing queues (GitHub keeps a
# self-hosted job queued for up to 24 h) while the run holds
# matrix-tier-cluster. So the bootstrap fails unless the runner of every
# PLANNED host is online; the host jobs are then skipped and teardown runs.
#
# MATRIX is the plan's host matrix ({"include":[{"host":...},...]}); a host
# counts as present when an online runner carries both the celeris-cluster
# label and the host's own. Polls up to RUNNERS_ONLINE_TRIES times (default
# 12), RUNNERS_ONLINE_PAUSE seconds apart (default 10). GH_TOKEN must be
# allowed to list the repository's runners (RUNNER_PAT).
#
# A runner that drops AFTER this check still leaves its host job queued: the
# time a job waits for a runner is not bounded by any limit of this workflow.
set -euo pipefail
: "${GH_TOKEN:?}" "${REPO:?}" "${MATRIX:?}"
tries=${RUNNERS_ONLINE_TRIES:-12}
pause=${RUNNERS_ONLINE_PAUSE:-10}

hosts=()
while IFS= read -r h; do
	[ -z "$h" ] || hosts+=("$h")
done < <(jq -r '.include[].host' <<<"$MATRIX" | sort -u)
if [ "${#hosts[@]}" -eq 0 ]; then
	echo "::error::the plan's matrix names no host"
	exit 1
fi

for ((i = 1; ; i++)); do
	# The labels of every online celeris-cluster runner, one per line.
	online=$(gh api --paginate "repos/$REPO/actions/runners?per_page=100" \
		--jq '.runners[] | select(.status == "online") | [.labels[].name] | select(index("celeris-cluster")) | .[]' | sort -u) || online=""
	missing=()
	for h in "${hosts[@]}"; do
		grep -qxF "$h" <<<"$online" || missing+=("$h")
	done
	if [ "${#missing[@]}" -eq 0 ]; then
		echo "every planned host has an online celeris-cluster runner: ${hosts[*]}"
		exit 0
	fi
	if [ "$i" -ge "$tries" ]; then
		break
	fi
	echo "attempt $i: no online runner yet for ${missing[*]}; retrying in ${pause}s"
	sleep "$pause"
done
echo "::error::no online celeris-cluster runner for planned host(s) ${missing[*]} after $tries look(s): its host job would queue for up to 24 h holding matrix-tier-cluster. The host jobs are skipped; teardown runs."
gh api --paginate "repos/$REPO/actions/runners?per_page=100" --jq '.runners[] | [.name, .status, ([.labels[].name] | join(","))] | @tsv' || true
exit 1
