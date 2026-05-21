package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// updateRecordedSessionsEnvVar enables replay bless mode for text-only fixture
// updates.
const updateRecordedSessionsEnvVar = "LOOP_UPDATE_RECORDED_SESSIONS"

// replayedSessionOutput captures the user-visible output produced by an
// offline session replay.
type replayedSessionOutput struct {
	stdout       string
	stderr       string
	stdoutChunks []string
	stderrChunks []string
	runError     *string
}

// updateRecordedSessionsEnabled reports whether replay should bless text-only
// fixture updates.
func updateRecordedSessionsEnabled() (bool, error) {
	raw, ok := os.LookupEnv(updateRecordedSessionsEnvVar)
	if !ok {
		return false, nil
	}

	enabled, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("invalid %s value %q",
			updateRecordedSessionsEnvVar, raw)
	}

	return enabled, nil
}

// sessionFixturePath resolves a relative fixture path under
// cmd/loop/testdata/sessions to an absolute path.
func sessionFixturePath(rel string) (string, error) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("locate session fixture dir")
	}

	loopDir := filepath.Dir(filename)

	return filepath.Join(
		loopDir, "testdata", "sessions", filepath.FromSlash(rel),
	), nil
}

// loadSessionFilePath reads and decodes a session fixture from disk.
func loadSessionFilePath(path string) (sessionFile, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return sessionFile{}, err
	}

	var fixture sessionFile
	if err := json.Unmarshal(blob, &fixture); err != nil {
		return sessionFile{}, err
	}

	return fixture, nil
}

// writeSessionFilePath writes a session fixture using the recorder's JSON
// formatting.
func writeSessionFilePath(path string, fixture sessionFile) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")

	return encoder.Encode(fixture)
}

// maybeUpdateSessionFixture rewrites the text-only portions of a recorded
// session if the replay output changed.
func maybeUpdateSessionFixture(path string, fixture sessionFile,
	output replayedSessionOutput) (bool, error) {

	updated, changed, err := rewriteSessionFixture(fixture, output)
	if err != nil || !changed {
		return changed, err
	}

	if err := writeSessionFilePath(path, updated); err != nil {
		return false, err
	}

	return true, nil
}

// rewriteSessionFixture rewrites stdout, stderr and run_error while leaving
// the rest of the recorded interaction unchanged.
func rewriteSessionFixture(fixture sessionFile, output replayedSessionOutput) (
	sessionFile, bool, error) {

	updated := fixture
	changed := false

	if !optionalTextEqual(updated.Metadata.RunError, output.runError) {
		updated.Metadata.RunError = cloneOptionalString(output.runError)
		changed = true
	}

	var err error
	updated.Events, changed, err = rewriteExitEventRunError(
		updated.Events, output.runError, changed,
	)
	if err != nil {
		return sessionFile{}, false, err
	}

	updated.Events, changed, err = rewriteTextEvents(
		updated.Events, eventStdout, fixtureStdout(fixture),
		output.stdout, output.stdoutChunks, changed,
	)
	if err != nil {
		return sessionFile{}, false, err
	}

	updated.Events, changed, err = rewriteTextEvents(
		updated.Events, eventStderr, fixtureStderr(fixture),
		output.stderr, output.stderrChunks, changed,
	)
	if err != nil {
		return sessionFile{}, false, err
	}

	return updated, changed, nil
}

// rewriteExitEventRunError updates the exit payload to match the replayed run
// error.
func rewriteExitEventRunError(events []sessionEvent, runError *string,
	changed bool) ([]sessionEvent, bool, error) {

	data, err := json.Marshal(exitPayload{
		RunError: cloneOptionalString(runError),
	})
	if err != nil {
		return nil, changed, err
	}

	updated := append([]sessionEvent(nil), events...)

	for i, event := range slices.Backward(updated) {
		if event.Kind != eventExit {
			continue
		}

		if bytes.Equal(event.Data, data) {
			return updated, changed, nil
		}

		updated[i].Data = data

		return updated, true, nil
	}

	updated = append(updated, sessionEvent{
		Kind: eventExit,
		Data: data,
	})

	return updated, true, nil
}

// rewriteTextEvents updates one text stream when the normalized aggregate text
// changed.
func rewriteTextEvents(events []sessionEvent, kind, recorded, actual string,
	chunks []string, changed bool) ([]sessionEvent, bool, error) {

	normalizedEqual := normalizeTimestamps(recorded) ==
		normalizeTimestamps(actual)

	sourceChunks := chunks
	sourceCombined := actual
	if normalizedEqual {
		sourceChunks = fixtureTextChunksByKind(events, kind)
		sourceCombined = recorded
	}

	replacement, err := canonicalTextEvents(
		events, kind, sourceChunks, sourceCombined,
	)
	if err != nil {
		return nil, changed, err
	}

	if normalizedEqual && textEventsMatch(events, kind, replacement) {
		return events, changed, nil
	}

	updated, err := replaceTextEvents(events, kind, replacement)
	if err != nil {
		return nil, changed, err
	}

	return updated, true, nil
}

// canonicalTextEvents builds the canonical replacement events for one text
// stream.
func canonicalTextEvents(events []sessionEvent, kind string, chunks []string,
	combined string) ([]sessionEvent, error) {

	indices := textEventIndices(events, kind)
	replacement := replacementTextChunks(chunks, combined)
	if len(indices) != len(replacement) {
		replacement = replacementTextChunks(nil, combined)
	}

	return buildReplacementTextEvents(events, indices, kind, replacement)
}

// replaceTextEvents rewrites the recorded events for a text stream using the
// provided canonical replacement events.
func replaceTextEvents(events []sessionEvent, kind string,
	replacements []sessionEvent) ([]sessionEvent, error) {

	indices := textEventIndices(events, kind)
	if len(indices) == 0 && len(replacements) == 0 {
		return append([]sessionEvent(nil), events...), nil
	}

	if replacements == nil {
		replacements = []sessionEvent{}
	}

	return replaceTextEventsWithCanonical(
		events, kind, indices, replacements,
	)
}

// replaceTextEventsWithCanonical splices canonical replacement events into the
// event stream.
func replaceTextEventsWithCanonical(events []sessionEvent, kind string,
	indices []int, replacements []sessionEvent) ([]sessionEvent, error) {

	if len(indices) == len(replacements) {
		updated := append([]sessionEvent(nil), events...)
		for i, idx := range indices {
			updated[idx].Data = replacements[i].Data
		}

		return updated, nil
	}

	if len(indices) > 0 {
		firstIdx := indices[0]
		updated := make(
			[]sessionEvent, 0,
			len(events)-len(indices)+len(replacements),
		)

		inserted := false
		for i, event := range events {
			if event.Kind == kind {
				if !inserted && i == firstIdx {
					updated = append(
						updated, replacements...,
					)
					inserted = true
				}
				continue
			}

			updated = append(updated, event)
		}

		return updated, nil
	}

	if len(replacements) == 0 {
		return append([]sessionEvent(nil), events...), nil
	}

	insertAt := len(events)
	for i, event := range events {
		if event.Kind == eventExit {
			insertAt = i
			break
		}
	}

	updated := make([]sessionEvent, 0, len(events)+len(replacements))
	updated = append(updated, events[:insertAt]...)
	updated = append(updated, replacements...)
	updated = append(updated, events[insertAt:]...)

	return updated, nil
}

// textEventsMatch reports whether the existing events already use the canonical
// representation for a text stream.
func textEventsMatch(events []sessionEvent, kind string,
	replacements []sessionEvent) bool {

	indices := textEventIndices(events, kind)
	if len(indices) != len(replacements) {
		return false
	}

	for i, idx := range indices {
		if events[idx].TimeMS != replacements[i].TimeMS {
			return false
		}
		if !bytes.Equal(events[idx].Data, replacements[i].Data) {
			return false
		}
	}

	return true
}

// buildReplacementTextEvents creates replacement text events while reusing the
// closest recorded timestamps when possible.
func buildReplacementTextEvents(events []sessionEvent, indices []int,
	kind string, chunks []string) ([]sessionEvent, error) {

	if len(chunks) == 0 {
		return nil, nil
	}

	replacements := make([]sessionEvent, 0, len(chunks))
	for i, chunk := range chunks {
		data, err := json.Marshal(newTextPayload(chunk))
		if err != nil {
			return nil, err
		}

		replacements = append(replacements, sessionEvent{
			TimeMS: replacementEventTime(events, indices, i),
			Kind:   kind,
			Data:   data,
		})
	}

	return replacements, nil
}

// replacementEventTime picks a stable timestamp for a replacement text event.
func replacementEventTime(events []sessionEvent, indices []int, idx int) int64 {
	if len(indices) == 0 {
		return 0
	}

	if idx < len(indices) {
		return events[indices[idx]].TimeMS
	}

	return events[indices[len(indices)-1]].TimeMS
}

// textEventIndices returns the indexes of text events with the given kind.
func textEventIndices(events []sessionEvent, kind string) []int {
	var indices []int
	for i, event := range events {
		if event.Kind == kind {
			indices = append(indices, i)
		}
	}

	return indices
}

// replacementTextChunks returns the recorded write chunks to persist. If the
// hook callbacks did not observe any chunks, fall back to the aggregate text so
// the fixture still captures the visible output.
func replacementTextChunks(chunks []string, combined string) []string {
	if len(chunks) > 0 {
		return append([]string(nil), chunks...)
	}

	if combined == "" {
		return nil
	}

	return []string{combined}
}

// fixtureStdout returns the concatenated stdout text stored in a fixture.
func fixtureStdout(fixture sessionFile) string {
	return fixtureTextByKind(fixture.Events, eventStdout)
}

// fixtureStderr returns the concatenated stderr text stored in a fixture.
func fixtureStderr(fixture sessionFile) string {
	return fixtureTextByKind(fixture.Events, eventStderr)
}

// fixtureTextByKind concatenates all text payloads for a given event kind.
func fixtureTextByKind(events []sessionEvent, kind string) string {
	var out bytes.Buffer
	for _, text := range fixtureTextChunksByKind(events, kind) {
		out.WriteString(text)
	}

	return out.String()
}

// fixtureTextChunksByKind returns the decoded text for each event of a given
// kind.
func fixtureTextChunksByKind(events []sessionEvent, kind string) []string {
	var chunks []string
	for _, event := range events {
		if event.Kind != kind {
			continue
		}

		var payload textPayload
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			continue
		}

		chunks = append(chunks, payload.text())
	}

	return chunks
}

// optionalTextEqual compares optional text values using the same timestamp
// normalization rules as replay assertions.
func optionalTextEqual(a, b *string) bool {
	switch {
	case a == nil && b == nil:
		return true

	case a == nil || b == nil:
		return false
	}

	return normalizeTimestamps(*a) == normalizeTimestamps(*b)
}

// cloneOptionalString copies an optional string.
func cloneOptionalString(value *string) *string {
	if value == nil {
		return nil
	}

	cloned := *value

	return &cloned
}

// TestUpdateRecordedSessionsEnabled verifies update-mode env parsing.
func TestUpdateRecordedSessionsEnabled(t *testing.T) {
	if current, ok := os.LookupEnv(updateRecordedSessionsEnvVar); ok {
		t.Setenv(updateRecordedSessionsEnvVar, current)
	} else {
		t.Setenv(updateRecordedSessionsEnvVar, "false")
	}
	_ = os.Unsetenv(updateRecordedSessionsEnvVar)

	enabled, err := updateRecordedSessionsEnabled()
	require.NoError(t, err)
	require.False(t, enabled)

	t.Setenv(updateRecordedSessionsEnvVar, "true")
	enabled, err = updateRecordedSessionsEnabled()
	require.NoError(t, err)
	require.True(t, enabled)

	t.Setenv(updateRecordedSessionsEnvVar, "invalid")
	enabled, err = updateRecordedSessionsEnabled()
	require.False(t, enabled)
	require.EqualError(
		t, err,
		"invalid LOOP_UPDATE_RECORDED_SESSIONS value \"invalid\"",
	)
}

// TestRewriteSessionFixtureUpdatesMatchingChunks verifies that bless mode
// rewrites text and run errors in place when the write counts are unchanged.
func TestRewriteSessionFixtureUpdatesMatchingChunks(t *testing.T) {
	oldErr := "old error"
	newErr := "new error"

	fixture := sessionFile{
		Metadata: sessionMetadata{
			RunError: &oldErr,
		},
		Events: []sessionEvent{
			textEvent(t, 5, eventStdout, "old stdout\n"),
			textEvent(t, 6, eventStderr, "old stderr\n"),
			exitEvent(t, 7, &oldErr),
		},
	}

	updated, changed, err := rewriteSessionFixture(fixture,
		replayedSessionOutput{
			stdout:       "new stdout\n",
			stderr:       "new stderr\n",
			stdoutChunks: []string{"new stdout\n"},
			stderrChunks: []string{"new stderr\n"},
			runError:     &newErr,
		},
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, &newErr, updated.Metadata.RunError)
	require.Equal(t, int64(5), updated.Events[0].TimeMS)
	require.Equal(t, "new stdout\n", textEventText(t, updated.Events[0]))
	require.Equal(t, int64(6), updated.Events[1].TimeMS)
	require.Equal(t, "new stderr\n", textEventText(t, updated.Events[1]))
	require.Equal(t, &newErr, exitEventRunError(t, updated.Events[2]))
}

// TestRewriteSessionFixtureReplacesChangedChunkCount verifies that bless mode
// collapses mismatched write counts to one text event to keep fixture diffs
// small.
func TestRewriteSessionFixtureReplacesChangedChunkCount(t *testing.T) {
	fixture := sessionFile{
		Events: []sessionEvent{
			textEvent(t, 10, eventStdout, "old "),
			textEvent(t, 11, eventStdout, "text\n"),
			exitEvent(t, 12, nil),
		},
	}

	updated, changed, err := rewriteSessionFixture(fixture,
		replayedSessionOutput{
			stdout:       "new output\n",
			stdoutChunks: []string{"new ", "output", "\n"},
		},
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.Len(t, updated.Events, 2)
	require.Equal(t, "new output\n", textEventText(t, updated.Events[0]))
	require.Equal(t, int64(10), updated.Events[0].TimeMS)
	require.Equal(t, eventExit, updated.Events[1].Kind)
}

// TestRewriteSessionFixtureSkipsNormalizedTimestampNoise verifies that bless
// mode does not churn fixtures when only timestamp formatting differs.
func TestRewriteSessionFixtureSkipsNormalizedTimestampNoise(t *testing.T) {
	fixture := sessionFile{
		Events: []sessionEvent{
			textEvent(
				t, 3, eventStdout,
				"2026-05-04T10:00:00-05:00\n",
			),
			exitEvent(t, 4, nil),
		},
	}

	updated, changed, err := rewriteSessionFixture(fixture,
		replayedSessionOutput{
			stdout:       "2026-05-04T15:00:00Z\n",
			stdoutChunks: []string{"2026-05-04T15:00:00Z\n"},
		},
	)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, fixture, updated)
}

// textEvent builds a text session event for tests.
func textEvent(t *testing.T, timeMS int64, kind, text string) sessionEvent {
	t.Helper()

	data, err := json.Marshal(newTextPayload(text))
	require.NoError(t, err)

	return sessionEvent{
		TimeMS: timeMS,
		Kind:   kind,
		Data:   data,
	}
}

// exitEvent builds an exit session event for tests.
func exitEvent(t *testing.T, timeMS int64, runError *string) sessionEvent {
	t.Helper()

	data, err := json.Marshal(exitPayload{
		RunError: cloneOptionalString(runError),
	})
	require.NoError(t, err)

	return sessionEvent{
		TimeMS: timeMS,
		Kind:   eventExit,
		Data:   data,
	}
}

// textEventText decodes a text payload for assertions.
func textEventText(t *testing.T, event sessionEvent) string {
	t.Helper()

	var payload textPayload
	require.NoError(t, json.Unmarshal(event.Data, &payload))

	return payload.text()
}

// exitEventRunError decodes an exit payload for assertions.
func exitEventRunError(t *testing.T, event sessionEvent) *string {
	t.Helper()

	var payload exitPayload
	require.NoError(t, json.Unmarshal(event.Data, &payload))

	return payload.RunError
}
