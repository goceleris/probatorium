#!/usr/bin/env bash
# The cluster run's eviction audit (celeris-stress-cluster.yml, bootstrap job,
# the first job inside matrix-tier-cluster). The guard refused this run if a
# run was pending in the group, but a few seconds pass between its look
# (CHECKED_AT) and this run entering the group; a cluster run dispatched in
# that window would have been cancelled by this one. This lists every run of
# a cluster workflow cancelled since the guard looked and reports each as an
# error annotation naming it. It cannot undo an eviction; it makes one
# impossible to miss. It never fails the job: the run has already entered.
set -uo pipefail
: "${GH_TOKEN:?}" "${REPO:?}" "${RUN_ID:?}" "${CHECKED_AT:?}"
since=$(date -u -d '7 days ago' +%Y-%m-%d 2>/dev/null || echo 2026-01-01)
if ! gh api --paginate "repos/$REPO/actions/runs?status=cancelled&created=%3E%3D$since&per_page=100" --jq '.workflow_runs[]' >cancelled.jsonl; then
	echo "::warning::could not list cancelled runs; the eviction audit did not run"
	exit 0
fi
jq -r --arg t "$CHECKED_AT" --arg self "$RUN_ID" '
	select(.updated_at >= $t and (.id | tostring) != $self)
	| select(.path | test("^\\.github/workflows/(matrix-[a-z]+-tier|benchmark-tier|celeris-stress)\\.yml$"))
	| [(.id | tostring), .path, .updated_at, .display_title] | @tsv' cancelled.jsonl >evicted.tsv || true
if [ -s evicted.tsv ]; then
	while IFS=$'\t' read -r id path at title; do
		echo "::error::run $id ($path, \"$title\") was cancelled at $at, after this run's guard looked at $CHECKED_AT. If matrix-tier-cluster evicted it when this run entered the group, re-dispatch it."
	done <evicted.tsv
else
	echo "no cluster run was cancelled between the guard's look ($CHECKED_AT) and this run entering matrix-tier-cluster"
fi
