#!/usr/bin/env bash
# Renders the evidence of one ansible/reboot.yml run (the directory the workflow gives it as reboot_out_dir) as a
# markdown job summary: what was asked, what the host looked like before and after, and the verdict. Read-only;
# it works on whatever the play managed to write, so a reboot that never came back still gets a summary.
#
# usage: reboot-summary.sh OUT_DIR HOST ANSIBLE_RC "REASON" RUN_ID
set -u
out=${1:?out dir}
host=${2:?host}
rc=${3:?ansible rc}
reason=${4:-}
run_id=${5:-}
d=$out/$host
pre=$d/pre-snapshot.json
post=$d/post-snapshot.json
phase=$(cat "$d/phase.txt" 2> /dev/null || echo "none (the play never started on the host)")
phase=${phase//$'\n'/}
# a code span cannot hold a backtick: the reason is free text
safe_reason=${reason//\`/\'}

echo "## Cluster reboot: $host"
echo
echo "- reason: \`$safe_reason\`"
echo "- workflow run: $run_id"
echo "- last phase reached: \`$phase\` (preflight-ok, rebooting, returned, verified)"
if [ "$rc" -eq 0 ] && [ "$phase" = verified ]; then
	echo "- **result: VERIFIED**"
else
	echo "- **result: NOT VERIFIED** (ansible-playbook exit $rc)"
	case "$phase" in
	rebooting)
		echo
		echo "> The reboot was issued and the host did not answer over the tailnet within the timeout. It may be down (msr1 has a documented shutdown panic in the r8127 driver, ops/power/quiesce-r8127.shutdown, that needs a PHYSICAL power-cycle) or UP but off the tailnet (tailscaled did not start at boot, or the NIC/bond did not come back): look at the console, or reach it over the LAN, before power-cycling. Do not dispatch anything that uses this host until it is back and verified."
		;;
	returned)
		echo
		echo "> The host answered after the reboot but failed a verification below. It is up; treat it as unverified until the problem is understood."
		;;
	preflight-ok | none*)
		echo
		echo "> The reboot was refused or never started: nothing was rebooted. The refusal is in the ansible log (artifact)."
		;;
	esac
fi

# a refusal (the reasons, written before the play failed) is the likeliest outcome of a first dispatch
for r in "$out"/*/refusal.txt; do
	[ -s "$r" ] || continue
	echo
	echo "### Refused: $(basename "$(dirname "$r")")"
	echo
	echo '```'
	cat "$r"
	echo '```'
done

if ! command -v jq > /dev/null 2>&1; then
	echo
	echo "(jq is not installed: the snapshots are in the artifact)"
	exit 0
fi

if [ -s "$d/result.json" ]; then
	echo
	jq -r '"- reboot took about \(.elapsed_seconds) s (the reboot module, until the host answered)"' "$d/result.json"
	jq -r 'if (.problems | length) > 0 then "\n### Problems\n" + (.problems | map("- " + .) | join("\n")) else empty end' "$d/result.json"
fi

if [ -s "$pre" ]; then
	echo
	echo "### Before and after"
	echo
	echo "| | before | after |"
	echo "|---|---|---|"
	for f in boot_id boot_time kernel uptime_s system_state; do
		b=$(jq -r ".$f" "$pre")
		a=$(if [ -s "$post" ]; then jq -r ".$f" "$post"; else echo "(no snapshot)"; fi)
		echo "| $f | \`$b\` | \`$a\` |"
	done
	for f in tailscaled_active tailscaled_enabled apcupsd_active resume_check_enabled power_event_installed tmpfiles_conf quiesce_hook run_celeris_owner; do
		b=$(jq -r ".prereq.$f" "$pre")
		a=$(if [ -s "$post" ]; then jq -r ".prereq.$f" "$post"; else echo "(no snapshot)"; fi)
		echo "| prerequisite $f | $b | $a |"
	done
	for f in lan_ip_present peer_ping_ok placement_vector_present max_vector_total runner_listeners; do
		b=$(jq -r ".$f" "$pre")
		a=$(if [ -s "$post" ]; then jq -r ".$f" "$post"; else echo "(no snapshot)"; fi)
		echo "| $f | $b | $a |"
	done
	echo
	echo "### NIC interrupt vectors (since-boot totals and IRQ affinity, recorded as the kernel left them)"
	echo
	echo "| when | iface | irq | name | total since boot | affinity | effective |"
	echo "|---|---|---|---|---|---|---|"
	jq -r '.nic_vectors[] | "| before | \(.iface) | \(.irq) | \(.name) | \(.total) | \(.affinity) | \(.effective) |"' "$pre"
	if [ -s "$post" ]; then
		jq -r '.nic_vectors[] | "| after | \(.iface) | \(.irq) | \(.name) | \(.total) | \(.affinity) | \(.effective) |"' "$post"
	fi
	echo
	echo "### Bond and crash dumps"
	echo
	jq -r '"- before: " + ((.bonds | map(.bond + " " + (.slaves | join(", "))) | join("; ")) // "none") + "; crash dumps: " + (.crash | join(", "))' "$pre"
	if [ -s "$post" ]; then
		jq -r '"- after: " + ((.bonds | map(.bond + " " + (.slaves | join(", "))) | join("; ")) // "none") + "; crash dumps: " + (.crash | join(", ")) + "; failed units: " + (.failed_units | join(", "))' "$post"
	fi
	echo
	echo "### What the next run re-applies by itself"
	echo
	echo "- Nothing is restored by hand. The IRQ affinity above is what the kernel chose at boot."
	echo "- **bench.yml** pins the msr1 \`nic_irq_placement\` (enp1s0-8 to CPU 1) before every bench run and reads it back; \`mage Validate\`, the stress playbooks and the soak do NOT, so a non-bench run on msr1 right after this reboot sees the kernel's placement above."
	echo "- Runners are registered per run (ephemeral) and removed by the run's teardown: none existed on the host, none is stranded. The first cluster bootstrap after this reboot is COLD (\`/tmp/celeris-runner-tarballs\` is tmpfs and was wiped): allow the 25-minute bootstrap timeout."
	echo "- The power guard is persistent (apcupsd, hooks, tmpfiles.d, the shutdown quiesce hook) and is checked above; the host sampler is started per run by the soak and needs nothing."
fi
