// racedemo is the I-RACE end-to-end fixture: it announces ready, then runs
// a deliberate write/write race and exits normally. Built with -race the
// detector prints a "WARNING: DATA RACE" report to stderr and lets the
// program finish; built without it there is nothing to report.
package main

import (
	"fmt"
	"os"
	"sync"
)

var shared int

func main() {
	fmt.Fprintln(os.Stderr, "ready addr=127.0.0.1:1")
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				shared++ // unsynchronised from two goroutines
			}
		}()
	}
	wg.Wait()
	fmt.Fprintln(os.Stdout, "done", shared)
}
