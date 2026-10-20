package main

import "os"

// PINPLAN7 arm V-NV (throwaway branch ab/pinplan-v7; evidence PLAN-pinplan7.md): the hand-pinned reference ON THE SAME CELERIS COMMIT as the RULE-D arm. celeris ab/vnv-on-7af74c6
// (45a90edf) is 7af74c6 (the head of PR #1085, the rx-port loop rule) plus the EXPERIMENT-ONLY planner knob CELERIS_AB_PLAN_CPUS (v4's knob code; with the knob set the rule is not consulted).
// The loops are 6, on 0,1,7,9,10,11: none on 6 and 8 (IRQ 127 / IRQ 135 NAPI CPUs), none on a little CPU: the plan RULE-D derives by itself.
//
// armWorkers is 0 as in the RULE-D module: Config.Workers is unset. armPlanCPUs is the knob's CPU list (celeris internal/platform/abplan_experiment.go, never intended to ship). Both are
// constants of the module and the knob is set from this constant before anything starts: nothing in the run's environment can unset or change it (a run-level value of the variable is overwritten).
const armWorkers = 0

const armPlanCPUs = "0-1,7,9-11"

func init() {
	if err := os.Setenv("CELERIS_AB_PLAN_CPUS", armPlanCPUs); err != nil {
		panic(err)
	}
}
