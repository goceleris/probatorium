#!/usr/bin/env bash
# The cluster host job's checkout (celeris-stress-cluster.yml): each case's
# celeris commit (CASE_SHAS: CASE=SHA tokens from the plan job, already
# proven to come from goceleris/celeris) into ./celeris-<CASE>, the directory
# shard.sh runs go test in. Two arms of one commit get two checkouts.
set -euo pipefail
: "${CASE_SHAS:?}"
url=https://github.com/goceleris/celeris.git
for kv in $CASE_SHAS; do
	name=${kv%%=*} sha=${kv#*=}
	if ! [[ $name =~ ^[A-Za-z0-9]+$ && $sha =~ ^[0-9a-f]{40}$ ]]; then
		echo "::error::$kv is not CASE=<full commit sha>"
		exit 1
	fi
	dir="celeris-$name"
	rm -rf "$dir"
	git init -q "$dir"
	ok=0
	for attempt in 1 2 3; do
		if git -C "$dir" fetch -q --depth=1 --no-tags "$url" "$sha"; then
			ok=1
			break
		fi
		echo "fetch of $sha failed (attempt $attempt); retrying"
		sleep $((attempt * 10))
	done
	[ "$ok" = 1 ] || { echo "::error::could not fetch celeris $sha"; exit 1; }
	git -C "$dir" checkout -q --detach FETCH_HEAD
	echo "$dir: $(git -C "$dir" rev-parse HEAD)"
done
