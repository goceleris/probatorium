#!/usr/bin/env bash
# The Go state of a cluster host job (celeris-stress-cluster.yml): the build
# cache, the module cache, GOPATH and the temporary files of every go test
# the job runs, kept apart from the host's shared ~/go and ~/.cache/go-build
# that the tiers build from.
#
# Sourced by tools/stresstally/cluster-host.sh (and by the tests):
#   gostate_env    exports GOCACHE, GOMODCACHE, GOPATH, TMPDIR and GOTOOLCHAIN
#   gostate_init   gostate_env, then creates the directories
# Needs RUNNER_TEMP.

gostate_env() {
	: "${RUNNER_TEMP:?}"
	export GOCACHE="$RUNNER_TEMP/go/cache" GOMODCACHE="$RUNNER_TEMP/go/mod" GOPATH="$RUNNER_TEMP/go/path"
	export TMPDIR="$RUNNER_TEMP/go/tmp" GOTOOLCHAIN=local
}

gostate_init() {
	gostate_env
	mkdir -p "$GOCACHE" "$GOMODCACHE" "$GOPATH" "$TMPDIR"
}
