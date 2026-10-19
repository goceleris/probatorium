package main

import "os"

// PINPLAN4 arm V-NV (throwaway branches ab/pinplan-v4, ab/pinplan-v4-90s; evidence PLAN-pinplan-v4.md). Hypothesis H2 and nothing else: loops only on the big
// CPUs that do not service a busy NIC vector, except CPU 1 (the TX-completion CPU): 6 loops on 0,1,7,9,10,11, none on 6 and 8 (IRQ 127 / IRQ 135 NAPI CPUs).
//
// armWorkers is 0 as in the PIN8 module: Config.Workers is unset. armPlanCPUs is the EXPERIMENT-ONLY CPU list of the knob the celeris branch ab/pinplan-v4-20261019 reads
// (CELERIS_AB_PLAN_CPUS, internal/platform/abplan_experiment.go, never intended to ship). Both are constants of the module and the knob is set from this
// constant before anything starts: nothing in the run's environment can unset or change it (a run-level value of the variable is overwritten).
const armWorkers = 0

const armPlanCPUs = "0-1,7,9-11"

func init() {
	if err := os.Setenv("CELERIS_AB_PLAN_CPUS", armPlanCPUs); err != nil {
		panic(err)
	}
}
