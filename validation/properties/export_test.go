package properties

// IMEM1WithoutBoundForTest is I-MEM-1 with the probatorium#466 lower-bound
// check off, for the replay tests in package properties_test: run against
// the same real series, it must reproduce the counts the gate recorded
// before the fix, which is what shows each fixture is the series the gate
// judged.
func IMEM1WithoutBoundForTest() Spec { return imem1WithoutBound() }
