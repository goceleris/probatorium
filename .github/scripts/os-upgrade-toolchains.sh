#!/usr/bin/env bash
# Lists every toolchain and fixture the cluster's ansible roles install, with what this repository PINS (or "floating":
# resolved afresh at each deploy) beside the newest upstream stable it can establish from the roles' own download
# URLs. Run by .github/workflows/cluster-os-upgrade.yml in survey mode on the GitHub-hosted runner, NOT on a cluster
# host: the toolchains live under /tmp on the hosts (a reboot wipes them) and exist there only during a run.
#
#   os-upgrade-toolchains.sh OUT.json            pins and upstream (needs curl, jq, git, network)
#   os-upgrade-toolchains.sh --pins-only         the pins as JSON on stdout (no network: what the tests read)
#
# Read-only and best effort: an upstream that does not answer is reported as "unknown", never as a failure. A
# "newer" of true means upstream is ahead of the pin; a floating tool is re-resolved by the next deploy by itself.
set -u
root=$(cd "$(dirname "$0")/../.." && pwd)
cd "$root" || exit 1

first() { sed -n "$1" "$2" | head -n 1; }
yamlval() { # yamlval KEY FILE : the quoted value of `KEY: "value"`
	sed -n "s/^[[:space:]]*$1:[[:space:]]*\"\([^\"]*\)\".*/\1/p" "$2" | head -n 1
}

go_mod=$(sed -n 's/^go \([0-9][0-9.]*\)$/\1/p' go.mod | head -n 1)
go_ci=$(sed -n 's/^[[:space:]]*go-version:[[:space:]]*"\([^"]*\)".*/\1/p' .github/workflows/lint.yml | head -n 1)
dotnet=$(yamlval dotnet_sdk_version ansible/roles/dotnet/defaults/main.yml)
drogon=$(yamlval drogon_version ansible/roles/cpp/defaults/main.yml)
h2o=$(yamlval h2o_commit ansible/roles/c/defaults/main.yml)
libreactor=$(yamlval libreactor_commit ansible/roles/c/defaults/main.yml)
py=$(yamlval python_version ansible/runner-setup.yml)
pybuild=$(yamlval python_build ansible/runner-setup.yml)
uv=$(yamlval uv_version ansible/runner-setup.yml)
acore=$(yamlval ansible_core_version ansible/runner-setup.yml)
aposix=$(yamlval ansible_posix_version ansible/runner-setup.yml)
runner=$(sed -n '/^  runner-version:/,/^  [a-z-]*:/p' .github/actions/cluster-runner-up/action.yml | sed -n 's/^[[:space:]]*default:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)
pg=$(grep -o '"postgres:[^"]*"' ansible/roles/dbservices/tasks/main.yml | head -n 1 | tr -d '"')
redis=$(grep -o '"redis:[^"]*"' ansible/roles/dbservices/tasks/main.yml | head -n 1 | tr -d '"')
memc=$(grep -o '"memcached:[^"]*"' ansible/roles/dbservices/tasks/main.yml | head -n 1 | tr -d '"')
node_line=$(grep -o 'latest-v[0-9]*\.x' ansible/roles/node/tasks/main.yml | head -n 1)
jdk_major=$(grep -o 'latest/[0-9]*/ga' ansible/roles/java/tasks/main.yml | head -n 1 | sed 's|latest/\([0-9]*\)/ga|\1|')
maven_line=$(grep -o 'maven/maven-[0-9]*' ansible/roles/java/tasks/main.yml | head -n 1 | sed 's|maven/maven-||')

# name | kind (pinned|floating) | pin | where it is pinned or resolved
rows() {
	cat << EOF
go (the module and the CI)|pinned|go.mod $go_mod, setup-go $go_ci|go.mod; .github/workflows/*.yml go-version
rust (rustup stable)|floating|stable at deploy time|ansible/roles/rust
java (Temurin $jdk_major)|floating|latest GA of $jdk_major at deploy time|ansible/roles/java (api.adoptium.net)
maven ($maven_line.x)|floating|latest $maven_line.x at deploy time|ansible/roles/java (dlcdn.apache.org)
node ($node_line)|floating|latest of $node_line at deploy time|ansible/roles/node (nodejs.org)
bun|floating|latest at deploy time|ansible/roles/bun (bun.sh)
zig|floating|latest stable of ziglang.org's index at deploy time|ansible/roles/zig
dotnet sdk|pinned|$dotnet|ansible/roles/dotnet/defaults/main.yml (and servers/aspnet/global.json)
python (uv-managed)|pinned|$py (build $pybuild)|ansible/runner-setup.yml; ansible/roles/python
uv|pinned|$uv|ansible/runner-setup.yml
ansible-core|pinned|$acore|ansible/runner-setup.yml
ansible.posix|pinned|$aposix|ansible/runner-setup.yml
actions runner|pinned|$runner|.github/actions/cluster-runner-up/action.yml runner-version
drogon (libdrogon)|pinned|$drogon|ansible/roles/cpp/defaults/main.yml
h2o (libh2o, a commit)|pinned|$h2o|ansible/roles/c/defaults/main.yml
libreactor (a commit; gated off)|pinned|$libreactor|ansible/roles/c/defaults/main.yml
postgres image|pinned|$pg|ansible/roles/dbservices
redis image|pinned|$redis|ansible/roles/dbservices
memcached image|pinned|$memc|ansible/roles/dbservices
gcc / g++ / build-essential / cmake / docker.io|floating|the hosts' apt packages (see each survey.json host_toolchain)|ansible/roles/c, cpp, dbservices
EOF
}

if [ "${1:-}" = "--pins-only" ]; then
	rows | jq -R -s 'split("\n") | map(select(length > 0) | split("|") | {name: .[0], kind: .[1], pin: .[2], where: .[3]})'
	exit 0
fi

out=${1:?usage: os-upgrade-toolchains.sh OUT.json | --pins-only}
if ! command -v curl > /dev/null || ! command -v jq > /dev/null || ! command -v git > /dev/null; then
	echo '[]' > "$out"
	echo "curl, jq and git are needed for the upstream lookups"
	exit 0
fi
get() { curl -fsSL --max-time 25 "$@" 2> /dev/null; }
ghget() {
	if [ -n "${GH_TOKEN:-}" ]; then get -H "Authorization: Bearer $GH_TOKEN" -H "Accept: application/vnd.github+json" "$1"; else get "$1"; fi
}
semver_max() { grep -E '^v?[0-9]+(\.[0-9]+)*$' | sort -V | tail -n 1; }

# bash 3 (a laptop) has no associative arrays: UP_<key> and SRC_<key> variables
put() { eval "UP_$1=\$2; SRC_$1=\$3"; }
put go "$(get 'https://go.dev/dl/?mode=json' | jq -r '[.[] | select(.stable)][0].version' | sed 's/^go//')" "go.dev/dl"
put rust "$(get https://static.rust-lang.org/dist/channel-rust-stable.toml | awk '/^\[pkg.rust\]/{f=1} f && /^version =/{gsub(/"/,"",$3); print $3; exit}')" "static.rust-lang.org channel-rust-stable"
put java "$(get "https://api.adoptium.net/v3/assets/latest/$jdk_major/hotspot?architecture=x64&image_type=jdk&os=linux" | jq -r '.[0].version.semver')" "api.adoptium.net latest/$jdk_major (newest LTS: $(get https://api.adoptium.net/v3/info/available_releases | jq -r '.most_recent_lts'))"
put maven "$(get https://dlcdn.apache.org/maven/maven-3/ | grep -o 'href="3\.[0-9.]*/"' | sed 's/href="\(.*\)\/"/\1/' | semver_max)" "dlcdn.apache.org/maven/maven-3"
nmaj=$(echo "$node_line" | sed 's/latest-v\([0-9]*\)\.x/\1/')
node_up=$(get "https://nodejs.org/dist/latest-v${nmaj}.x/SHASUMS256.txt" | grep -o 'node-v[0-9.]*-linux-x64.tar.xz' | head -n 1 | sed 's/node-v\(.*\)-linux.*/\1/')
put node "$node_up" "nodejs.org/dist/$node_line (newest LTS: $(get https://nodejs.org/dist/index.json | jq -r '[.[] | select(.lts != false)][0].version'))"
put bun "$(ghget https://api.github.com/repos/oven-sh/bun/releases/latest | jq -r '.tag_name' | sed 's/^bun-v//')" "github.com/oven-sh/bun releases/latest"
put zig "$(get https://ziglang.org/download/index.json | jq -r 'keys[]' | grep -E '^[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -n 1)" "ziglang.org/download/index.json"
dotnet_major=$(echo "$dotnet" | cut -d. -f1-2)
rel=$(get https://builds.dotnet.microsoft.com/dotnet/release-metadata/releases-index.json)
put dotnet "$(echo "$rel" | jq -r --arg c "$dotnet_major" '."releases-index"[] | select(."channel-version" == $c) | ."latest-sdk"')" "releases-index.json channel $dotnet_major (newest active channel: $(echo "$rel" | jq -r '[."releases-index"[] | select(."support-phase" != "eol")][0]."channel-version" + " sdk " + [."releases-index"[] | select(."support-phase" != "eol")][0]."latest-sdk"'))"
put python "$(get https://endoflife.date/api/python.json | jq -r --arg c "$(echo "$py" | cut -d. -f1-2)" '.[] | select(.cycle == $c) | .latest')" "endoflife.date python $(echo "$py" | cut -d. -f1-2) cycle (newest cycle: $(get https://endoflife.date/api/python.json | jq -r '.[0].cycle + " " + .[0].latest'))"
put uv "$(ghget https://api.github.com/repos/astral-sh/uv/releases/latest | jq -r '.tag_name')" "github.com/astral-sh/uv releases/latest"
put acore "$(get https://pypi.org/pypi/ansible-core/json | jq -r '.info.version')" "pypi.org ansible-core"
put runner "$(ghget https://api.github.com/repos/actions/runner/releases/latest | jq -r '.tag_name' | sed 's/^v//')" "github.com/actions/runner releases/latest"
put postgres "$(get https://endoflife.date/api/postgresql.json | jq -r '.[0].latest + " (major " + .[0].cycle + ")"')" "endoflife.date postgresql (the image tag pins a major: compare majors)"
put redis "$(get https://endoflife.date/api/redis.json | jq -r '.[0].latest + " (minor " + .[0].cycle + ")"')" "endoflife.date redis (the image tag pins a minor)"
put drogon "$(git ls-remote --tags --refs https://github.com/drogonframework/drogon.git 2> /dev/null | sed 's#.*refs/tags/##' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -n 1)" "git ls-remote tags drogonframework/drogon"
put h2o "$(git ls-remote https://github.com/h2o/h2o.git refs/heads/master 2> /dev/null | cut -f1)" "git ls-remote h2o/h2o master"
put libreactor "$(git ls-remote https://github.com/fredrikwidlund/libreactor.git HEAD 2> /dev/null | cut -f1)" "git ls-remote fredrikwidlund/libreactor HEAD"

lookup() { # lookup ROW-NAME-PREFIX -> key
	case "$1" in
	"go "*) echo go ;; rust*) echo rust ;; java*) echo java ;; maven*) echo maven ;; node*) echo node ;; bun*) echo bun ;;
	zig*) echo zig ;; "dotnet sdk"*) echo dotnet ;; python*) echo python ;; uv*) echo uv ;; ansible-core*) echo acore ;;
	"actions runner"*) echo runner ;; "postgres image"*) echo postgres ;; "redis image"*) echo redis ;; drogon*) echo drogon ;; "h2o "*) echo h2o ;; libreactor*) echo libreactor ;; *) echo "" ;;
	esac
}

{
	rows | while IFS='|' read -r name kind pin where; do
		k=$(lookup "$name")
		u=""
		s=""
		if [ -n "$k" ]; then
			eval "u=\${UP_$k:-}; s=\${SRC_$k:-}"
		fi
		newer=unknown
		if [ -n "$u" ] && [ "$kind" = pinned ]; then
			newer=unknown
			pin_core=$(echo "$pin" | grep -oE '[0-9a-f]{40}|v?[0-9]+(\.[0-9]+)+' | tail -n 1)
			case "$name" in "postgres image"* | "redis image"*) pin_core="" ;; esac
			if [ -n "$pin_core" ]; then newer=false; fi
			if [ -n "$pin_core" ] && [ "${pin_core#v}" != "${u#v}" ]; then
				top=$(printf '%s\n%s\n' "${pin_core#v}" "${u#v}" | sort -V | tail -n 1)
				if [ "$top" = "${u#v}" ] || [ ${#pin_core} -eq 40 ]; then newer=true; fi
			fi
		elif [ -n "$u" ]; then
			newer=n/a-floating
		fi
		jq -n --arg name "$name" --arg kind "$kind" --arg pin "$pin" --arg where "$where" --arg up "$u" --arg src "$s" --arg newer "$newer" \
			'{name: $name, kind: $kind, pin: $pin, where: $where, upstream: (if $up == "" then "unknown" else $up end), upstream_source: $src, upstream_ahead: $newer}'
	done
} | jq -s '.' > "$out"
echo "wrote $out: $(jq length "$out") rows, $(jq '[.[] | select(.upstream == "unknown")] | length' "$out") without an answer from upstream"
