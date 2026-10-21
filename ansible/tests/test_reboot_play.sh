#!/usr/bin/env bash
# Runs the REAL ansible/reboot.yml (not --check) end to end on local connections, with three things replaced and
# nothing else: the reboot task (a flag file stands in for the machine restarting), the snapshot (a wrapper that
# reads a "before" fake host until the flag exists and an "after" one once it does), and sudo (a stand-in). It
# checks the control flow that only a real run shows: what refuses before the reboot, that ONE unreachable or
# failing peer stops the reboot of the target, that a host which came back wrong is not verified, and that a
# verified one leaves its evidence.
#   bash ansible/tests/test_reboot_play.sh      (needs ansible-playbook; skips with exit 0 without)
set -u
here=$(cd "$(dirname "$0")/.." && pwd)
command -v ansible-playbook > /dev/null || { echo "SKIP: ansible-playbook not installed"; exit 0; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
pb=$work/pb
mkdir -p "$pb" "$work/bin" "$work/state"
cp -R "$here/files" "$here/tasks" "$here/vars" "$pb/"
cp "$here/reboot.yml" "$pb/reboot.yml"

pass=0
fail=0
ok() { pass=$((pass + 1)); echo "ok   $1"; }
bad() { fail=$((fail + 1)); echo "FAIL $1" >&2; }
check() { local n=$1; shift; if "$@"; then ok "$n"; else bad "$n"; fi; }

cat > "$work/bin/sudo" << 'EOT'
#!/bin/bash
while [ $# -gt 0 ]; do case "$1" in -u | -p | -g | -C | -U) shift 2 ;; -*) shift ;; *) break ;; esac; done
exec "$@"
EOT
chmod +x "$work/bin/sudo"

# the test copy: the reboot becomes a flag file, the polling does not wait, the snapshot is the wrapper
python3 - "$pb/reboot.yml" "$work" << 'PY'
import re, sys
p, work = sys.argv[1], sys.argv[2]
s = open(p).read()
m = re.search(r"    - name: Reboot \| issue the reboot.*?register: reboot_result\n", s, re.S)
assert m, "reboot task not found"
s = s.replace(m.group(0), """    - name: Reboot | (test) the flag stands in for the restart
      ansible.builtin.copy:
        content: ""
        dest: "{{ lookup('env', 'STATE') }}/rebooted"
        mode: "0644"
      register: reboot_result
""")
s = s.replace("{{ playbook_dir }}/files/reboot_snapshot.sh", work + "/snapshot-wrapper.sh")
# the preflight snapshot also tells the wrapper which host asks
old = "{{ manifest_path | quote }} {{ results_root | quote }} - - -"
assert old in s
s = s.replace(old, old + " {{ inventory_hostname }}")
assert "retries: 36" in s and "delay: 5" in s
s = s.replace("retries: 36", "retries: 2").replace("delay: 5", "delay: 0")
open(p, "w").write(s)
PY
grep -q "snapshot-wrapper" "$pb/reboot.yml" || { echo "FAIL: the test copy was not patched"; exit 1; }

cat > "$work/snapshot-wrapper.sh" << EOT
#!/bin/bash
# the host's snapshot: BEFORE tree until the flag exists, AFTER once it does. The preflight call carries a sixth
# argument, the host that asks; FAIL_HOST makes that one host's preflight fail.
if [ \$# -ge 6 ]; then
	[ "\$6" = "\${FAIL_HOST:-}" ] && { echo "snapshot failed (test)" >&2; exit 3; }
	set -- "\${@:1:5}"
fi
root=$work/before
[ -e "\$STATE/rebooted" ] && root=$work/\${AFTER:-after}
PATH=$work/stub:\$PATH STUB_FLAGS=$work/flags HOST_SNAPSHOT_ROOT=\$root exec bash $here/files/reboot_snapshot.sh "\$@"
EOT
chmod +x "$work/snapshot-wrapper.sh"

# fake hosts and stand-in commands (see test_reboot_judge.sh)
mkdir -p "$work/stub" "$work/flags"
cat > "$work/stub/systemctl" << 'EOT'
#!/bin/bash
S=${STUB_FLAGS:?}
case "$1" in
is-active) [ -e "$S/active-$2" ] && echo active || echo inactive ;;
is-enabled) [ -e "$S/enabled-$2" ] && echo enabled || echo disabled ;;
is-system-running) echo running ;;
esac
EOT
printf '#!/bin/bash\n[ -e "${STUB_FLAGS:?}/addr" ] && echo "2: bond0    inet 192.168.50.199/24 scope global bond0"\nexit 0\n' > "$work/stub/ip"
printf '#!/bin/bash\n[ -e "${STUB_FLAGS:?}/ping" ]\n' > "$work/stub/ping"
printf '#!/bin/bash\n[ -e "${STUB_FLAGS:?}/modinfo" ]\n' > "$work/stub/modinfo"
chmod +x "$work/stub/"*
touch "$work/flags/active-tailscaled" "$work/flags/enabled-tailscaled" "$work/flags/active-apcupsd" "$work/flags/enabled-celeris-resume-check.service" "$work/flags/addr" "$work/flags/ping" "$work/flags/modinfo"

mktree() { # mktree DIR BOOT_ID TOTAL
	local r=$1
	mkdir -p "$r/sys/class/net/enp1s0/device/msi_irqs" "$r/proc/irq/167" "$r/proc/net/bonding" "$r/proc/sys/kernel/random" "$r/boot" "$r/run/celeris" \
		"$r/usr/lib/systemd/system-shutdown" "$r/usr/local/sbin" "$r/etc/tmpfiles.d"
	ln -s /x/r8127 "$r/sys/class/net/enp1s0/device/driver"
	touch "$r/sys/class/net/enp1s0/device/msi_irqs/167" "$r/boot/vmlinuz-7.0.0-14-generic" "$r/usr/local/sbin/cluster-power-event" "$r/etc/tmpfiles.d/celeris-power.conf"
	chmod +x "$r/usr/local/sbin/cluster-power-event"
	printf ' 167:  %s  0  ITS-MSI 2  Edge      enp1s0-8\n' "$3" > "$r/proc/interrupts"
	echo 1 > "$r/proc/irq/167/smp_affinity_list"
	echo 1 > "$r/proc/irq/167/effective_affinity_list"
	echo "$2" > "$r/proc/sys/kernel/random/boot_id"
	echo 7.0.0-14-generic > "$r/proc/sys/kernel/osrelease"
	echo "100.0 1.0" > "$r/proc/uptime"
	printf 'btime 1700000000\n' > "$r/proc/stat"
	printf 'Bonding Mode: IEEE 802.3ad\n\nSlave Interface: enp1s0\nMII Status: up\n' > "$r/proc/net/bonding/bond0"
	printf '#!/bin/sh\n' > "$r/usr/lib/systemd/system-shutdown/quiesce-r8127.shutdown"
	chmod +x "$r/usr/lib/systemd/system-shutdown/quiesce-r8127.shutdown"
}
mktree "$work/before" aaaaaaaa-0000-0000-0000-000000000001 4050000000
mktree "$work/after" bbbbbbbb-0000-0000-0000-000000000002 1200
mktree "$work/after-same-boot" aaaaaaaa-0000-0000-0000-000000000001 1200

cat > "$pb/inventory.yml" << 'EOT'
all:
  vars:
    ansible_connection: local
    ansible_user: mini
    ansible_python_interpreter: "{{ ansible_playbook_python }}"
    manifest_path: /tmp/celeris-bench-manifest.json
    results_root: /tmp/celeris-results
  children:
    bench_targets:
      hosts:
        msa2-server:
          lan_ip: 192.168.50.65
        msr1:
          lan_ip: 192.168.50.199
          nic_irq_placement: {iface: enp1s0, vector: enp1s0-8, cpu: 1}
    loadgen:
      hosts:
        msa2-client:
          lan_ip: 192.168.50.195
    cluster:
      children:
        bench_targets:
        loadgen:
EOT

# play ID EXTRA_VARS_JSON -> $log $rc, $out = $work/out.ID
play() {
	local id=$1 extra=$2
	out=$work/out.$id
	rm -rf "$out" "$work/state"/*
	mkdir -p "$out"
	log=$work/play.$id.log
	(cd "$pb" && PATH=$work/bin:$PATH STATE=$work/state ANSIBLE_LOCALHOST_WARNING=False ANSIBLE_INVENTORY_UNPARSED_WARNING=False ANSIBLE_DEPRECATION_WARNINGS=False \
		ANSIBLE_ROLES_PATH=/nonexistent ansible-playbook -i inventory.yml reboot.yml \
		-e "{\"reboot_reason\":\"rule D counters\",\"reboot_run_id\":\"4242\",\"reboot_out_dir\":\"$out\"}" -e "$extra" "${@:3}") > "$log" 2>&1
	rc=$?
}
logged() { grep -qF -- "$1" "$log"; }
rebooted() { [ -e "$work/state/rebooted" ]; }
GOOD='{"reboot_host":"msr1","reboot_confirm":"REBOOT-msr1"}'

play ok "$GOOD"
check "a healthy reboot: the play succeeds" test "$rc" = 0
check "it rebooted" rebooted
check "the phase reached is verified" bash -c "[ \"\$(cat '$out/msr1/phase.txt')\" = verified ]"
check "pre, post and preflight records exist for the target" bash -c "test -s '$out/msr1/pre-snapshot.json' && test -s '$out/msr1/post-snapshot.json' && test -s '$out/msr1/preflight-snapshot.json'"
check "the other hosts left a preflight record and nothing else" bash -c "test -s '$out/msa2-client/preflight-snapshot.json' && test ! -e '$out/msa2-client/pre-snapshot.json'"
check "the verdict records both boot ids and no problem" bash -c "grep -q 'boot_id_before.*aaaaaaaa' '$out/msr1/result.json' && grep -q 'boot_id_after.*bbbbbbbb' '$out/msr1/result.json' && grep -q '\"problems\": \[\]' '$out/msr1/result.json'"
check "the reason is in the verdict as given" bash -c "grep -q 'rule D counters' '$out/msr1/result.json'"

play badconfirm '{"reboot_host":"msr1","reboot_confirm":"REBOOT-msa2-server"}'
check "a wrong confirmation fails" test "$rc" != 0
check "a wrong confirmation reboots nothing" bash -c "! [ -e '$work/state/rebooted' ]"
check "a wrong confirmation touches no host (no preflight record)" bash -c "test ! -e '$out/msr1/preflight-snapshot.json'"
play client '{"reboot_host":"msa2-client","reboot_confirm":"REBOOT-msa2-client"}'
check "msa2-client is refused" test "$rc" != 0
check "msa2-client is not rebooted" bash -c "! [ -e '$work/state/rebooted' ]"
play multiline '{"reboot_host":"msr1","reboot_confirm":"REBOOT-msr1","reboot_reason":"two\nlines"}'
check "a multi-line reason is refused" test "$rc" != 0
play all '{"reboot_host":"all","reboot_confirm":"REBOOT-all"}'
check "a host pattern such as all is refused" test "$rc" != 0

# refusals on the target
mkdir -p "$work/before/tmp/celeris-results/old"
play results "$GOOD"
check "entries in results_root refuse the reboot" test "$rc" != 0
check "the refusal says nothing was rebooted and names the entries" logged "refused, nothing was rebooted: results_root /tmp/celeris-results holds 1 entr(ies) a reboot destroys (old)"
check "a refused reboot does not reboot" bash -c "! [ -e '$work/state/rebooted' ]"
check "the refusal is written for the step summary" bash -c "grep -q 'results_root /tmp/celeris-results holds 1 entr' '$out/msr1/refusal.txt'"
play results_ok '{"reboot_host":"msr1","reboot_confirm":"REBOOT-msr1","allow_results_loss":true}'
check "allow_results_loss=true goes ahead" test "$rc" = 0
rm -rf "$work/before/tmp"

# a manifest on ANY host refuses: here the loadgen's (all three read the same tree, so the target's too)
touch "$work/before/tmp.manifest-marker"
mkdir -p "$work/before/tmp" && touch "$work/before/tmp/celeris-bench-manifest.json"
play manifest "$GOOD"
check "a bench manifest refuses" test "$rc" != 0
check "the refusal names the manifest" logged "exists on"
check "the manifest refusal is written for the step summary" bash -c "grep -q 'celeris-bench-manifest.json exists on' '$out/msr1/refusal.txt'"
check "nothing is rebooted with a manifest present" bash -c "! [ -e '$work/state/rebooted' ]"
rm -rf "$work/before/tmp" "$work/before/tmp.manifest-marker"

# the property that matters most: a peer that fails its preflight stops the target
FAIL_HOST=msa2-client play peer "$GOOD"
check "a failing preflight snapshot on the loadgen fails the run" test "$rc" != 0
check "the failure is the loadgen's" logged "snapshot failed (test)"
check "a failing preflight on a peer stops the reboot of the target" bash -c "! [ -e '$work/state/rebooted' ]"

# the host came back with the same boot id: rebooted but not verified, and the evidence is kept
AFTER=after-same-boot play sameboot "$GOOD"
check "an unchanged boot id fails the run" test "$rc" != 0
check "it says the host is NOT verified and why" logged "the host rebooted but is NOT verified: boot_id did not change"
check "the phase stays at returned" bash -c "[ \"\$(cat '$out/msr1/phase.txt')\" = returned ]"
check "the evidence of a failed verification is kept" bash -c "test -s '$out/msr1/post-snapshot.json' && grep -q 'boot_id did not change' '$out/msr1/result.json'"

# a host that never answers the fabric after the reboot
rm -f "$work/flags/ping"
play noping "$GOOD"
touch "$work/flags/ping"
check "no answer from the loadgen over the fabric is not verified" logged "no answer from 192.168.50.195 over the fabric"

echo "reboot_play: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
