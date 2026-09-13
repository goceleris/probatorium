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
	// 2 workers x 24 conns/worker = 48 active connections; at the measured
	// ~0.95 conns per walker the floor must clear that with margin.
	if float64(adaptiveCellConcurrencyFloor)*0.93 < 48*1.1 {
		t.Errorf("floor %d does not clear the 48-connection threshold with 10%% margin", adaptiveCellConcurrencyFloor)
	}
}
