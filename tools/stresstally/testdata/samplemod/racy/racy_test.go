package racy

import (
	"sync"
	"testing"
)

// Races on purpose (under -race): the race detector fails the test.
func TestRacy(t *testing.T) {
	n := 0
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); n++ }()
	}
	wg.Wait()
	_ = n
}

func TestCalm(t *testing.T) {}
