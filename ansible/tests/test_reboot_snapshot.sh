#!/usr/bin/env bash
# Tests ansible/files/reboot_snapshot.sh against a FAKE /proc, /sys, /boot and /var tree. Plain bash, no root,
# no NIC:
#   bash ansible/tests/test_reboot_snapshot.sh
# In a container (what proves the Linux awk/sort/ls behaviour the cluster hosts have):
#   docker run --rm -v "$PWD":/repo:ro debian:stable-slim bash /repo/ansible/tests/test_reboot_snapshot.sh
# workflow_cluster_reboot_test.go runs this file in CI and decodes the script's JSON strictly.
#
# What it pins: the NIC vectors are found by NAME and by msi_irqs (never by number) and summed over the CPUs,
# a vector of another device is not reported, the placement vector is looked up by exact name, the state a
# reboot must not interrupt (manifest, results, runner worker, resume-pending) is reported, and the script
# WRITES NOTHING (the tree is identical after a run).
set -u

here=$(cd "$(dirname "$0")" && pwd)
snap=$here/../files/reboot_snapshot.sh
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

pass=0
fail=0
ok() { pass=$((pass + 1)); echo "ok   $1"; }
bad() { fail=$((fail + 1)); echo "FAIL $1" >&2; }
has() { grep -qF -- "$1" <<< "$out"; }
check() { # $1 name, $2 the exact JSON fragment that must be in $out
	if has "$2"; then ok "$1"; else bad "$1: no $2 in: $out"; fi
}
nocheck() {
	if has "$2"; then bad "$1: found $2 in: $out"; else ok "$1"; fi
}

# mkroot DIR - an msr1-shaped host: two r8127 ports in a bond, vectors enp1s0-8 (irq 167) and enp49s0-8 (irq 135),
# a vector of ANOTHER device whose name merely resembles a NIC's (nvme0q1), one runner listener.
mkroot() {
	local r=$1
	mkdir -p "$r/sys/class/net/lo" "$r/sys/class/net/bond0" "$r/sys/class/net/enp1s0/device/msi_irqs" "$r/sys/class/net/enp49s0/device/msi_irqs" \
		"$r/proc/irq/166" "$r/proc/irq/167" "$r/proc/irq/135" "$r/proc/net/bonding" "$r/proc/sys/kernel/random" "$r/proc/4242" \
		"$r/boot" "$r/run/celeris" "$r/tmp" "$r/usr/lib/systemd/system-shutdown" "$r/usr/local/sbin" "$r/var/lib" "$r/etc/tmpfiles.d"
	ln -s /nonexistent/drivers/r8127 "$r/sys/class/net/enp1s0/device/driver"
	ln -s /nonexistent/drivers/r8127 "$r/sys/class/net/enp49s0/device/driver"
	touch "$r/sys/class/net/enp1s0/device/msi_irqs/166" "$r/sys/class/net/enp1s0/device/msi_irqs/167" "$r/sys/class/net/enp1s0/device/msi_irqs/170"
	touch "$r/sys/class/net/enp49s0/device/msi_irqs/135"
	# 181 is listed by the port but its name does not start with the interface (found by number only); 136 is not
	# listed by the port but its name does (found by name only)
	touch "$r/sys/class/net/enp1s0/device/msi_irqs/181"
	cat > "$r/proc/interrupts" << 'EOT'
           CPU0       CPU1       CPU2
 135:         10 4000000000  50000000  ITS-MSI 4  Edge      enp49s0-8
 136:          5          0          0  ITS-MSI 5  Edge      enp49s0-9
 181:          7          0          0  ITS-MSI 6  Edge      r8127-rx-0
 166:          0          0         12  ITS-MSI 1  Edge      enp1s0-0
 167:          0          0       9000  ITS-MSI 2  Edge      enp1s0-8
 180:         99         99         99  ITS-MSI 9  Edge      nvme0q1
IPI0:          1          2          3  Rescheduling interrupts
EOT
	echo 0-11 > "$r/proc/irq/166/smp_affinity_list"
	echo 1 > "$r/proc/irq/167/smp_affinity_list"
	echo 1 > "$r/proc/irq/167/effective_affinity_list"
	echo 0-11 > "$r/proc/irq/135/smp_affinity_list"
	echo 8 > "$r/proc/irq/135/effective_affinity_list"
	echo 4f8d7e5a-1111-2222-3333-444455556666 > "$r/proc/sys/kernel/random/boot_id"
	echo 7.0.0-14-generic > "$r/proc/sys/kernel/osrelease"
	echo "12345.67 99999.00" > "$r/proc/uptime"
	printf 'cpu 1 2 3\nbtime 1700000000\n' > "$r/proc/stat"
	cat > "$r/proc/net/bonding/bond0" << 'EOT'
Ethernet Channel Bonding Driver: v6.1
Bonding Mode: IEEE 802.3ad Dynamic link aggregation
MII Status: up

Slave Interface: enp1s0
MII Status: up
Speed: 10000 Mbps

Slave Interface: enp49s0
MII Status: down
Speed: Unknown
EOT
	echo Runner.Listener > "$r/proc/4242/comm"
	touch "$r/boot/vmlinuz-7.0.0-9-generic" "$r/boot/vmlinuz-7.0.0-14-generic" "$r/boot/vmlinuz-6.19.0-1-generic"
	printf '#!/bin/sh\n' > "$r/usr/lib/systemd/system-shutdown/quiesce-r8127.shutdown"
	chmod +x "$r/usr/lib/systemd/system-shutdown/quiesce-r8127.shutdown"
}
run() { # run ROOT ARGS...; leaves stdout in $out and the status in $rc
	local r=$1
	shift
	out=$(HOST_SNAPSHOT_ROOT=$r bash "$snap" "$@" 2> "$work/stderr")
	rc=$?
}
# names, sizes, mtimes and modes of every entry (GNU stat: this test is for Linux), then every file's bytes
digest() {
	(
		cd "$1" || exit 1
		find . -print0 | LC_ALL=C sort -z | xargs -0 stat -c '%n %s %Y %a' | cksum
		find . -type f -print0 | LC_ALL=C sort -z | xargs -0 cat | cksum
	)
}

R=$work/host
mkroot "$R"
before=$(digest "$R")
[ -n "$before" ] || { echo "FAIL: cannot digest the tree (GNU stat and cksum are needed)" >&2; exit 1; }
run "$R" /tmp/celeris-bench-manifest.json /tmp/celeris-results enp1s0-8 - -
[ "$rc" -eq 0 ] && ok "exit 0" || bad "exit $rc: $(cat "$work/stderr")"
after=$(digest "$R")
[ "$before" = "$after" ] && ok "writes nothing: the tree is identical after the run" || bad "the run changed the tree"
check "boot id" '"boot_id":"4f8d7e5a-1111-2222-3333-444455556666"'
check "kernel" '"kernel":"7.0.0-14-generic"'
check "btime" '"btime":1700000000'
check "uptime" '"uptime_s":12345'
check "newest installed kernel is the highest version, not the last in the directory" '"newest_installed_kernel":"7.0.0-14-generic"'
check "r8127 seen" '"r8127":true'
check "both NICs listed, lo and the bond are not" '"nics":[{"iface":"enp1s0","driver":"r8127"},{"iface":"enp49s0","driver":"r8127"}]'
check "vector summed over the CPUs, found by msi_irqs and name, above 2^32" '"iface":"enp49s0","irq":135,"name":"enp49s0-8","total":4050000010,"affinity":"0-11","effective":"8"'
check "TX-completion vector with its single-CPU affinity" '"irq":167,"name":"enp1s0-8","total":9000,"affinity":"1","effective":"1"'
check "every active vector of the port is reported, not only the placement one" '"irq":166,"name":"enp1s0-0"'
check "a vector found by NAME only (not in msi_irqs) is reported" '"irq":136,"name":"enp49s0-9","total":5'
check "a vector found by msi_irqs only (a name that does not start with the interface) is reported" '"iface":"enp1s0","irq":181,"name":"r8127-rx-0","total":7'
nocheck "no vector of another device" '"nvme0q1"'
nocheck "an msi_irqs entry with no /proc/interrupts line is not reported" '"irq":170'
check "max vector total" '"max_vector_total":4050000010'
check "placement vector present by exact name" '"placement_vector":"enp1s0-8","placement_vector_present":true'
check "bond slaves with their link state" '"bond":"bond0","mode":"IEEE 802.3ad Dynamic link aggregation","slaves":["enp1s0=up","enp49s0=down"]'
check "no manifest" '"manifest_present":false'
check "no results" '"results_entries":0'
check "one runner listener, no worker" '"runner_listeners":1,"runner_workers":0'
check "no resume pending" '"resume_pending":false'
check "no crash dump" '"crash":[]'
check "the shutdown quiesce hook is installed" '"quiesce_hook":true'
check "the fabric checks are off with -" '"lan_ip_present":null'
check "the peer ping is off with -" '"peer_ping_ok":null'
check "a probe that cannot run says so (no modinfo on the test PATH or no module)" '"modinfo_r8127":'

# the placement vector is matched by exact name: a prefix, a suffix and another NIC's vector are all absent
for v in enp1s0 enp1s0-80 1s0-8 nvme; do
	run "$R" /tmp/m /tmp/r "$v" - -
	check "placement vector $v is not enp1s0-8" '"placement_vector_present":false'
done
run "$R" /tmp/m /tmp/r - - -
check "no placement vector asked: null, not false" '"placement_vector_present":null'

# in-flight state a reboot must not interrupt or destroy
mkdir -p "$R/tmp/celeris-results/20261020T010101-bench-msr1" "$R/var/crash/202610201200" "$R/var/lib/celeris-power" "$R/tmp/actions-runner-msr1" "$R/proc/4343"
touch "$R/tmp/celeris-bench-manifest.json" "$R/var/lib/celeris-power/resume-pending.json" "$R/tmp/celeris-results/x"
echo Runner.Worker > "$R/proc/4343/comm"
run "$R" /tmp/celeris-bench-manifest.json /tmp/celeris-results enp1s0-8 - -
check "manifest present" '"manifest_present":true'
check "results entries counted and listed" '"results_entries":2,"results_list":["20261020T010101-bench-msr1","x"]'
check "resume pending" '"resume_pending":true'
check "runner dir listed" '"runner_dirs":["actions-runner-msr1"]'
check "a runner worker is counted" '"runner_workers":1'
check "a crash dump is listed" '"crash":["202610201200"]'

# a host that is not r8127 has no module probe; a host with no bond has no bonds; a missing hook is false
R2=$work/plain
mkroot "$R2"
rm -f "$R2/sys/class/net/enp1s0/device/driver" "$R2/sys/class/net/enp49s0/device/driver" "$R2/usr/lib/systemd/system-shutdown/quiesce-r8127.shutdown"
rm -rf "$R2/proc/net/bonding/bond0"
run "$R2" /tmp/m /tmp/r - - -
check "not r8127" '"r8127":false,"modinfo_r8127":null'
check "no bonds" '"bonds":[]'
check "missing quiesce hook" '"quiesce_hook":false'

# fabric checks: the address is looked up by exact match (a host cannot match 192.168.50.19 for .195)
if command -v ip > /dev/null 2>&1; then
	run "$R" /tmp/m /tmp/r - 203.0.113.77 -
	check "an address the host does not carry is absent" '"lan_ip_present":false'
	run "$R" /tmp/m /tmp/r - 127.0.0.1 -
	check "an address the host carries is present" '"lan_ip_present":true'
else
	echo "SKIP fabric address checks: no ip(8)"
fi

# the unit-file state of tailscaled is reported as systemctl prints it (a stand-in systemctl on PATH)
mkdir -p "$work/stub"
cat > "$work/stub/systemctl" << 'EOT'
#!/bin/bash
case "$1 $2" in
"is-enabled tailscaled") echo enabled-runtime; exit 1 ;;
"is-active tailscaled") echo active ;;
"is-system-running "*) echo running ;;
*) exit 3 ;;
esac
EOT
chmod +x "$work/stub/systemctl"
out=$(PATH=$work/stub:$PATH HOST_SNAPSHOT_ROOT=$R bash "$snap" /tmp/m /tmp/r - - - 2>&1)
check "tailscaled active and its unit-file state" '"tailscaled_active":true,"tailscaled_enabled":"enabled-runtime"'
out=$(HOST_SNAPSHOT_ROOT=$R PATH=/usr/bin:/bin bash "$snap" /tmp/m /tmp/r - - - 2>&1)

# usage
out=$(bash "$snap" a b 2>&1)
rc=$?
[ "$rc" -eq 2 ] && ok "wrong argument count exits 2" || bad "wrong argument count exited $rc"

echo "reboot_snapshot: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
