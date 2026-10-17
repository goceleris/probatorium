#!/usr/bin/env bash
# hostgate/collect.sh -- READ-ONLY host state for the pre-tier gate
# (probatorium#473). Run on a cluster host through ansible's `script` module
# (ansible/tasks/host_gate.yml); prints the line format hostgate.Parse reads.
#
# Everything here is a read: /proc, /proc/pressure/io, `ss -ltnp`, `stat`,
# `readlink`. Nothing is signalled, stopped, removed or written, and the
# command word list is pinned by TestGateFilesCannotDisturbACluster. Run as
# root (ansible `become`) so /proc/<pid>/exe and `ss -p` can see every user's
# processes; as a normal user it reports only its own and says so.
#
# Command lines are NOT copied whole: a SUT supervisor is `bash -c <script>`
# and the script embeds the fixture database DSN. A shell shows only its name;
# any other process shows its first words with URL credentials masked.
#
# Records (tab separated):
#   hostgate v1 | host H | epoch N | psi io <raw /proc/pressure/io line>
#   ss <raw ss line> | proc pid ppid user age_s comm exe cwd args | note T
#   end ok        <- last line; its absence means the output was cut short
set -u
LC_ALL=C
export LC_ALL

min_age=${HOSTGATE_MIN_PROC_AGE:-300}

printf 'hostgate\tv1\n'
printf 'host\t%s\n' "$(hostname -s 2>/dev/null || echo unknown)"
printf 'epoch\t%s\n' "$(date +%s)"

if [ "$(id -u)" != 0 ]; then
	printf 'note\tnot root: only this user'"'"'s processes and sockets are visible\n'
fi

if [ -r /proc/pressure/io ]; then
	while IFS= read -r l; do
		printf 'psi\tio\t%s\n' "$l"
	done </proc/pressure/io
else
	printf 'note\tno /proc/pressure/io (kernel without PSI)\n'
fi

if command -v ss >/dev/null 2>&1; then
	timeout 20 ss -H -ltnp 2>/dev/null | while IFS= read -r l; do
		printf 'ss\t%s\n' "$l"
	done
else
	printf 'fatal\tss is not installed; cannot list listeners\n'
fi

clk=$(getconf CLK_TCK 2>/dev/null || echo 100)
up=$(cut -d' ' -f1 /proc/uptime 2>/dev/null)
up=${up%%.*}
case "$up$clk" in
'' | *[!0-9]*) printf 'fatal\tcannot read /proc/uptime\n' ;;
esac

for d in /proc/[0-9]*; do
	pid=${d#/proc/}
	exe=$(readlink "$d/exe" 2>/dev/null) || continue
	[ -n "$exe" ] || continue # kernel thread, or not ours to read
	stat=$(cat "$d/stat" 2>/dev/null) || continue
	comm=${stat#*(}
	comm=${comm%)*}
	rest=${stat##*) }
	# shellcheck disable=SC2086
	set -- $rest # state ppid pgrp session tty tpgid flags minflt cminflt majflt cmajflt utime stime cutime cstime priority nice threads itreal starttime
	[ "$#" -ge 20 ] || continue
	ppid=$2
	start=${20}
	case "$ppid$start" in '' | *[!0-9]*) continue ;; esac
	age=$((up - start / clk))
	[ "$age" -ge "$min_age" ] || continue
	cwd=$(readlink "$d/cwd" 2>/dev/null || echo '?')
	user=$(stat -c %U "$d" 2>/dev/null || echo '?')
	case "${exe##*/}" in
	bash | sh | dash | zsh | ksh) args="${exe##*/} [shell arguments elided]" ;;
	*) args=$(tr '\0' ' ' <"$d/cmdline" 2>/dev/null | cut -d' ' -f1-4 | sed -E 's#(://[^:/@ ]+:)[^@ ]+@#\1***@#g') ;;
	esac
	# One record per line: no tabs, no newlines in any field.
	clean() { printf '%s' "$1" | tr '\t\r\n' '   ' | cut -c1-200; }
	printf 'proc\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
		"$pid" "$ppid" "$(clean "$user")" "$age" "$(clean "$comm")" "$(clean "$exe")" "$(clean "$cwd")" "$(clean "$args")"
done

printf 'end\tok\n'
