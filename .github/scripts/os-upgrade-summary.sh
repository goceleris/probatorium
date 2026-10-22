#!/usr/bin/env bash
# Renders the evidence of one ansible/os-upgrade.yml run (the directory the workflow gives it as os_out_dir) as a
# markdown job summary. Read-only (it also writes <host>/package-diff.txt next to the evidence); it works on whatever
# the play managed to write, so a run that failed half way still gets a summary.
#
# usage: os-upgrade-summary.sh OUT_DIR MODE HOST ANSIBLE_RC "REASON" RUN_ID
#   MODE survey | upgrade; HOST is "-" for a survey
set -u
out=${1:?out dir}
mode=${2:?mode}
host=${3:?host}
rc=${4:?ansible rc}
reason=${5:-}
run_id=${6:-}
safe_reason=${reason//\`/\'}

if ! command -v jq > /dev/null 2>&1; then
	echo "## Cluster OS $mode"
	echo
	echo "(jq is not installed: the records are in the artifact)"
	exit 0
fi

hosts="msa2-server msr1 msa2-client"
j() { jq -r "$1" "$2" 2> /dev/null; }

survey_section() {
	local s=$out/$1/survey.json
	[ -s "$s" ] || { echo "- $1: no survey record"; return; }
	echo "### $1"
	echo
	jq -r '
		"- OS: \(.os_release.PRETTY_NAME // "?") (\(.arch)); running kernel `\(.kernel)`; installed: \(.installed_kernels | join(", ")); newest of the same flavor: `\(.newest_same_flavor)`",
		"- grub: default after a power-cycle = `\(.boot.default_kernel)`, saved_entry `\(.boot.saved_entry)`, GRUB_DEFAULT=`\(.boot.settings.GRUB_DEFAULT // "unset")`, saved logic in grub.cfg: \(.boot.cfg_uses_saved), EFI: \(.boot.efi), Secure Boot: \(.boot.secure_boot), /boot/grub on `\(.boot.boot_fs)` (`\(.boot.boot_source)`), one-shot tools: grub-reboot \(.boot.grub_reboot) grub-set-default \(.boot.grub_set_default) grub-editenv \(.boot.grub_editenv) update-grub \(.boot.update_grub), recordfail timeout `\(.boot.cfg_recordfail_timeout)`",
		"- **GRUB boot proof** (every one-shot and fallback depends on it): \(if .boot.proof.ok then "OK" else "NOT PROVEN: " + (.boot.proof.problems | join(" / ")) end); BOOT_IMAGE `\(.boot.proof.boot_image)`, EFI loader `\(.boot.proof.efi_loader)`, GRUB prefix `\(.boot.proof.prefix)`",
		"- apt (lists \(.packages.lists_age_hours // "?") h old): would install/upgrade \(.packages.simulation.inst | length) package(s) (kernel packages: \(.packages.simulation.kernel_inst | join(", ") | if . == "" then "none" else . end)); would REMOVE: \(.packages.simulation.remv | join(", ") | if . == "" then "none" else . end); kept back: \(.packages.kept_back | join(", ") | if . == "" then "none" else . end); held back by phasing: \(.packages.phased_held_back | join(", ") | if . == "" then "none" else . end)",
		"- held: \(.packages.held | join(", ") | if . == "" then "none" else . end) (kernel/boot/dkms: \(.packages.held_kernel | join(", ") | if . == "" then "none" else . end)); third-party repos: \(.packages.third_party_repos | join(", ") | if . == "" then "none" else . end); pin files: \(.packages.pin_files | join(", ") | if . == "" then "none" else . end)",
		"- dpkg: audit \(.packages.dpkg_audit | length) finding(s), lock held \(.packages.dpkg_lock_held), package processes `\(.packages.package_processes | join(", "))`, policy-rc.d present \(.packages.policy_rc_d_present)",
		"- unattended-upgrades: installed \(.unattended_upgrades.installed), periodic `\(.unattended_upgrades.unattended_upgrade)`, removes unused kernels `\(.unattended_upgrades.remove_unused_kernel_packages)`, automatic reboot `\(.unattended_upgrades.automatic_reboot)`, needrestart \(.unattended_upgrades.needrestart_installed); units: \(.unattended_upgrades.units | to_entries | map("\(.key)=\(.value.active)/\(.value.enabled)") | join(", "))",
		"- reboot-required: \(.reboot_required) \(.reboot_required_pkgs | join(" "))",
		"- disk free (KiB): / \(.disk["/"].free_kib // "?"), /boot \(.disk["/boot"].free_kib // "?"), /boot/efi \(.disk["/boot/efi"].free_kib // "n/a"), /var \(.disk["/var"].free_kib // "?")",
		"- time sync: NTPSynchronized=`\(.time_sync.raw)`; crash dumps: \(.crash | join(", ") | if . == "" then "none" else . end); linux-firmware `\(.firmware.linux_firmware)`",
		"- dkms: \(.dkms.status | map("\(.name)/\(.version) \(.kernel) \(.state)") | join("; ") | if . == "" then "none" else . end)",
		"- last OS-upgrade record: \(.marker | if . == {} then "none" else "run \(.run_id): \(.kernel_before) -> \(.kernel_after), permanent \(.permanent), \(.finished_utc)" end)"
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

host_table() {
	echo "| host | OS | running kernel | grub default | newest same flavor | upgrades / removals / kept back | reboot-required | disk free / and /boot (GiB) | NTP | crash dumps |"
	echo "|---|---|---|---|---|---|---|---|---|---|"
	for h in $hosts; do
		s=$out/$h/survey.json
		[ -s "$s" ] || { echo "| $h | (no record) | | | | | | | | |"; continue; }
		jq -r --arg h "$h" '"| \($h) | \(.os_release.PRETTY_NAME // "?") | `\(.kernel)` | `\(.boot.default_kernel)` | `\(.newest_same_flavor)` | \(.packages.simulation.inst | length) / \(.packages.simulation.remv | length) / \(.packages.kept_back | length) | \(.reboot_required) | \(((.disk["/"].free_kib // 0) / 1048576) | floor) / \(((.disk["/boot"].free_kib // 0) / 1048576 * 10) | floor / 10) | \(.time_sync.raw) | \(.crash | length) |"' "$s"
	done
}

echo "## Cluster OS $mode: $host"
echo
echo "- reason: \`$safe_reason\`"
echo "- workflow run: $run_id"

if [ "$mode" = survey ]; then
	if [ "$rc" -eq 0 ]; then echo "- **result: SURVEYED** (read-only; nothing on any host was changed, except, when refresh_lists was set, what apt-get update writes: /var/lib/apt/lists, /var/cache/apt/*.bin and its post-invoke hooks)"; else echo "- **result: SURVEY FAILED** (ansible-playbook exit $rc): the hosts that answered have a record below"; fi
	echo
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
	echo "- Disarming clears only grub's next_entry. It does not touch the saved default, the packages or the running kernel. If the host did not answer, nothing was cleared: it is down, and a power-cycle boots whatever next_entry names (the NEW kernel, once) before the saved default."
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
	echo "- mode=repair never reboots and never touches grub's saved default. \`configure\` runs \`dpkg --configure -a\`; \`abort-new-kernel\` first purges the kernel versions newer than the running one that a failed upgrade left half-installed, so the host stays on the old kernel (a kernel installed and working before is never removed). If \`configure\` fails again because a DKMS module (r8127) does not build for the new kernel, the campaign cannot move this host to that kernel until the module builds: dispatch \`abort-new-kernel\`."
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
echo "- last phase reached: \`$phase\` (preflight-ok, pinned, upgrading, upgraded, dkms-ok, rebooting, returned, verified, permanent)"
if [ "$rc" -eq 0 ] && [ "$phase" = permanent ]; then
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
verified)
	echo "- Power-cycle: **not needed**. The new kernel runs and passed verification, but making it the default FAILED (see the log). grub's saved default is still the OLD kernel, so a reboot returns to it. Either accept the new kernel as the default by a reviewed change that runs grub-set-default for it, or reboot back with the cluster-reboot workflow (red with 'kernel changed', expected) and dispatch the upgrade again. The upgrade REFUSES to run while the host runs a kernel other than grub's saved default."
	;;
returned)
	echo "- Power-cycle: **not needed while $host answers** (if it stops answering later, read the 'rebooting' case: the one-shot is consumed by now, so a power-cycle boots the OLD kernel). $host answered after the reboot but failed verification (below). The saved default is the OLD kernel; the new kernel was NOT made the default. Reboot it back with the cluster-reboot workflow: it returns to the old kernel and ends red with 'kernel changed', which is expected here. Do NOT dispatch the upgrade again before that: the run refuses (it would move the fallback to the unverified kernel). Do not run measurements on $host until this is understood."
	;;
rebooting)
	st=$(jq -r '.status // ""' "$d/result.json" 2> /dev/null)
	case "$st" in
	failed-before-boot-disarmed)
		echo "- Power-cycle: **not needed**. The run failed after arming the one-shot and before the new kernel booted; $host answered and the one-shot was CLEARED (disarm). The old kernel runs and is the default."
		;;
	failed-new-kernel-may-have-booted)
		echo "- Power-cycle: **not needed yet**. The run failed after arming, but no one-shot is pending any more: grub consumed it, so the NEW kernel has probably booted (slowly) and $host answers. Dispatch a survey before anything else."
		;;
	*)
		echo "- Power-cycle: **YES if $host stays silent, and what it boots depends on why.** The reboot was issued (or about to be) and the host did not answer within the timeout; it could not be asked to clear the one-shot."
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
