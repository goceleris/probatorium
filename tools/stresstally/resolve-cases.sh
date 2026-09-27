#!/usr/bin/env bash
# The cluster's provenance check (celeris-stress.yml, plan job): resolve every
# case's celeris ref (REFS: CASE=REF tokens, validated by `stresstally plan`)
# to ONE commit, and refuse any commit that no branch or tag of
# goceleris/celeris contains. A cluster shard runs on bare metal as a user
# with sudo, so it may run only code a celeris maintainer pushed; a fork's
# commit can be fetched by sha through its pull ref, so the sha alone proves
# nothing. Writes case_shas (CASE=SHA tokens) to $GITHUB_OUTPUT.
#
# The history is fetched without trees or blobs (--filter=tree:0): commits
# only, a few MB, enough for `for-each-ref --contains`.
set -euo pipefail
: "${REFS:?}" "${GITHUB_OUTPUT:?}"
url=https://github.com/goceleris/celeris.git
repo="${RUNNER_TEMP:-/tmp}/celeris-provenance"
rm -rf "$repo"
git clone -q --bare --filter=tree:0 "$url" "$repo"
out=()
for kv in $REFS; do
	name=${kv%%=*} ref=${kv#*=}
	if ! [[ $name =~ ^[A-Za-z0-9]+$ ]]; then
		echo "::error::case name $name is not plain"
		exit 1
	fi
	git -C "$repo" fetch -q --no-tags origin "$ref"
	sha=$(git -C "$repo" rev-parse --verify 'FETCH_HEAD^{commit}')
	if ! [[ $sha =~ ^[0-9a-f]{40}$ ]]; then
		echo "::error::case $name: celeris $ref resolved to '$sha', not a commit"
		exit 1
	fi
	holder=$(git -C "$repo" for-each-ref --count=1 --contains "$sha" --format='%(refname)' refs/heads refs/tags)
	if [ -z "$holder" ]; then
		echo "::error::case $name: celeris $ref is $sha, which no branch or tag of goceleris/celeris contains. The cluster runs only code a celeris maintainer pushed (a fork's commit is reachable only through its pull ref); push it to a goceleris/celeris branch, or use target github."
		exit 1
	fi
	echo "case $name: celeris $ref is $sha, contained in $holder"
	out+=("$name=$sha")
done
printf 'case_shas=%s\n' "${out[*]}" >>"$GITHUB_OUTPUT"
