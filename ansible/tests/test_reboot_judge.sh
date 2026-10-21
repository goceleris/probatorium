#!/usr/bin/env bash
# Runs the REAL judgement of ansible/reboot.yml (ansible/vars/reboot_judge.yml, the Jinja that decides whether to
# refuse a reboot and whether a rebooted host is verified) under ansible-playbook on localhost, against snapshots
# produced by the real ansible/files/reboot_snapshot.sh on fake /proc and /sys trees, and checks each verdict:
#   bash ansible/tests/test_reboot_judge.sh      (needs ansible-playbook; skips with exit 0 without)
# A Jinja template that only ever runs on a freshly rebooted cluster host would be a template nobody has run.
set -u
here=$(cd "$(dirname "$0")/.." && pwd)
command -v ansible-playbook > /dev/null || { echo "SKIP: ansible-playbook not installed"; exit 0; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/pb/vars" "$work/stub"
cp "$here/vars/reboot_judge.yml" "$work/pb/vars/"

pass=0
fail=0
ok() { pass=$((pass + 1)); echo "ok   $1"; }
bad() { fail=$((fail + 1)); echo "FAIL $1" >&2; }

# the owner of /run/celeris is whoever runs this test; the host's is the inventory's ansible_user, so pin both
me=mini
# Stand-ins for the host's commands: a service is active when its flag file exists; the fabric address is carried
# when the "addr" flag exists; the peer answers when the "ping" flag exists; modinfo finds r8127 when "modinfo" exists.
cat > "$work/stub/systemctl" << 'EOT'
#!/bin/bash
S=${STUB_FLAGS:?}
case "$1" in
is-active) [ -e "$S/active-$2" ] && echo active || echo inactive ;;
is-enabled) if [ -e "$S/enabled-$2" ]; then echo enabled; elif [ -e "$S/enabled-runtime-$2" ]; then echo enabled-runtime; else echo disabled; fi ;;
is-system-running) echo running ;;
--failed) : ;;
esac
EOT
cat > "$work/stub/ip" << 'EOT'
#!/bin/bash
[ -e "${STUB_FLAGS:?}/addr" ] && echo "2: bond0    inet 192.168.50.199/24 brd 192.168.50.255 scope global bond0"
exit 0
EOT
cat > "$work/stub/ping" << 'EOT'
#!/bin/bash
[ -e "${STUB_FLAGS:?}/ping" ]
EOT
cat > "$work/stub/modinfo" << 'EOT'
#!/bin/bash
[ -e "${STUB_FLAGS:?}/modinfo" ]
EOT
chmod +x "$work/stub/"*

# mktree DIR BOOT_ID TOTAL - an msr1-shaped host (see test_reboot_snapshot.sh), one vector's per-CPU counts summing to TOTAL
mktree() {
	local r=$1 boot=$2 total=$3
	mkdir -p "$r/sys/class/net/bond0" "$r/sys/class/net/enp1s0/device/msi_irqs" "$r/sys/class/net/enp49s0/device/msi_irqs" \
		"$r/proc/irq/167" "$r/proc/irq/135" "$r/proc/net/bonding" "$r/proc/sys/kernel/random" "$r/boot" "$r/run/celeris" \
		"$r/usr/lib/systemd/system-shutdown" "$r/usr/local/sbin" "$r/etc/tmpfiles.d" "$r/var/crash"
	ln -s /x/r8127 "$r/sys/class/net/enp1s0/device/driver"
	ln -s /x/r8127 "$r/sys/class/net/enp49s0/device/driver"
	touch "$r/sys/class/net/enp1s0/device/msi_irqs/167" "$r/sys/class/net/enp49s0/device/msi_irqs/135"
	printf ' 135:  %s  0  ITS-MSI 4  Edge      enp49s0-8\n 167:  0  9  ITS-MSI 2  Edge      enp1s0-8\n' "$total" > "$r/proc/interrupts"
	echo 1 > "$r/proc/irq/167/smp_affinity_list"
	echo 1 > "$r/proc/irq/167/effective_affinity_list"
	echo 0-11 > "$r/proc/irq/135/smp_affinity_list"
	echo 8 > "$r/proc/irq/135/effective_affinity_list"
	echo "$boot" > "$r/proc/sys/kernel/random/boot_id"
	echo 7.0.0-14-generic > "$r/proc/sys/kernel/osrelease"
	echo "100.0 100.0" > "$r/proc/uptime"
	printf 'btime 1700000000\n' > "$r/proc/stat"
	printf 'Bonding Mode: IEEE 802.3ad\n\nSlave Interface: enp1s0\nMII Status: up\n\nSlave Interface: enp49s0\nMII Status: up\n' > "$r/proc/net/bonding/bond0"
	touch "$r/boot/vmlinuz-7.0.0-14-generic" "$r/usr/local/sbin/cluster-power-event" "$r/etc/tmpfiles.d/celeris-power.conf"
	chmod +x "$r/usr/local/sbin/cluster-power-event"
	printf '#!/bin/sh\n' > "$r/usr/lib/systemd/system-shutdown/quiesce-r8127.shutdown"
	chmod +x "$r/usr/lib/systemd/system-shutdown/quiesce-r8127.shutdown"
}
flags_good() { # flags_good DIR - everything a healthy host shows
	mkdir -p "$1"
	touch "$1/active-tailscaled" "$1/enabled-tailscaled" "$1/active-apcupsd" "$1/enabled-celeris-resume-check.service" "$1/addr" "$1/ping" "$1/modinfo"
}
snap() { # snap ROOT FLAGSDIR -> stdout
	PATH=$work/stub:$PATH STUB_FLAGS=$2 HOST_SNAPSHOT_ROOT=$1 bash "$here/files/reboot_snapshot.sh" /tmp/celeris-bench-manifest.json /tmp/celeris-results enp1s0-8 192.168.50.199 192.168.50.195 2> /dev/null
}

cat > "$work/pb/play.yml" << 'EOT'
- name: judgement under test
  hosts: localhost
  connection: local
  gather_facts: false
  vars_files:
    - vars/reboot_judge.yml
  vars:
    ansible_user: "{{ lookup('env', 'JUDGE_USER') }}"
    results_root: /tmp/celeris-results
    reboot_counter_ceiling: 1000000000
    placement_vector: enp1s0-8
    lan_ip: 192.168.50.199
    fabric_peer: 192.168.50.195
    allow_results_loss: "{{ allow | default(false) }}"
    pre_target: "{{ lookup('file', pre_file) | from_json }}"
    post_raw:
      stdout: "{{ lookup('file', post_file, errors='ignore') | default('', true) }}"
  tasks:
    - ansible.builtin.copy:
        content: "{{ refusals_str | trim }}"
        dest: "{{ out_dir }}/refusals.txt"
    - ansible.builtin.copy:
        content: "{{ post_problems_str | trim }}"
        dest: "{{ out_dir }}/problems.txt"
EOT

# judge PRE.json POST.json [EXTRA_VARS_JSON] -> $refusals, $problems
judge() {
	local o=$work/out.$RANDOM
	mkdir -p "$o"
	(cd "$work/pb" && JUDGE_USER=$me ANSIBLE_LOCALHOST_WARNING=False ANSIBLE_INVENTORY_UNPARSED_WARNING=False ANSIBLE_DEPRECATION_WARNINGS=False \
		ansible-playbook -i localhost, play.yml -e "pre_file=$1" -e "post_file=$2" -e "out_dir=$o" -e "${3:-{\}}") > "$o/log" 2>&1 || {
		bad "ansible-playbook failed: $(tail -n 15 "$o/log")"
		return 1
	}
	refusals=$(cat "$o/refusals.txt")
	problems=$(cat "$o/problems.txt")
}
expect() { # expect NAME VALUE SUBSTRING  (SUBSTRING empty: VALUE must be empty)
	if [ -z "$3" ]; then
		if [ -z "$2" ]; then ok "$1"; else bad "$1: expected nothing, got: $2"; fi
	else
		case "$2" in *"$3"*) ok "$1" ;; *) bad "$1: expected '$3' in: $2" ;; esac
	fi
}
mut() { # mut FILE SED_EXPR  -> a mutated copy of a snapshot, path on stdout
	local f=$work/m.$RANDOM.json
	sed "$2" "$1" > "$f"
	cmp -s "$1" "$f" && echo "MUTATION DID NOT APPLY: $2" >&2
	echo "$f"
}

flags_good "$work/flags"
mktree "$work/pre" aaaaaaaa-0000-0000-0000-000000000001 4050000000
mktree "$work/post" bbbbbbbb-0000-0000-0000-000000000002 1200
snap "$work/pre" "$work/flags" | sed 's/"run_celeris_owner":"[^"]*"/"run_celeris_owner":"mini"/' > "$work/pre.json"
snap "$work/post" "$work/flags" | sed 's/"run_celeris_owner":"[^"]*"/"run_celeris_owner":"mini"/' > "$work/post.json"

judge "$work/pre.json" "$work/post.json" || true
expect "a healthy reboot: nothing to refuse" "$refusals" ""
expect "a healthy reboot: verified" "$problems" ""

judge "$work/pre.json" "$work/pre.json"
expect "the same boot id after the reboot is a failure" "$problems" "boot_id did not change"
judge "$work/pre.json" "$(mut "$work/post.json" 's/"kernel":"7.0.0-14-generic"/"kernel":"7.0.0-20-generic"/')"
expect "a different kernel is a failure" "$problems" "kernel changed: 7.0.0-14-generic -> 7.0.0-20-generic"
judge "$work/pre.json" "$(mut "$work/post.json" 's/"crash":\[\]/"crash":["202610211200"]/')"
expect "a new crash dump is a failure" "$problems" "new crash dump(s) in /var/crash: 202610211200"
judge "$work/pre.json" "$(mut "$work/post.json" 's/"apcupsd_active":true/"apcupsd_active":false/')"
expect "a power-guard prerequisite that held before and does not now is a failure" "$problems" "prerequisite regressed: apcupsd_active"
judge "$work/pre.json" "$(mut "$work/post.json" 's/"tailscaled_active":true/"tailscaled_active":false/')"
expect "tailscaled not active after the reboot is a failure" "$problems" "prerequisite regressed: tailscaled_active"
judge "$work/pre.json" "$(mut "$work/post.json" 's/"quiesce_hook":true/"quiesce_hook":false/')"
expect "the shutdown hook gone after the reboot is a failure" "$problems" "prerequisite regressed: quiesce_hook"
judge "$work/pre.json" "$(mut "$work/post.json" 's/"run_celeris_owner":"[^"]*"/"run_celeris_owner":"nobody"/')"
expect "/run/celeris owned by someone else is a failure" "$problems" "/run/celeris is not owned by $me (nobody)"
judge "$work/pre.json" "$(mut "$work/post.json" 's/"peer_ping_ok":true/"peer_ping_ok":false/')"
expect "no answer from the peer over the fabric is a failure" "$problems" "no answer from 192.168.50.195 over the fabric"
judge "$work/pre.json" "$(mut "$work/post.json" 's/"lan_ip_present":true/"lan_ip_present":false/')"
expect "the fabric address not configured is a failure" "$problems" "fabric address 192.168.50.199 is not configured"
judge "$work/pre.json" "$(mut "$work/post.json" 's/"placement_vector_present":true/"placement_vector_present":false/')"
expect "the pinned NIC vector missing is a failure" "$problems" "NIC vector enp1s0-8 (bench.yml pins it) is not in /proc/interrupts"
judge "$work/pre.json" "$(mut "$work/post.json" 's/"max_vector_total":1200/"max_vector_total":4050000000/')"
expect "counters that were not reset are a failure" "$problems" "the counters were not reset"
judge "$work/pre.json" "$(mut "$work/post.json" 's/"enp49s0=up"/"enp49s0=down"/')"
expect "a bond that lost a slave is a failure" "$problems" "bond lost a slave: 2 up before, 1 up now"
judge "$work/pre.json" "$work/does-not-exist.json"
expect "no snapshot after the reboot is a failure" "$problems" "no readable snapshot after the reboot"
# a prerequisite that never held is not blamed on the reboot
rm -f "$work/flags/active-apcupsd"
snap "$work/pre" "$work/flags" | sed 's/"run_celeris_owner":"[^"]*"/"run_celeris_owner":"mini"/' > "$work/pre-noapc.json"
snap "$work/post" "$work/flags" | sed 's/"run_celeris_owner":"[^"]*"/"run_celeris_owner":"mini"/' > "$work/post-noapc.json"
judge "$work/pre-noapc.json" "$work/post-noapc.json"
expect "a prerequisite that did not hold before is not a regression" "$problems" ""
touch "$work/flags/active-apcupsd"

# refusals, judged on the snapshot taken just before the reboot
judge "$(mut "$work/pre.json" 's/"results_entries":0/"results_entries":3/; s/"results_list":\[\]/"results_list":["a","b"]/')" "$work/post.json"
expect "entries in results_root refuse" "$refusals" "results_root /tmp/celeris-results holds 3 entr(ies) a reboot destroys (a, b)"
judge "$(mut "$work/pre.json" 's/"results_entries":0/"results_entries":3/; s/"results_list":\[\]/"results_list":["a","b"]/')" "$work/post.json" '{"allow":true}'
expect "allow_results_loss=true lets them go" "$refusals" ""
judge "$(mut "$work/pre.json" 's/"resume_pending":false/"resume_pending":true/')" "$work/post.json"
expect "a pending power-loss resume refuses" "$refusals" "resume-pending.json exists"
judge "$(mut "$work/pre.json" 's/"runner_workers":0/"runner_workers":1/')" "$work/post.json"
expect "an executing runner job refuses" "$refusals" "1 runner job(s) are executing on the host"
judge "$(mut "$work/pre.json" 's/"runner_listeners":0/"runner_listeners":2/')" "$work/post.json"
expect "an idle runner listener alone does not refuse (the reboot clears it)" "$refusals" ""
judge "$(mut "$work/pre.json" 's/"quiesce_hook":true/"quiesce_hook":false/')" "$work/post.json"
expect "an r8127 host without the quiesce hook refuses" "$refusals" "no shutdown quiesce hook"
judge "$(mut "$work/pre.json" 's/"modinfo_r8127":true/"modinfo_r8127":false/')" "$work/post.json"
expect "an r8127 module missing for the running kernel refuses" "$refusals" "r8127 module is not available for the running kernel 7.0.0-14-generic"
judge "$(mut "$work/pre.json" 's/"newest_installed_kernel":"7.0.0-14-generic"/"newest_installed_kernel":"7.0.0-20-generic"/')" "$work/post.json"
expect "a newer kernel installed than the running one refuses" "$refusals" "kernel 7.0.0-20-generic is installed and 7.0.0-14-generic is running"
# tailscaled running but not enabled at boot: the host would come back off the tailnet, unreachable to CI
for st in disabled enabled-runtime; do
	judge "$(mut "$work/pre.json" "s/\"tailscaled_enabled\":\"enabled\"/\"tailscaled_enabled\":\"$st\"/")" "$work/post.json"
	expect "tailscaled running but '$st' at boot refuses" "$refusals" "tailscaled is running but systemctl is-enabled says \"$st\""
done
judge "$(mut "$work/pre.json" 's/"tailscaled_active":true,"tailscaled_enabled":"enabled"/"tailscaled_active":false,"tailscaled_enabled":"disabled"/')" "$work/post.json"
expect "tailscaled not running at all is not the unit-file case (the host is then reached some other way)" "$refusals" ""
# a host without r8127 is not held to the r8127 rules
judge "$(mut "$work/pre.json" 's/"r8127":true,"modinfo_r8127":true/"r8127":false,"modinfo_r8127":null/; s/"quiesce_hook":true/"quiesce_hook":false/')" "$work/post.json"
expect "a host without r8127 needs neither the hook nor the module" "$refusals" ""

echo "reboot_judge: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
