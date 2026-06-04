package main

// Byte-slice comparison helpers for the routing hot path. They take the
// literal as a string and compare byte-wise so no []byte(s) conversion is
// allocated, keeping dispatch allocation-free for the static endpoints.

func equal(b []byte, s string) bool {
	if len(b) != len(s) {
		return false
	}
	for i := range b {
		if b[i] != s[i] {
			return false
		}
	}
	return true
}

func hasPrefix(b []byte, s string) bool {
	if len(b) < len(s) {
		return false
	}
	for i := range len(s) {
		if b[i] != s[i] {
			return false
		}
	}
	return true
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}
