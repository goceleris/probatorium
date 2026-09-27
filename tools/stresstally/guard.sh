#!/usr/bin/env bash
# The cluster guard's inputs (celeris-stress.yml, job cluster-guard): every
# run of this repository that is not completed, and the jobs of every other
# celeris-stress run among them, as JSON lines in $GUARD_DIR, for
# `stresstally guard` to judge. It records when it looked (checked_at, also a
# step output) BEFORE listing, so the bootstrap's eviction audit covers the
# whole window between this look and the run entering matrix-tier-cluster.
# Read-only: GH_TOKEN is the run's token with actions: read.
set -euo pipefail
: "${GH_TOKEN:?}" "${REPO:?}" "${RUN_ID:?}" "${GUARD_DIR:?}"
mkdir -p "$GUARD_DIR"
checked_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
printf '%s\n' "$checked_at" >"$GUARD_DIR/checked_at"
for status in in_progress queued pending waiting requested action_required; do
	gh api --paginate "repos/$REPO/actions/runs?status=$status&per_page=100" --jq '.workflow_runs[]' >"$GUARD_DIR/runs-$status.jsonl"
done
cat "$GUARD_DIR"/runs-*.jsonl |
	jq -r --arg p ".github/workflows/celeris-stress.yml" 'select(.path == $p) | .id' | sort -u |
	while read -r id; do
		[ "$id" != "$RUN_ID" ] || continue
		gh api --paginate "repos/$REPO/actions/runs/$id/jobs?per_page=100" --jq '.jobs[]' >"$GUARD_DIR/jobs-$id.jsonl"
	done
if [ -n "${GITHUB_OUTPUT-}" ]; then
	printf 'checked_at=%s\n' "$checked_at" >>"$GITHUB_OUTPUT"
fi
