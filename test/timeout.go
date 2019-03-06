package test

import (
	"os"
	"runtime/pprof"
	"testing"
	"time"

	"github.com/fortytw2/leaktest"
)

// Guard implements a test level timeout.
func Guard(t *testing.T) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-time.After(5 * time.Second):
			pprof.Lookup("goroutine").WriteTo(os.Stdout, 1)

			panic("test timeout")
		case <-done:
		}
	}()

	fn := leaktest.Check(t)

	return func() {
		close(done)
		fn()
	}
}
