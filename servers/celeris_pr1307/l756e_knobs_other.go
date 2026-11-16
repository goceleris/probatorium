//go:build !linux

package main

import "fmt"

func processAllowedList() string { return "" }

func applyAffinityAllThreads(st *affinityState) error {
	return fmt.Errorf("%s is set but this host is not Linux: refusing to run an arm that cannot be honoured", affinityEnv)
}
