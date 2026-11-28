#!/usr/bin/env bash
# The build job of idle-cell-baremetal.yml (GitHub-hosted runner): the static linux binaries of every arm of the
# celeris#1485 cells for ONE architecture, plus the record of what they are made of.
#
#   build.sh <celeris clone with full history> <out dir (new)> <arch: arm64|amd64> <main sha> <head sha> <old sha>
#
# Arms (one tool source, tools/idlecell/tool, built against a celeris worktree each, CGO off):
#   main       celeris at <main sha>                                    idlehold-server-main, idlecost-main
#   head       <head sha> MERGED with <main sha> (nothing is pushed)    idlehold-server-head
#   head-aw    head, plus patch/head-aw.patch (walk at every tick)      idlehold-server-head-aw
#   main-skip  main, plus patch/main-skip.patch (walk once a second)    idlehold-server-main-skip
#   old        <old sha> (the pre-#1474 4 ms idle wait)                 idlecost-old
# and idleclient (the holder of the idle connections; no celeris code).
# Every binary's sha256, every commit and tree, the patches' sha256 and the Go version go to BUILD-INFO.txt.
# No removal anywhere: the output and work directories are new.
set -euo pipefail
clone=${1:?celeris clone} out=${2:?out dir} arch=${3:?arch} main_sha=${4:?main sha} head_sha=${5:?head sha} old_sha=${6:?old sha}
here=$(cd "$(dirname "$0")" && pwd)
case "$arch" in arm64 | amd64) ;; *) echo "::error::arch must be arm64 or amd64, got $arch" >&2; exit 2 ;; esac
for s in "$main_sha" "$head_sha" "$old_sha"; do
	[[ $s =~ ^[0-9a-f]{40}$ ]] || { echo "::error::not a full commit sha: $s" >&2; exit 2; }
done
[ ! -e "$out" ] || { echo "::error::$out exists: the output directory must be new" >&2; exit 2; }
mkdir -p "$out/bin" "$out/patch"
work=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/idlecell-build-XXXXXX")
info="$out/BUILD-INFO.txt"
say() { printf '%s\n' "$*" | tee -a "$info"; }
cd "$clone"
# (nothing is committed: the merged tree is computed with merge-tree, no identity is needed and none is written)

say "idle-cell build: arch=$arch go=$(go version) built_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
say "celeris origin/main now: $(git rev-parse origin/main)"
for pair in "main:$main_sha" "old:$old_sha"; do
	git cat-file -e "${pair#*:}^{commit}" || { echo "::error::${pair%%:*} sha ${pair#*:} is not in the clone" >&2; exit 1; }
	git merge-base --is-ancestor "${pair#*:}" origin/main || { echo "::error::${pair%%:*} sha ${pair#*:} is not an ancestor of origin/main" >&2; exit 1; }
done
git cat-file -e "$head_sha^{commit}" || { echo "::error::head sha $head_sha is not in the clone" >&2; exit 1; }
branches=$(git branch -r --contains "$head_sha" | tr -d ' ' | tr '\n' ' ')
[ -n "$branches" ] || { echo "::error::head sha $head_sha is on no origin branch" >&2; exit 1; }
say "head $head_sha is on: $branches"

wt() { # name sha
	git worktree add --detach -q "$work/wt-$1" "$2"
}
wt main "$main_sha"
wt old "$old_sha"
# head = <head sha> merged with <main sha>: the merged TREE is computed with merge-tree and read into a worktree, so no commit is made
# and no git identity is needed (a hosted runner has none, and none is invented here). A conflict refuses the build.
if ! head_tree=$(git merge-tree --write-tree "$main_sha" "$head_sha" 2>"$work/merge-head.log"); then
	cat "$work/merge-head.log" >&2
	printf '%s\n' "$head_tree" >&2
	echo "::error::head $head_sha does not merge cleanly into main $main_sha" >&2
	exit 1
fi
say "merge of head into main (merge-tree): tree $head_tree"
wt head "$main_sha"
(cd "$work/wt-head" && git read-tree -u --reset "$head_tree")
wt head-aw "$main_sha"
(cd "$work/wt-head-aw" && git read-tree -u --reset "$head_tree")
wt main-skip "$main_sha"
for pair in "head-aw:head-aw.patch" "main-skip:main-skip.patch"; do
	d=$work/wt-${pair%%:*}
	cp "$here/patch/${pair#*:}" "$out/patch/"
	(cd "$d" && git apply --check "$here/patch/${pair#*:}" && git apply "$here/patch/${pair#*:}") ||
		{ echo "::error::${pair#*:} does not apply to ${pair%%:*}: the code it patches moved" >&2; exit 1; }
	say "${pair#*:} applied to ${pair%%:*}: sha256 $(sha256sum "$here/patch/${pair#*:}" | cut -d' ' -f1); files: $(cd "$d" && git diff --name-only | tr '\n' ' ')"
done
for v in main old head head-aw main-skip; do
	tree=$(cd "$work/wt-$v" && git add -A >/dev/null && git write-tree)
	say "tree[$v]=$tree commit_base=$(cd "$work/wt-$v" && git rev-parse HEAD)"
done
say "main=$main_sha head=$head_sha (merged into main) old=$old_sha"
say "tool source sha256: $(cd "$here/tool" && cat server/*.go client/*.go idlecost/*.go go.mod | sha256sum | cut -d' ' -f1)"

# One tool module copy per arm, its celeris replaced by that arm's worktree.
build() { # arm pkg out-name
	local arm=$1 pkg=$2 name=$3 t=$work/tool-$1
	if [ ! -d "$t" ]; then
		mkdir -p "$t"
		cp -R "$here/tool/." "$t/"
		(cd "$t" && go mod edit -replace "github.com/goceleris/celeris=$work/wt-$arm" && GOTOOLCHAIN=${GOTOOLCHAIN:-local} go mod tidy >/dev/null)
	fi
	(cd "$t" && GOTOOLCHAIN=${GOTOOLCHAIN:-local} CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -buildvcs=false -o "$out/bin/$name" "./$pkg")
	say "built $name"
}
for v in main head head-aw main-skip; do build "$v" server "idlehold-server-$v"; done
build main idlecost idlecost-main
build old idlecost idlecost-old
build main client idleclient
(cd "$out/bin" && sha256sum -- * >"$out/SHA256SUMS")
say "binaries (sha256):"
sed 's/^/  /' "$out/SHA256SUMS" | tee -a "$info"
(cd "$out/bin" && file -- * 2>/dev/null | sed "s/^/  /" | tee -a "$info") || true
