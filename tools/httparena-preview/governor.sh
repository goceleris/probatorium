#!/usr/bin/env bash
# CPU frequency governor for the preview run: record, set to performance, verify, restore. Nothing here runs unless asked.
#   governor.sh record  <file>            per-CPU line: cpu<TAB>governor<TAB>driver<TAB>epp (epp "-" when the file is absent) + host-wide lines
#   governor.sh set     <orig-file>       records to <orig-file> first, then sets governor=performance on every CPU (sudo -n tee) and,
#                                         where present, energy_performance_preference=performance (best effort: see below);
#                                         read-back verifies the GOVERNOR on every CPU and exits 1 if one did not take (BEFORE benchmarking)
#   governor.sh restore <orig-file>       writes the recorded governor and EPP back on every CPU; read-back must equal the record, else exit 1
#   governor.sh verify  <file>            exit 0 when the current per-CPU state equals the record
# Drivers (read from scaling_driver, recorded): intel_pstate / amd-pstate-epp in active mode offer only performance/powersave;
# acpi-cpufreq offers more. performance must be in scaling_available_governors or `set` refuses. EPP is written AFTER the governor
# on `set` (the kernel may refuse an EPP write under the performance governor: that is a warning, not a failure, because the
# performance governor already pins the hint on those drivers) and BEFORE nothing on `restore`: the governor goes back first,
# then EPP (writable again under powersave). Test hooks: CPU_SYSFS (default /sys/devices/system/cpu), SUDO (default "sudo -n"; "" for a fake tree).
set -u
CPU_SYSFS=${CPU_SYSFS:-/sys/devices/system/cpu}
SUDO=${SUDO-sudo -n}
mode=${1:?record|set|restore|verify}; F=${2:?file}

cpus() { for d in "$CPU_SYSFS"/cpu[0-9]*; do [ -d "$d/cpufreq" ] && basename "$d"; done | sort -V; }
rd() { tr -d '\n' < "$1" 2>/dev/null || printf '%s' "-"; }
record() {
  local c d
  for c in $(cpus); do
    d="$CPU_SYSFS/$c/cpufreq"
    printf '%s\t%s\t%s\t%s\n' "$c" "$(rd "$d/scaling_governor")" "$(rd "$d/scaling_driver")" \
      "$([ -f "$d/energy_performance_preference" ] && rd "$d/energy_performance_preference" || printf -)"
  done
  # host-wide facts, recorded (never changed): pstate mode, boost
  for f in amd_pstate/status intel_pstate/status intel_pstate/no_turbo cpufreq/boost; do
    [ -f "$CPU_SYSFS/$f" ] && printf '#%s\t%s\n' "$f" "$(rd "$CPU_SYSFS/$f")"
  done
}
write() { printf '%s' "$2" | $SUDO tee "$1" > /dev/null 2>&1; }   # $SUDO unquoted on purpose: "sudo -n" is two words
cur_cpu_lines() { record | grep -v '^#'; }

case "$mode" in
  record) record > "$F" ;;
  verify) diff <(grep -v '^#' "$F") <(cur_cpu_lines) > /dev/null ;;
  set)
    record > "$F"
    [ -s "$F" ] && grep -qv '^#' "$F" || { echo "governor: no cpufreq on this host: cannot set"; exit 1; }
    for c in $(cpus); do
      d="$CPU_SYSFS/$c/cpufreq"
      grep -qw performance "$d/scaling_available_governors" 2>/dev/null || { echo "governor: performance is not available on $c"; exit 1; }
    done
    for c in $(cpus); do write "$CPU_SYSFS/$c/cpufreq/scaling_governor" performance; done
    bad=0
    for c in $(cpus); do [ "$(rd "$CPU_SYSFS/$c/cpufreq/scaling_governor")" = performance ] || bad=$((bad + 1)); done
    if [ "$bad" -ne 0 ]; then echo "governor: $bad CPU(s) did not take performance (no passwordless sudo? read-only sysfs?)"; exit 1; fi
    ew=0
    for c in $(cpus); do
      e="$CPU_SYSFS/$c/cpufreq/energy_performance_preference"
      [ -f "$e" ] || continue
      write "$e" performance; [ "$(rd "$e")" = performance ] || ew=$((ew + 1))
    done
    [ "$ew" -eq 0 ] || echo "governor: EPP stayed as it was on $ew CPU(s) under the performance governor (kernel refuses the write): warning only"
    echo "governor: performance set and read back on $(cpus | wc -l) CPU(s)"
    ;;
  restore)
    [ -s "$F" ] || { echo "governor: no record at $F: nothing to restore"; exit 0; }
    rc=0
    # governor first, then EPP
    while IFS=$'\t' read -r c g dr e; do
      case "$c" in '#'*|'') continue;; esac
      [ -d "$CPU_SYSFS/$c/cpufreq" ] && write "$CPU_SYSFS/$c/cpufreq/scaling_governor" "$g"
    done < "$F"
    while IFS=$'\t' read -r c g dr e; do
      case "$c" in '#'*|'') continue;; esac
      [ "$e" != "-" ] && [ -f "$CPU_SYSFS/$c/cpufreq/energy_performance_preference" ] && write "$CPU_SYSFS/$c/cpufreq/energy_performance_preference" "$e"
    done < "$F"
    if verify_out=$(diff <(grep -v '^#' "$F") <(cur_cpu_lines)); then echo "governor: restored, equal to the record"; else echo "governor: NOT restored: current state differs from the record"; rc=1; fi
    exit $rc
    ;;
  *) echo "unknown mode $mode"; exit 2 ;;
esac
