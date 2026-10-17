#!/usr/bin/env bash
# Runs the REAL ansible/tasks/nic_irq_placement.yml and nic_irq_placement_end.yml (not --check) on a local
# connection against a fake /proc and /sys tree, with a stand-in sudo, and checks what the play does:
#   pin applied + record written, second run idempotent (changed=0), readback mismatch fails the play,
#   an unset / empty variable never reaches the tasks (the gate bench.yml uses), end-of-run drift is recorded.
#   bash ansible/tests/test_nic_irq_placement_play.sh      (needs ansible-playbook; skips with exit 0 without)
set -u
here=$(cd "$(dirname "$0")/.." && pwd)
command -v ansible-playbook > /dev/null || { echo "SKIP: ansible-playbook not installed"; exit 0; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/pb" "$work/bin"
cp -R "$here/files" "$here/tasks" "$work/pb/"
cat > "$work/bin/sudo" << 'EOT'
#!/bin/bash
# test-only: while the flag file exists, become fails the way a missing sudo password does (the script never runs)
if [ -n "${SUDO_FAIL_FLAG:-}" ] && [ -e "$SUDO_FAIL_FLAG" ]; then echo "sudo: a password is required" >&2; exit 1; fi
while [ $# -gt 0 ]; do case "$1" in -u|-p|-g|-C|-U) shift 2;; -*) shift;; *) break;; esac; done
exec "$@"
EOT
chmod +x "$work/bin/sudo"
cat > "$work/pb/play.yml" << 'EOT'
- name: nic irq placement under test
  hosts: localhost
  connection: local
  gather_facts: false
  vars:
    bench_target: localhost
    bench_run_dir: "{{ run_dir }}"
  tasks:
    - ansible.builtin.file: {path: "{{ run_dir }}", state: directory}
    # the gate bench.yml puts around both includes
    - ansible.builtin.include_tasks: tasks/nic_irq_placement.yml
      when:
        - inventory_hostname == bench_target
        - (nic_irq_placement | default({})) | length > 0
    # test-only: the vector disappears between the pin and the end-of-run readback (a driver re-requested its IRQs)
    - ansible.builtin.copy:
        content: ""
        dest: "{{ lookup('env', 'NIC_IRQ_ROOT') }}/proc/interrupts"
      when: test_vector_vanishes | default(false) | bool
    # test-only: become breaks between the pin and the end-of-run readback, so the script never runs there
    - ansible.builtin.copy:
        content: ""
        dest: "{{ lookup('env', 'SUDO_FAIL_FLAG') }}"
      when: test_become_fails | default(false) | bool
    - ansible.builtin.include_tasks: tasks/nic_irq_placement_end.yml
      when:
        - inventory_hostname == bench_target
        - (nic_irq_placement | default({})) | length > 0
        - nic_irq_record is defined
EOT

pass=0
fail=0
ok() { pass=$((pass + 1)); echo "ok   $1"; }
bad() { fail=$((fail + 1)); echo "FAIL $1" >&2; }
check() { local n=$1; shift; if "$@"; then ok "$n"; else bad "$n"; fi; }

mkroot() { # mkroot DIR follows|stale
	local r=$1
	mkdir -p "$r/sys/class/net/enp1s0/device/msi_irqs" "$r/sys/class/net/enp49s0/device/msi_irqs" "$r/proc/irq/167" "$r/proc/irq/169"
	touch "$r/sys/class/net/enp1s0/device/msi_irqs/167" "$r/sys/class/net/enp49s0/device/msi_irqs/169"
	printf ' 167:  0  0  ITS-MSI 2 Edge      enp1s0-8\n 169:  0  0  ITS-MSI 4 Edge      enp49s0-8\n' > "$r/proc/interrupts"
	echo 0-11 > "$r/proc/irq/167/smp_affinity_list"
	echo 0-11 > "$r/proc/irq/169/smp_affinity_list"
	if [ "$2" = follows ]; then
		ln -s smp_affinity_list "$r/proc/irq/167/effective_affinity_list"
		ln -s smp_affinity_list "$r/proc/irq/169/effective_affinity_list"
	else
		echo 8 > "$r/proc/irq/167/effective_affinity_list"
		echo 8 > "$r/proc/irq/169/effective_affinity_list"
	fi
}
play() { # play ROOT RUNDIR EXTRA_VARS_JSON [MORE_VARS_JSON] -> $log, $rc
	log=$work/play.log
	rm -f "$work/sudo-fail"
	(cd "$work/pb" && PATH=$work/bin:$PATH SUDO_FAIL_FLAG=$work/sudo-fail NIC_IRQ_ROOT=$1 ANSIBLE_LOCALHOST_WARNING=False ANSIBLE_INVENTORY_UNPARSED_WARNING=False \
		ANSIBLE_DEPRECATION_WARNINGS=False ansible-playbook -i localhost, play.yml -e "run_dir=$2" -e "$3" -e "${4:-{\}}") > "$log" 2>&1
	rc=$?
}
PLACEMENT='{"nic_irq_placement":{"iface":"enp1s0","vector":"enp1s0-8","cpu":1}}'
logged() { grep -qF -- "$1" "$log"; }

R=$work/r1
mkroot "$R" follows
play "$R" "$work/run1" "$PLACEMENT"
check "play succeeds" test "$rc" = 0
echo "---- log line and record of the first run ----"
grep -A4 'NIC IRQ placement on' "$log" | head -8
cat "$work/run1/nic-irq-placement.json" 2> /dev/null
echo "----"
check "the vector was pinned to CPU 1" test "$(cat "$R/proc/irq/167/smp_affinity_list")" = 1
check "enp49s0's vector is untouched" test "$(cat "$R/proc/irq/169/smp_affinity_list")" = 0-11
check "the log prints interface, vector, cpu and CHANGED" logged "enp1s0 vector enp1s0-8 -> CPU 1 (CHANGED"
check "the log prints the before/after of the irq" logged "irq 167: smp_affinity_list 0-11 -> 1"
check "nic-irq-placement.json exists in the run dir" test -s "$work/run1/nic-irq-placement.json"
check "the record has host, interface, irq, cpu" bash -c "grep -q '\"host\": \"localhost\"' '$work/run1/nic-irq-placement.json' && grep -q '\"interface\": \"enp1s0\"' '$work/run1/nic-irq-placement.json' && grep -q '\"irq\": 167' '$work/run1/nic-irq-placement.json' && grep -q '\"cpu\": 1' '$work/run1/nic-irq-placement.json'"
check "the record carries the pre-existing value" bash -c "grep -q '\"affinity\": \"0-11\"' '$work/run1/nic-irq-placement.json'"
check "the end-of-run readback is in the record, verified" bash -c "python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); assert d[\"at_end\"][\"verified\"] is True and d[\"verified\"] is True' '$work/run1/nic-irq-placement.json'"

play "$R" "$work/run2" "$PLACEMENT"
check "second run succeeds" test "$rc" = 0
check "second run: already in place, nothing changed" logged "already in place"
check "second run: no pin task reported changed" bash -c "! grep -E '^changed: .*pin the vector' '$log'"

R=$work/r2
mkroot "$R" stale
play "$R" "$work/run3" "$PLACEMENT"
check "readback mismatch FAILS the play" test "$rc" != 0
check "the failure names the mismatch" logged "MISMATCH irq 167"
check "no record is written for an unverified pin" test ! -e "$work/run3/nic-irq-placement.json"

R=$work/r4
mkroot "$R" follows
play "$R" "$work/run8" "$PLACEMENT" '{"test_vector_vanishes":true}'
check "end-of-run exit 2 (vector gone): the play still succeeds" test "$rc" = 0
check "end-of-run exit 2: the WARNING fires" logged "WARNING: NIC IRQ placement DRIFTED"
check "end-of-run exit 2: the record is written, at_end.verified=false, with the message" bash -c "python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); assert d[\"verified\"] is True and d[\"at_end\"][\"verified\"] is False and \"no interrupt named\" in d[\"at_end_error\"]' '$work/run8/nic-irq-placement.json'"

R=$work/r6
mkroot "$R" follows
play "$R" "$work/run10" "$PLACEMENT" '{"test_become_fails":true}'
check "end-of-run become failure: the play still succeeds" test "$rc" = 0
check "end-of-run become failure: the WARNING fires" logged "WARNING: NIC IRQ placement DRIFTED"
check "end-of-run become failure: not read as success, at_end.verified=false and the error is recorded" bash -c "python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); assert d[\"verified\"] is True and d[\"at_end\"][\"verified\"] is False and \"password\" in d[\"at_end_error\"]' '$work/run10/nic-irq-placement.json'"

R=$work/r5
mkroot "$R" follows
play "$R" "$work/run9" '{"nic_irq_placement":{"iface":"enp1s0","vector":"enp1s0-9","cpu":1}}'
check "unknown vector at pin time fails the play" test "$rc" != 0
check "unknown vector: the log says no interrupt named" logged "no interrupt named"
check "unknown vector: no record, vector untouched" bash -c "[ ! -e '$work/run9/nic-irq-placement.json' ] && [ \$(cat '$R/proc/irq/167/smp_affinity_list') = 0-11 ]"

R=$work/r3
mkroot "$R" follows
play "$R" "$work/run4" '{"nic_irq_placement":{}}'
check "empty variable: play succeeds" test "$rc" = 0
check "empty variable: the vector is not touched" test "$(cat "$R/proc/irq/167/smp_affinity_list")" = 0-11
check "empty variable: no record" test ! -e "$work/run4/nic-irq-placement.json"
play "$R" "$work/run5" '{"unrelated":1}'
check "unset variable: no-op, no record, vector untouched" bash -c "[ \$(cat '$R/proc/irq/167/smp_affinity_list') = 0-11 ] && [ ! -e '$work/run5/nic-irq-placement.json' ]"

play "$R" "$work/run6" '{"nic_irq_placement":{"iface":"enp1s0","vector":"enp1s0-8","cpu":"0-3"}}'
check "a CPU list in the variable fails the play" test "$rc" != 0
play "$R" "$work/run11" '{"nic_irq_placement":{"iface":"enp1s0","vector":"enp1s0-8","cpu":"01"}}'
check "a leading-zero CPU in the variable fails the play" test "$rc" != 0
play "$R" "$work/run12" '{"nic_irq_placement":{"iface":"enp1s0","vector":"enp49s0-8","cpu":1}}'
check "a vector of another interface fails the play, nothing written" bash -c "[ $rc != 0 ] && [ \$(cat '$R/proc/irq/169/smp_affinity_list') = 0-11 ]"
play "$R" "$work/run7" '{"nic_irq_placement":{"iface":"enp1s0"}}'
check "a malformed variable fails the play" test "$rc" != 0

echo "nic_irq_placement play: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
