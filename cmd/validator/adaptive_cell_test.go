package main

import "testing"

// Only adaptive cells are resized: the floor and the two-worker cap exist
// to make the controller's promotion threshold reachable there, and an
// explicit -refapp-workers cap keeps applying to every engine.
func TestAdaptiveCellSizing(t *testing.T) {
	if got := cellRefappWorkers("adaptive", 0); got != adaptiveCellWorkers {
		t.Errorf("adaptive cell workers = %d, want %d", got, adaptiveCellWorkers)
	}
	if got := cellRefappWorkers("adaptive", 4); got != 4 {
		t.Errorf("an explicit cap must win on adaptive too, got %d", got)
	}
	for _, e := range []string{"iouring", "epoll", "std"} {
		if got := cellRefappWorkers(e, 0); got != 0 {
			t.Errorf("%s: workers must stay at the celeris default, got %d", e, got)
		}
		if got := cellConcurrencyFloor(e); got != 0 {
			t.Errorf("%s: no concurrency floor, got %d", e, got)
		}
	}
	if got := cellConcurrencyFloor("adaptive"); got != adaptiveCellConcurrencyFloor {
		t.Errorf("adaptive floor = %d, want %d", got, adaptiveCellConcurrencyFloor)
	}
	// The floor must clear the controller's SINGLE-TICK snap, not its
	// two-tick sustain: 2 workers x highWatermark 48 = 96 active
	// connections. Sizing for the sustain path gave a 17% margin and
	// promoted only 9 of 16 cells (probatorium run 34864823296), with the
	// connection counts identical in the promoted and unpromoted groups.
	// At the measured 0.93 active connections per walker the floor must
	// still clear 96 with headroom for the sampling dip.
	if got := float64(adaptiveCellConcurrencyFloor) * 0.93; got < 96*1.05 {
		t.Errorf("floor %d yields ~%.0f active conns, which does not clear the 96-connection snap with 5%% margin",
			adaptiveCellConcurrencyFloor, got)
	}
}
