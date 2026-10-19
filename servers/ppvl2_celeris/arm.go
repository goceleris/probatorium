package main

import "os"

// PINPLAN4 arm V-L2 (throwaway branches ab/pinplan-v4, ab/pinplan-v4-90s; evidence PLAN-pinplan-v4.md). Hypothesis H1 and nothing else: #1049's 8 loops
// on the big CPUs 0-1,6-11 PLUS two loops pinned to the little CPUs 2 and 5 (10 loops).
//
// armWorkers is 0 as in the PIN8 module: Config.Workers is unset. armPlanCPUs is the EXPERIMENT-ONLY CPU list of the knob the celeris branch ab/pinplan-v4-20261019 reads
// (CELERIS_AB_PLAN_CPUS, internal/platform/abplan_experiment.go, never intended to ship). Both are constants of the module and the knob is set from this
// constant before anything starts: nothing in the run's environment can unset or change it (a run-level value of the variable is overwritten).
const armWorkers = 0

const armPlanCPUs = "0-2,5-11"

func init() {
	if err := os.Setenv("CELERIS_AB_PLAN_CPUS", armPlanCPUs); err != nil {
		panic(err)
	}
}
