package bridge

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const (
	maxMessage     = 64 << 20
	maxCallTime    = 30 * time.Second
	tokenHeader    = "X-Asset-Bridge-Token"
	deadlineHeader = "X-Asset-Bridge-Deadline"
)

type envelope struct {
	Version uint32
	Method  Method
	Payload json.RawMessage
}

type response struct {
	Version uint32
	Payload json.RawMessage
	Error   string
}

// Client connects only to an explicit local Unix socket. The token is never
// placed in a URL or response and redirects are disabled.
type Client struct {
	http  *http.Client
	token string
}

// NewClient constructs an authenticated Unix transport without making a call.
func NewClient(socket string, token []byte) (*Client, error) {
	if !filepath.IsAbs(socket) || len(token) != 32 {
		return nil, ErrInvalid
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (
			net.Conn, error) {

			info, err := os.Lstat(filepath.Dir(socket))
			if err != nil {
				return nil, err
			}
			if !info.IsDir() || !owned(info) ||
				info.Mode().Perm()&0077 != 0 {

				return nil, ErrInvalid
			}
			info, err = os.Lstat(socket)
			if err != nil {
				return nil, err
			}
			if info.Mode()&os.ModeSocket == 0 || !owned(info) ||
				info.Mode().Perm()&0077 != 0 {

				return nil, ErrInvalid
			}
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
		DisableCompression: true,
	}
	return &Client{
		token: hex.EncodeToString(token),
		http: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request,
				[]*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// Close releases idle connections after the daemon has joined its workers.
func (c *Client) Close() { c.http.CloseIdleConnections() }

// Call propagates a bounded deadline and rejects unknown protocol responses.
func (c *Client) Call(ctx context.Context, method Method, input,
	result any) error {
	if !known(method) {
		return ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, maxCallTime)
	defer cancel()
	payload, err := json.Marshal(input)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(envelope{Version, method, payload})
	if err != nil {
		return err
	}
	if len(raw) > maxMessage {
		return ErrInvalid
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://asset-runtime/v1/call", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set(tokenHeader, c.token)
	deadline, _ := ctx.Deadline()
	encodedDeadline := strconv.FormatInt(deadline.UnixNano(), 10)
	req.Header.Set(deadlineHeader, encodedDeadline)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return ErrRemote
	}
	raw, err = io.ReadAll(io.LimitReader(res.Body, maxMessage+1))
	if err != nil {
		return err
	}
	if len(raw) > maxMessage {
		return ErrRemote
	}
	var reply response
	if err := decode(raw, &reply); err != nil || reply.Version != Version {
		return ErrRemote
	}
	switch reply.Error {
	case "":
	case "pending":
		return ErrPending
	case "invalid":
		return ErrInvalid
	default:
		return ErrRemote
	}
	if result == nil {
		return nil
	}
	return decode(reply.Payload, result)
}

// Handler authenticates before decoding and exposes only versioned methods.
// Backend failures are opaque even on this private authenticated interface.
func Handler(token []byte, backend Backend) (http.Handler, error) {
	if len(token) != 32 || backend == nil {
		return nil, ErrInvalid
	}
	want := hex.EncodeToString(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/call" ||
			subtle.ConstantTimeCompare([]byte(want),
				[]byte(r.Header.Get(tokenHeader))) != 1 {

			http.Error(w, "unavailable", http.StatusUnauthorized)
			return
		}
		deadline, err := strconv.ParseInt(
			r.Header.Get(deadlineHeader), 10, 64,
		)
		if err != nil || deadline <= time.Now().UnixNano() {
			http.Error(w, "unavailable", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), maxCallTime)
		defer cancel()
		ctx, cancelDeadline := context.WithDeadline(
			ctx, time.Unix(0, deadline),
		)
		defer cancelDeadline()
		r.Body = http.MaxBytesReader(w, r.Body, maxMessage)
		raw, err := io.ReadAll(r.Body)
		var request envelope
		if err != nil || decode(raw, &request) != nil ||
			request.Version != Version || !known(request.Method) {

			http.Error(w, "unavailable", http.StatusBadRequest)
			return
		}
		value, callErr := backend.Call(
			ctx, request.Method, request.Payload,
		)
		reply := response{Version: Version}
		switch {
		case errors.Is(callErr, ErrPending):
			reply.Error = "pending"
		case errors.Is(callErr, ErrInvalid):
			reply.Error = "invalid"
		case callErr != nil:
			reply.Error = "failed"
		default:
			reply.Payload, err = json.Marshal(value)
			if err != nil {
				reply.Error = "failed"
			}
		}
		encoded, err := json.Marshal(reply)
		if err != nil || len(encoded) > maxMessage {
			http.Error(w, "unavailable",
				http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(encoded)
	}), nil
}

// Listen creates a private Unix socket and holds an exclusive process lock.
// A stopped process leaves a stale socket after SIGKILL; only an owned socket
// that refuses connections can be removed, while live listeners are refused.
func Listen(socket string) (net.Listener, error) {
	if !filepath.IsAbs(socket) {
		return nil, ErrInvalid
	}
	info, err := os.Lstat(filepath.Dir(socket))
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 || !owned(info) {
		return nil, errors.New(
			"asset runtime directory must be private",
		)
	}
	fd, err := syscall.Open(socket+".lock",
		syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	lock := os.NewFile(uintptr(fd), socket+".lock")
	info, err = lock.Stat()
	if err != nil || !info.Mode().IsRegular() || !owned(info) ||
		info.Mode().Perm()&0077 != 0 {

		_ = lock.Close()
		return nil, errors.New("invalid asset runtime lock file")
	}
	flags := syscall.LOCK_EX | syscall.LOCK_NB
	if err := syscall.Flock(fd, flags); err != nil {
		_ = lock.Close()
		return nil, errors.New("asset runtime already running")
	}
	if err := removeStaleSocket(socket); err != nil {
		_ = lock.Close()
		return nil, err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	if err := os.Chmod(socket, 0600); err != nil {
		_ = listener.Close()
		_ = lock.Close()
		return nil, err
	}
	return &lockedListener{Listener: listener, lock: lock}, nil
}

type lockedListener struct {
	net.Listener
	lock *os.File
	once sync.Once
	err  error
}

func (l *lockedListener) Close() error {
	l.once.Do(func() {
		l.err = l.Listener.Close()
		_ = l.lock.Close()
	})
	return l.err
}

func owned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Getuid())
}

func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 || !owned(info) {
		return errors.New("invalid asset runtime socket")
	}
	connection, err := net.DialTimeout("unix", path, 100*time.Millisecond)
	if err == nil {
		_ = connection.Close()
		return errors.New("asset runtime already running")
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(info, current) {
		return errors.New("asset runtime socket changed")
	}
	return os.Remove(path)
}

// ReadToken reads exactly 32 raw secret bytes from a private regular file.
func ReadToken(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || !owned(info) ||
		info.Mode().Perm()&0077 != 0 ||
		info.Size() != 32 {

		return nil, errors.New("invalid asset runtime token file")
	}
	return os.ReadFile(path)
}

func decode(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("trailing asset bridge data")
	}
	return nil
}

func known(method Method) bool {
	switch method {
	case CheckReady, Height, VerifyAdmission, ValidateDeposits,
		ValidateInvoice, BuildFallback, VerifyClaim, ValidateExecution,
		RevealPreimage, RecoverClaim, InspectRefund, Inventory,
		CreateDeposit, RefundDeposit, PrepareRecovery, ValidateOffer,
		VerifyFinalizeFallback, LookupPreimage, SignCooperative,
		InspectClaim, RecoverRefund, StoreRefundProof:

		return true
	default:
		return false
	}
}
