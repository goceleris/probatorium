#!/usr/bin/env python3
"""Mutation check that the restore ACTUALLY restores and that the compare actually compares: each mutant is a copy of ansible/files/cpu_power.py or of .github/scripts/cpu-governor.sh with
ONE line changed in the way a bug would change it, run against the real test suite (test_cpu_power.py through CPU_POWER_SCRIPT, test_cpu_governor.sh through SCRIPT). A mutant the suite
does not fail is a hole in the tests: this file fails. Mutants live in a temp directory; nothing is written to the repository.
   python3 -I -B ansible/tests/test_cpu_power_mutations.py [--list]"""
import os
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
PY = os.path.join(HERE, "..", "files", "cpu_power.py")
SH = os.path.join(HERE, "..", "..", ".github", "scripts", "cpu-governor.sh")

# (name, file, original text, replacement). The original must occur exactly once. A refused write is reported twice (the write error and the read-back compare), so dropping only
# one of the two is an equivalent mutant and is not listed; the read-back mutant below removes the second line of defence and the refused-write tests kill it.
PY_MUTANTS = [
    ("restore skips the last CPU's governor", PY, "    for c in cpus:\n        name = \"cpu%d\" % c\n        d = os.path.join(sysfs(), name, \"cpufreq\")\n        g = want",
     "    for c in cpus[:-1]:\n        name = \"cpu%d\" % c\n        d = os.path.join(sysfs(), name, \"cpufreq\")\n        g = want"),
    ("restore never writes EPP back", PY, "        err = wr(epp_file, w)\n", "        err = None\n"),
    ("restore never writes idle flags back", PY, "                err = wr(os.path.join(sysfs(), name, \"cpuidle\", have[s][\"state\"], \"disable\"), v)\n", "                err = None\n"),
    ("restore never writes the scaling limits back", PY, "                    err = wr(d + \"/\" + fn, w)\n", "                    err = None\n"),
    ("the read-back is not compared", PY, "    for line in diff_views(want, got):\n", "    for line in []:\n"),
    ("performance writes idle flags too (disables C3)", PY, "                    view[\"cpus\"][name][\"idle\"][s] = 1", "                    view[\"cpus\"][name][\"idle\"][s] = 1"),
    ("the already-equal governor is written anyway", PY, "        if rd(d + \"/scaling_governor\") != g:\n", "        if True:\n"),
    ("compare ignores the governor", PY, "for k in (\"driver\", \"governor\", \"epp\", \"min_khz\", \"max_khz\"):", "for k in (\"driver\", \"epp\", \"min_khz\", \"max_khz\"):"),
    ("compare ignores EPP", PY, "for k in (\"driver\", \"governor\", \"epp\", \"min_khz\", \"max_khz\"):", "for k in (\"driver\", \"governor\", \"min_khz\", \"max_khz\"):"),
    ("compare ignores the scaling limits", PY, "for k in (\"driver\", \"governor\", \"epp\", \"min_khz\", \"max_khz\"):", "for k in (\"driver\", \"governor\", \"epp\"):"),
    ("compare ignores the idle flags", PY, "            if ia.get(s) != ib.get(s):\n", "            if False:\n"),
    ("compare ignores host-wide settings", PY, "        if ga.get(k) != gb.get(k):\n", "        if False:\n"),
    ("compare ignores a CPU missing from one record", PY, "        if c not in ca or c not in cb:\n", "        if False:\n"),
    ("the state view drops the idle flags", PY, "\"idle\": {s.get(\"name\"): s.get(\"disable\") for s in e.get(\"idle\", [])}}", "\"idle\": {}}"),
    ("the original record is not write-once", PY, "    if \"--once\" in a and os.path.exists(out):", "    if False:"),
    ("a CPU without cpufreq is given a governor to set", PY, "        if e.get(\"governor\") in (None, \"-\"):\n            continue", "        if False:\n            continue"),
    ("an unavailable governor is not refused", PY, "        if arm != \"as-found\" and g not in cpu_entry(c)[\"available_governors\"]:", "        if False:"),
    ("the gate does not require the restore status", PY, "    if not status.startswith(need):\n", "    if False:\n"),
    ("the gate does not recompute orig vs final", PY, "            diff = diff_views(state_view(recs[\"orig\"]), state_view(recs[\"final\"]))\n", "            diff = []\n"),
    ("the gate accepts an applied governor that is not performance", PY, "                if e.get(\"governor\") not in (\"performance\", \"-\"):\n", "                if False:\n"),
    ("the gate accepts changed idle flags in the applied record", PY, "            if ia != io:\n", "            if False:\n"),
    ("a vanished CPU is not noticed", PY, "    if cpus != online:\n", "    if False:\n"),
]
SH_MUTANTS = [
    ("the restore verdict ignores the compare", SH, "  if [ \"$cmp_rc\" -ne 0 ]; then\n    verdict \"NOT-EQUAL", "  if false; then\n    verdict \"NOT-EQUAL"),
    ("the restore play is not run (as-found never applied)", SH, "    ap restore final || rc=$?", "    :"),
    ("the restore compares only the first host", SH, "      \"$PY\" -I -B \"$CPPY\" compare --a \"$f\" --b \"$DIR/final/$h.json\" || cmp_rc=1\n", "      [ -n \"$done1\" ] || \"$PY\" -I -B \"$CPPY\" compare --a \"$f\" --b \"$DIR/final/$h.json\" || cmp_rc=1\n      done1=1\n"),
    ("a failed restore play is reported as restored", SH, "    verdict \"NOT-RESTORED $stamp", "    :; verdict \"RESTORED-EQUAL $stamp"),
    ("the set step ignores an apply that did not take", SH, "    ap apply applied performance || {", "    ap apply applied performance || :; false || {"),
    ("the original is not recorded before the change", SH, "  ap record orig || die", "  ap record orig || :; false || die"),
    ("every run is limited to the loadgen only", SH, "    *) echo \"${BENCH_TARGET},msa2-client\" ;;", "    *) echo \"msa2-client\" ;;"),
    ("keep changes the governor", SH, "  if [ \"$GOV\" = performance ]; then\n    ap apply", "  if true; then\n    ap apply"),
    ("a stale record is accepted", SH, "  if [ -e \"$DIR\" ] && [ -n \"$(ls -A \"$DIR\" 2> /dev/null)\" ]; then die", "  if false; then die"),
    ("the restore skips the results copy", SH, "collect() { # copy the records to the results directory the job uploads; never fails the step\n", "collect() { return 0\n"),
    ("an unknown governor is accepted", SH, "case \"$GOV\" in performance | keep) ;; *) die", "case \"$GOV\" in performance | keep | *) ;; *) die"),
]
# a mutant that does not change anything (the idle-write mutant is replaced below by a behavioural one)
PY_MUTANTS[7] = ("performance writes idle flags too (disables C3)", PY,
                 "    for c in cpus:\n        name = \"cpu%d\" % c\n        have = {s[\"name\"]: s for s in idle_states(c)}\n        for s, v in want[\"cpus\"][name][\"idle\"].items():\n            if s in have and have[s][\"disable\"] != v and v is not None:",
                 "    for c in cpus:\n        name = \"cpu%d\" % c\n        have = {s[\"name\"]: s for s in idle_states(c)}\n        if arm == \"performance\":\n            want[\"cpus\"][name][\"idle\"][\"C3\"] = 1\n        for s, v in want[\"cpus\"][name][\"idle\"].items():\n            if s in have and have[s][\"disable\"] != v and v is not None:")


def suite(kind, path):
    env = dict(os.environ)
    if kind == "py":
        env["CPU_POWER_SCRIPT"] = path
        cmd = [sys.executable, "-I", "-B", os.path.join(HERE, "test_cpu_power.py")]
    else:
        env["SCRIPT"] = path
        cmd = ["bash", os.path.join(HERE, "test_cpu_governor.sh")]
    return subprocess.run(cmd, env=env, capture_output=True, text=True).returncode


def main():
    muts = PY_MUTANTS + SH_MUTANTS
    if "--list" in sys.argv:
        for m in muts:
            print(m[0])
        return 0
    base = [("cpu_power.py", suite("py", PY)), ("cpu-governor.sh", suite("sh", SH))]
    for n, rc in base:
        if rc != 0:
            print("the unmutated %s fails its own suite (rc=%d): fix that first" % (n, rc))
            return 2
    tmp = tempfile.mkdtemp(prefix="cpupower-mut-")
    survivors = []
    for i, (name, src, old, new) in enumerate(muts):
        text = open(src).read()
        if text.count(old) != 1:
            print("BROKEN MUTANT (pattern occurs %d times): %s" % (text.count(old), name))
            survivors.append(name)
            continue
        kind = "py" if src == PY else "sh"
        path = os.path.join(tmp, "mutant-%d.%s" % (i, kind))
        with open(path, "w") as f:
            f.write(text.replace(old, new))
        os.chmod(path, 0o755)
        if kind == "sh":  # the script finds ansible/ through CPUGOV_ANSIBLE_DIR, set by the suite
            pass
        rc = suite(kind, path)
        print("%s  %s" % ("killed  " if rc != 0 else "SURVIVED", name))
        if rc == 0:
            survivors.append(name)
    print("%d mutants, %d survivors" % (len(muts), len(survivors)))
    return 1 if survivors else 0


if __name__ == "__main__":
    sys.exit(main())
