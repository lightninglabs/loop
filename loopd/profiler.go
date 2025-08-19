package loopd

import (
	"context"
	"fmt"
	"net/http"
	"net/http/pprof"
	"sync"
	"time"
)

const (
	profilerCloseTimeout = 5 * time.Second
)

// Profiler is a wrapper around Go's net/http/pprof package which enables
// graceful startup and shutdown of the pprof server.
type Profiler struct {
	srv  *http.Server
	done chan struct{}
	once sync.Once
}

// NewProfiler creates a new Profiler instance that listens on the specified
// port.
func NewProfiler(port int) *Profiler {
	mux := http.NewServeMux()

	// Landing redirect
	mux.Handle("/", http.RedirectHandler(
		"/debug/pprof/", http.StatusSeeOther),
	)

	// pprof endpoints
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
		// Use some defailt timeouts to prevent hanging connections
		// (gosec).
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	return &Profiler{
		srv:  srv,
		done: make(chan struct{}),
	}
}

// Start initializes the pprof server and starts listening for requests.
func (p *Profiler) Start() {
	p.once.Do(func() {
		go func() {
			infof("Profiler (pprof) listening on %s", p.srv.Addr)
			err := p.srv.ListenAndServe()
			if err != nil && err != http.ErrServerClosed {
				errorf("Profiler (pprof) server error: %v",
					err)
			}

			// Signal termination.
			close(p.done)
		}()
	})
}

// Stop attempts a graceful shutdown and waits for completion. Pass a context
// with timeout to limit how long you’re willing to wait.
func (p *Profiler) Stop() error {
	ctxt, cancel := context.WithTimeout(
		context.Background(), profilerCloseTimeout,
	)
	defer cancel()

	// Ask the server to gracefully shut down: stop accepting new
	// connections, let in-flight handlers finish.
	err := p.srv.Shutdown(ctxt)
	if err != nil {
		// Attempt to force close the server if shutdown fails.
		err = p.srv.Close()
		if err != nil {
			errorf("Profiler (pprof) server close error: %v",
				err)
		}
	}

	// Wait for the ListenAndServe goroutine to exit.
	select {
	case <-p.done:
	case <-ctxt.Done():
		// If the caller’s context ends before goroutine exit, bail out.
		return ctxt.Err()
	}

	return nil
}
