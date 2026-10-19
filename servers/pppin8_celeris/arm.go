package main

// armWorkers is the Config.Workers of this PINPLAN3 arm (throwaway branches ab/pinplan-v3, ab/pinplan-v3-90s; evidence PLAN-pinplan-v3.md).
// A constant of the module, not an environment variable: nothing in the run can unset or change it. 0 leaves Config.Workers unset, so the
// engine's planner decides (on msr1 with celeris #1049: 8 loops, one per non-little CPU); N > 0 is an explicit Config.Workers of N, which #1049
// honours by wrapping the extra loops round-robin over the non-little CPUs 0-1,6-11, each pinned.
const armWorkers = 0
