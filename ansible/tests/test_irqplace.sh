#!/usr/bin/env bash
# THROWAWAY (ab/irqplace-v1). Tests ansible/files/irqplace.sh against a FAKE /proc and /sys tree, the way test_nic_irq_pin.sh does:
#   docker run --rm -v "$PWD":/repo:ro debian:stable-slim bash /repo/ansible/tests/test_irqplace.sh
# "follows": effective_affinity_list is a symlink to smp_affinity_list (a healthy kernel); "stale": it never changes (readback mismatch).
set -u
here=$(cd "$(dirname "$0")" && pwd)
irqp=$here/../files/irqplace.sh
pin=$here/../files/nic_irq_pin.sh
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
pass=0
fail=0
ok() { pass=$((pass + 1)); echo "ok   $1"; }
bad() { fail=$((fail + 1)); echo "FAIL $1" >&2; }
check() { local name=$1; shift; if "$@"; then ok "$name"; else bad "$name"; fi; }

mkroot() { # ROOT MODE ; msr1 shape: enp1s0-8 on irq 167, standing on CPU 1
	local r=$1 mode=$2
	mkdir -p "$r/sys/class/net/enp1s0/device/msi_irqs" "$r/proc/irq/167"
	touch "$r/sys/class/net/enp1s0/device/msi_irqs/167"
	cat > "$r/proc/interrupts" << 'EOT'
           CPU0       CPU1       CPU2       CPU3       CPU4
 167:          0          0          0          0       9000  ITS-MSI 2  Edge      enp1s0-8
EOT
	echo 1 > "$r/proc/irq/167/smp_affinity_list"
	if [ "$mode" = follows ]; then ln -s smp_affinity_list "$r/proc/irq/167/effective_affinity_list"; else echo 8 > "$r/proc/irq/167/effective_affinity_list"; fi
}
cpu() { cat "$1/proc/irq/167/smp_affinity_list"; }
run() { # ROOT ARGS... ; env for the script: fake root, fake dir, the real pin script
	local r=$1; shift
	NIC_IRQ_ROOT=$r IRQPLACE_DIR=$work/d IRQPLACE_PIN=$pin bash "$irqp" "$@" > "$work/out" 2> "$work/err"
	rc=$?
}
alive() { kill -0 "$1" 2> /dev/null; }

# 1. apply 4 arms, pins with readback, records
R=$work/r1; mkroot "$R" follows; rm -rf "$work/d"; mkdir -p "$work/run1"
run "$R" apply enp1s0 enp1s0-8 4 1 600 "$work/run1"
check "apply 4: exit 0" test "$rc" = 0
check "apply 4: vector on CPU 4" test "$(cpu "$R")" = 4
check "apply 4: armed marker" test -f "$work/d/armed"
check "apply 4: watchdog pid recorded" test -s "$work/d/watchdog.pid"
check "apply 4: APPLY line in the run dir with the verified JSON" grep -q 'APPLY .* cpu=4 standing=1 rc=0 .*"verified":true' "$work/run1/irqplace.txt"
wpid=$(cat "$work/d/watchdog.pid")
check "apply 4: watchdog is alive" alive "$wpid"

# 2. restore cell: back on 1, marker gone, watchdog gone, VERIFIED line
run "$R" restore cell enp1s0 enp1s0-8 1 0 "$work/run1"
check "restore: exit 0" test "$rc" = 0
check "restore: vector on CPU 1 (explicit)" test "$(cpu "$R")" = 1
check "restore: armed marker removed" test ! -f "$work/d/armed"
sleep 0.3
check "restore: watchdog killed" test ! -f "$work/d/watchdog.pid"
check "restore: watchdog process gone" bash -c "! kill -0 $wpid 2>/dev/null"
check "restore: RESTORE VERIFIED line" grep -q 'RESTORE .* who=cell cpu=1 VERIFIED' "$work/run1/irqplace.txt"

# 3. restore always when nothing is armed is a silent no-op (no write, no log line)
before=$(wc -l < "$work/d/log")
run "$R" restore always enp1s0 enp1s0-8 1
check "always/not armed: exit 0" test "$rc" = 0
check "always/not armed: no new log line" test "$(wc -l < "$work/d/log")" = "$before"

# 4. cancelled run: apply 4, nobody restores, the cleanup layer does
run "$R" apply enp1s0 enp1s0-8 4 1 600 "$work/run1"
run "$R" restore cleanup enp1s0 enp1s0-8 1 0 "$work/run1"
check "cleanup after cancel: exit 0" test "$rc" = 0
check "cleanup after cancel: vector back on 1" test "$(cpu "$R")" = 1
check "cleanup after cancel: marker removed" test ! -f "$work/d/armed"

# 5. watchdog: nobody restores, the watchdog does, after its sleep
run "$R" apply enp1s0 enp1s0-8 4 1 2 "$work/run1"
check "watchdog: vector on 4 right after apply" test "$(cpu "$R")" = 4
sleep 5
check "watchdog: vector back on 1 after its sleep" test "$(cpu "$R")" = 1
check "watchdog: marker removed" test ! -f "$work/d/armed"
check "watchdog: who=watchdog VERIFIED recorded" grep -q 'RESTORE .* who=watchdog cpu=1 VERIFIED' "$work/run1/irqplace.txt"

# 6. readback mismatch: apply fails (rc 3), stays armed; restore fails too and keeps the marker
R2=$work/r2; mkroot "$R2" stale; rm -rf "$work/d"
run "$R2" apply enp1s0 enp1s0-8 4 1 600 "$work/run1"
check "stale readback: apply exits 3" test "$rc" = 3
check "stale readback: stays armed" test -f "$work/d/armed"
run "$R2" restore always enp1s0 enp1s0-8 1
check "stale readback: restore exits non-zero" test "$rc" != 0
check "stale readback: marker kept" test -f "$work/d/armed"
check "stale readback: FAILED line recorded" grep -q 'who=always cpu=1 FAILED' "$work/d/log"
run "$R2" restore cell enp1s0 enp1s0-8 1 >/dev/null 2>&1
if [ -s "$work/d/watchdog.pid" ]; then kill -TERM "$(cat "$work/d/watchdog.pid")" 2> /dev/null; fi

# 7. allowlist and inputs: only CPUs 1 and 4, plain integers; refused before any write
R3=$work/r3; mkroot "$R3" follows; rm -rf "$work/d"
run "$R3" apply enp1s0 enp1s0-8 5 1 600 "$work/run1"
check "CPU 5 refused (exit 2)" test "$rc" = 2
check "CPU 5: vector untouched" test "$(cpu "$R3")" = 1
run "$R3" apply enp1s0 enp1s0-8 4x 1 600 "$work/run1"
check "CPU 4x refused (exit 2)" test "$rc" = 2
run "$R3" restore bogus enp1s0 enp1s0-8 1
check "unknown WHO refused (exit 2)" test "$rc" = 2

# 8. probe CPU == standing: pins 1 with readback, arms nothing, starts no watchdog
run "$R3" apply enp1s0 enp1s0-8 1 1 600 "$work/run1"
check "apply 1: exit 0" test "$rc" = 0
check "apply 1: not armed" test ! -f "$work/d/armed"
check "apply 1: no watchdog" test ! -f "$work/d/watchdog.pid"
check "apply 1: vector on 1" test "$(cpu "$R3")" = 1

echo "passed $pass, failed $fail"
[ "$fail" = 0 ]
