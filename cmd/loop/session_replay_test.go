package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// sessionsFS exposes the recorded session fixtures for replay tests.
var sessionsFS = func() fs.FS {
	// Locate this file to anchor the testdata path.
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		return emptyFS{}
	}

	// Derive the cmd/loop directory containing this file.
	loopDir := path.Dir(filename)

	// Build a filesystem rooted at the cmd/loop directory.
	cmdLoopFS := os.DirFS(loopDir)

	// Restrict the filesystem to the session fixture directory.
	sub, err := fs.Sub(cmdLoopFS, "testdata/sessions")
	if err != nil {
		return emptyFS{}
	}

	return sub
}()

// emptyFS is a filesystem that always reports no such file.
type emptyFS struct{}

// Open implements fs.FS by always returning a missing file error.
func (emptyFS) Open(string) (fs.File, error) {
	return nil, fs.ErrNotExist
}

// recordedSession holds the captured data required for a replay.
type recordedSession struct {
	args     []string
	env      map[string]string
	stdin    string
	stdout   string
	stderr   string
	runError *string
	conn     *recordedClientConn
}

// loadRecordedSessionFS loads a session from an fs.FS.
func loadRecordedSessionFS(fsys fs.FS, path string) (*recordedSession, error) {
	// Read the JSON file from the provided filesystem.
	blob, err := fs.ReadFile(fsys, path)
	if err != nil {
		return nil, err
	}

	// Parse the recorded JSON into a replay struct.
	return parseRecordedSession(blob)
}

// parseRecordedSession decodes a session JSON blob into replay data.
func parseRecordedSession(blob []byte) (*recordedSession, error) {
	// Decode the JSON file into the sessionFile struct.
	var data sessionFile
	if err := json.Unmarshal(blob, &data); err != nil {
		return nil, err
	}

	// Build a gRPC replay connection from recorded events.
	conn, err := newRecordedClientConn(data.Events)
	if err != nil {
		return nil, err
	}

	// Build buffers for replay IO streams.
	var (
		stdoutBuilder strings.Builder
		stderrBuilder strings.Builder
		stdinBuilder  strings.Builder
	)

	// Collect stdin/stdout/stderr payloads into buffers.
	for _, event := range data.Events {
		switch event.Kind {
		case eventStdout:
			var payload textPayload
			err := json.Unmarshal(event.Data, &payload)
			if err != nil {
				return nil, err
			}
			stdoutBuilder.WriteString(payload.Text)

		case eventStderr:
			var payload textPayload
			err := json.Unmarshal(event.Data, &payload)
			if err != nil {
				return nil, err
			}
			stderrBuilder.WriteString(payload.Text)

		case eventStdin:
			var payload stdinPayload
			err := json.Unmarshal(event.Data, &payload)
			if err != nil {
				return nil, err
			}
			stdinBuilder.WriteString(payload.Text)
		}
	}

	// Initialize the replay payload with metadata.
	return &recordedSession{
		args:     append([]string(nil), data.Metadata.Args...),
		env:      data.Metadata.Env,
		runError: data.Metadata.RunError,
		conn:     conn,
		stdin:    stdinBuilder.String(),
		stdout:   stdoutBuilder.String(),
		stderr:   stderrBuilder.String(),
	}, nil
}

// applyEnv sets environment variables and returns a restore function.
func applyEnv(values map[string]string) func() {
	// No environment changes are needed.
	if len(values) == 0 {
		return func() {}
	}

	// Track previous values for restoration.
	type previous struct {
		value string
		set   bool
	}

	// Capture existing environment and apply recorded values.
	prev := make(map[string]previous, len(values))
	for k, v := range values {
		curr, set := os.LookupEnv(k)
		prev[k] = previous{value: curr, set: set}
		_ = os.Setenv(k, v)
	}

	// Restore the original environment when done.
	return func() {
		for k, p := range prev {
			if p.set {
				_ = os.Setenv(k, p.value)
			} else {
				_ = os.Unsetenv(k)
			}
		}
	}
}

// protoMarshal encodes protobufs in a stable JSON form for comparisons.
var protoMarshal = protojson.MarshalOptions{
	UseProtoNames:   true,
	EmitUnpopulated: true,
}

// protoUnmarshal decodes protobuf JSON while ignoring unknown fields.
var protoUnmarshal = protojson.UnmarshalOptions{
	DiscardUnknown: true,
}

// replayTransport provides a recorded gRPC connection for session replays.
type replayTransport struct {
	conn *recordedClientConn
}

// Dial returns the recorded gRPC connection for replay.
func (t *replayTransport) Dial(cmd *cli.Command) (daemonConn, func(), error) {
	// Ignore the command; replays always use the recorded connection.
	return t.conn, func() {}, nil
}

// UnaryInterceptor returns nil because replay uses a recorded connection.
func (t *replayTransport) UnaryInterceptor() grpc.UnaryClientInterceptor {
	// No wrapping is needed for the recorded connection.
	return nil
}

// StreamInterceptor returns nil because replay uses a recorded connection.
func (t *replayTransport) StreamInterceptor() grpc.StreamClientInterceptor {
	// No wrapping is needed for the recorded connection.
	return nil
}

// recordedClientConn replays gRPC events captured during a session run.
type recordedClientConn struct {
	events []grpcPayload
	idx    int
	mu     sync.Mutex
}

// GetState reports the connection as shut down for replay.
func (c *recordedClientConn) GetState() connectivity.State {
	return connectivity.Shutdown
}

// WaitForStateChange returns immediately because replay is static.
func (c *recordedClientConn) WaitForStateChange(ctx context.Context,
	state connectivity.State) bool {

	return true
}

// Connect is a no-op for the replay connection.
func (c *recordedClientConn) Connect() {}

// newRecordedClientConn builds a replay connection from session events.
func newRecordedClientConn(events []sessionEvent) (*recordedClientConn, error) {
	var payloads []grpcPayload
	for _, event := range events {
		if event.Kind != eventGrpc {
			continue
		}

		var payload grpcPayload
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return nil, err
		}
		payloads = append(payloads, payload)
	}

	return &recordedClientConn{events: payloads}, nil
}

// Invoke replays unary gRPC calls by consuming recorded events.
func (c *recordedClientConn) Invoke(ctx context.Context, method string,
	args interface{}, reply interface{}, opts ...grpc.CallOption) error {

	// Guard concurrent access to the event stream.
	c.mu.Lock()
	defer c.mu.Unlock()

	// Validate the recorded request against the actual request.
	req, reqIdx, err := c.consumeLocked(method, "request")
	if err != nil {
		return err
	}

	err = compareMessageWithContext(
		method, req.Event, reqIdx, args, req.Payload,
	)
	if err != nil {
		return err
	}

	// Fetch the recorded response or error.
	resp, respIdx, err := c.consumeLocked(method, "response", "error")
	if err != nil {
		return err
	}

	// Translate recorded errors into returned errors.
	if resp.Event == "error" {
		if resp.Error == io.EOF.Error() {
			return io.EOF
		}

		return errors.New(resp.Error)
	}

	// Decode protobuf responses when the reply is a proto message.
	if replyMsg, ok := reply.(proto.Message); ok {
		if resp.MessageType != "" {
			got := string(proto.MessageName(replyMsg))
			if got != resp.MessageType {
				return fmt.Errorf("grpc %s response[%d] type "+
					"mismatch: got %s want %s", method,
					respIdx, got, resp.MessageType)
			}
		}

		if len(resp.Payload) > 0 {
			err := protoUnmarshal.Unmarshal(resp.Payload, replyMsg)
			if err != nil {
				return fmt.Errorf("grpc %s response[%d] "+
					"unmarshal: %w", method, respIdx, err)
			}
		}

		return nil
	}

	// Decode JSON responses when the reply is not a proto message.
	if len(resp.Payload) > 0 {
		if err := json.Unmarshal(resp.Payload, reply); err != nil {
			return fmt.Errorf("grpc %s response[%d] unmarshal: "+
				"%w", method, respIdx, err)
		}
	}

	return nil
}

// NewStream creates a replay stream backed by recorded events.
func (c *recordedClientConn) NewStream(ctx context.Context,
	desc *grpc.StreamDesc, method string,
	opts ...grpc.CallOption) (grpc.ClientStream, error) {

	// Create a stream wrapper that consumes events as needed.
	return &replayStream{
		conn:   c,
		method: method,
		ctx:    ctx,
	}, nil
}

// replayStream is a stream implementation backed by recorded events.
type replayStream struct {
	conn   *recordedClientConn
	method string
	ctx    context.Context
}

// Header returns empty metadata for replay.
func (s *replayStream) Header() (metadata.MD, error) {
	return nil, nil
}

// Trailer returns empty metadata for replay.
func (s *replayStream) Trailer() metadata.MD {
	return nil
}

// CloseSend is a no-op for replay streams.
func (s *replayStream) CloseSend() error {
	return nil
}

// Context returns the stream context.
func (s *replayStream) Context() context.Context {
	return s.ctx
}

// SendMsg validates an outgoing stream message against recorded events.
func (s *replayStream) SendMsg(m interface{}) error {
	// Protect the event stream with the shared mutex.
	s.conn.mu.Lock()
	defer s.conn.mu.Unlock()

	// Consume and compare the next recorded send event.
	evt, evtIdx, err := s.conn.consumeLocked(s.method, "send")
	if err != nil {
		return err
	}

	return compareMessageWithContext(
		s.method, evt.Event, evtIdx, m, evt.Payload,
	)
}

// RecvMsg replays the next recorded stream response.
func (s *replayStream) RecvMsg(m interface{}) error {
	// Protect the event stream with the shared mutex.
	s.conn.mu.Lock()
	defer s.conn.mu.Unlock()

	// Consume the next recorded receive event.
	evt, evtIdx, err := s.conn.consumeLocked(s.method, "recv", "error")
	if err != nil {
		return err
	}

	// Translate a recorded error into a return value.
	if evt.Event == "error" {
		if evt.Error == io.EOF.Error() {
			return io.EOF
		}

		return errors.New(evt.Error)
	}

	// Decode protobuf payloads into the provided message.
	if msg, ok := m.(proto.Message); ok {
		if evt.MessageType != "" {
			got := string(proto.MessageName(msg))
			if got != evt.MessageType {
				return fmt.Errorf("grpc %s recv[%d] type "+
					"mismatch: got %s want %s", s.method,
					evtIdx, got, evt.MessageType)
			}
		}

		err := protoUnmarshal.Unmarshal(evt.Payload, msg)
		if err != nil {
			return fmt.Errorf("grpc %s recv[%d] unmarshal: %w",
				s.method, evtIdx, err)
		}

		return nil
	}

	// Decode JSON payloads into non-protobuf messages.
	if len(evt.Payload) > 0 {
		if err := json.Unmarshal(evt.Payload, m); err != nil {
			return fmt.Errorf("grpc %s recv[%d] unmarshal: %w",
				s.method, evtIdx, err)
		}
	}

	return nil
}

// consumeLocked fetches the next matching event.
// The caller must hold c.mu.
func (c *recordedClientConn) consumeLocked(method, expected string,
	alternatives ...string) (*grpcPayload, int, error) {

	// Ensure there is another event to consume.
	if c.idx >= len(c.events) {
		return nil, c.idx, fmt.Errorf("grpc %s event[%d] missing, "+
			"expected %s", method, c.idx, expected)
	}

	// Fetch the next event and advance the cursor.
	idx := c.idx
	evt := c.events[c.idx]
	c.idx++

	// Verify the method matches.
	if evt.Method != method {
		return nil, idx, fmt.Errorf("grpc event[%d] unexpected "+
			"method %s, want %s", idx, evt.Method, method)
	}

	// Accept the expected event or a documented alternative.
	if evt.Event == expected || contains(alternatives, evt.Event) {
		return &evt, idx, nil
	}

	return nil, idx, fmt.Errorf("grpc %s event[%d] unexpected event %s, "+
		"expected %s", method, idx, evt.Event, expected)
}

// compareMessageWithContext marshals and compares a message against recorded
// JSON.
func compareMessageWithContext(method, event string, idx int, msg interface{},
	raw json.RawMessage) error {

	// Nothing to compare if there is no recorded payload.
	if len(raw) == 0 {
		return nil
	}

	// Marshal the actual message to JSON and compare.
	switch typed := msg.(type) {
	case proto.Message:
		actual, err := protoMarshal.Marshal(typed)
		if err != nil {
			return err
		}

		return compareJSONWithContext(method, event, idx, actual, raw)

	default:
		actual, err := json.Marshal(typed)
		if err != nil {
			return err
		}

		return compareJSONWithContext(method, event, idx, actual, raw)
	}
}

// compareJSONWithContext compares two JSON-encoded payloads with context.
func compareJSONWithContext(method, event string, idx int, actual []byte,
	recorded json.RawMessage) error {

	// Decode the actual and recorded payloads to generic maps.
	var (
		actualValue   interface{}
		recordedValue interface{}
	)

	if err := json.Unmarshal(actual, &actualValue); err != nil {
		return fmt.Errorf("grpc %s %s[%d] unmarshal actual: %w",
			method, event, idx, err)
	}
	if err := json.Unmarshal(recorded, &recordedValue); err != nil {
		return fmt.Errorf("grpc %s %s[%d] unmarshal recorded: %w",
			method, event, idx, err)
	}

	// Compare the decoded payloads and report any diff.
	if diff := cmp.Diff(recordedValue, actualValue); diff != "" {
		return fmt.Errorf("grpc %s %s[%d] mismatch (-want +got):\n%s",
			method, event, idx, diff)
	}

	return nil
}

// contains returns true when v is in the slice.
func contains(values []string, v string) bool {
	for _, value := range values {
		if value == v {
			return true
		}
	}

	return false
}

// TestRecordedSessions replays all recorded sessions and compares output.
func TestRecordedSessions(t *testing.T) {
	// Skip the test entirely when there is no session directory.
	if _, err := fs.ReadDir(sessionsFS, "."); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			t.Skip("no recorded sessions present")
		}
		require.NoError(t, err)
	}

	// Collect all session JSON files.
	var sessionFiles []string
	walkErr := fs.WalkDir(sessionsFS, ".",
		func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if strings.HasSuffix(d.Name(), sessionFileExt) {
				sessionFiles = append(sessionFiles, path)
			}

			return nil
		})
	require.NoError(t, walkErr)
	if len(sessionFiles) == 0 {
		t.Skip("no recorded sessions present")
	}

	for _, path := range sessionFiles {
		t.Run(path, func(t *testing.T) {
			// Force deterministic JSON output for replay.
			prevDeterministic := forceDeterministicJSON
			forceDeterministicJSON = true
			defer func() {
				forceDeterministicJSON = prevDeterministic
			}()

			// Load the recorded session fixture.
			replay, err := loadRecordedSessionFS(sessionsFS, path)
			require.NoErrorf(t, err, "load session %s", path)

			// Capture replay output for comparison.
			var (
				stdoutBuf bytes.Buffer
				stderrBuf bytes.Buffer
			)

			// Hook stdout for capture.
			stdoutUnhook, err := hookStdout(
				os.Stdout, nil, func(p []byte) {
					stdoutBuf.Write(p)
				},
			)
			require.NoErrorf(t, err, "hook stdout for %s", path)

			// Hook stderr for capture.
			stderrUnhook, err := hookStderr(
				os.Stderr, nil, func(p []byte) {
					stderrBuf.Write(p)
				},
			)
			require.NoErrorf(t, err, "hook stderr for %s", path)

			// Hook stdin to feed the recorded input.
			stdinUnhook, err := hookStdin(
				os.Stdin, bytes.NewBufferString(replay.stdin),
				nil,
			)
			require.NoErrorf(t, err, "hook stdin for %s", path)

			// Install the replay gRPC transport.
			restoreTransport := hookGrpc(
				&replayTransport{conn: replay.conn},
			)
			defer restoreTransport()

			// Apply the recorded environment variables.
			restoreEnv := applyEnv(replay.env)
			defer restoreEnv()

			// Use a fixed clock during replay.
			restoreClock := hookClock(
				clock.NewTestClock(
					time.Unix(sessionClockStartUnix, 0),
				),
			)
			defer restoreClock()

			// Ensure the session recorder is disabled during
			// replay runs.
			sessionRec = nil

			// Run the CLI command with a fresh root.
			cmd := newRootCommandForReplay()

			err = cmd.Run(t.Context(), replay.args)
			if err != nil {
				fmt.Fprintf(os.Stderr, "[loop] %v\n", err)
			}

			// Restore IO hooks before checking output.
			require.NoErrorf(
				t, stdoutUnhook(), "unhook stdout for %s", path,
			)
			require.NoErrorf(
				t, stderrUnhook(), "unhook stderr for %s", path,
			)
			require.NoErrorf(
				t, stdinUnhook(), "unhook stdin for %s", path,
			)

			// Validate the recorded error status matches the
			// replay result.
			if replay.runError != nil {
				require.Error(t, err, "expected run error")

				require.Equalf(
					t, *replay.runError, err.Error(),
					"run error mismatch for %s", path,
				)
			} else {
				require.NoErrorf(
					t, err, "command failed for %s", path,
				)
			}

			// Compare captured output to the recorded streams.
			requireTextEqual(
				t, "stdout", replay.stdout, stdoutBuf.String(),
			)
			requireTextEqual(
				t, "stderr", replay.stderr, stderrBuf.String(),
			)
		})
	}
}

// newRootCommandForReplay returns a root command clone with fresh flag state.
func newRootCommandForReplay() *cli.Command {
	// Clone the root command tree to avoid shared flag state.
	return cloneCommandForReplay(newRootCommand())
}

// cloneCommandForReplay deep-clones a command tree for deterministic replays.
func cloneCommandForReplay(cmd *cli.Command) *cli.Command {
	// Guard against nil command trees.
	if cmd == nil {
		return nil
	}

	// Clone the command struct and nested configuration.
	cloned := cloneCommandStruct(cmd)
	cloned.Flags, cloned.MutuallyExclusiveFlags = cloneFlagsWithGroups(
		cmd.Flags, cmd.MutuallyExclusiveFlags,
	)
	cloned.Arguments = cloneArguments(cmd.Arguments)
	cloned.Commands = cloneCommands(cmd.Commands)

	return cloned
}

// cloneCommandStruct copies exported fields of a command into a new instance.
func cloneCommandStruct(cmd *cli.Command) *cli.Command {
	// Guard against nil command trees.
	if cmd == nil {
		return nil
	}

	// Copy exported fields only to avoid deep internals.
	src := reflect.ValueOf(cmd).Elem()
	dst := reflect.New(src.Type()).Elem()
	copyExportedFields(dst, src)

	return dst.Addr().Interface().(*cli.Command)
}

// cloneCommands clones a list of subcommands for replay.
func cloneCommands(cmds []*cli.Command) []*cli.Command {
	// Return nil to preserve the original structure.
	if len(cmds) == 0 {
		return nil
	}

	// Clone each subcommand recursively.
	cloned := make([]*cli.Command, len(cmds))
	for i, cmd := range cmds {
		cloned[i] = cloneCommandForReplay(cmd)
	}

	return cloned
}

// cloneFlagsWithGroups clones flags and rebinds mutually exclusive groups.
func cloneFlagsWithGroups(flags []cli.Flag,
	groups []cli.MutuallyExclusiveFlags) ([]cli.Flag,
	[]cli.MutuallyExclusiveFlags) {

	// Clone flags and then remap mutually exclusive groups.
	clonedFlags, clonedMap := cloneFlags(flags)
	clonedGroups := cloneMutuallyExclusiveFlags(groups, clonedMap)

	return clonedFlags, clonedGroups
}

// cloneFlags creates fresh flag instances and returns a map of originals to
// clones.
func cloneFlags(flags []cli.Flag) ([]cli.Flag, map[cli.Flag]cli.Flag) {
	// Return an empty map when no flags are present.
	if len(flags) == 0 {
		return nil, map[cli.Flag]cli.Flag{}
	}

	// Clone each flag and record the mapping.
	cloned := make([]cli.Flag, len(flags))
	clonedMap := make(map[cli.Flag]cli.Flag, len(flags))
	for i, flag := range flags {
		if flag == nil {
			continue
		}
		flagCopy := cloneFlag(flag)
		cloned[i] = flagCopy
		clonedMap[flag] = flagCopy
	}

	return cloned, clonedMap
}

// cloneMutuallyExclusiveFlags clones flag groups using the provided flag map.
func cloneMutuallyExclusiveFlags(groups []cli.MutuallyExclusiveFlags,
	clonedMap map[cli.Flag]cli.Flag) []cli.MutuallyExclusiveFlags {

	// Return nil to preserve the original structure.
	if len(groups) == 0 {
		return nil
	}

	// Clone each group and its flags.
	clonedGroups := make([]cli.MutuallyExclusiveFlags, len(groups))
	for i, group := range groups {
		clonedGroup := cli.MutuallyExclusiveFlags{
			Required: group.Required,
			Category: group.Category,
		}

		if len(group.Flags) == 0 {
			clonedGroups[i] = clonedGroup
			continue
		}

		clonedGroup.Flags = make([][]cli.Flag, len(group.Flags))
		for j, option := range group.Flags {
			if len(option) == 0 {
				continue
			}

			clonedOption := make([]cli.Flag, len(option))
			for k, flag := range option {
				if flag == nil {
					continue
				}

				clone, ok := clonedMap[flag]
				if !ok {
					clone = cloneFlag(flag)
					clonedMap[flag] = clone
				}
				clonedOption[k] = clone
			}
			clonedGroup.Flags[j] = clonedOption
		}

		clonedGroups[i] = clonedGroup
	}

	return clonedGroups
}

// cloneFlag clones a single flag by copying its exported fields.
func cloneFlag(flag cli.Flag) cli.Flag {
	// Preserve nil flags as-is.
	if flag == nil {
		return nil
	}

	// Clone the concrete flag struct.
	cloned, ok := cloneStructWithExportedFields(flag)
	if !ok {
		return flag
	}
	clonedFlag, ok := cloned.(cli.Flag)
	if !ok {
		return flag
	}

	return clonedFlag
}

// cloneArguments clones positional argument definitions.
func cloneArguments(args []cli.Argument) []cli.Argument {
	// Return nil to preserve the original structure.
	if len(args) == 0 {
		return nil
	}

	// Clone each argument.
	cloned := make([]cli.Argument, len(args))
	for i, arg := range args {
		cloned[i] = cloneArgument(arg)
	}

	return cloned
}

// cloneArgument clones a single argument by copying its exported fields.
func cloneArgument(arg cli.Argument) cli.Argument {
	// Preserve nil arguments as-is.
	if arg == nil {
		return nil
	}

	// Clone the concrete argument struct.
	cloned, ok := cloneStructWithExportedFields(arg)
	if !ok {
		return arg
	}
	clonedArg, ok := cloned.(cli.Argument)
	if !ok {
		return arg
	}

	return clonedArg
}

// cloneStructWithExportedFields clones a pointer-to-struct by exported fields.
func cloneStructWithExportedFields(src interface{}) (interface{}, bool) {
	// Validate the input type.
	if src == nil {
		return nil, false
	}

	value := reflect.ValueOf(src)
	if value.Kind() != reflect.Ptr ||
		value.Elem().Kind() != reflect.Struct {

		return nil, false
	}

	// Allocate a new struct value and copy exported fields.
	cloned := reflect.New(value.Elem().Type())
	copyExportedFields(cloned.Elem(), value.Elem())

	return cloned.Interface(), true
}

// copyExportedFields copies exported fields from src into dst.
func copyExportedFields(dst, src reflect.Value) {
	// Iterate fields and copy only exported ones.
	for i := 0; i < src.NumField(); i++ {
		field := src.Type().Field(i)
		if field.PkgPath != "" {
			continue
		}

		dstField := dst.Field(i)
		if !dstField.CanSet() {
			continue
		}

		dstField.Set(cloneValue(src.Field(i)))
	}
}

// cloneValue shallow-clones slices and maps while preserving other values.
func cloneValue(value reflect.Value) reflect.Value {
	// Return invalid values as-is.
	if !value.IsValid() {
		return value
	}

	// Clone supported types while preserving value semantics.
	switch value.Kind() {
	case reflect.Slice:
		if value.IsNil() {
			return value
		}
		cloned := reflect.MakeSlice(
			value.Type(), value.Len(), value.Len(),
		)
		reflect.Copy(cloned, value)

		return cloned

	case reflect.Map:
		if value.IsNil() {
			return value
		}
		cloned := reflect.MakeMapWithSize(value.Type(), value.Len())
		for _, key := range value.MapKeys() {
			cloned.SetMapIndex(key, value.MapIndex(key))
		}

		return cloned

	default:
		return value
	}
}

// requireTextEqual compares text after normalizing timestamps.
func requireTextEqual(t *testing.T, label, expected, actual string) {
	t.Helper()

	// Normalize timezone-dependent timestamps before comparing.
	expected = normalizeTimestamps(expected)
	actual = normalizeTimestamps(actual)
	if diff := cmp.Diff(expected, actual); diff != "" {
		t.Fatalf("%s mismatch (-want +got):\n%s", label, diff)
	}
}

// rfc3339TimestampRegex matches RFC3339 timestamps embedded in CLI output.
var rfc3339TimestampRegex = regexp.MustCompile(
	`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})`,
)

// timeStringTimestampRegex matches time.String-style timestamps embedded in
// CLI output.
var timeStringTimestampRegex = regexp.MustCompile(
	`\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} [+-]\d{4} [A-Z]{2,5}`,
)

// normalizeTimestamps rewrites embedded timestamps to UTC to avoid
// environment-dependent timezone output during session replay.
func normalizeTimestamps(text string) string {
	// Normalize RFC3339 timestamps first.
	rfc3339Replacer := func(ts string) string {
		parsed, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return ts
		}

		return parsed.UTC().Format(time.RFC3339Nano)
	}
	text = rfc3339TimestampRegex.ReplaceAllStringFunc(
		text, rfc3339Replacer,
	)

	// Normalize time.String timestamps next.
	timeReplacer := func(ts string) string {
		parsed, err := time.Parse("2006-01-02 15:04:05 -0700 MST", ts)
		if err != nil {
			return ts
		}

		return parsed.UTC().Format("2006-01-02 15:04:05 -0700 MST")
	}

	text = timeStringTimestampRegex.ReplaceAllStringFunc(
		text, timeReplacer,
	)

	return text
}

// TestCloneCommandForReplayResetsFlagState verifies cloned commands reset flag
// state.
func TestCloneCommandForReplayResetsFlagState(t *testing.T) {
	// Prepare a flag that has been set.
	originalFlag := &cli.StringFlag{
		Name:    "alpha",
		Usage:   "alpha usage",
		Aliases: []string{"a"},
	}
	require.NoError(t, originalFlag.Set("alpha", "value"))
	require.True(t, originalFlag.IsSet())

	// Prepare a shared flag that appears in multiple command locations.
	sharedFlag := &cli.BoolFlag{Name: "shared"}
	require.NoError(t, sharedFlag.Set("shared", "true"))
	require.True(t, sharedFlag.IsSet())

	// Prepare a parsed argument.
	originalArg := &cli.StringArg{
		Name:      "arg",
		UsageText: "arg usage",
		Value:     "default",
	}
	_, err := originalArg.Parse([]string{"parsed"})
	require.NoError(t, err)
	require.Equal(t, "parsed", originalArg.Get())

	// Build a command tree with flags, args, and metadata.
	root := &cli.Command{
		Name:  "root",
		Flags: []cli.Flag{originalFlag, sharedFlag},
		MutuallyExclusiveFlags: []cli.MutuallyExclusiveFlags{
			{
				Flags: [][]cli.Flag{
					{originalFlag},
					{sharedFlag},
				},
				Required: true,
				Category: "cat",
			},
		},
		Arguments: []cli.Argument{originalArg},
		Metadata: map[string]interface{}{
			"key": "value",
		},
		Commands: []*cli.Command{
			{
				Name:  "sub",
				Flags: []cli.Flag{sharedFlag},
			},
		},
	}

	// Clone the command tree for replay.
	cloned := cloneCommandForReplay(root)

	// Validate structural equivalence.
	require.NotSame(t, root, cloned)
	require.Len(t, cloned.Flags, len(root.Flags))
	require.Len(t, cloned.Commands, len(root.Commands))
	require.Len(
		t, cloned.MutuallyExclusiveFlags,
		len(root.MutuallyExclusiveFlags),
	)
	require.Len(t, cloned.Arguments, len(root.Arguments))

	// Ensure flags are cloned and reset.
	clonedAlpha := findFlagByName(
		t, cloned.Flags, "alpha",
	).(*cli.StringFlag)
	require.NotSame(t, originalFlag, clonedAlpha)
	require.False(t, clonedAlpha.IsSet())
	require.Equal(t, originalFlag.Name, clonedAlpha.Name)
	require.Equal(t, originalFlag.Usage, clonedAlpha.Usage)
	require.Equal(t, originalFlag.Aliases, clonedAlpha.Aliases)

	// Ensure the clone is independent.
	clonedAlpha.Aliases[0] = "b"
	require.Equal(t, []string{"a"}, originalFlag.Aliases)

	// Ensure shared flags are cloned and reset.
	clonedShared := findFlagByName(
		t, cloned.Flags, "shared",
	).(*cli.BoolFlag)
	require.NotSame(t, sharedFlag, clonedShared)
	require.False(t, clonedShared.IsSet())

	// Ensure the mutual exclusion groups reference clones.
	group := cloned.MutuallyExclusiveFlags[0]
	require.Same(t, clonedAlpha, group.Flags[0][0].(*cli.StringFlag))
	require.Same(t, clonedShared, group.Flags[1][0].(*cli.BoolFlag))

	// Ensure positional arguments are cloned and reset.
	clonedArg := cloned.Arguments[0].(*cli.StringArg)
	require.NotSame(t, originalArg, clonedArg)
	require.Equal(t, "default", clonedArg.Get())

	// Ensure metadata maps are independent.
	cloned.Metadata["key"] = "updated"
	require.Equal(t, "value", root.Metadata["key"])
}

// findFlagByName locates a flag by name or alias.
func findFlagByName(t *testing.T, flags []cli.Flag, name string) cli.Flag {
	t.Helper()

	// Scan each flag and its aliases for the name.
	for _, flag := range flags {
		if flag == nil {
			continue
		}
		for _, candidate := range flag.Names() {
			if candidate == name {
				return flag
			}
		}
	}
	t.Fatalf("flag %q not found", name)

	return nil
}
