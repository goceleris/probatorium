#!/usr/bin/env bash
# The Go state of a cluster host job (celeris-stress-cluster.yml): the build
# cache, the module cache, GOPATH and the temporary files of every go test
# the job runs, kept apart from the host's shared ~/go and ~/.cache/go-build
# that the tiers build from.
#
# Sourced by tools/stresstally/cluster-host.sh (and by the tests):
#   gostate_env    exports GOCACHE, GOMODCACHE, GOPATH, GOFLAGS, GOTOOLCHAIN
#                  and TMPDIR
#   gostate_init   wipes what a previous job may have left, then gostate_env
#                  and creates the directories
#   gostate_wipe   deletes all of it, read-only module directories included
# Run as `gostate.sh wipe` by the host job's last step, which always runs.
#
# Nothing here may be left where the runner-dir wipes cannot delete it:
# teardown's "Wipe runner dir" and the next bootstrap's "Wipe stale runner
# dir" (ansible/runner-teardown.yml, ansible/runner-setup.yml) delete
# /tmp/actions-runner-<host> as the runner's own user without chmod, and cmd/go
# makes every module directory read-only unless -modcacherw. One read-only
# module cache there would fail that teardown and every later cluster
# bootstrap on the host. So:
#   - GOFLAGS=-modcacherw: the module cache is writable from the start, which
#     holds even when no later step runs (a lost runner, a killed job);
#   - gostate_wipe (chmod -R u+w, then rm -rf) in the job's last step.
# (stresstally plan refuses GOFLAGS from a dispatch, so nothing overrides it.)
#
# TMPDIR is /tmp/cstress, not a directory under RUNNER_TEMP: t.TempDir lives
# there and celeris tests put Unix sockets in it (107 usable bytes). Under
# /tmp/actions-runner-msa2-server/_work/_temp a socket path of
# adaptive/prebound_listener_test.go came to 112 bytes and the test skipped on
# msa2-server only; under /tmp/cstress it is 8 bytes longer than GitHub's /tmp.
# The group keeps any other stress job off the host, so one fixed name is
# safe, and gostate_init reclaims what a lost job left there.
# STRESS_GO_TMPDIR overrides it for the tests only; the workflow never sets it.

gostate_tmpdir() { printf '%s' "${STRESS_GO_TMPDIR:-/tmp/cstress}"; }

gostate_env() {
	: "${RUNNER_TEMP:?}"
	export GOCACHE="$RUNNER_TEMP/go/cache" GOMODCACHE="$RUNNER_TEMP/go/mod" GOPATH="$RUNNER_TEMP/go/path"
	export GOFLAGS=-modcacherw GOTOOLCHAIN=local
	TMPDIR=$(gostate_tmpdir)
	export TMPDIR
}

gostate_wipe() {
	: "${RUNNER_TEMP:?}"
	local d
	for d in "$RUNNER_TEMP/go" "$(gostate_tmpdir)"; do
		case "$d" in
		/?*/?*) ;;
		*)
			echo "::error::gostate: refusing to wipe '$d'" >&2
			return 1
			;;
		esac
		[ -e "$d" ] || continue
		chmod -R u+w "$d" 2>/dev/null || true
		if ! rm -rf "$d"; then
			echo "::error::gostate: could not delete $d; the runner-dir wipe will fail on it too" >&2
			return 1
		fi
	done
}

gostate_init() {
	gostate_wipe
	gostate_env
	mkdir -p "$GOCACHE" "$GOMODCACHE" "$GOPATH" "$TMPDIR"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
	case "${1-}" in
	wipe) gostate_wipe ;;
	*)
		echo "usage: gostate.sh wipe" >&2
		exit 2
		;;
	esac
fi
