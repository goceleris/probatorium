#!/usr/bin/env bash
# Rehearsal of a composition end to end, on a checkout. NOT wired into any
# workflow or mage target; run by hand (needs mage, and bun + a goceleris/docs
# clone for step 3).
#
#   tools/mergedocs/rehearse.sh CAND_RUN_DIR PEERS_RUN_DIR META_CAND META_PEERS WORKDIR [DOCS_CLONE]
#
# 1. compose            go run ./tools/mergedocs ... --out WORKDIR/composed
# 2. mage Publish       PUBLISH_DRYRUN=1 on composed/results.json (integrity and
#                       data-completeness gates run on the merged document)
# 2b. install            go run ./tools/mergedocs install ... (composition.json next to summary.json)
# 3. docs build         bun scripts/build-data.ts --validate-only on the tree
# Never pushes, never dispatches. Set MERGEDOCS_FLAGS for extra composer flags.
set -euo pipefail
cand=$1; peers=$2; mc=$3; mp=$4; work=$5; docs=${6:-}
here=$(cd "$(dirname "$0")/../.." && pwd)
cd "$here"
rm -rf "$work/composed" "$work/tree"
mkdir -p "$work"
# shellcheck disable=SC2086
go run ./tools/mergedocs --candidate "$cand" --peers "$peers" --meta-candidate "$mc" --meta-peers "$mp" \
  --out "$work/composed" ${MERGEDOCS_FLAGS:-}
date=$(python3 -I -B -c 'import json,sys;print(json.load(open(sys.argv[1]))["publish"]["publish_date"])' "$work/composed/COMPOSITION.json")
mage -compile "$work/magebin" >/dev/null
PUBLISH_DRYRUN=1 PUBLISH_VERSION="${PUBLISH_VERSION:-v0.0.0-rehearsal}" BENCH_START_DATE="$date" PUBLISH_RUN_ID=run-1 \
  PUBLISH_OUT="$work/tree" PUBLISH_RESULTS="$work/composed/results.json" "$work/magebin" publish
# 2b. attach composition.json (+ twin.json) to the published cell: mage Publish (frozen)
#     writes four files only. In a real publish the docs checkout is DOCS_REPO_DIR.
go run ./tools/mergedocs install --composed "$work/composed" --docs "$work/tree" --version "${PUBLISH_VERSION:-v0.0.0-rehearsal}"
if [ -n "$docs" ]; then
  (cd "$docs" && RESULTS_ROOT="$work/tree/results" bun scripts/build-data.ts --validate-only)
fi
echo "rehearsal ok: $work/tree/results"
