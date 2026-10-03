#!/usr/bin/env bash
# Resolves the celeris package patterns of a stress run against the celeris
# checkout it tests, so one row works on both sides of celeris#443, which moves
# the engine, adaptive, probe, resource and protocol packages under internal/
# and each driver's protocol package to a new place.
#
#   bash pkgpaths.sh DIR PATTERN...
#
# prints the resolved patterns, space-separated, on one line of stdout, and one
# line per pattern on stderr: "PATTERN -> RESOLVED (why)". Sourced, it only
# defines its functions: shard.sh and cluster-host.sh call resolve_pkg.
#
# A pattern resolves to the first candidate that is a Go package in DIR (a
# directory holding a .go file; for DIR/... any .go file below it):
#   1. the pattern as written;
#   2. ./P, where P is the pattern's layout-free name: a leading internal/ is
#      dropped and driver/X/internal/protocol is read as driver/X/protocol;
#   3. ./internal/P;
#   4. for P = driver/X/protocol[/rest]: ./driver/X/internal/protocol[/rest],
#      then ./internal/driver/X/protocol[/rest].
# So ./engine/iouring runs ./engine/iouring before the move and
# ./internal/engine/iouring after it, and a row that already names the new path
# runs the old one at a commit before the move. A pattern none of them matches
# is passed on as written: go test then reports it, as before this resolver.
# . and ./... are never rewritten. Each candidate is one argv element; nothing
# is eval'd or globbed.
set -euo pipefail

# is_pkg DIR REL RECURSIVE: DIR/REL is a Go package (RECURSIVE=1: holds a .go
# file at any depth, for a /... pattern).
is_pkg() {
	local d="$1/$2" f
	[ -d "$d" ] || return 1
	if [ "$3" = 1 ]; then
		[ -n "$(find "$d" -name '*.go' -type f -print -quit 2>/dev/null)" ]
		return
	fi
	for f in "$d"/*.go; do
		[ -f "$f" ] && return 0
	done
	return 1
}

# resolve_pkg DIR PATTERN: sets pkg_out (the pattern to run) and pkg_why.
resolve_pkg() {
	local dir=$1 pat=$2 rel rec=0 suf="" p x rest c
	pkg_out=$pat pkg_why="as named"
	case "$pat" in
	. | ./...) return 0 ;;
	./*) rel=${pat#./} ;;
	*)
		pkg_why="not a ./ pattern; passed as named"
		return 0
		;;
	esac
	if [[ $rel == */... ]]; then rel=${rel%/...} rec=1 suf=/...; fi
	# The layout-free name.
	p=${rel#internal/}
	if [[ $p =~ ^driver/([^/]+)/internal/protocol(/.*)?$ ]]; then
		p="driver/${BASH_REMATCH[1]}/protocol${BASH_REMATCH[2]}"
	fi
	local -a cand=("$rel" "$p" "internal/$p")
	if [[ $p =~ ^driver/([^/]+)/protocol(/.*)?$ ]]; then
		x=${BASH_REMATCH[1]} rest=${BASH_REMATCH[2]}
		cand+=("driver/$x/internal/protocol$rest" "internal/driver/$x/protocol$rest")
	fi
	for c in "${cand[@]}"; do
		if is_pkg "$dir" "$c" "$rec"; then
			pkg_out="./$c$suf"
			if [ "$c" = "$rel" ]; then
				pkg_why="as named"
			else
				pkg_why="./$rel$suf is not a package at this commit"
			fi
			return 0
		fi
	done
	pkg_why="no candidate is a package at this commit (${cand[*]}); passed as named"
}

pkgpaths_main() {
	local dir=${1:?usage: pkgpaths.sh DIR PATTERN...} pat
	local -a resolved=()
	shift
	for pat in "$@"; do
		resolve_pkg "$dir" "$pat"
		resolved+=("$pkg_out")
		printf '%s -> %s (%s)\n' "$pat" "$pkg_out" "$pkg_why" >&2
	done
	printf '%s\n' "${resolved[*]-}"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
	pkgpaths_main "$@"
fi
