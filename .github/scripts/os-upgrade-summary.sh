#!/usr/bin/env bash
# Renders the evidence of one ansible/os-upgrade.yml run (the directory the workflow gives it as os_out_dir) as a
# markdown job summary. Read-only (it also writes <host>/package-diff.txt next to the evidence); it works on whatever
# the play managed to write, so a run that failed half way still gets a summary.
#
# usage: os-upgrade-summary.sh OUT_DIR MODE HOST ANSIBLE_RC "REASON" RUN_ID [CONFIRM]
#   CONFIRM (optional) is the typed confirmation: a rehearsal's summary says whether it attested a console at the rack
#   MODE survey | rehearse | upgrade | disarm | repair; HOST is "-" for a survey
set -u
out=${1:?out dir}
mode=${2:?mode}
host=${3:?host}
rc=${4:?ansible rc}
reason=${5:-}
run_id=${6:-}
confirm=${7:-}
safe_reason=${reason//\`/\'}

if ! command -v jq > /dev/null 2>&1; then
	echo "## Cluster OS $mode"
	echo
	echo "(jq is not installed: the records are in the artifact)"
	exit 0
fi

hosts="msa2-server msr1 msa2-client"
# The package-manager programs that count as a package run (os_upgrade.py PKG_RUN_NAMES, vars/os_upgrade_judge.yml os_busy_process_names:
# a test compares the three lists). A survey recorded by the first release of the tool also listed the idle daemons `unattended-upgr`
# and `packagekitd` (kernel comm names): they are not package runs, and are never shown as busy.
busy_names="apt apt-get aptitude dpkg unattended-upgrade apt.systemd.daily dkms update-grub grub-mkconfig update-initramfs"
j() { jq -r "$1" "$2" 2> /dev/null; }

survey_section() {
	local s=$out/$1/survey.json
	[ -s "$s" ] || { echo "- $1: no survey record"; return; }
	echo "### $1"
	echo
	jq -r --arg busy "$busy_names" '
		"- OS: \(.os_release.PRETTY_NAME // "?") (\(.arch)); running kernel `\(.kernel)`; installed: \(.installed_kernels | join(", ")); newest of the same flavor: `\(.newest_same_flavor)`",
		"- grub: saved default `\(.boot.default_kernel)` (what a power-cycle boots when GRUB reads the grubenv, see the boot proof), GRUB'"'"'s first menu entry `\(.boot.entry0_kernel // "unknown")` (what it boots when it does not), saved_entry `\(.boot.saved_entry)`, GRUB_DEFAULT=`\(.boot.settings.GRUB_DEFAULT // "unset")`, saved logic in grub.cfg: \(.boot.cfg_uses_saved), EFI: \(.boot.efi), Secure Boot: \(.boot.secure_boot), /boot/grub on `\(.boot.boot_fs)` (`\(.boot.boot_source)`), one-shot tools: grub-reboot \(.boot.grub_reboot) grub-set-default \(.boot.grub_set_default) grub-editenv \(.boot.grub_editenv) update-grub \(.boot.update_grub), recordfail timeout `\(.boot.cfg_recordfail_timeout)`",
		"- **next boot** (what a power-cycle or the next reboot gives): \(if .boot.next_entry != "" then "the ONE-SHOT entry `" + .boot.next_entry + "` (a run armed it and the host has not booted it: dispatch mode=disarm, or expect that kernel once)" else "grub saved default `" + (.boot.default_kernel | if . == "" then "unknown" else . end) + "` (a GRUB that ignores the grubenv boots its first menu entry `" + (.boot.entry0_kernel // "unknown") + "` instead)" end); upgrade drop-in present: \(.boot.dropin_present); kernel packages newer than the running kernel: \(.packages.new_kernel_pkgs | join(", ") | if . == "" then "none" else . end); of which half-installed or recorded as failed: \(.packages.broken_kernel_pkgs | join(", ") | if . == "" then "none" else . end)",
		"- **GRUB boot proof** (every one-shot and fallback depends on it): \(if .boot.proof.ok then "OK" else "NOT PROVEN: " + (.boot.proof.problems | join(" / ")) end); BOOT_IMAGE `\(.boot.proof.boot_image)`, EFI loader `\(.boot.proof.efi_loader)`, GRUB prefix `\(.boot.proof.prefix)`",
		"- apt (lists \(.packages.lists_age_hours // "?") h old): would install/upgrade \(.packages.simulation.inst | length) package(s) (kernel packages: \(.packages.simulation.kernel_inst | join(", ") | if . == "" then "none" else . end)); would REMOVE: \(.packages.simulation.remv | join(", ") | if . == "" then "none" else . end); kept back: \(.packages.kept_back | join(", ") | if . == "" then "none" else . end); held back by phasing: \(.packages.phased_held_back | join(", ") | if . == "" then "none" else . end)",
		"- held: \(.packages.held | join(", ") | if . == "" then "none" else . end) (kernel/boot/dkms: \(.packages.held_kernel | join(", ") | if . == "" then "none" else . end)); third-party repos: \(.packages.third_party_repos | join(", ") | if . == "" then "none" else . end); pin files: \(.packages.pin_files | join(", ") | if . == "" then "none" else . end)",
		"- dpkg: audit \(.packages.dpkg_audit | length) finding(s), lock held \(.packages.dpkg_lock_held)\(.packages.locks_held // [] | if length > 0 then " (" + join(", ") + ")" else "" end), package runs `\(.packages.package_processes | map(select(IN($busy | split(" ")[]))) | join(", ") | if . == "" then "none" else . end)`, policy-rc.d present \(.packages.policy_rc_d_present)",
		"- unattended-upgrades: installed \(.unattended_upgrades.installed), periodic `\(.unattended_upgrades.unattended_upgrade)`, removes unused kernels `\(.unattended_upgrades.remove_unused_kernel_packages)`, automatic reboot `\(.unattended_upgrades.automatic_reboot)`, needrestart \(.unattended_upgrades.needrestart_installed); units: \(.unattended_upgrades.units | to_entries | map("\(.key)=\(.value.active)/\(.value.enabled)") | join(", "))",
		"- reboot-required: \(.reboot_required) \(.reboot_required_pkgs | join(" "))",
		"- disk free (KiB): / \(.disk["/"].free_kib // "?"), /boot \(.disk["/boot"].free_kib // "?"), /boot/efi \(.disk["/boot/efi"].free_kib // "n/a"), /var \(.disk["/var"].free_kib // "?")",
		"- time sync: NTPSynchronized=`\(.time_sync.raw)`; crash dumps: \(.crash | join(", ") | if . == "" then "none" else . end); linux-firmware `\(.firmware.linux_firmware)`",
		"- dkms: \(.dkms.status | map("\(.name)/\(.version) \(.kernel) \(.state)") | join("; ") | if . == "" then "none" else . end)",
		"- last OS-upgrade record: \(.marker | if . == {} then "none" else "run \(.run_id): \(.kernel_before) -> \(.kernel_after), permanent \(.permanent), \(.finished_utc)" end)",
		"- NOT stopped by a freeze: tailscale auto-update `\(.self_updaters.tailscale_auto_update // "not recorded")` (check `\(.self_updaters.tailscale_check_updates // "not recorded")`), snapd refresh `\(.self_updaters.snap_refresh // "not recorded")`; also fwupd, motd-news and ubuntu-pro-client timers where installed",
		"- freeze: \(.freeze // {} | if . == {} then "not frozen: the apt timers and unattended-upgrades run, so the platform can change by itself" else "run \(.run_id) at \(.utc), status \(.status); stopped or disabled: \(.units | to_entries | map(select(.value.changes | length > 0) | "\(.key) (\(.value.changes | join("+")))") | join(", ") | if . == "" then "nothing" else . end); needrestart list-only drop-in: \(.needrestart.dropin_written); platform: \(.platform.kernel // "?"), \(.platform.packages // "?") packages, sha256 \((.platform.dpkg_sha256 // "?")[0:16]), holds \(.platform.held // [] | join(",") | if . == "" then "none" else . end); drifted since: \(.drifted // [] | join("; ") | if . == "" then "no" else . end)" end)"
	' "$s"
	local ps=$out/$1/preflight-snapshot.json
	if [ -s "$ps" ]; then
		jq -r '"- run state: boot `\(.boot_time)` (up \(.uptime_s) s), manifest present \(.manifest_present), runner jobs executing \(.runner_workers), results_root entries \(.results_entries), power-loss resume pending \(.resume_pending), failed units: \(.failed_units | join(", ") | if . == "" then "none" else . end)"' "$ps"
	fi
	echo
	echo "| NIC | driver (version) | PCI id | link Mb/s | state |"
	echo "|---|---|---|---|---|"
	jq -r '.nics[] | "| \(.iface) | \(.driver) (\(.driver_version)) | \(.pci_id) | \(.speed_mbps) | \(.operstate) |"' "$s"
	if jq -e '.r8127.present' "$s" > /dev/null 2>&1; then
		echo
		echo "**r8127 (out-of-tree) against the in-tree r8169, per kernel**"
		echo
		jq -r '"- r8127: loaded version `\(.r8127.loaded_version)`, source `\(.r8127.source)` (dkms: \(.r8127.dkms | map("\(.version) \(.kernel) \(.state)") | join("; ")); source dirs: \(.r8127.dkms_source_dirs | join(", "))); PCI id(s) \(.r8127.pci_ids | join(", ")); bound: \(.r8127.bound | to_entries | map("\(.key)=\(.value)") | join(", ")); r8169 blacklisted: \(.r8127.r8169_blacklisted); modprobe lines: \(.r8127.modprobe_lines | join(" / ") | if . == "" then "none" else . end)",
			"- firmware: rtl_nic files \(.r8127.firmware_rtl_nic_files), RTL8127 blobs: \(.r8127.firmware_rtl8127_files | join(", ") | if . == "" then "none" else . end)"' "$s"
		echo
		echo "| kernel | r8127 built (version) | in-tree r8169 present | r8169 lists the PCI id | modules claiming the id |"
		echo "|---|---|---|---|---|"
		jq -r '.r8127.per_kernel | to_entries[] | .key as $k | .value as $v | "| `\($k)` | \($v.r8127.found) (\($v.r8127.version)) \($v.r8127.dkms_state | join("/")) | \($v.r8169.found or $v.r8169.builtin) | \($v.intree_lists_id | to_entries | map("\(.key)=\(.value)") | join(", ")) | \($v.claims | to_entries | map(.value | join("+")) | join(", ")) |"' "$s"
		echo
		jq -r '(.newest_same_flavor) as $n | (.r8127.per_kernel[$n] // null) as $v
			| if $v == null then "- **in-tree driver as an option on the newest installed kernel**: unknown (no record for `\($n)`)"
			  elif ($v.intree_lists_id | to_entries | all(.value)) then "- **in-tree driver as an option on `\($n)`: the in-tree r8169 of that kernel LISTS the PCI id** (and, with r8127 also installed, which driver binds is a race unless r8169 is blacklisted). Not a decision of this workflow: README.md and PLAN.md \"r8127: out-of-tree vs in-tree\" say what to test first."
			  else "- **in-tree driver as an option on `\($n)`: NO, its r8169 does not list the PCI id**; r8127 stays the only driver." end' "$s"
	fi
	echo
}

# The checklist: every fact a person must read BEFORE a host-changing dispatch, with a mark and the decision it informs.
# Thresholds are the judge's (ansible/vars/os_upgrade_judge.yml; a test compares them). PASS / FAIL / NOTE (a human reads it).
root_min=6291456
boot_min=524288
efi_min=32768
checklist() {
	echo "### Checklist: what to read before any host-changing dispatch"
	echo
	echo "PASS = fine. FAIL = the upgrade or the rehearsal will be refused, or would be unsafe: fix or decide first. NOTE = a person reads it and decides, or a step that has not been done yet (the rehearsal)."
	echo
	echo "| host | check | value | mark | decision it informs |"
	echo "|---|---|---|---|---|"
	for h in $hosts; do
		local sv=$out/$h/survey.json ps=$out/$h/preflight-snapshot.json
		if [ ! -s "$sv" ]; then echo "| $h | survey record | none | FAIL | nothing can be decided for this host |"; continue; fi
		[ -s "$ps" ] || ps=/dev/null
		jq -r --arg h "$h" --arg busy "$busy_names" --argjson rootmin "$root_min" --argjson bootmin "$boot_min" --argjson efimin "$efi_min" --slurpfile snap <(if [ -s "$ps" ]; then cat "$ps"; else echo '{}'; fi) '
			def row($c; $v; $m; $d): "| \($h) | \($c) | \($v | tostring | gsub("\\|"; "/")) | **\($m)** | \($d) |";
			def gib($k): (((.disk[$k].free_kib // 0) / 1048576 * 10) | floor / 10);
			($snap[0] // {}) as $sn
			| (.boot.proof) as $pf
			| .packages as $pk
			| (.packages.simulation.inst | map(select(.name | test("^(docker|containerd|runc|tailscale|openssh-server|grub|shim|netplan|systemd$|linux-firmware)"))) | map("\(.name) \(.old) -> \(.new)") | join("; ")) as $access
			| ($pk.package_processes | map(select(IN($busy | split(" ")[])))) as $busyprocs
			| ($pk.locks_held // []) as $locks
			| .newest_same_flavor as $nsf
			| .kernel as $krun
			| (.r8127.per_kernel[.newest_same_flavor] // null) as $nk
			| (.packages.simulation.kernel_inst | map(select(test("^linux-image-[0-9]"))) ) as $kimg
			| (
			row("GRUB booted this host and reads the grubenv Linux writes"; (if $pf.ok then "BOOT_IMAGE \($pf.boot_image); loader \($pf.efi_loader); prefix \($pf.prefix); stub fs \($pf.stub_uuid) = /boot/grub fs \($pf.boot_uuid)" else ($pf.problems | join(" / ")) end); (if $pf.ok then "PASS" else "FAIL" end); "if FAIL, every rehearsal and upgrade is refused; a one-shot cannot be trusted"),
			row("no one-shot pending (next_entry)"; (if .boot.next_entry == "" then "none" else .boot.next_entry end); (if .boot.next_entry == "" then "PASS" else "FAIL" end); "if FAIL, mode=disarm first (an upgrade refuses)"),
			row("upgrade pin vs the running kernel"; "drop-in \(.boot.dropin_present); saved default \(.boot.default_kernel | if . == "" then "unset" else . end); running \(.kernel)"; (if (.boot.dropin_present | not) then "NOTE" elif .boot.default_kernel == .kernel then "PASS" else "FAIL" end); "NOTE: not pinned yet, the first step pins. FAIL: the host runs a kernel other than its saved default: reboot it back (cluster-reboot) first"),
			row("kernel a GRUB that ignores the grubenv would boot (first menu entry)"; "first entry `\(.boot.entry0_kernel | if . == "" then "unknown" else . end)`; running `\(.kernel)`; saved default `\(.boot.default_kernel | if . == "" then "unset" else . end)`"; (if .boot.entry0_kernel == .kernel then "PASS" else "FAIL" end); "FAIL = a rehearsal is refused (it would boot that kernel if GRUB does not read the grubenv, on every power-cycle after it) unless the dispatcher attests a person at the rack with a console (README, Rehearsal at a console)"),
			row("boot filesystem of /boot/grub"; "\(.boot.boot_fs) on \(.boot.boot_source) \(.boot.boot_stack)"; (if .boot.boot_stack == "" and (.boot.boot_fs | IN("ext4","vfat","xfs","ext3","ext2")) then "PASS" else "FAIL" end); "if FAIL, grub cannot write its environment block there, so no one-shot"),
			row("GRUB_DEFAULT and the recordfail timeout"; "GRUB_DEFAULT=\(.boot.settings.GRUB_DEFAULT // "unset"); menu timeout after a failed boot \(.boot.cfg_recordfail_timeout | if . == "" then "n/a" else . end) s"; (if .boot.cfg_recordfail_timeout == "-1" then "FAIL" else "PASS" end); "-1 = a failed boot waits at the menu for ever (the pin sets 5 s)"),
			row("apt would remove nothing protected"; ($pk.simulation.remv | join(", ") | if . == "" then "no removals" else . end) + (if ($pk.simulation.protected_removals | length) > 0 then "; PROTECTED: " + ($pk.simulation.protected_removals | join(", ")) else "" end); (if ($pk.simulation.protected_removals | length) == 0 and $pk.simulation.rc == 0 then "PASS" else "FAIL" end); "if FAIL, the upgrade refuses before it changes a package"),
			row("dpkg clean, nothing held, apt idle, no policy-rc.d"; "audit \($pk.dpkg_audit | length); held kernel/boot \($pk.held_kernel | join(",")); package runs \($busyprocs | join(",") | if . == "" then "none" else . end); locks held \(if ($locks | length) > 0 then ($locks | join(",")) elif $pk.dpkg_lock_held then "yes" else "none" end); policy-rc.d \($pk.policy_rc_d_present)"; (if ($pk.dpkg_audit | length) == 0 and ($pk.held_kernel | length) == 0 and ($busyprocs | length) == 0 and ($locks | length) == 0 and ($pk.dpkg_lock_held | not) and ($pk.policy_rc_d_present | not) then "PASS" else "FAIL" end); "if FAIL, the upgrade refuses (a broken dpkg: mode=repair; apt or dpkg busy: an unattended upgrade is running, wait for it)"),
			row("the way in is intact today (sshd -t, ssh/tailscaled enabled, netplan generate)"; (.access_problems | join(" / ") | if . == "" then "ok" else . end); (if (.access_problems | length) == 0 then "PASS" else "FAIL" end); "if FAIL, an upgrade would change every package and then refuse to reboot, for ever"),
			row("disk free"; "/ \(gib("/")) GiB (min \($rootmin / 1048576)); /boot \(gib("/boot")) GiB (min \($bootmin / 1048576 * 1000 | floor / 1000)); /boot/efi \(.disk["/boot/efi"].free_kib // "n/a") KiB (min \($efimin))"; (if ((.disk["/"].free_kib // 0) >= $rootmin) and ((.disk["/boot"].free_kib // 0) >= $bootmin) and ((.disk["/boot/efi"] == null) or (.disk["/boot/efi"].free_kib >= $efimin)) then "PASS" else "FAIL" end); "if FAIL, three kernels and their initrds do not fit: the upgrade refuses"),
			row("clock synchronized"; .time_sync.raw; (if .time_sync.ntp_synchronized then "PASS" else "NOTE" end); "the verification compares it before and after"),
			row("a new kernel is on offer"; ($kimg | join(", ") | if . == "" then (if $nsf != $krun then "none offered by apt, but `\($nsf)` is ALREADY INSTALLED and newer than the running `\($krun)`: it is the target of an upgrade (the upgrade reboots into it, once)" else "none (a rehearsal-only window; the upgrade would reboot into the same kernel)" end) else . end); (if ($kimg | length) == 1 then "PASS" else "NOTE" end); "exactly one new ABI expected; the target is the newest installed kernel of the running flavor"),
			row("the kernel version parses (X.Y.Z-N-flavor)"; .kernel; (if (.kernel | test("^[0-9]+\\.[0-9]+\\.[0-9]+-[0-9]+-[a-z0-9-]+$")) then "PASS" else "FAIL" end); "if FAIL, the ordering of kernels is unreliable (a vendor kernel name)"),
			row("DKMS modules installed for the running kernel"; (.dkms.installed_for_running | join(", ") | if . == "" then "none" else . end); (if .r8127.present then (if (.dkms.installed_for_running | index("r8127")) != null then "PASS" else "FAIL" end) else (if (.dkms.installed_for_running | length) == 0 then "PASS" else "NOTE" end) end); "an r8127 host must show r8127 here; a server with an in-tree NIC should show none"),
			row("one-shot rehearsal done on the running kernel"; (.rehearsal | if . == {} then "none" else "\(.kernel) by run \(.run_id) at \(.utc)" + (if (.valid // false) then "" else "; STALE: \(.stale // "unknown")" end) end); (if (.rehearsal.valid // false) and .rehearsal.kernel == .kernel then "PASS" else "NOTE" end); "REQUIRED before an upgrade (the upgrade is refused without a valid one). mode=rehearse tests that GRUB reads and clears the grubenv Linux writes; it goes stale when grub or shim change"),
			row("kernels armed earlier and never verified"; ([.armed_kernels[] | "\(.kernel) (run \(.run_id))"] | join(", ") | if . == "" then "none" else . end); (if (.armed_kernels | length) == 0 then "PASS" else "FAIL" end); "if FAIL, the host may have been stranded by it; retry_kernel=<it> to try again, or mode=repair abort-new-kernel"),
			row("run state"; "manifest \($sn.manifest_present // "?"); runner jobs \($sn.runner_workers // "?"); power-loss resume \($sn.resume_pending // "?"); crash dumps \(.crash | length)"; (if ($sn | length) == 0 then "NOTE" elif ($sn.manifest_present // false) or (($sn.runner_workers // 0) > 0) or ($sn.resume_pending // false) then "FAIL" else "PASS" end); "NOTE: no preflight snapshot of this host, so nothing is known about a run on it. If FAIL, the upgrade refuses beside a run"),
			row("access-path packages and third-party moves the full-upgrade brings"; ($access | if . == "" then "none" else . end) + "; third-party repos: " + ($pk.third_party_repos | join(", ") | if . == "" then "none" else . end); "NOTE"; "read the major jumps (docker, containerd, tailscale, openssh, grub, shim, netplan, systemd, linux-firmware): a full-upgrade has no checkpoint, and none of them is undone by a power-cycle"),
			row("platform frozen against apt self-updates (apt timers, unattended-upgrades, needrestart restarts); NOT covered: tailscale auto-update, snapd refresh, fwupd, motd-news, ubuntu-pro-client"; "tailscale auto-update \(.self_updaters.tailscale_auto_update // "not recorded"), snap refresh \(.self_updaters.snap_refresh // "not recorded"); units \(.unattended_upgrades.units | to_entries | map("\(.key)=\(.value.active)/\(.value.enabled)") | join(", "))" + (if (.freeze // {}) != {} then "; freeze of run \(.freeze.run_id) at \(.freeze.utc), status \(.freeze.status); drifted: \(.freeze.drifted // [] | join("; ") | if . == "" then "no" else . end)" else "; no freeze record" end); (if (.freeze // {}) == {} then "NOTE" elif .freeze.status == "frozen" and ((.freeze.drifted // []) | length) == 0 then "PASS" else "FAIL" end); "NOTE: not frozen, the host updates itself (unattended-upgrades already installed the kernel -38 by itself): dispatch mode=freeze before the evidence campaign. FAIL: frozen, but a unit came back, the PLATFORM it recorded changed (the running kernel, the package list, the holds or the grub saved default: an upgrade, tailscale or a manual apt, a reboot onto another kernel) or the freeze did not complete: mode=thaw, fix, then mode=freeze again (an upgrade is refused on a frozen host: thaw, upgrade, freeze again)"),
			row("unattended-upgrades and phasing"; "periodic \(.unattended_upgrades.unattended_upgrade); removes unused kernels \(.unattended_upgrades.remove_unused_kernel_packages); phased and held back: \($pk.phased_held_back | join(", ") | if . == "" then "none" else . end)"; "NOTE"; "the pin keeps the old kernel from unattended removal; phased packages are not forced (PLAN d.4, d.5)")
			),
			(if .r8127.present then (
			row("r8127: where the module comes from"; "\(.r8127.source); loaded \(.r8127.loaded_version); dkms \(.r8127.dkms | map("\(.version) \(.kernel) \(.state)") | join("; "))"; (if (.r8127.source | startswith("package:")) or .r8127.source == "dkms-source-without-package" then "PASS" else "FAIL" end); "manual-build or none: apt runs and the reboot is then refused (nothing to rebuild for the new kernel)"),
			row("r8127: firmware blob rtl8127a-1.fw"; (.r8127.firmware_rtl8127_files | join(", ") | if . == "" then "absent" else . end); (if (.r8127.firmware_rtl8127_files | length) > 0 then "PASS" else "NOTE" end); "absent: the in-tree driver (if it binds) would run without PHY fixes"),
			row("r8127: Secure Boot and the signer of the running module"; "Secure Boot \(.boot.secure_boot); signer `\(.r8127.per_kernel[.kernel].r8127.signer // "")`"; (if .boot.secure_boot == "enabled" and ((.r8127.per_kernel[.kernel].r8127.signer // "") == "") then "FAIL" else "NOTE" end); "enabled with an unsigned running module is impossible to reproduce: the new build must be signed by the same key"),
			row("r8127: the r8169 decision (does the newest kernel'"'"'s in-tree driver list 10ec:8127?)"; (if $nk == null then "no record for the newest kernel" else "newest `\(.newest_same_flavor)`: lists the id \($nk.intree_lists_id | to_entries | map("\(.key)=\(.value)") | join(",")); r8169 blacklisted \(.r8127.r8169_blacklisted); running kernel lists it \(.r8127.per_kernel[.kernel].intree_lists_id | to_entries | map(.value) | any)" end); (if $nk == null then "NOTE" elif ($nk.intree_lists_id | to_entries | map(.value) | any) and (.r8127.r8169_blacklisted | not) then "ACTION" else "PASS" end); "ACTION: dispatch the first upgrade with blacklist_r8169=true (decided beforehand), or the reboot is refused and the window wasted")
			) else empty end)
		' "$sv"
	done
	echo
}

host_table() {
	echo "| host | OS | running kernel | NEXT BOOT | newest same flavor | upgrades / removals / kept back | reboot-required | disk free / and /boot (GiB) | NTP | crash dumps |"
	echo "|---|---|---|---|---|---|---|---|---|---|"
	for h in $hosts; do
		s=$out/$h/survey.json
		[ -s "$s" ] || { echo "| $h | (no record) | | | | | | | | |"; continue; }
		jq -r --arg h "$h" '"| \($h) | \(.os_release.PRETTY_NAME // "?") | `\(.kernel)` | `\(if .boot.next_entry != "" then "one-shot: " + .boot.next_entry else .boot.default_kernel end)` | `\(.newest_same_flavor)` | \(.packages.simulation.inst | length) / \(.packages.simulation.remv | length) / \(.packages.kept_back | length) | \(.reboot_required) | \(((.disk["/"].free_kib // 0) / 1048576) | floor) / \(((.disk["/boot"].free_kib // 0) / 1048576 * 10) | floor / 10) | \(.time_sync.raw) | \(.crash | length) |"' "$s"
	done
}

echo "## Cluster OS $mode: $host"
echo
echo "- reason: \`$safe_reason\`"
echo "- workflow run: $run_id"
if [ "$mode" = rehearse ]; then
	case "$confirm" in
	*-CONSOLE) echo "- confirmation: **CONSOLE ATTESTED**: the dispatcher stated that a person is at the rack of $host with a console. A person approving this run should know that." ;;
	"") echo "- confirmation: not recorded" ;;
	*) echo "- confirmation: no console attested" ;;
	esac
fi

if [ "$mode" = survey ]; then
	if [ "$rc" -eq 0 ]; then echo "- **result: SURVEYED** (read-only; nothing on any host was changed, except, when refresh_lists was set, what apt-get update writes: /var/lib/apt/lists, /var/cache/apt/*.bin and its post-invoke hooks)"; else echo "- **result: SURVEY FAILED** (ansible-playbook exit $rc): the hosts that answered have a record below"; fi
	echo
	checklist
	host_table
	echo
	for h in $hosts; do survey_section "$h"; done
	if [ -s "$out/toolchains.json" ]; then
		echo "### Toolchains the roles install: pinned or floating, against upstream"
		echo
		echo "Floating tools are resolved afresh at every deploy (and live in tmpfs: a reboot wipes them), so the OS upgrade needs no action for them. Pinned ones change only by a reviewed commit."
		echo
		echo "| tool | kind | pin | newest upstream | upstream ahead of the pin | pinned in | upstream source |"
		echo "|---|---|---|---|---|---|---|"
		jq -r '.[] | "| \(.name) | \(.kind) | `\(.pin)` | `\(.upstream)` | \(.upstream_ahead) | \(.where) | \(.upstream_source) |"' "$out/toolchains.json"
		echo
	fi
	for r in "$out"/*/refusal.txt; do
		[ -s "$r" ] || continue
		echo "### Refused: $(basename "$(dirname "$r")")"
		echo
		echo '```'
		cat "$r"
		echo '```'
	done
	exit 0
fi

# ---------------------------------------------------------------- disarm
if [ "$mode" = disarm ]; then
	d=$out/$host
	if [ "$rc" -eq 0 ] && [ -s "$d/result.json" ]; then echo "- **result: DISARMED**"; else echo "- **result: NOT DISARMED** (ansible-playbook exit $rc)"; fi
	echo
	if [ -s "$d/disarm.json" ]; then
		jq -r '"- one-shot pending before: `\(.next_entry_before)` (was armed: \(.was_armed)); after: `\(.next_entry_after)`; grub saved default: `\(.saved_entry)`"' "$d/disarm.json"
	fi
	if [ -s "$d/result.json" ]; then
		jq -r '"- running kernel `\(.kernel_running)`; saved default `\(.saved_entry)`; \(.power_cycle)"' "$d/result.json"
	fi
	if [ -n "$(jq -r '.forgot_armed[0] // ""' "$d/disarm.json" 2> /dev/null)" ]; then
		echo "- **The host has not rebooted since the arming** (same boot id): the armed record of $(jq -r '.forgot_armed | join(", ")' "$d/disarm.json") was dropped, that kernel never booted, so the next upgrade is not refused for a boot that never happened."
	fi
	echo "- Disarming clears only grub's next_entry. It does not touch the saved default, the packages or the running kernel. If the host did not answer, nothing was cleared: it is down, and a power-cycle boots whatever next_entry names before the saved default (after an upgrade's arming: the NEW kernel, once; after a rehearsal's arming: the running kernel again)."
	for r in "$out"/*/refusal.txt; do
		[ -s "$r" ] || continue
		echo
		echo "### Refused: $(basename "$(dirname "$r")")"
		echo
		echo '```'
		cat "$r"
		echo '```'
	done
	exit 0
fi

# ---------------------------------------------------------------- freeze / thaw
if [ "$mode" = freeze ] || [ "$mode" = thaw ]; then
	d=$out/$host
	ok=$(jq -r '.freeze_ok // false' "$d/result.json" 2> /dev/null)
	if [ "$mode" = freeze ]; then
		if [ "$rc" -eq 0 ] && [ "$ok" = true ]; then echo "- **result: FROZEN**: the apt timers and unattended-upgrades are stopped and disabled on $host (needrestart restarts nothing by itself), and the platform below is recorded. Nothing was rebooted; no package changed."; else echo "- **result: NOT FROZEN, OR NOT COMPLETELY** (ansible-playbook exit $rc): the record of what was changed is kept on the host; mode=thaw undoes a freeze that failed half way."; fi
	else
		if [ "$rc" -eq 0 ] && [ "$ok" = true ]; then echo "- **result: THAWED**: the units the freeze recorded it changed are back as they were (they may run within minutes: the apt timers are persistent), and the needrestart drop-in is gone. The host updates itself again."; else echo "- **result: NOT THAWED, OR NOT COMPLETELY** (ansible-playbook exit $rc): the freeze record is kept; dispatch mode=thaw again."; fi
	fi
	echo
	if [ -s "$d/result.json" ]; then
		jq -r --arg mode "$mode" '"- status `\(.status)`; running kernel `\(.kernel_running)` (not rebooted)\(if (.error // "") != "" then "; error: " + .error else "" end)",
			(.freeze as $f | if ($f // {}) == {} then empty else
			"- record of run \($f.run_id) at \($f.utc) (previous states, what was changed, what it is now):",
			($f.units | to_entries[] | "  - `\(.key)`: present \(.value.present); before \(.value.before.active)/\(.value.before.enabled); changed: \(.value.changes | join(" + ") | if . == "" then "nothing" else . end)\(if $mode == "freeze" then "; now " + ($f.after[.key].active // "?") + "/" + ($f.after[.key].enabled // "?") else "" end)"),
			(if $mode == "thaw" then "- restored: \($f.restored // [] | join("; ") | if . == "" then "nothing" else . end)" else empty end),
			"- needrestart: installed \($f.needrestart.installed); restart mode before `\($f.needrestart.mode_before)`; list-only drop-in written: \($f.needrestart.dropin_written)",
			"- **platform** (what the release results can cite): kernel `\($f.platform.kernel // "?")`, \($f.platform.os // "?"), \($f.platform.packages // "?") packages, sha256 of the package list `\($f.platform.dpkg_sha256 // "?")`, holds: \($f.platform.held // [] | join(", ") | if . == "" then "none" else . end)" end)' "$d/result.json" 2> /dev/null
		if [ "$mode" = thaw ]; then
			jq -r 'if (.drifted_at_thaw // []) | length == 0 then "- **platform at thaw time: DID NOT MOVE** since the freeze (running kernel, grub saved default, package list SHA-256, holds, units): the end-of-campaign proof" else "- **PLATFORM MOVED since the freeze** (found at thaw time): \(.drifted_at_thaw | join("; ")). The results of the campaign do not name this host as frozen" end' "$d/result.json" 2> /dev/null
		else
			jq -r '"- drifted after: \(.drifted_after // [] | join("; ") | if . == "" then "nothing" else . end)"' "$d/result.json" 2> /dev/null
		fi
	fi
	echo "- The evidence directory holds freeze.json (the record, or the thaw's), dpkg-l.txt (dpkg -l), apt-held.txt (apt-mark showhold), kernel.txt and the survey after (post-survey.json): the exact platform. A freeze covers the apt timers, unattended-upgrades and needrestart's automatic restarts only: snapd refreshes, motd-news, firmware updates and anything else a host does by itself are not touched."
	for r in "$out"/*/refusal.txt; do
		[ -s "$r" ] || continue
		echo
		echo "### Refused: $(basename "$(dirname "$r")")"
		echo
		echo '```'
		cat "$r"
		echo '```'
	done
	exit 0
fi

# ---------------------------------------------------------------- repair
if [ "$mode" = repair ]; then
	d=$out/$host
	if [ "$rc" -eq 0 ] && [ "$(jq -r '.status // ""' "$d/result.json" 2> /dev/null)" = repaired ]; then echo "- **result: REPAIRED**"; else echo "- **result: NOT REPAIRED** (ansible-playbook exit $rc)"; fi
	echo
	if [ -s "$d/result.json" ]; then
		jq -r '"- action `\(.repair_action)`; running kernel `\(.kernel_running)` (not rebooted); grub saved default `\(.saved_entry)`; one-shot pending: `\(.next_entry_after)`",
			"- dpkg --audit after: \(.dpkg_audit_after | length) finding(s) \(.dpkg_audit_after | join(" / ")); kernel packages newer than the running kernel still installed: \(.new_kernel_pkgs_after | join(", ") | if . == "" then "none" else . end)"' "$d/result.json"
	fi
	if [ -n "$(jq -r '.kernel_metapackages_removed // ""' "$d/result.json" 2> /dev/null)" ]; then
		echo "- **KERNEL METAPACKAGES WERE REMOVED WITH THE ABORTED KERNEL: $(jq -r '.kernel_metapackages_removed' "$d/result.json").** Later full-upgrades install no kernel on $host until they are installed again (a deliberate decision: installing them brings the newest kernel back)."
	fi
	echo "- mode=repair never reboots and never touches grub's saved default. \`configure\` runs \`dpkg --configure -a\`; \`abort-new-kernel\` first purges the kernel versions newer than the running one that a failed upgrade left half-installed, so the host stays on the old kernel (a kernel installed and working before is never removed). If \`configure\` fails again because a DKMS module (r8127) does not build for the new kernel, the campaign cannot move this host to that kernel until the module builds: dispatch \`abort-new-kernel\`."
	if [ -n "$(jq -r '.holds_released[0] // ""' "$d/result.json" 2> /dev/null)" ]; then
		echo "- **HOLDS RELEASED** (put by this tool and left by a run that died): $(jq -r '.holds_released | join(" ; ")' "$d/result.json"). Only holds recorded by this tool are released; any other hold is somebody's decision."
	fi
	if [ -n "$(jq -r '.holds_not_released[0] // ""' "$d/result.json" 2> /dev/null)" ]; then
		echo "- **HOLDS NOT RELEASED** (apt-mark unhold did not take, the dpkg lock was busy?): $(jq -r '.holds_not_released | join(" ; ")' "$d/result.json"). They and their record are still in place: dispatch mode=repair with repair_action=release-holds again."
	fi
	if [ -s "$d/repair.log" ]; then
		echo
		echo '```'
		tail -n 30 "$d/repair.log"
		echo '```'
	fi
	for r in "$out"/*/refusal.txt; do
		[ -s "$r" ] || continue
		echo
		echo "### Refused: $(basename "$(dirname "$r")")"
		echo
		echo '```'
		cat "$r"
		echo '```'
	done
	exit 0
fi

# ---------------------------------------------------------------- upgrade
d=$out/$host
pre=$d/pre-snapshot.json
post=$d/post-snapshot.json
phase=$(cat "$d/phase.txt" 2> /dev/null || echo "none (the play never started on the host)")
phase=${phase//$'\n'/}
echo "- last phase reached: \`$phase\` (preflight-ok, pinned, upgrading, upgraded, dkms-ok, arming, rebooting, returned, verified, permanent, rehearsed)"
if [ "$mode" = rehearse ] && [ "$rc" -eq 0 ] && [ "$phase" = rehearsed ]; then
	echo "- **result: REHEARSED**: GRUB took the one-shot, cleared it (next_entry is empty), and $host runs the same kernel again. The one-shot mechanism works on this host; nothing else was changed (no package, no kernel)."
elif [ "$rc" -eq 0 ] && [ "$phase" = permanent ]; then
	if [ "$(jq -r '.kernel_before == .kernel_after' "$d/result.json" 2> /dev/null)" = true ]; then
		echo "- **result: UPGRADED AND VERIFIED; no new kernel was installed, the running kernel stays the default**"
	else
		echo "- **result: UPGRADED, VERIFIED, NEW KERNEL IS NOW THE DEFAULT**"
	fi
else
	echo "- **result: NOT COMPLETE** (ansible-playbook exit $rc)"
fi

if [ "$rc" -eq 124 ] || [ "$rc" -eq 137 ]; then
	echo "- **ansible-playbook was killed by its overall timeout (exit $rc) in phase \`$phase\`.** The host may be slow, not dead: do not power-cycle on this alone; survey first."
fi
# What the person must do. Exact, by how far the run got.
echo
echo "### Power-cycle and recovery: what a person must do"
echo
echo "> A power-cycle changes **which kernel boots**. It restores nothing else: a regression in what both kernels share (bootloader, ssh, tailscale, netplan, systemd, firmware) is not undone by it, and the host would then need its console."
echo
case "$phase" in
permanent)
	echo "- Power-cycle: **not needed**. $host runs the new kernel, which is now grub's saved default; the old kernel is still installed (nothing was auto-removed) and is the fallback to pick by hand in grub or by \`grub-set-default\` through a later change."
	;;
rehearsed)
	echo "- Power-cycle: **not needed**. The host booted the one-shot and runs its kernel normally."
	;;
verified)
	echo "- Power-cycle: **not needed**. The new kernel runs and passed verification, but making it the default FAILED or was refused (see the log; the step may have changed grub's saved default before failing: dispatch a survey and read \`boot.saved_entry\` and \`boot.next_entry\`; a one-shot still pending after the boot means GRUB did not consume it). Either accept the new kernel as the default by a reviewed change that runs grub-set-default for it, or reboot back with the cluster-reboot workflow (red with 'kernel changed', expected; only if GRUB consumed its one-shot: if boot.next_entry was still pending after the boot, GRUB ignores the grubenv and cluster-reboot boots GRUB's first menu entry, boot.entry0_kernel, again: then only the console helps) and dispatch the upgrade again. The upgrade REFUSES to run while the host runs a kernel other than grub's saved default."
	;;
returned)
	# the text depends on whether a one-shot was still pending after the boot (result.json): never "consumed by now" beside a pending one
	echo "- Power-cycle: $(jq -r '.power_cycle // "see result.json"' "$d/result.json" 2> /dev/null)"
	if [ "$mode" != rehearse ]; then
		echo "- The saved default is the OLD kernel; the new kernel was NOT made the default. Do NOT dispatch the upgrade again before the host is back on its saved default: the run refuses (it would move the fallback to the unverified kernel). Do not run measurements on $host until this is understood."
	fi
	;;
arming | rebooting)
	st=$(jq -r '.status // ""' "$d/result.json" 2> /dev/null)
	if [ "$mode" = rehearse ]; then
		# a rehearsal arms the running kernel: no new kernel is in play, the playbook words each case for it
		echo "- Power-cycle: $(jq -r '.power_cycle // "see result.json"' "$d/result.json" 2> /dev/null)"
		st=rehearse-text-printed
	fi
	case "$st" in
	rehearse-text-printed) ;;
	failed-before-arming)
		echo "- Power-cycle: **not needed**. The arming did not complete: nothing is armed and nothing was rebooted ($host answered the rescue and no one-shot is pending). The old kernel runs and is the default."
		;;
	failed-one-shot-gone-host-not-rebooted)
		echo "- Power-cycle: **not needed while $host answers**. The arming succeeded, the host did not reboot, and no one-shot is pending any more: something cleared it (GRUB reading another environment block than the one Linux writes?). Dispatch a survey; do not dispatch the upgrade again before this is understood."
		;;
	failed-before-boot-disarmed)
		echo "- Power-cycle: **not needed**. The run failed after arming the one-shot and before the new kernel booted; $host answered and the one-shot was CLEARED (disarm). The old kernel runs and is the default."
		;;
	failed-rebooted-one-shot-not-consumed)
		echo "- Power-cycle: **not needed while $host answers, but it HAS REBOOTED** (new boot id) and GRUB did **not** consume the one-shot (next_entry was still set; the rescue cleared it): GRUB does not read the environment block Linux writes, so the saved default is NOT what a power-cycle or a reboot boots: GRUB boots its FIRST menu entry (\`$(jq -r '.first_entry_kernel // "unknown"' "$d/result.json" 2> /dev/null)\`) every time, the cluster-reboot workflow included, and only the console (Esc or Shift at power-on) or a reviewed change brings another kernel back. Dispatch a survey (\`kernel\`, \`boot.entry0_kernel\` = what GRUB boots; \`boot.default_kernel\` is only the saved default and is NOT what boots); do not dispatch the upgrade again before this is understood."
		;;
	failed-boot-unknown)
		echo "- Power-cycle: **not needed while $host answers, but WHETHER IT REBOOTED IS UNKNOWN**: it answered the disarm and not the read of its boot id. Dispatch a survey before anything else (\`kernel\`, \`boot.next_entry\`, \`boot.saved_entry\`)."
		;;
	failed-rebooted-nothing-armed)
		echo "- Power-cycle: **not needed while $host answers, but it HAS REBOOTED** (new boot id) although the arming did not complete: it rebooted for another reason and runs what GRUB chose. Dispatch a survey before anything else."
		;;
	failed-new-kernel-may-have-booted)
		echo "- Power-cycle: **not needed yet**. The run failed after arming, but no one-shot is pending any more: grub consumed it, so the NEW kernel has probably booted (slowly) and $host answers. Dispatch a survey before anything else."
		;;
	*)
		echo "- Power-cycle: **YES if $host stays silent, and what it boots depends on why.** The run stopped at \`$phase\`: **a one-shot may be armed** (the host did not answer the rescue, so it could not be asked to clear it), and the reboot may or may not have been issued."
		echo "  1. The NEW kernel booted and has no network: grub already consumed the one-shot, so a power-cycle boots the saved default, the OLD kernel."
		echo "  2. The OLD kernel hung or panicked while shutting down (msr1 has a documented r8127 power-off panic, ops/power/quiesce-r8127.shutdown), or the reboot was never issued and the host is unreachable for another reason: the one-shot is **STILL ARMED**, so the next boot after the power-cycle is the **NEW kernel**, once; if that fails too, a second power-cycle boots the OLD kernel."
		echo "  3. The host is only slow (a run killed by its timeout reads the same): wait, then survey."
		echo "  After the power-cycle, dispatch a survey: \`kernel\` and \`boot.next_entry\` say which case it was; \`mode=disarm\` clears a one-shot that is still armed. **A power-cycle restores the choice of kernel only**: if the upgrade broke what both kernels share (bootloader, ssh, tailscale, netplan, firmware), it restores nothing and the host needs its console. **Ask the maintainer to power-cycle $host physically** only when it is silent."
		;;
	esac
	;;
upgrading)
	echo "- Power-cycle: **not needed**. Nothing was rebooted (upgrading). The apt run is DETACHED on the host: if this run lost its poll, apt may still be running. Dispatch a survey and wait until its \`package_processes\` is empty before anything else. The old kernel is still running and is grub's saved default."
	;;
dkms-ok)
	if [ -s "$d/arm.json" ]; then
		echo "- Power-cycle: **not needed**, nothing was rebooted. But the arming step ran ($(jq -r '"next_entry `" + .next_entry + "`"' "$d/arm.json")): if the run failed before the reboot step and could not clear it, the one-shot may be STILL ARMED. Dispatch **mode=disarm** for $host (it is a no-op when nothing is pending)."
	else
		echo "- Power-cycle: **not needed**. Nothing was rebooted (dkms-ok) and no one-shot was armed. The old kernel is still running and is grub's saved default."
	fi
	;;
none* | preflight-ok | pinned | upgraded)
	echo "- Power-cycle: **not needed**. Nothing was rebooted ($phase). The old kernel is still running and is still grub's default (from phase \`pinned\` on, it is pinned as the saved default)."
	;;
esac
if [ -s "$d/result.json" ]; then
	echo
	jq -r '"- result.json: status `\(.status)`; kernel before `\(.kernel_before)`, intended `\(.target_kernel)`, running after `\(.kernel_after // "n/a")`, grub default `\(.default_kernel // "n/a")`", "- result.json power_cycle: \(.power_cycle)"' "$d/result.json"
	jq -r 'if (.problems | length) > 0 then "\n### Problems\n" + (.problems | map("- " + .) | join("\n")) else empty end' "$d/result.json"
fi

for r in "$out"/*/refusal.txt; do
	[ -s "$r" ] || continue
	echo
	echo "### Refused: $(basename "$(dirname "$r")")"
	echo
	echo '```'
	cat "$r"
	echo '```'
done
for r in "$out"/*/holds-left.txt; do
	[ -s "$r" ] || continue
	echo
	echo "### ACCESS-PATH HOLDS NOT RELEASED: $(basename "$(dirname "$r")")"
	echo
	echo '```'
	cat "$r"
	echo '```'
done

# the package diff (before / after dpkg -l), also written as a file
if [ -s "$d/dpkg-before.txt" ] && [ -s "$d/dpkg-after.txt" ]; then
	diff -u "$d/dpkg-before.txt" "$d/dpkg-after.txt" > "$d/package-diff.txt" || true
	echo
	echo "### Packages"
	echo
	added=$(grep -c '^+[^+]' "$d/package-diff.txt" 2> /dev/null || echo 0)
	removed=$(grep -c '^-[^-]' "$d/package-diff.txt" 2> /dev/null || echo 0)
	echo "- package-diff.txt: $added line(s) added or changed, $removed removed (a version change shows as one of each); the artifact holds dpkg-before.txt, dpkg-after.txt, apt-update.log and apt-upgrade.log."
	echo
	echo '```'
	grep -E '^[+-](linux-image|linux-modules|linux-headers|linux-firmware|r8127|dkms|grub|tailscale|docker|containerd|openssh-server|systemd|libc6)' "$d/package-diff.txt" | head -n 40
	echo '```'
fi

if [ -s "$d/simulate.json" ]; then
	echo
	jq -r '"- apt plan: \(.inst | length) install(s)/upgrade(s), removals: \(.remv | join(", ") | if . == "" then "none" else . end), kept back: \(.kept_back | join(", ") | if . == "" then "none" else . end)"' "$d/simulate.json"
fi
if [ -s "$d/dkms.json" ]; then
	echo
	jq -r '"- dkms-check for `\(.target)`: missing modules \(.missing_modules | join(", ") | if . == "" then "none" else . end); built now \(.built_now) (rc \(.build_rc)); r8127 built \(.r8127.found) (version `\(.r8127.version)`, signer `\(.r8127.signer)`); headers \(.headers_present); initrd \(.initrd_present); in-tree r8169 lists the r8127 PCI id \(.intree_lists_id), r8169 blacklisted \(.r8169_blacklisted); Secure Boot `\(.secure_boot)`; grub entry for the old kernel `\(.running_entry_id)`, for the new `\(.target_entry_id)`; saved default resolves to `\(.saved_resolves_to)`"' "$d/dkms.json"
fi
if [ -s "$d/arm.json" ]; then
	jq -r '"- one-shot armed: saved_entry `\(.saved_entry)`, next_entry `\(.next_entry)`"' "$d/arm.json"
fi
if [ -s "$d/toolcheck.json" ]; then
	jq -r '"- toolchain bootstrap prerequisites after the reboot: C compiler builds and runs \(.cc_ok), TLS to github.com \(.https_ok); warnings: \(.warnings | join("; ") | if . == "" then "none" else . end)"' "$d/toolcheck.json"
fi

if [ -s "$pre" ]; then
	echo
	echo "### Before and after"
	echo
	echo "| | before | after |"
	echo "|---|---|---|"
	for f in boot_id boot_time kernel uptime_s system_state; do
		b=$(j ".$f" "$pre")
		a=$(if [ -s "$post" ]; then j ".$f" "$post"; else echo "(no snapshot)"; fi)
		echo "| $f | \`$b\` | \`$a\` |"
	done
	for f in tailscaled_active tailscaled_enabled apcupsd_active resume_check_enabled power_event_installed tmpfiles_conf quiesce_hook run_celeris_owner; do
		b=$(j ".prereq.$f" "$pre")
		a=$(if [ -s "$post" ]; then j ".prereq.$f" "$post"; else echo "(no snapshot)"; fi)
		echo "| prerequisite $f | $b | $a |"
	done
	for f in lan_ip_present peer_ping_ok placement_vector_present max_vector_total runner_listeners; do
		b=$(j ".$f" "$pre")
		a=$(if [ -s "$post" ]; then j ".$f" "$post"; else echo "(no snapshot)"; fi)
		echo "| $f | $b | $a |"
	done
	if [ -s "$d/pre-survey.json" ] && [ -s "$d/post-survey.json" ]; then
		echo
		echo "| NIC | driver before | driver after | link Mb/s before | link Mb/s after |"
		echo "|---|---|---|---|---|"
		jq -r -n --slurpfile a "$d/pre-survey.json" --slurpfile b "$d/post-survey.json" '$a[0].nics[] as $n | ($b[0].nics | map(select(.iface == $n.iface)) | .[0] // {}) as $m | "| \($n.iface) | \($n.driver) \($n.driver_version) | \($m.driver // "gone") \($m.driver_version // "") | \($n.speed_mbps) | \($m.speed_mbps // "gone") |"'
	fi
	echo
	echo "### Bond and crash dumps"
	echo
	jq -r '"- before: " + ((.bonds | map(.bond + " " + (.slaves | join(", "))) | join("; ")) // "none") + "; crash dumps: " + (.crash | join(", "))' "$pre"
	if [ -s "$post" ]; then
		jq -r '"- after: " + ((.bonds | map(.bond + " " + (.slaves | join(", "))) | join("; ")) // "none") + "; crash dumps: " + (.crash | join(", ")) + "; failed units: " + (.failed_units | join(", "))' "$post"
	fi
fi
if [ -s "$post" ]; then
	echo
	echo "### What the next run re-applies by itself"
	echo
	echo "- Nothing is restored by hand. The IRQ affinity is what the kernel chose at boot."
	echo "- **bench.yml** pins the msr1 \`nic_irq_placement\` (enp1s0-8 to CPU 1) before every bench run and reads it back; \`mage Validate\`, the stress playbooks and the soak do NOT."
	echo "- The \`/tmp\` tool cache is gone: the first bootstrap after this upgrade is COLD (allow the 25-minute bootstrap timeout), and every floating toolchain (rust, node, java, maven, bun, zig) is resolved afresh to whatever upstream is then."
	echo "- Kernel packages: the old kernel and its packages are still installed and marked manual (so autoremove and unattended-upgrades leave them); remove them deliberately, later."
fi
exit 0
