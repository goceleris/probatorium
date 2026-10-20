package main

// armWorkers is the Config.Workers of this PINPLAN6 arm RULE-D (throwaway branches ab/pinplan-v6, ab/pinplan-v6-90s; evidence PLAN-pinplan-v6.md).
// A constant of the module, not an environment variable: nothing in the run can unset or change it. 0 leaves Config.Workers unset, so the
// engine's planner decides: the celeris this module requires is the head of celeris PR #1085 (branch ab/rulend-20261020 = fix/pinplan-rxfree-1036
// 7af74c6 = #1049 + main + the rx-port loop rule), and on msr1 with the standing pin (IRQ 167 on CPU 1) the rule is expected to plan 6 loops on
// CPUs 0,1,7,9,10,11 by itself. There is NO planner knob in this module or in that celeris commit: the plan is derived at Listen from /proc and /sys.
const armWorkers = 0
