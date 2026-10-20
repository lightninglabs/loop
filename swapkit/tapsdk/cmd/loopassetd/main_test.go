package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type pipeListener struct {
	connections chan net.Conn
	closed      chan struct{}
	once        sync.Once
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case connection := <-l.connections:
		return connection, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (*pipeListener) Addr() net.Addr {
	return &net.UnixAddr{Name: "runtime-test", Net: "unix"}
}

func TestShutdownCancelsAndJoinsActiveActions(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	listener := &pipeListener{
		connections: make(chan net.Conn, 1),
		closed:      make(chan struct{}),
	}
	started, canceled, release := make(chan struct{}), make(chan struct{}),
		make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, listener, http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				close(started)
				<-r.Context().Done()
				close(canceled)
				<-release
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
	}()
	client, server := net.Pipe()
	defer client.Close()
	listener.connections <- server
	_, err := io.WriteString(
		client, "GET / HTTP/1.1\r\nHost: runtime\r\n\r\n",
	)
	require.NoError(t, err)
	responseDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, client)
		close(responseDone)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("handler context was not canceled")
	}
	select {
	case <-done:
		t.Fatal("serve returned before the journal-using action joined")
	default:
	}
	close(release)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("shutdown did not join")
	}
	<-responseDone
}
