#!/usr/bin/env bash
# Regenerates the stresstally test corpus: REAL `go test -v` output of the
# sample module in samplemod/, one file per shape the tally must classify.
# The tests wrap each body in a stress header and trailer. Paths are
# rewritten to /src and /goroot so the corpus does not depend on the machine.
#
#   bash tools/stresstally/testdata/regen.sh
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
cd "$here/samplemod"
goroot=$(go env GOROOT)
run() { # name expected-exit env... -- go test args...
	local name=$1 want=$2
	shift 2
	local envs=()
	while [ "$1" != "--" ]; do envs+=("$1"); shift; done
	shift
	env "${envs[@]}" go test "$@" >"$here/$name.txt.tmp" 2>&1
	local rc=$?
	sed -e "s#$here/samplemod#/src#g" -e "s#$goroot#/goroot#g" "$here/$name.txt.tmp" >"$here/$name.txt"
	rm -f "$here/$name.txt.tmp"
	echo "$name: go test exit $rc (want $want)"
	[ "$rc" = "$want" ] || { echo "regen: $name exited $rc, want $want" >&2; exit 1; }
}
clean='^(TestPass|TestSub|TestSkip|TestFail|TestSubFail|TestParallel|TestPanic|TestGoroutinePanic|TestSlow|TestOnlyB|Example)$'
run good 0 X=1 -- -v -count=2 -shuffle=42 -run "$clean" ./a ./b ./c
run fail 1 SAMPLE_FAIL=1 -- -v -count=1 -run "$clean" ./a
run panic 1 SAMPLE_PANIC=1 -- -v -count=1 -run "$clean" ./a ./b
run gpanic 1 SAMPLE_GPANIC=1 -- -v -count=1 -run '^(TestPass|TestGoroutinePanic)$' ./a
run timeout 1 SAMPLE_SLOW=1 -- -v -count=1 -timeout=1s -run "$clean" ./a
run none 0 X=1 -- -v -count=1 -run '^TestNoSuch$' ./a ./b
run glued 0 X=1 -- -v -count=1 -run '^(TestPass|TestNoNewline)$' ./a
run skiponly 0 X=1 -- -v -count=3 -run '^TestSkip$' ./a
run build 1 X=1 -- -v -count=1 ./b ./broken
run race 1 X=1 -- -v -count=1 -race ./racy
