package a

import (
	"fmt"
	"os"
	"testing"
	"time"
)

func TestPass(t *testing.T) { t.Log("hello from TestPass") }

func TestSub(t *testing.T) {
	t.Run("one", func(t *testing.T) {})
	t.Run("two words", func(t *testing.T) {
		t.Run("deep", func(t *testing.T) {})
	})
	t.Run("skipper", func(t *testing.T) { t.Skip("nope") })
}

func TestSkip(t *testing.T) { t.Skip("skipped on purpose") }

func TestFail(t *testing.T) {
	if os.Getenv("SAMPLE_FAIL") == "1" {
		t.Errorf("boom: want 1 got 2")
		t.Fatalf("second line")
	}
}

func TestSubFail(t *testing.T) {
	t.Run("ok", func(t *testing.T) {})
	t.Run("bad", func(t *testing.T) {
		if os.Getenv("SAMPLE_FAIL") == "1" {
			t.Error("sub failed")
		}
	})
}

func TestParallel(t *testing.T) {
	for i := range 3 {
		t.Run(fmt.Sprint("p", i), func(t *testing.T) {
			t.Parallel()
			time.Sleep(10 * time.Millisecond)
			t.Log("parallel log")
		})
	}
}

func TestNoNewline(t *testing.T) {
	fmt.Print("partial line without newline")
}

func TestPanic(t *testing.T) {
	if os.Getenv("SAMPLE_PANIC") == "1" {
		panic("kaboom")
	}
}

func TestGoroutinePanic(t *testing.T) {
	if os.Getenv("SAMPLE_GPANIC") == "1" {
		done := make(chan struct{})
		go func() { panic("goroutine kaboom") }()
		<-done
	}
}

func TestSlow(t *testing.T) {
	if os.Getenv("SAMPLE_SLOW") == "1" {
		time.Sleep(5 * time.Second)
	}
}

func Example() {
	fmt.Println("hello")
	// Output: hello
}
