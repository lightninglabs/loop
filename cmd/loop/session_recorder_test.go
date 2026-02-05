package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	testLoopArg     = "loop"
	testOutArg      = "out"
	testLoopOutSlug = "loop-out"
	testVersion     = "test-version"
	testClockStart  = int64(1769407086)
)

// TestDeriveSessionSlug verifies slug derivation from CLI arguments.
func TestDeriveSessionSlug(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "empty_args",
			want: "",
		},
		{
			name: "binary_only",
			args: []string{"/usr/local/bin/loop"},
			want: testLoopArg,
		},
		{
			name: "with_subcommand",
			args: []string{testLoopArg, testOutArg},
			want: testLoopOutSlug,
		},
		{
			name: "stops_at_flag",
			args: []string{testLoopArg, testOutArg, "--network", "regtest"},
			want: testLoopOutSlug,
		},
		{
			name: "skips_empty_args",
			args: []string{testLoopArg, "", "quote", testOutArg},
			want: "loop-quote-out",
		},
		{
			name: "sanitizes_tokens",
			args: []string{testLoopArg, "Quote", "Out"},
			want: "loop-quote-out",
		},
		{
			name: "sanitizes_path_base",
			args: []string{"/tmp/loop-cli", testOutArg},
			want: "loop-cli-out",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			slug := deriveSessionSlug(test.args)
			require.Equal(t, test.want, slug)
		})
	}
}

// TestSanitizeSlug verifies slug normalization behavior.
func TestSanitizeSlug(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "already_clean",
			input: testLoopOutSlug,
			want:  testLoopOutSlug,
		},
		{
			name:  "upper_and_spaces",
			input: "Loop Out",
			want:  testLoopOutSlug,
		},
		{
			name:  "symbols_collapsed",
			input: "loop@@@out",
			want:  testLoopOutSlug,
		},
		{
			name:  "trims_dashes",
			input: "--loop-out--",
			want:  testLoopOutSlug,
		},
		{
			name:  "empty_to_default",
			input: "!!!",
			want:  defaultSessionSlug,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			slug := sanitizeSlug(test.input)
			require.Equal(t, test.want, slug)
		})
	}
}

// TestParseSessionCounter verifies session counter parsing.
func TestParseSessionCounter(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int
		ok    bool
	}{
		{
			name:  "with_suffix",
			input: "01_loop-out.json",
			want:  1,
			ok:    true,
		},
		{
			name:  "with_extra_underscores",
			input: "12_loop_in.json",
			want:  12,
			ok:    true,
		},
		{
			name:  "no_suffix",
			input: "99",
			want:  99,
			ok:    true,
		},
		{
			name:  "no_extension",
			input: "7_loop-out",
			want:  7,
			ok:    true,
		},
		{
			name:  "empty_name",
			input: "",
			ok:    false,
		},
		{
			name:  "missing_prefix",
			input: "_loop.json",
			ok:    false,
		},
		{
			name:  "non_numeric_prefix",
			input: "loop.json",
			ok:    false,
		},
		{
			name:  "mixed_prefix",
			input: "1a_loop.json",
			ok:    false,
		},
		{
			name:  "negative_prefix",
			input: "-1_loop.json",
			ok:    false,
		},
		{
			name:  "plus_prefix",
			input: "+1_loop.json",
			ok:    false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value, ok := parseSessionCounter(test.input)
			require.Equal(t, test.ok, ok)
			if test.ok {
				require.Equal(t, test.want, value)
			}
		})
	}
}

// TestSessionFinalizeWritesFileWhenHookRestoreFails verifies that session
// data is still written to disk when hook restoration returns an error.
func TestSessionFinalizeWritesFileWhenHookRestoreFails(t *testing.T) {
	recorder := &sessionRecorder{
		started:  time.Now().Add(-time.Second),
		filePath: filepath.Join(t.TempDir(), "session.json"),
		metadata: sessionMetadata{
			Args:           []string{testLoopArg, testOutArg},
			Env:            map[string]string{},
			Version:        testVersion,
			ClockStartUnix: testClockStart,
		},
		hooksStarted: true,
		stdoutUnhook: func() error {
			return errors.New("stdout restore failed")
		},
	}

	err := recorder.Finalize(nil)
	require.EqualError(t, err, "stdout restore failed")

	blob, err := os.ReadFile(recorder.filePath)
	require.NoError(t, err)

	var data sessionFile
	require.NoError(t, json.Unmarshal(blob, &data))
	require.Equal(t, recorder.metadata.Args, data.Metadata.Args)
	require.Equal(t, recorder.metadata.Version, data.Metadata.Version)
	require.NotNil(t, data.Metadata.Duration)
	require.Nil(t, data.Metadata.RunError)
	require.Len(t, data.Events, 1)
	require.Equal(t, eventExit, data.Events[0].Kind)
}

// TestSessionFinalizeReportsWriteAndHookErrors verifies that a write failure
// and hook restoration failure are both returned.
func TestSessionFinalizeReportsWriteAndHookErrors(t *testing.T) {
	recorder := &sessionRecorder{
		started:  time.Now().Add(-time.Second),
		filePath: t.TempDir(),
		metadata: sessionMetadata{
			Args:           []string{testLoopArg, testOutArg},
			Env:            map[string]string{},
			Version:        testVersion,
			ClockStartUnix: testClockStart,
		},
		hooksStarted: true,
		stdoutUnhook: func() error {
			return errors.New("stdout restore failed")
		},
	}

	err := recorder.Finalize(nil)
	require.ErrorContains(t, err, "is a directory")
	require.ErrorContains(t, err, "stdout restore failed")
}

// TestSessionFinalizeReportsDeferredMarshalError verifies that marshal
// failures encountered while recording are reported from Finalize while the
// session file is still written to disk.
func TestSessionFinalizeReportsDeferredMarshalError(t *testing.T) {
	recorder := &sessionRecorder{
		started:  time.Now().Add(-time.Second),
		filePath: filepath.Join(t.TempDir(), "session.json"),
		metadata: sessionMetadata{
			Args:           []string{testLoopArg, testOutArg},
			Env:            map[string]string{},
			Version:        testVersion,
			ClockStartUnix: testClockStart,
		},
	}

	// This logEvent will fail, because chan int is not JSON marshallable.
	recorder.logEvent(eventStdout, make(chan int))

	err := recorder.Finalize(nil)
	require.ErrorContains(t, err, "marshal session stdout event")
	require.ErrorContains(t, err, "unsupported type: chan int")

	blob, err := os.ReadFile(recorder.filePath)
	require.NoError(t, err)

	var data sessionFile
	require.NoError(t, json.Unmarshal(blob, &data))
	require.Equal(t, recorder.metadata.Args, data.Metadata.Args)
	require.Equal(t, recorder.metadata.Version, data.Metadata.Version)
	require.NotNil(t, data.Metadata.Duration)
	require.Nil(t, data.Metadata.RunError)
	require.Len(t, data.Events, 1)
	require.Equal(t, eventExit, data.Events[0].Kind)
}

// TestNewSessionRecorderRequiresRepoRoot verifies that recording fails clearly
// when the session fixture directory is not available from the current working
// directory.
func TestNewSessionRecorderRequiresRepoRoot(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv(sessionEnvVar, "true")

	recorder, err := newSessionRecorder([]string{testLoopArg, testOutArg})
	require.Nil(t, recorder)
	require.EqualError(t, err, "cmd/loop/testdata/sessions does not "+
		"exist; run session recording from the repository root")
}

// TestNewSessionRecorderCapturesClockStart verifies that new recordings store
// the clock start used for deterministic replay.
func TestNewSessionRecorderCapturesClockStart(t *testing.T) {
	repoRoot := t.TempDir()
	require.NoError(
		t, os.MkdirAll(filepath.Join(repoRoot, sessionDefaultDir), 0o755),
	)
	t.Chdir(repoRoot)
	t.Setenv(sessionEnvVar, "true")

	before := time.Now().Unix()
	recorder, err := newSessionRecorder([]string{testLoopArg, testOutArg})
	after := time.Now().Unix()
	require.NoError(t, err)
	require.NotNil(t, recorder)
	require.GreaterOrEqual(t, recorder.metadata.ClockStartUnix, before)
	require.LessOrEqual(t, recorder.metadata.ClockStartUnix, after)
	require.Equal(
		t, time.Unix(recorder.metadata.ClockStartUnix, 0),
		recorder.ClockStart(),
	)
}
