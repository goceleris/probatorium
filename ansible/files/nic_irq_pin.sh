#!/usr/bin/env bash
# nic_irq_pin.sh - pin one NIC interrupt vector to ONE CPU and VERIFY it by readback.
#
# Run by ansible/tasks/nic_irq_placement.yml on the bench target (root). The
# placement is a standing, documented part of the msr1 benchmark setup, see the
# comment on nic_irq_placement in ansible/inventory.yml (goceleris/celeris#1036).
#
# Usage: nic_irq_pin.sh MODE IFACE VECTOR CPU
#   MODE    "pin" writes the CPU and verifies; "verify" only reads back and checks (no write)
#   IFACE   interface the vector belongs to (e.g. enp1s0); /sys/class/net/<iface> must exist, and every
#           IRQ of the vector must be bound to it (see "Binding of VECTOR to IFACE" below), else exit 2
#   VECTOR  interrupt NAME as /proc/interrupts prints it in its last column (e.g. enp1s0-8).
#           Matched by exact equality, never as a pattern and never by IRQ number: the
#           number changes across boots. Every line with that name is pinned.
#   CPU     the ONE CPU that must service the vector (a plain integer without leading zeros, not a
#           list or a mask)
# Arguments, not environment: ansible.builtin.script does not apply the task's `environment`.
# Environment: NIC_IRQ_ROOT is a test override, a prefix for /proc and /sys. Empty on a host.
#
# Binding of VECTOR to IFACE: a vector name alone could belong to another port (enp49s0-8 is a valid name
# on msr1), so a vector is accepted when EITHER (a) each of its IRQs is listed under
# /sys/class/net/<iface>/device/msi_irqs/ OR (b) its name starts with "<iface>-" as /proc/interrupts prints
# it. It is refused (exit 2, before a single write) only when BOTH fail. Either alone suffices because an
# out-of-tree driver may expose one of them differently. The JSON says which check passed: "binding" is
# "msi_irqs" when every IRQ was listed there, else "name".
#
# Why an explicit pin and not a restore: writing a mask back (0-11) does not put the vector back on the CPU
# it was on, the kernel picks one from the mask. So the CPU is written as a single-CPU list and the EFFECTIVE
# affinity (what the interrupt controller actually programmed) is read back and must equal it.
#
# Idempotent: when smp_affinity_list AND effective_affinity_list already equal the CPU nothing is written
# and "changed" is false.
#
# stdout: one JSON object once the checks of the inputs have passed (also on a failed write or readback, with
# "verified": false), stderr: human lines. Every refusal (exit 2) happens BEFORE the first write and prints no
# JSON: bad input, an unknown vector, a vector that is not the interface's, an IRQ without its affinity files.
# Exit 0 only when every matching vector is verified on the CPU; 2 on a refusal, 3 on a write or readback
# failure.
set -u

R=${NIC_IRQ_ROOT:-}
[ $# -eq 4 ] || { echo "usage: nic_irq_pin.sh MODE IFACE VECTOR CPU" >&2; exit 2; }
mode=$1
iface=$2
vector=$3
cpu=$4

die() { echo "nic_irq_pin: $*" >&2; exit 2; }

case "$mode" in pin | verify) ;; *) die "MODE must be pin or verify, got '$mode'" ;; esac
[[ $iface =~ ^[A-Za-z0-9_.-]+$ ]] || die "IFACE '$iface' is empty or not an interface name"
[[ $vector =~ ^[A-Za-z0-9_.:-]+$ ]] || die "VECTOR '$vector' is empty or not an interrupt name"
[[ $cpu =~ ^(0|[1-9][0-9]*)$ ]] || die "CPU '$cpu' is not a single CPU number (digits only, no leading zero)"
[ -d "$R/sys/class/net/$iface" ] || die "interface $iface does not exist on this host"

irqs=$(awk -v n="$vector" '$NF == n { sub(":", "", $1); print $1 }' "$R/proc/interrupts" | sort -n)
[ -n "$irqs" ] || die "no interrupt named '$vector' in /proc/interrupts"

# Every IRQ of the vector must belong to IFACE, and must have its affinity files, before anything is written.
binding=msi_irqs
for irq in $irqs; do
	if [ -e "$R/sys/class/net/$iface/device/msi_irqs/$irq" ]; then
		echo "nic_irq_pin: irq $irq ($vector) bound to $iface: listed in /sys/class/net/$iface/device/msi_irqs" >&2
	else
		case "$vector" in
		"$iface"-*)
			binding=name
			echo "nic_irq_pin: irq $irq ($vector) bound to $iface: not listed in device/msi_irqs, the name starts with '$iface-'" >&2
			;;
		*) die "irq $irq ($vector) is neither listed in /sys/class/net/$iface/device/msi_irqs nor named '$iface-*': refusing to pin another device's vector" ;;
		esac
	fi
	[ -r "$R/proc/irq/$irq/smp_affinity_list" ] || die "irq $irq has no smp_affinity_list"
	[ -r "$R/proc/irq/$irq/effective_affinity_list" ] || die "irq $irq has no effective_affinity_list: the pin cannot be verified"
done

trim() { tr -d '[:space:]' < "$1" 2> /dev/null; }
aff() { trim "$R/proc/irq/$1/smp_affinity_list"; }
eff() { trim "$R/proc/irq/$1/effective_affinity_list"; }
jstr() { printf '"%s"' "$1"; }

changed=false
verified=true
items=""
for irq in $irqs; do
	d=$R/proc/irq/$irq
	b_aff=$(aff "$irq")
	b_eff=$(eff "$irq")
	if [ "$mode" = pin ] && { [ "$b_aff" != "$cpu" ] || [ "$b_eff" != "$cpu" ]; }; then
		echo "nic_irq_pin: irq $irq ($vector): smp_affinity_list=$b_aff effective=$b_eff -> writing $cpu" >&2
		if ! echo "$cpu" > "$d/smp_affinity_list"; then
			echo "nic_irq_pin: FAILED: could not write $cpu to $d/smp_affinity_list" >&2
			verified=false
		fi
		changed=true
	fi
	# The effective mask is updated by the kernel after the write; give it a moment before calling a mismatch.
	a_aff=$(aff "$irq")
	a_eff=$(eff "$irq")
	for _ in 1 2 3 4 5 6 7 8 9 10; do
		{ [ "$a_aff" = "$cpu" ] && [ "$a_eff" = "$cpu" ]; } && break
		sleep 0.1
		a_aff=$(aff "$irq")
		a_eff=$(eff "$irq")
	done
	if [ "$a_aff" != "$cpu" ] || [ "$a_eff" != "$cpu" ]; then
		echo "nic_irq_pin: MISMATCH irq $irq ($vector): wanted CPU $cpu, read smp_affinity_list=$a_aff effective_affinity_list=$a_eff" >&2
		verified=false
	else
		echo "nic_irq_pin: irq $irq ($vector) verified on CPU $cpu (before: affinity=$b_aff effective=$b_eff)" >&2
	fi
	[ -n "$items" ] && items="$items,"
	items="$items{\"irq\":$irq,\"before\":{\"affinity\":$(jstr "$b_aff"),\"effective\":$(jstr "$b_eff")},\"after\":{\"affinity\":$(jstr "$a_aff"),\"effective\":$(jstr "$a_eff")}}"
done

printf '{"interface":%s,"vector":%s,"binding":%s,"cpu":%s,"changed":%s,"verified":%s,"irqs":[%s]}\n' \
	"$(jstr "$iface")" "$(jstr "$vector")" "$(jstr "$binding")" "$cpu" "$changed" "$verified" "$items"
[ "$verified" = true ] || exit 3
exit 0
