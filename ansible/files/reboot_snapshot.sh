#!/usr/bin/env bash
# reboot_snapshot.sh - READ-ONLY record of one cluster host, taken before and after a reboot by
# ansible/reboot.yml (run by ansible.builtin.script, as root). It writes nothing, signals nothing and
# starts and stops nothing: it reads /proc, /sys and a few files, and asks systemctl for state and
# modinfo for a module's presence.
#
# Usage: reboot_snapshot.sh MANIFEST RESULTS_ROOT PLACEMENT_VECTOR LAN_IP PEER_IP
#   MANIFEST          the bench manifest path (/tmp/celeris-bench-manifest.json); present = a run owns the host
#   RESULTS_ROOT      the results tmpfs dir (/tmp/celeris-results); entries in it die with a reboot
#   PLACEMENT_VECTOR  the interrupt NAME bench.yml pins (enp1s0-8), or "-" when the host has none
#   LAN_IP            this host's address on the 20G fabric (inventory lan_ip), or "-"
#   PEER_IP           a peer's fabric address to ping, or "-"
# Environment: HOST_SNAPSHOT_ROOT is a test override, a prefix for every path read. Empty on a host.
#
# stdout: ONE JSON object. Exit 0 whatever it found (a finding is data, not an error); 2 on bad usage.
#
# NIC vectors are found by NAME, never by number: the IRQ numbers change per boot. A vector belongs to a
# NIC when its IRQ is listed under /sys/class/net/<iface>/device/msi_irqs/ or its name starts with
# "<iface>-". "total" is the sum of the per-CPU counts of /proc/interrupts since boot (a 32-bit counter
# per CPU on some kernels, which is why the reboot is wanted at all).
set -u

R=${HOST_SNAPSHOT_ROOT:-}
[ $# -eq 5 ] || { echo "usage: reboot_snapshot.sh MANIFEST RESULTS_ROOT PLACEMENT_VECTOR LAN_IP PEER_IP" >&2; exit 2; }
manifest=$1
results_root=$2
placement_vector=$3
lan_ip=$4
peer_ip=$5

jesc() {
	local s=$1
	s=$(printf '%s' "$s" | tr -d '\000-\010\013-\037')
	s=${s//\\/\\\\}
	s=${s//\"/\\\"}
	printf '%s' "$s"
}
js() { printf '"%s"' "$(jesc "$1")"; }
jbool() { if "$@" > /dev/null 2>&1; then printf 'true'; else printf 'false'; fi; }
rd() { local v=""; [ -r "$1" ] && read -r v < "$1"; printf '%s' "$v"; }
# a JSON array of strings from stdin, one element per line
jlines() {
	local out="" l
	while IFS= read -r l; do
		[ -n "$l" ] || continue
		[ -n "$out" ] && out="$out,"
		out="$out$(js "$l")"
	done
	printf '[%s]' "$out"
}

boot_id=$(rd "$R/proc/sys/kernel/random/boot_id")
kernel=$(rd "$R/proc/sys/kernel/osrelease")
btime=""
if [ -r "$R/proc/stat" ]; then btime=$(awk '$1 == "btime" { print $2; exit }' "$R/proc/stat"); fi
uptime_s=$(awk '{ print int($1); exit }' "$R/proc/uptime" 2> /dev/null)
boot_time=""
case "$btime" in
'' | *[!0-9]*) btime=0 ;;
*) boot_time=$(date -u -d "@$btime" '+%Y-%m-%d %H:%M:%S' 2> /dev/null || true) ;;
esac
case "$uptime_s" in '' | *[!0-9]*) uptime_s=0 ;; esac

# ---- NIC vectors -------------------------------------------------------------------------------------
vectors=""
nics=""
r8127=false
max_total=0
for ifdir in "$R"/sys/class/net/*; do
	[ -e "$ifdir/device" ] || continue # lo, bonds, tailscale0, veth and docker bridges have no device
	iface=${ifdir##*/}
	drv=""
	[ -L "$ifdir/device/driver" ] && drv=$(basename "$(readlink "$ifdir/device/driver")")
	[ "$drv" = r8127 ] && r8127=true
	[ -n "$nics" ] && nics="$nics,"
	nics="$nics{\"iface\":$(js "$iface"),\"driver\":$(js "$drv")}"
	irqs=$(
		{
			ls "$ifdir/device/msi_irqs" 2> /dev/null
			awk -v p="$iface-" '$1 ~ /^[0-9]+:$/ && index($NF, p) == 1 { sub(":", "", $1); print $1 }' "$R/proc/interrupts" 2> /dev/null
		} | sort -un
	)
	for irq in $irqs; do
		row=$(awk -v n="$irq:" '$1 == n { t = 0; for (i = 2; i <= NF; i++) { if ($i ~ /^[0-9]+$/) t += $i; else break } printf "%.0f\t%s\n", t, $NF; exit }' "$R/proc/interrupts" 2> /dev/null)
		[ -n "$row" ] || continue # a vector listed by the device but not requested: not an active vector
		total=${row%%$'\t'*}
		name=${row#*$'\t'}
		aff=$(rd "$R/proc/irq/$irq/smp_affinity_list")
		eff=$(rd "$R/proc/irq/$irq/effective_affinity_list")
		[ "$total" -gt "$max_total" ] 2> /dev/null && max_total=$total
		[ -n "$vectors" ] && vectors="$vectors,"
		vectors="$vectors{\"iface\":$(js "$iface"),\"irq\":$irq,\"name\":$(js "$name"),\"total\":$total,\"affinity\":$(js "$aff"),\"effective\":$(js "$eff")}"
	done
done

placement_json=null
if [ "$placement_vector" != "-" ]; then
	if awk -v n="$placement_vector" '$1 ~ /^[0-9]+:$/ && $NF == n { f = 1 } END { exit !f }' "$R/proc/interrupts" 2> /dev/null; then
		placement_json=true
	else
		placement_json=false
	fi
fi

# ---- bonds ------------------------------------------------------------------------------------------
bonds=""
for b in "$R"/proc/net/bonding/*; do
	[ -r "$b" ] || continue
	bname=${b##*/}
	slaves=$(awk '/^Slave Interface:/ { s = $3 } /^MII Status:/ { if (s != "") { print s "=" $3; s = "" } }' "$b" 2> /dev/null | jlines)
	mode=$(sed -n 's/^Bonding Mode: *//p' "$b" 2> /dev/null | head -n 1)
	[ -n "$bonds" ] && bonds="$bonds,"
	bonds="$bonds{\"bond\":$(js "$bname"),\"mode\":$(js "$mode"),\"slaves\":$slaves}"
done

# ---- fabric -----------------------------------------------------------------------------------------
lan_present=null
if [ "$lan_ip" != "-" ]; then
	lan_present=false
	if command -v ip > /dev/null 2>&1 && ip -4 -o addr show 2> /dev/null | awk '{ split($4, a, "/"); print a[1] }' | grep -qxF -- "$lan_ip"; then
		lan_present=true
	fi
fi
peer_ping=null
if [ "$peer_ip" != "-" ]; then
	peer_ping=false
	if command -v ping > /dev/null 2>&1 && ping -c 3 -W 2 -- "$peer_ip" > /dev/null 2>&1; then
		peer_ping=true
	fi
fi

# ---- the run state a reboot would destroy or must not interrupt ---------------------------------
manifest_present=$(jbool test -e "$R$manifest")
results_entries=$(ls -A "$R$results_root" 2> /dev/null | wc -l | tr -d ' ')
results_list=$(ls -A "$R$results_root" 2> /dev/null | head -n 20 | jlines)
resume_pending=$(jbool test -e "$R/var/lib/celeris-power/resume-pending.json")
runner_dirs=$(ls -d "$R"/tmp/actions-runner-* 2> /dev/null | sed 's|.*/||' | jlines)
listeners=0
workers=0
for c in "$R"/proc/[0-9]*/comm; do
	[ -r "$c" ] || continue
	{ read -r comm < "$c"; } 2> /dev/null || continue
	case "$comm" in
	Runner.Listener) listeners=$((listeners + 1)) ;;
	Runner.Worker) workers=$((workers + 1)) ;;
	esac
done

# ---- crash dumps and the kernel that will boot ----------------------------------------------
crash=$(ls -A "$R/var/crash" 2> /dev/null | jlines)
newest_kernel=$(ls "$R"/boot/vmlinuz-* 2> /dev/null | sed 's|.*/vmlinuz-||' | sort -V | tail -n 1)
modinfo_r8127=null
if [ "$r8127" = true ]; then
	modinfo_r8127=false
	if command -v modinfo > /dev/null 2>&1 && [ -n "$kernel" ] && modinfo -k "$kernel" r8127 > /dev/null 2>&1; then
		modinfo_r8127=true
	fi
fi

# ---- the OS-upgrade pin: a newer kernel installed does not matter to a reboot when grub's saved default IS the running kernel and
# no one-shot is pending (ansible/os-upgrade.yml writes the drop-in and sets the saved entry before it installs anything).
# That is only true if GRUB really reads the saved entry, which nothing but a completed rehearsal shows. ALL of these must hold:
#   1. the drop-in of the OS upgrade exists (GRUB_DEFAULT=saved)
#   2. a rehearsal record exists for the RUNNING kernel, was ok, and was taken with the boot chain (grub*, shim*) at the versions
#      installed now (a GRUB that was upgraded since is a GRUB nobody rehearsed)
#   3. grub.cfg really says set default="${saved_entry}"
#   4. saved_entry is submenu>entry, the entry is a NORMAL (not recovery) entry of the running kernel, and both ids exist in grub.cfg
#   5. no one-shot is pending
# The chain string is the one ansible/files/os_upgrade.py boot_chain() records: installed grub-*, grub2-common and shim* as
# package=version, byte-sorted, joined by "|".
boot_chain() {
	command -v dpkg-query > /dev/null 2>&1 || return 0
	dpkg-query -W -f '${Package}=${Version} ${Status}\n' 'grub-*' 'grub2-common' 'shim*' 2> /dev/null |
		awk '$NF == "installed" { print $1 }' | LC_ALL=C sort | paste -sd'|' -
}
pinned=false
rehearsal_json="$R/var/lib/celeris-os-upgrade/rehearsal.json"
if [ -e "$R/etc/default/grub.d/99-celeris-os-upgrade.cfg" ] && [ -n "$kernel" ] && [ -s "$rehearsal_json" ] && command -v grub-editenv > /dev/null 2>&1; then
	rec=$(cat "$rehearsal_json" 2> /dev/null)
	rec_chain=$(printf '%s' "$rec" | sed -n 's/.*"chain": "\([^"]*\)".*/\1/p')
	cfg="$R/boot/grub/grub.cfg"
	genv=$(grub-editenv list 2> /dev/null)
	saved=$(printf '%s\n' "$genv" | sed -n 's/^saved_entry=//p')
	pending=$(printf '%s\n' "$genv" | sed -n 's/^next_entry=//p')
	parent=${saved%%>*}
	leaf=${saved##*>}
	if printf '%s' "$rec" | grep -qF "\"kernel\": \"$kernel\"" &&
		printf '%s' "$rec" | grep -qF '"ok": true' &&
		[ -n "$rec_chain" ] && [ "$rec_chain" = "$(boot_chain)" ] &&
		[ -r "$cfg" ] && grep -qF 'set default="${saved_entry}"' "$cfg" &&
		[ "$parent" != "$saved" ] && [ -n "$parent" ] && [ -n "$leaf" ] &&
		case "$leaf" in "gnulinux-$kernel-advanced-"*) true ;; *) false ;; esac &&
		grep -E '^[[:space:]]*submenu ' "$cfg" | grep -qF "\$menuentry_id_option '$parent'" &&
		grep -E '^[[:space:]]*menuentry ' "$cfg" | grep -qF "\$menuentry_id_option '$leaf'" &&
		[ -z "$pending" ]; then
		pinned=true
	fi
fi

# ---- the persistent prerequisites a reboot must leave in place (ansible/power-guard.yml) --------
run_celeris_owner=""
[ -d "$R/run/celeris" ] && run_celeris_owner=$(stat -c %U "$R/run/celeris" 2> /dev/null)
quiesce_hook=$(jbool test -x "$R/usr/lib/systemd/system-shutdown/quiesce-r8127.shutdown")
power_event=$(jbool test -x "$R/usr/local/sbin/cluster-power-event")
tmpfiles_conf=$(jbool test -f "$R/etc/tmpfiles.d/celeris-power.conf")
svc_active() { command -v systemctl > /dev/null 2>&1 && [ "$(systemctl is-active "$1" 2> /dev/null)" = active ] && printf true || printf false; }
svc_enabled() { command -v systemctl > /dev/null 2>&1 && [ "$(systemctl is-enabled "$1" 2> /dev/null)" = enabled ] && printf true || printf false; }
# the unit-file state as systemctl prints it (enabled, enabled-runtime, disabled, static, ...): "enabled-runtime"
# lasts only until the next boot, which is exactly the state in which a reboot loses the tailnet
svc_enabled_state() { command -v systemctl > /dev/null 2>&1 && systemctl is-enabled "$1" 2> /dev/null | head -n 1; }
tailscaled=$(svc_active tailscaled)
tailscaled_enabled=$(svc_enabled_state tailscaled)
apcupsd=$(svc_active apcupsd)
resume_check=$(svc_enabled celeris-resume-check.service)
system_state=""
failed_units="[]"
if command -v systemctl > /dev/null 2>&1; then
	system_state=$(systemctl is-system-running 2> /dev/null | head -n 1)
	failed_units=$(systemctl --failed --no-legend --plain 2> /dev/null | awk '{ print $1 }' | jlines)
fi

printf '{"boot_id":%s,"boot_time":%s,"btime":%s,"uptime_s":%s,"kernel":%s,"newest_installed_kernel":%s,' \
	"$(js "$boot_id")" "$(js "$boot_time")" "$btime" "$uptime_s" "$(js "$kernel")" "$(js "$newest_kernel")"
printf '"nics":[%s],"nic_vectors":[%s],"max_vector_total":%s,"placement_vector":%s,"placement_vector_present":%s,"bonds":[%s],' \
	"$nics" "$vectors" "$max_total" "$(js "$placement_vector")" "$placement_json" "$bonds"
printf '"lan_ip":%s,"lan_ip_present":%s,"peer_ip":%s,"peer_ping_ok":%s,' \
	"$(js "$lan_ip")" "$lan_present" "$(js "$peer_ip")" "$peer_ping"
printf '"manifest_present":%s,"results_entries":%s,"results_list":%s,"resume_pending":%s,"runner_dirs":%s,"runner_listeners":%s,"runner_workers":%s,' \
	"$manifest_present" "$results_entries" "$results_list" "$resume_pending" "$runner_dirs" "$listeners" "$workers"
printf '"crash":%s,"r8127":%s,"modinfo_r8127":%s,"grub_pinned_to_running":%s,' "$crash" "$r8127" "$modinfo_r8127" "$pinned"
printf '"prereq":{"tailscaled_active":%s,"tailscaled_enabled":%s,"apcupsd_active":%s,"resume_check_enabled":%s,"power_event_installed":%s,"tmpfiles_conf":%s,"quiesce_hook":%s,"run_celeris_owner":%s},' \
	"$tailscaled" "$(js "$tailscaled_enabled")" "$apcupsd" "$resume_check" "$power_event" "$tmpfiles_conf" "$quiesce_hook" "$(js "$run_celeris_owner")"
printf '"system_state":%s,"failed_units":%s}\n' "$(js "$system_state")" "$failed_units"
