package test

import (
	"os"
	"runtime/pprof"
	"testing"
	"time"

	"github.com/fortytw2/leaktest"
)

// GuardConfig stores options for Guard function.
type GuardConfig struct {
	timeout time.Duration
}

// GuardOption is an option for Guard function.
type GuardOption func(*GuardConfig)

// WithGuardTimeout sets timeout for the guard. Default is 5s.
func WithGuardTimeout(timeout time.Duration) GuardOption {
	return func(c *GuardConfig) {
		c.timeout = timeout
	}
}

// Guard implements a test level timeout.
func Guard(t *testing.T, opts ...GuardOption) func() {
	cfg := GuardConfig{
		timeout: 5 * time.Second,
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	done := make(chan struct{})
	go func() {
		select {
		case <-time.After(cfg.timeout):
			err := pprof.Lookup("goroutine").WriteTo(os.Stdout, 1)
			if err != nil {
				panic(err)
			}

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
