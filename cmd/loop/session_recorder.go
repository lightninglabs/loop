package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lightninglabs/loop"
	"github.com/urfave/cli/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	sessionEnvVar     = "LOOP_SESSION_RECORD"
	sessionDefaultDir = "cmd/loop/testdata/sessions"
	sessionFileExt    = ".json"
)

// sessionClockStartUnix is the fixed timestamp used for recorded CLI sessions.
const sessionClockStartUnix int64 = 1769407086

// Event identifiers in recorded sessions.
const (
	eventStdout = "stdout"
	eventStderr = "stderr"
	eventStdin  = "stdin"
	eventGrpc   = "grpc"
	eventExit   = "exit"
	eventSignal = "signal"
)

// grpcMarshalOptions is marshalling options to encode gRPC messages in recorded
// sessions.
var grpcMarshalOptions = protojson.MarshalOptions{
	UseProtoNames:   true,
	EmitUnpopulated: true,
}

// sessionRecorder captures CLI IO and gRPC traffic for replay.
type sessionRecorder struct {
	mu       sync.Mutex
	started  time.Time
	filePath string
	slug     string

	metadata sessionMetadata
	events   []sessionEvent

	finalizeOnce sync.Once

	hooksMu      sync.Mutex
	hooksStarted bool
	stdoutUnhook func() error
	stderrUnhook func() error
	stdinUnhook  func() error
}

// sessionFile is a single recorded CLI session in JSON format. They are
// stored inside sessionDefaultDir.
type sessionFile struct {
	Metadata sessionMetadata `json:"metadata"`
	Events   []sessionEvent  `json:"events"`
}

// sessionMetadata stores static session details and runtime metadata.
type sessionMetadata struct {
	Args     []string          `json:"args"`
	Env      map[string]string `json:"env"`
	Version  string            `json:"version"`
	RunError *string           `json:"run_error,omitempty"`
	Duration *time.Duration    `json:"duration,omitempty"`
}

// sessionEvent records a single timestamped payload entry.
type sessionEvent struct {
	TimeMS int64           `json:"time_ms"`
	Kind   string          `json:"kind"`
	Data   json.RawMessage `json:"data"`
}

// textPayload records stdout/stderr text chunks.
type textPayload struct {
	Text string `json:"text"`
}

// stdinPayload records stdin chunks.
type stdinPayload struct {
	Text string `json:"text"`
}

// grpcPayload records a gRPC message or error event.
type grpcPayload struct {
	Method      string          `json:"method"`
	Event       string          `json:"event"`
	MessageType string          `json:"message_type,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	Error       string          `json:"error,omitempty"`
}

// exitPayload records the final run error, if any.
type exitPayload struct {
	RunError *string `json:"run_error,omitempty"`
}

// signalPayload records handled signals.
type signalPayload struct {
	Signal string `json:"signal"`
}

// newSessionRecorder creates a recorder when session recording is enabled.
func newSessionRecorder(args []string) (*sessionRecorder, error) {
	// Session recording is disabled unless the env var is set.
	envValue, ok := os.LookupEnv(sessionEnvVar)
	if !ok {
		return nil, nil
	}

	// Allow explicit disable by setting the env var to false.
	enabled, err := strconv.ParseBool(envValue)
	if err != nil {
		return nil, fmt.Errorf("invalid %s value %q", sessionEnvVar,
			envValue)
	}
	if !enabled {
		return nil, nil
	}

	// Initialize the recorder before collecting metadata.
	recorder := &sessionRecorder{
		started: time.Now(),
	}

	// Derive the slug before resolving the output file path.
	recorder.slug = deriveSessionSlug(args)

	// Capture metadata that remains stable for the session.
	metadata := sessionMetadata{
		Args:    append([]string(nil), args...),
		Env:     collectSessionEnv(),
		Version: loop.RichVersion(),
	}
	recorder.metadata = metadata

	// Resolve the session file location.
	baseDir, fileName, err := recorder.resolveFilePath()
	if err != nil {
		return nil, err
	}
	recorder.filePath = filepath.Join(baseDir, fileName)

	return recorder, nil
}

// collectSessionEnv extracts the environment variables recorded in sessions.
func collectSessionEnv() map[string]string {
	env := make(map[string]string)

	// Record only LOOPCLI_ variables, excluding the recording toggle.
	for _, kv := range os.Environ() {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := parts[0]
		value := parts[1]
		if key == sessionEnvVar {
			continue
		}
		if strings.HasPrefix(key, "LOOPCLI_") {
			env[key] = value
		}
	}

	return env
}

// resolveFilePath chooses the output directory and filename.
func (r *sessionRecorder) resolveFilePath() (string, string, error) {
	counter, err := nextSessionCounter(sessionDefaultDir)
	if err != nil {
		return "", "", err
	}

	slug := r.slug
	if slug == "" {
		slug = "session"
	}

	name := fmt.Sprintf("%02d_%s%s", counter, slug, sessionFileExt)

	return sessionDefaultDir, name, nil
}

// logEvent records a new event with the elapsed timestamp.
func (r *sessionRecorder) logEvent(kind string, payload interface{}) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}

	event := sessionEvent{
		TimeMS: time.Since(r.started).Milliseconds(),
		Kind:   kind,
		Data:   data,
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.events = append(r.events, event)
}

// Start attaches stdin/stdout/stderr hooks for session recording.
func (r *sessionRecorder) Start(stdinSource io.Reader,
	stdoutForward, stderrForward io.Writer) error {

	r.hooksMu.Lock()
	defer r.hooksMu.Unlock()

	if r.hooksStarted {
		return nil
	}

	// Capture stdout and stderr first, then stdin.
	origStdout := os.Stdout
	if stdoutForward == nil {
		stdoutForward = origStdout
	}
	outHook, err := hookStdout(origStdout, stdoutForward, func(p []byte) {
		r.logEvent(eventStdout, textPayload{Text: string(p)})
	})
	if err != nil {
		return err
	}

	origStderr := os.Stderr
	if stderrForward == nil {
		stderrForward = origStderr
	}
	errHook, err := hookStderr(origStderr, stderrForward, func(p []byte) {
		r.logEvent(eventStderr, textPayload{Text: string(p)})
	})
	if err != nil {
		_ = outHook()

		return err
	}

	origStdin := os.Stdin
	if stdinSource == nil {
		stdinSource = origStdin
	}
	stdinHook, err := hookStdin(origStdin, stdinSource, func(p []byte) {
		r.logEvent(eventStdin, stdinPayload{Text: string(p)})
	})
	if err != nil {
		_ = errHook()
		_ = outHook()

		return err
	}

	r.stdoutUnhook = outHook
	r.stderrUnhook = errHook
	r.stdinUnhook = stdinHook
	r.hooksStarted = true

	return nil
}

// stopHooks detaches any active IO hooks.
func (r *sessionRecorder) stopHooks() error {
	r.hooksMu.Lock()
	defer r.hooksMu.Unlock()

	var firstErr error
	if r.stdoutUnhook != nil {
		if err := r.stdoutUnhook(); err != nil && firstErr == nil {
			firstErr = err
		}
		r.stdoutUnhook = nil
	}
	if r.stderrUnhook != nil {
		if err := r.stderrUnhook(); err != nil && firstErr == nil {
			firstErr = err
		}
		r.stderrUnhook = nil
	}
	if r.stdinUnhook != nil {
		if err := r.stdinUnhook(); err != nil && firstErr == nil {
			firstErr = err
		}
		r.stdinUnhook = nil
	}
	r.hooksStarted = false

	return firstErr
}

// logExit records the final outcome and duration.
func (r *sessionRecorder) logExit(runErr error) {
	var payload exitPayload
	if runErr != nil {
		msg := runErr.Error()
		payload.RunError = &msg
	}

	// Store the exit event first for the event stream.
	r.logEvent(eventExit, payload)

	// Update metadata with the final run state.
	duration := time.Since(r.started)

	r.mu.Lock()
	defer r.mu.Unlock()

	if runErr != nil {
		msg := runErr.Error()
		r.metadata.RunError = &msg
	} else {
		r.metadata.RunError = nil
	}
	r.metadata.Duration = &duration
}

// finalize writes the recorded session to disk once.
func (r *sessionRecorder) finalize(runErr error) error {
	var finalizeErr error
	r.finalizeOnce.Do(func() {
		if err := r.stopHooks(); err != nil {
			finalizeErr = err

			return
		}

		r.logExit(runErr)

		r.mu.Lock()
		metadata := r.metadata
		events := append([]sessionEvent(nil), r.events...)
		r.mu.Unlock()

		fileContent := sessionFile{
			Metadata: metadata,
			Events:   events,
		}

		err := os.MkdirAll(filepath.Dir(r.filePath), 0o755)
		if err != nil {
			finalizeErr = err

			return
		}

		file, err := os.Create(r.filePath)
		if err != nil {
			finalizeErr = err

			return
		}
		defer file.Close()

		encoder := json.NewEncoder(file)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(fileContent); err != nil {
			finalizeErr = err

			return
		}
	})

	return finalizeErr
}

// Finalize records the exit event and flushes the session to disk.
func (r *sessionRecorder) Finalize(runErr error) error {
	return r.finalize(runErr)
}

// Dial uses the direct gRPC connection for recording.
func (r *sessionRecorder) Dial(cmd *cli.Command) (daemonConn, func(), error) {
	return dialDirectConn(cmd)
}

// UnaryInterceptor captures unary RPCs for session playback.
func (r *sessionRecorder) UnaryInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply interface{},
		cc *grpc.ClientConn, invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption) error {

		r.logGRPCMessage(method, "request", req, nil)

		err := invoker(ctx, method, req, reply, cc, opts...)
		if err != nil {
			r.logGRPCMessage(method, "error", nil, err)

			return err
		}

		r.logGRPCMessage(method, "response", reply, nil)

		return nil
	}
}

// StreamInterceptor captures stream RPCs for session playback.
func (r *sessionRecorder) StreamInterceptor() grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc,
		cc *grpc.ClientConn, method string, streamer grpc.Streamer,
		opts ...grpc.CallOption) (grpc.ClientStream, error) {

		clientStream, err := streamer(ctx, desc, cc, method, opts...)
		if err != nil {
			r.logGRPCMessage(method, "error", nil, err)

			return nil, err
		}

		return &recordingClientStream{
			ClientStream: clientStream,
			recorder:     r,
			method:       method,
		}, nil
	}
}

// recordingClientStream wraps a gRPC stream and logs message events.
type recordingClientStream struct {
	grpc.ClientStream

	recorder *sessionRecorder
	method   string
}

// SendMsg records the outgoing stream message.
func (s *recordingClientStream) SendMsg(m interface{}) error {
	s.recorder.logGRPCMessage(s.method, "send", m, nil)
	err := s.ClientStream.SendMsg(m)
	if err != nil {
		s.recorder.logGRPCMessage(s.method, "error", nil, err)
	}

	return err
}

// RecvMsg records the incoming stream message.
func (s *recordingClientStream) RecvMsg(m interface{}) error {
	err := s.ClientStream.RecvMsg(m)
	if err != nil {
		s.recorder.logGRPCMessage(s.method, "error", nil, err)

		return err
	}

	s.recorder.logGRPCMessage(s.method, "recv", m, nil)

	return nil
}

// logGRPCMessage captures gRPC request/response data in the event stream.
func (r *sessionRecorder) logGRPCMessage(method, event string, msg interface{},
	receptionErr error) {

	payload := grpcPayload{Method: method, Event: event}

	if receptionErr != nil {
		payload.Error = receptionErr.Error()
		r.logEvent(eventGrpc, payload)

		return
	}

	if msg != nil {
		if protoMsg, ok := msg.(proto.Message); ok {
			payload.MessageType = string(
				proto.MessageName(protoMsg),
			)
			data, err := grpcMarshalOptions.Marshal(protoMsg)
			if err == nil {
				payload.Payload = data
			}
		} else {
			data, err := json.Marshal(msg)
			if err == nil {
				payload.Payload = data
			}
		}
	}

	r.logEvent(eventGrpc, payload)
}

// LogSignal records an incoming signal event.
func (r *sessionRecorder) LogSignal(sig os.Signal) {
	r.logEvent(eventSignal, signalPayload{Signal: sig.String()})
}

// InjectContext tags the outgoing context with the session name.
func (r *sessionRecorder) InjectContext(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(
		ctx, "loop-session", filepath.Base(r.filePath),
	)
}

// deriveSessionSlug builds a stable slug from the command arguments.
func deriveSessionSlug(args []string) string {
	if len(args) == 0 {
		return ""
	}

	base := filepath.Base(args[0])
	tokens := []string{base}

	for _, arg := range args[1:] {
		if strings.HasPrefix(arg, "-") {
			break
		}
		if arg == "" {
			continue
		}
		tokens = append(tokens, arg)
	}

	return sanitizeSlug(strings.Join(tokens, "-"))
}

// sanitizeSlug normalizes a session slug to a safe filename.
func sanitizeSlug(value string) string {
	value = strings.ToLower(value)
	var builder strings.Builder
	lastDash := false
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			builder.WriteRune(r)
			lastDash = false

			continue
		}

		if !lastDash {
			builder.WriteRune('-')
			lastDash = true
		}
	}

	slug := strings.Trim(builder.String(), "-")
	if slug == "" {
		return "session"
	}

	return slug
}

// nextSessionCounter finds the next available session number.
func nextSessionCounter(baseDir string) (int, error) {
	maxCounter := 0
	entries, err := os.ReadDir(baseDir)
	if errors.Is(err, fs.ErrNotExist) {
		return 1, nil
	}
	if err != nil {
		return 0, err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if filepath.Ext(entry.Name()) != sessionFileExt {
			continue
		}

		counter, ok := parseSessionCounter(entry.Name())
		if !ok {
			continue
		}
		if counter > maxCounter {
			maxCounter = counter
		}
	}

	return maxCounter + 1, nil
}

// parseSessionCounter extracts the numeric prefix from a session filename.
func parseSessionCounter(name string) (int, bool) {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	if base == "" {
		return 0, false
	}

	parts := strings.SplitN(base, "_", 2)
	if parts[0] == "" {
		return 0, false
	}

	for _, r := range parts[0] {
		if r < '0' || r > '9' {
			return 0, false
		}
	}

	value, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, false
	}

	if value < 0 {
		return 0, false
	}

	return value, true
}
