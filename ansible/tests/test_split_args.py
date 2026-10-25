#!/usr/bin/env python3
"""Runs ansible's own argument splitter over the raw free-form string of every shell / command task of every YAML file under ansible/ (tasks, playbooks, role tasks, nested
blocks), after YAML parsing and before any templating. It is the check that would have stopped P2c dispatch 40235051270, which died at task load
("failed at splitting arguments, either an unbalanced jinja2 block or quotes") because ONE apostrophe in a Python comment inside the `ansible.builtin.shell: |` block of
ansible/tasks/run_bench_cell.yml ("the SUT's port") flipped the splitter's quote state for the rest of the script. probatorium#411 was the same class. ansible/load-check.sh
(--syntax-check with every tasks file imported statically) catches it too but needs the whole playbook machinery; this one is a few lines, names the line where the string stops
parsing, and has a --selftest that proves it is live.

usage: python3 ansible/tests/test_split_args.py [--selftest]      (needs ansible-core on the path, the cluster's pin; PyYAML comes with it)
exit 0 = every free-form string splits; 1 = at least one does not (each is printed with the first line after which no continuation can parse); 2 = usage / nothing checked
"""
import glob
import os
import sys

import yaml
from ansible.parsing.splitter import split_args

FREEFORM = ("ansible.builtin.shell", "ansible.builtin.command", "shell", "command", "ansible.builtin.raw", "raw", "ansible.builtin.script", "script")


def walk(node):
    """every task dict in a parsed tasks list / playbook, blocks and plays included"""
    if isinstance(node, list):
        for x in node:
            yield from walk(x)
    elif isinstance(node, dict):
        if any(k in node for k in FREEFORM):
            yield node
        for k in ("tasks", "pre_tasks", "post_tasks", "handlers", "block", "rescue", "always"):
            if k in node:
                yield from walk(node[k])


def first_bad_line(s):
    """the first line at which the prefix of the string stops being splittable and never recovers (state-flip), for the message"""
    lines = s.split("\n")
    last_ok = None
    for i in range(1, len(lines) + 1):
        try:
            split_args("\n".join(lines[:i]))
            last_ok = i
        except Exception:
            pass
    if last_ok is None:
        return 1, lines[0]
    nxt = last_ok + 1
    return nxt, lines[nxt - 1] if nxt <= len(lines) else ""


def check_string(s):
    try:
        split_args(s)
        return None
    except Exception as e:  # AnsibleParserError
        n, line = first_bad_line(s)
        return "%s (first unrecoverable line %d: %s)" % (str(e).splitlines()[0][:100], n, line.strip()[:160])


def check_file(path):
    bad, n = [], 0
    docs = list(yaml.safe_load_all(open(path)))
    for doc in docs:
        for task in walk(doc):
            for k in FREEFORM:
                if k in task and isinstance(task[k], str):   # the dict form (cmd: |) is not split
                    n += 1
                    err = check_string(task[k])
                    if err:
                        bad.append("%s: task %r: %s" % (path, str(task.get("name", "?"))[:80], err))
    return n, bad


def selftest():
    ok = check_string("d=1\ncat > f <<'EOF'\necho hi\nEOF\n# the port is fine\n") is None
    broken = check_string("d=1\ncat > f <<'EOF'\n# the SUT's port\nEOF\n")
    unbalanced_jinja = check_string("echo {{ x \n")
    for label, cond in (("a balanced string splits", ok), ("a comment with one apostrophe is rejected", broken is not None and "unrecoverable line 3" in broken),
                        ("an unbalanced jinja block is rejected", unbalanced_jinja is not None)):
        print(("ok   " if cond else "FAIL ") + label)
        if not cond:
            return 1
    return 0


def main(argv):
    if "--selftest" in argv:
        return selftest()
    root = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
    files = sorted(glob.glob(os.path.join(root, "*.yml")) + glob.glob(os.path.join(root, "tasks", "*.yml")) + glob.glob(os.path.join(root, "roles", "*", "tasks", "*.yml")))
    checked, bad = 0, []
    for f in files:
        n, b = check_file(f)
        checked += n
        bad += b
    for b in bad:
        print("FAIL " + os.path.relpath(b.split(": task")[0], root) + b[len(b.split(": task")[0]):])
    print("ansible split_args check: %d free-form shell/command strings in %d files, %d failed" % (checked, len(files), len(bad)))
    return 1 if bad else (0 if checked else 2)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
