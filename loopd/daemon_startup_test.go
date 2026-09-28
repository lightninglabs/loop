package loopd

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/lndclient"
	"github.com/stretchr/testify/require"
)

// startupListener serves no connections and unblocks when shutdown closes it.
type startupListener struct {
	closed chan struct{}
	once   sync.Once
}

// Accept waits until the HTTP server closes its listener.
func (l *startupListener) Accept() (net.Conn, error) {
	<-l.closed
	return nil, net.ErrClosed
}

// Close releases a pending Accept call exactly once.
func (l *startupListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

// Addr supplies the listener identity expected by http.Server.
func (l *startupListener) Addr() net.Addr {
	return &net.TCPAddr{}
}

// TestDaemonShutdownWaitsForStartup forces shutdown before the REST server is
// assigned and verifies that the listener created afterward is still closed.
func TestDaemonShutdownWaitsForStartup(t *testing.T) {
	setLogger(btclog.Disabled)

	for _, runtimeFailure := range []bool{false, true} {
		name := "shutdown signal"
		if runtimeFailure {
			name = "runtime error"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d := New(nil, nil)
				d.mainCtx, d.mainCtxCancel =
					context.WithCancel(t.Context())
				cleaned := false
				go d.handleRuntimeErrors(func() { cleaned = true })
				var wantErr error
				if runtimeFailure {
					wantErr = errors.New("manager stopped")
					d.internalErrChan <- wantErr
				} else {
					d.Stop()
				}
				synctest.Wait()
				require.NoError(t, d.mainCtx.Err())
				require.False(t, cleaned)

				// Startup still owns listener creation and wait-group
				// registration until it releases the startup barrier.
				listener := &startupListener{closed: make(chan struct{})}
				d.restServer = &http.Server{}
				served := make(chan error, 1)
				d.wg.Go(func() {
					served <- d.restServer.Serve(listener)
				})
				synctest.Wait()
				close(d.startupDone)
				synctest.Wait()

				require.ErrorIs(t, <-served, http.ErrServerClosed)
				require.ErrorIs(t, <-d.ErrChan, wantErr)
				require.ErrorIs(t, d.mainCtx.Err(), context.Canceled)
				require.True(t, cleaned)
			})
		})
	}
}

// TestDaemonStartupFailureReleasesBarrier covers failures before the runtime
// error handler starts, including the subserver path that has no web servers.
func TestDaemonStartupFailureReleasesBarrier(t *testing.T) {
	setLogger(btclog.Disabled)

	for _, subserver := range []bool{false, true} {
		name := "standalone"
		if subserver {
			name = "subserver"
		}
		t.Run(name, func(t *testing.T) {
			lndErr := errors.New("lnd connection failed")
			d := New(&Config{
				Network: "regtest", Server: &loopServerConfig{},
			}, &ListenerCfg{
				getLnd: func(lndclient.Network, *lndConfig) (
					*lndclient.GrpcLndServices, error) {

					return nil, lndErr
				},
			})
			if subserver {
				require.ErrorContains(t, d.StartAsSubserver(nil, false),
					"no swap server address specified")
			} else {
				require.ErrorIs(t, d.Start(), lndErr)
			}
			select {
			case <-d.startupDone:
			default:
				t.Fatal("startup failure did not release shutdown")
			}
		})
	}
}
