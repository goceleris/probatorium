#!/usr/bin/env bash
# Load every playbook, tasks file and role task list under ansible/ the way
# ansible-playbook loads it at run time, and fail if any of them does not.
#
# Why this is more than `ansible-playbook --syntax-check <playbook>`:
# include_tasks and include_role are DYNAMIC. --syntax-check never opens the
# file they name, so a tasks file ansible cannot load passes it and fails
# only on the cluster, at the first cell that includes it. That is how
# probatorium#411 broke every Benchmark Tier dispatch: an apostrophe in a
# comment inside the free-form `ansible.builtin.shell: |` block of
# tasks/run_bench_cell.yml ("failed at splitting arguments, either an
# unbalanced jinja2 block or quotes"; runs 36306263697 and 36306273363),
# while `--syntax-check bench.yml` stayed green. So each tasks file is also
# imported STATICALLY (import_tasks / import_role ... tasks_from), which
# makes --syntax-check load and parse every task in it: module resolution,
# free-form argument splitting, YAML.
#
# Needs ansible-playbook on PATH and the collections the playbooks name
# (ansible.posix) on ANSIBLE_COLLECTIONS_PATH. Checks everything, prints
# ansible's own error for each failure, and exits non-zero if any failed.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

export ANSIBLE_CONFIG="$here/ansible.cfg"
export ANSIBLE_ROLES_PATH="$here/roles"
export ANSIBLE_LOCALHOST_WARNING=False
export ANSIBLE_INVENTORY_UNPARSED_WARNING=False
# A key given twice in one mapping is an error, not a warning: YAML keeps the
# last one silently, so a second `become:` or `when:` overrides the first
# without anything failing (run_bench_cell.yml carried a doubled `become`).
export ANSIBLE_DUPLICATE_YAML_DICT_KEY=error

checked=0
failed=0
check() { # $1 label, $2 playbook
	checked=$((checked + 1))
	local out
	if out=$(ansible-playbook --syntax-check -i localhost, "$2" 2>&1); then
		echo "ok   $1"
	else
		failed=$((failed + 1))
		echo "FAIL $1"
		printf '%s\n' "$out" | sed 's/^/     /'
	fi
}

wrapper() { # $1 wrapper path; the task list follows on stdin
	{
		printf -- '- hosts: localhost\n  gather_facts: false\n  tasks:\n'
		cat
	} >"$1"
}

for pb in "$here"/*.yml; do
	case "$(basename "$pb")" in
	inventory.yml) continue ;;
	esac
	check "playbook $(basename "$pb")" "$pb"
done

for tf in "$here"/tasks/*.yml; do
	w="$work/tasks-$(basename "$tf")"
	printf -- '    - ansible.builtin.import_tasks: %s\n' "$tf" | wrapper "$w"
	check "tasks/$(basename "$tf")" "$w"
done

for tf in "$here"/roles/*/tasks/*.yml; do
	role=$(basename "$(dirname "$(dirname "$tf")")")
	w="$work/role-$role-$(basename "$tf")"
	printf -- '    - ansible.builtin.import_role:\n        name: %s\n        tasks_from: %s\n' \
		"$role" "$(basename "$tf")" | wrapper "$w"
	check "roles/$role/tasks/$(basename "$tf")" "$w"
done

echo "ansible load check: $checked checked, $failed failed"
[ "$checked" -gt 0 ] && [ "$failed" -eq 0 ]
