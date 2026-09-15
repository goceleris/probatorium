package main

import (
	"fmt"
	"os"
	"unsafe"
)

//go:noinline
func trip() {
	b := make([]byte, 16)
	fmt.Fprintln(os.Stdout, "before")
	_ = os.Stdout.Sync()
	p := unsafe.Pointer(&b[1])
	q := (*[1]*int)(p) // misaligned, elem has pointers -> checkptr: misaligned pointer conversion
	fmt.Fprintln(os.Stdout, q)
}

func main() {
	fmt.Fprintln(os.Stderr, "ready addr=127.0.0.1:1")
	trip()
	fmt.Fprintln(os.Stdout, "after")
}
