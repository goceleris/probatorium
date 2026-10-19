package main

import "os"

// PINPLAN5 arm X (throwaway branches ab/pinplan-v5, ab/pinplan-v5-90s; evidence PLAN-pinplan-v5.md). The discriminating arm of the V-NV rule work: 6 pinned loops on the big CPUs
// 0,1,6,7,8,9 (the rule-B plan "leave the two highest-numbered big CPUs free"). It keeps loops on CPUs 6 and 8 (the two busiest NIC-vector CPUs of the standing state) and frees the
// two quiet CPUs 10 and 11: the same loop count as V-NV (6) on a different CPU set.
//
// armWorkers is 0 as in the PIN8 module: Config.Workers is unset. armPlanCPUs is the EXPERIMENT-ONLY CPU list of the knob the celeris branch ab/pinplan-v4-20261019 (3d0ddb0a) reads
// (CELERIS_AB_PLAN_CPUS, internal/platform/abplan_experiment.go, never intended to ship). Both are constants of the module and the knob is set from this
// constant before anything starts: nothing in the run's environment can unset or change it (a run-level value of the variable is overwritten).
const armWorkers = 0

const armPlanCPUs = "0-1,6-9"

func init() {
	if err := os.Setenv("CELERIS_AB_PLAN_CPUS", armPlanCPUs); err != nil {
		panic(err)
	}
}
