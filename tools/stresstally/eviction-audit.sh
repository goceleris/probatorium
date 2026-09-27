#!/usr/bin/env bash
# The cluster run's eviction audit (celeris-stress-cluster.yml, bootstrap job,
# the first job inside matrix-tier-cluster). The guard refused this run if a
# run was pending in the group, but some seconds pass between its look
# (CHECKED_AT) and this run entering the group; a cluster run that became
# pending in that window was cancelled by the entry. This reports, as error
# annotations, every run of a cluster workflow cancelled in the entry window.
# It cannot undo an eviction; it makes one impossible to miss. It never fails
# the job: the run has already entered.
#
# The window: from the guard's look to 2 minutes after this run's
# cluster-guard job ended, which is when its cluster job is queued into the
# group (and evicts what is pending there). Not until this bootstrap runs: a
# run that waited behind a holder bootstraps hours later, and every cancel
# of those hours, manual ones included, would be named. A cancel inside the
# window is still only possibly an eviction; the message says so.
set -uo pipefail
: "${GH_TOKEN:?}" "${REPO:?}" "${RUN_ID:?}" "${CHECKED_AT:?}"
since=$(date -u -d '7 days ago' +%Y-%m-%d 2>/dev/null || echo 2026-01-01)
entered=$(gh api "repos/$REPO/actions/runs/$RUN_ID/jobs?per_page=100" \
	--jq '.jobs[] | select(.name == "cluster-guard") | .completed_at' 2>/dev/null | tail -n 1)
if [[ $entered =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]]; then
	until=$(jq -rn --arg t "$entered" '$t | fromdateiso8601 + 120 | todateiso8601')
else
	entered="unknown"
	until=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	echo "::warning::could not read when this run's cluster-guard ended; the audit covers everything from the guard's look ($CHECKED_AT) to now ($until), so it may name cancels this run did not cause"
fi
if ! gh api --paginate "repos/$REPO/actions/runs?status=cancelled&created=%3E%3D$since&per_page=100" --jq '.workflow_runs[]' >cancelled.jsonl; then
	echo "::warning::could not list cancelled runs; the eviction audit did not run"
	exit 0
fi
jq -r --arg t "$CHECKED_AT" --arg u "$until" --arg self "$RUN_ID" '
	select(.updated_at >= $t and .updated_at <= $u and (.id | tostring) != $self)
	| select(.path | test("^\\.github/workflows/(matrix-[a-z]+-tier|benchmark-tier|celeris-stress)\\.yml$"))
	| [(.id | tostring), .path, .updated_at, .display_title] | @tsv' cancelled.jsonl >evicted.tsv || true
if [ -s evicted.tsv ]; then
	while IFS=$'\t' read -r id path at title; do
		echo "::error::run $id ($path, \"$title\") was cancelled at $at, while this run entered matrix-tier-cluster (the guard looked at $CHECKED_AT; its job ended at $entered). It may have been evicted by this run's entry, or cancelled for another reason: check it, and re-dispatch it if it was evicted."
	done <evicted.tsv
else
	echo "no cluster run was cancelled while this run entered matrix-tier-cluster ($CHECKED_AT to $until)"
fi
