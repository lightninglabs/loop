package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUnixAuthenticationVersionAndRestart(t *testing.T) {
	token := make([]byte, 32)
	token[0] = 1
	var calls atomic.Int32
	backend := BackendFunc(func(ctx context.Context, method Method,
		payload json.RawMessage) (any, error) {

		calls.Add(1)
		require.Equal(t, Height, method)
		_, ok := ctx.Deadline()
		require.True(t, ok)
		return uint32(123), nil
	})
	handler, err := Handler(token, backend)
	require.NoError(t, err)
	socket := filepath.Join(privateDir(t), "runtime.sock")
	serve := func() (*http.Server, chan error) {
		listener, err := Listen(socket)
		require.NoError(t, err)
		info, err := os.Stat(socket)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0600), info.Mode().Perm())
		server := &http.Server{Handler: handler}
		done := make(chan error, 1)
		go func() { done <- server.Serve(listener) }()
		return server, done
	}
	server, done := serve()
	client, err := NewClient(socket, token)
	require.NoError(t, err)
	t.Cleanup(client.Close)
	var height uint32
	require.NoError(t, client.Call(t.Context(), Height, nil, &height))
	require.EqualValues(t, 123, height)
	wrong, err := NewClient(socket, make([]byte, 32))
	require.NoError(t, err)
	t.Cleanup(wrong.Close)
	err = wrong.Call(t.Context(), Height, nil, &height)
	require.ErrorIs(t, err, ErrRemote)
	err = client.Call(t.Context(), "Unknown", nil, nil)
	require.ErrorIs(t, err, ErrInvalid)
	require.EqualValues(t, 1, calls.Load())
	_, err = Listen(socket)
	require.Error(t, err)
	require.NoError(t, server.Close())
	require.ErrorIs(t, <-done, http.ErrServerClosed)
	server, done = serve()
	require.NoError(t, client.Call(t.Context(), Height, nil, &height))
	require.NoError(t, server.Close())
	require.ErrorIs(t, <-done, http.ErrServerClosed)
}

func TestDeadlineAndOpaqueErrors(t *testing.T) {
	token := make([]byte, 32)
	observed := make(chan error, 1)
	handler, err := Handler(token, BackendFunc(func(ctx context.Context,
		method Method, _ json.RawMessage) (any, error) {

		if method == CheckReady {
			return nil, errors.New("private signer material")
		}
		<-ctx.Done()
		observed <- ctx.Err()
		return nil, ctx.Err()
	}))
	require.NoError(t, err)
	listener, err := Listen(filepath.Join(privateDir(t), "runtime.sock"))
	require.NoError(t, err)
	server := &http.Server{Handler: handler}
	t.Cleanup(func() { _ = server.Close() })
	go func() { _ = server.Serve(listener) }()
	client, err := NewClient(listener.Addr().String(), token)
	require.NoError(t, err)
	t.Cleanup(client.Close)
	err = client.Call(t.Context(), CheckReady, nil, nil)
	require.ErrorIs(t, err, ErrRemote)
	require.NotContains(t, err.Error(), "private signer")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	require.Error(t, client.Call(ctx, Height, nil, nil))
	select {
	case err := <-observed:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("backend did not observe caller deadline")
	}
}

func TestMalformedProtocolAndTokenFile(t *testing.T) {
	token := make([]byte, 32)
	handler, err := Handler(token, BackendFunc(func(context.Context, Method,
		json.RawMessage) (any, error) {

		t.Fatal("invalid request reached backend")
		return nil, nil
	}))
	require.NoError(t, err)
	for _, raw := range []string{
		`{"Version":2,"Method":"Height","Payload":null}`,
		`{"Version":1,"Method":"Unknown","Payload":null}`,
		`{"Version":1,"Method":"Height","Payload":null,"Extra":1}`,
		`{"Version":1,"Method":"Height","Payload":null} {}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/call",
			strings.NewReader(raw))
		req.Header.Set(tokenHeader, strings.Repeat("00", 32))
		req.Header.Set(deadlineHeader, strconv.FormatInt(
			time.Now().Add(time.Hour).UnixNano(), 10))
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, req)
		require.Equal(t, http.StatusBadRequest, result.Code)
	}
	path := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(path, token, 0600))
	actual, err := ReadToken(path)
	require.NoError(t, err)
	require.Equal(t, token, actual)
	require.NoError(t, os.Chmod(path, 0644))
	_, err = ReadToken(path)
	require.Error(t, err)
	require.NoError(t, os.Chmod(path, 0600))
	require.NoError(t, os.WriteFile(path, []byte("short"), 0600))
	_, err = ReadToken(path)
	require.Error(t, err)
}

func privateDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "asset-bridge-")
	require.NoError(t, err)
	require.NoError(t, os.Chmod(dir, 0700))
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestProcessCrashRestartsStaleSocket(t *testing.T) {
	socket := filepath.Join(privateDir(t), "runtime.sock")
	start := func() *exec.Cmd {
		command := exec.Command(os.Args[0],
			"-test.run=^TestBridgeChildProcess$")
		command.Env = append(os.Environ(), "ASSET_BRIDGE_CHILD="+socket)
		output, err := command.StdoutPipe()
		require.NoError(t, err)
		require.NoError(t, command.Start())
		t.Cleanup(func() {
			_ = command.Process.Kill()
			_ = command.Wait()
		})
		ready := make(chan bool, 1)
		go func() {
			scanner := bufio.NewScanner(output)
			ready <- scanner.Scan() && scanner.Text() == "ready"
		}()
		select {
		case ok := <-ready:
			require.True(t, ok)
		case <-time.After(10 * time.Second):
			t.Fatal("bridge child startup timed out")
		}
		return command
	}
	first := start()
	client, err := NewClient(socket, make([]byte, 32))
	require.NoError(t, err)
	t.Cleanup(client.Close)
	var height uint32
	require.NoError(t, client.Call(t.Context(), Height, nil, &height))
	require.EqualValues(t, 456, height)
	require.NoError(t, first.Process.Kill())
	require.Error(t, first.Wait())
	_, err = os.Lstat(socket)
	require.NoError(t, err)
	second := start()
	require.NoError(t, client.Call(t.Context(), Height, nil, &height))
	require.EqualValues(t, 456, height)
	require.NoError(t, second.Process.Kill())
	require.Error(t, second.Wait())
}

func TestBridgeChildProcess(t *testing.T) {
	socket := os.Getenv("ASSET_BRIDGE_CHILD")
	if socket == "" {
		t.Skip("subprocess helper")
	}
	handler, err := Handler(make([]byte, 32), BackendFunc(func(
		context.Context, Method, json.RawMessage) (any, error) {

		return uint32(456), nil
	}))
	require.NoError(t, err)
	listener, err := Listen(socket)
	require.NoError(t, err)
	fmt.Println("ready")
	require.NoError(t, http.Serve(listener, handler))
}

func TestListenRejectsSymlinksAndUncoordinatedLiveSocket(t *testing.T) {
	dir := privateDir(t)
	path := filepath.Join(dir, "runtime.sock")
	target := filepath.Join(dir, "target")
	require.NoError(t, os.WriteFile(target, []byte("keep"), 0600))
	require.NoError(t, os.Symlink(target, path))
	_, err := Listen(path)
	require.Error(t, err)
	content, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "keep", string(content))
	require.NoError(t, os.Remove(path))
	live, err := net.Listen("unix", path)
	require.NoError(t, err)
	defer live.Close()
	require.NoError(t, os.Chmod(path, 0600))
	_, err = Listen(path)
	require.Error(t, err)
	info, err := os.Lstat(path)
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&os.ModeSocket)
}
