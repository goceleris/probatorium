package properties

import "fmt"

// adaptiveSwitchBudgetPerHour caps adaptive engine switches per rolling
// hour. The adaptive controller hysteresis is designed for ~minutes
// between switches under sustained load shifts; more than 600/h
// (>10/min) is flapping.
const adaptiveSwitchBudgetPerHour int64 = 600

// IENGAdaptive is the adaptive engine invariant: the switch counter
// must not flap, and every switch must complete cleanly (no zombie
// engine state). Catches the standby suspension regressions from
// PR #49.
//
// Wave 6 STUB: the validation-tagged build is needed to expose finer-
// grained per-engine state. Today we cap the switch rate; wave 7 adds
// the cleanup-verification predicate.
var IENGAdaptive = Spec{
	ID:          "I-ENG-ADAPTIVE",
	Description: "adaptive engine switches do not flap and complete cleanly",
	Tier:        "engine",
	Predicate: func(snap *Snapshot, ctx Context) (bool, string) {
		// TODO(wave-7): per-engine state cleanup assertion.
		if snap.AdaptiveSwitches < 0 {
			return false, fmt.Sprintf("I-ENG-ADAPTIVE violated: switches counter negative (%d)", snap.AdaptiveSwitches)
		}
		// Flap detection: compare snap.AdaptiveSwitches against the
		// oldest in-history sample within the last hour.
		if Forever(ctx) < 0 {
			return true, ""
		}
		cutoff := ctx.Now.Add(-3600).Unix()
		var base int64 = -1
		for _, h := range ctx.History {
			if h.TS >= cutoff {
				base = h.AdaptiveSwitches
				break
			}
		}
		if base >= 0 {
			delta := snap.AdaptiveSwitches - base
			if delta > adaptiveSwitchBudgetPerHour {
				return false, fmt.Sprintf(
					"I-ENG-ADAPTIVE violated: %d adaptive switches in last hour exceeds budget of %d",
					delta, adaptiveSwitchBudgetPerHour)
			}
		}
		return true, ""
	},
}
