#!/usr/bin/env bash
# Tests ansible/files/nic_irq_pin.sh against a FAKE /proc and /sys tree. Plain bash, no root, no NIC:
#   bash ansible/tests/test_nic_irq_pin.sh
# In a container (what proves the Linux awk/sort/tr behaviour the cluster hosts have):
#   docker run --rm -v "$PWD":/repo:ro debian:stable-slim bash /repo/ansible/tests/test_nic_irq_pin.sh
# workflow_nic_irq_test.go runs this file in CI and decodes the script's JSON into report.NICIRQPlacement.
#
# The fake tree does not emulate the kernel. Two fixtures stand in for it:
#   - "follows": effective_affinity_list is a symlink to smp_affinity_list, so a pin shows up in the readback
#     the way it does on a healthy kernel.
#   - "stale":   effective_affinity_list is a plain file that never changes, the readback mismatch.
set -u

here=$(cd "$(dirname "$0")" && pwd)
pin=$here/../files/nic_irq_pin.sh
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

pass=0
fail=0
ok() { pass=$((pass + 1)); echo "ok   $1"; }
bad() { fail=$((fail + 1)); echo "FAIL $1" >&2; }
check() { # $1 name, then a command run as the condition
	local name=$1
	shift
	if "$@"; then ok "$name"; else bad "$name"; fi
}

# mkroot DIR EFFECTIVE_MODE  - the msr1 shape: enp1s0-8 on irq 167 (CPU 4), enp49s0-8 on irq 169 (CPU 8)
mkroot() {
	local r=$1 mode=$2
	mkdir -p "$r/sys/class/net/enp1s0" "$r/sys/class/net/enp49s0" "$r/proc/irq/167" "$r/proc/irq/168" "$r/proc/irq/169"
	cat > "$r/proc/interrupts" << 'EOT'
           CPU0       CPU1       CPU2       CPU3       CPU4
 166:          0          0          0          0         12  ITS-MSI 1  Edge      enp1s0-0
 167:          0          0          0          0       9000  ITS-MSI 2  Edge      enp1s0-8
 168:          0          0          0          0         30  ITS-MSI 3  Edge      enp1s0-80
 169:          0          0          0          0         77  ITS-MSI 4  Edge      enp49s0-8
EOT
	echo 0-11 > "$r/proc/irq/167/smp_affinity_list"
	echo 0-11 > "$r/proc/irq/168/smp_affinity_list"
	echo 0-11 > "$r/proc/irq/169/smp_affinity_list"
	case $mode in
	follows)
		ln -s smp_affinity_list "$r/proc/irq/167/effective_affinity_list"
		ln -s smp_affinity_list "$r/proc/irq/168/effective_affinity_list"
		ln -s smp_affinity_list "$r/proc/irq/169/effective_affinity_list"
		;;
	stale)
		echo 8 > "$r/proc/irq/167/effective_affinity_list"
		echo 8 > "$r/proc/irq/168/effective_affinity_list"
		echo 8 > "$r/proc/irq/169/effective_affinity_list"
		;;
	esac
}
irqfile() { cat "$1/proc/irq/$2/$3" 2> /dev/null; }
run() { # run ROOT ARGS...; leaves stdout in $out and the status in $rc
	local r=$1
	shift
	out=$(NIC_IRQ_ROOT=$r bash "$pin" "$@" 2> "$work/stderr")
	rc=$?
}
has() { case $out in *"$1"*) return 0 ;; *) return 1 ;; esac; }
errhas() { grep -qF -- "$1" "$work/stderr"; }
nowrite() { ! errhas "writing"; }

# 1. pin applied: the vector is written, read back on CPU 1, the others are untouched
R=$work/pin
mkroot "$R" follows
run "$R" pin enp1s0 enp1s0-8 1
check "pin: exit 0" test "$rc" = 0
check "pin: smp_affinity_list is 1" test "$(irqfile "$R" 167 smp_affinity_list)" = 1
check "pin: effective_affinity_list reads 1" test "$(irqfile "$R" 167 effective_affinity_list)" = 1
check "pin: JSON says changed + verified" has '"changed":true,"verified":true'
check "pin: JSON records the value found before (0-11)" has '"before":{"affinity":"0-11","effective":"0-11"}'
check "pin: JSON records the value read back after" has '"after":{"affinity":"1","effective":"1"}'
check "pin: JSON carries interface, vector, cpu" has '"interface":"enp1s0","vector":"enp1s0-8","cpu":1'
check "pin: resolved the IRQ by name (167)" has '"irq":167'
check "pin: enp1s0-80 (name prefix) untouched" test "$(irqfile "$R" 168 smp_affinity_list)" = 0-11
check "pin: enp49s0-8 untouched" test "$(irqfile "$R" 169 smp_affinity_list)" = 0-11

# 2. idempotent: a second run writes nothing and reports changed=false
run "$R" pin enp1s0 enp1s0-8 1
check "idempotent: exit 0" test "$rc" = 0
check "idempotent: changed=false" has '"changed":false,"verified":true'
check "idempotent: before equals after" has '"before":{"affinity":"1","effective":"1"},"after":{"affinity":"1","effective":"1"}'
check "idempotent: nothing was written" nowrite

# 3. readback mismatch fails loudly: the write lands but the effective CPU stays 8
R=$work/stale
mkroot "$R" stale
run "$R" pin enp1s0 enp1s0-8 1
check "mismatch: exit 3" test "$rc" = 3
check "mismatch: JSON verified=false" has '"verified":false'
check "mismatch: JSON shows the stale effective CPU" has '"after":{"affinity":"1","effective":"8"}'
check "mismatch: stderr names the MISMATCH" errhas "MISMATCH irq 167"

# 4. the mask already says 1 but the effective CPU is 8 (a stuck vector): it is NOT reported as placed
R=$work/stuck
mkroot "$R" follows
rm "$R/proc/irq/167/effective_affinity_list"
echo 1 > "$R/proc/irq/167/smp_affinity_list"
echo 8 > "$R/proc/irq/167/effective_affinity_list"
run "$R" pin enp1s0 enp1s0-8 1
check "stuck vector (mask 1, effective 8): exit 3, the mask alone is not a pin" test "$rc" = 3
check "stuck vector: it was rewritten, not skipped" errhas "writing 1"

# 5. verify mode never writes
R=$work/verify
mkroot "$R" follows
run "$R" verify enp1s0 enp1s0-8 1
check "verify on an unplaced vector: exit 3" test "$rc" = 3
check "verify: nothing written (still 0-11)" test "$(irqfile "$R" 167 smp_affinity_list)" = 0-11
check "verify: changed=false" has '"changed":false'
run "$R" pin enp1s0 enp1s0-8 1
run "$R" verify enp1s0 enp1s0-8 1
check "verify on a placed vector: exit 0" test "$rc" = 0

# 6. refusals
R=$work/refuse
mkroot "$R" follows
run "$R" pin enp1s0 enp1s0-9 1
check "unknown vector name: exit 2" test "$rc" = 2
check "unknown vector name: nothing written" test "$(irqfile "$R" 167 smp_affinity_list)" = 0-11
run "$R" pin enp9s0 enp1s0-8 1
check "unknown interface: exit 2" test "$rc" = 2
run "$R" pin enp1s0 enp1s0-8 0-3
check "a CPU list is refused (single CPU only): exit 2" test "$rc" = 2
run "$R" pin enp1s0 'enp1s0-8;x' 1
check "a vector that is not a plain name is refused: exit 2" test "$rc" = 2
run "$R" pin enp1s0 enp1s0-8
check "missing argument: exit 2" test "$rc" = 2
rm "$R/proc/irq/167/effective_affinity_list"
run "$R" pin enp1s0 enp1s0-8 1
check "no effective_affinity_list (cannot verify): exit 2" test "$rc" = 2
check "no effective_affinity_list: nothing written" test "$(irqfile "$R" 167 smp_affinity_list)" = 0-11

# 7. enp49s0 is only touched when asked for by name (the maintainer excluded it from the standing placement)
R=$work/other
mkroot "$R" follows
run "$R" pin enp1s0 enp1s0-8 1
check "enp49s0-8 stays on its own mask after the standing pin" test "$(irqfile "$R" 169 smp_affinity_list)" = 0-11

echo "nic_irq_pin: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
