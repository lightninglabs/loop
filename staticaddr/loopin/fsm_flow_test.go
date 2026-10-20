package loopin

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBitcoinTransitionCompatibility(t *testing.T) {
	// This fixture freezes the complete transition table from before
	// Bitcoin and asset loop-ins shared the protocol graph package.
	want, err := os.ReadFile("testdata/bitcoin-v0-transitions.json")
	require.NoError(t, err)
	actual := make(map[string]map[string]string)
	for state, definition := range (&FSM{}).LoopInStatesV0() {
		require.NotNil(t, definition.Action)
		events := make(map[string]string)
		for event, next := range definition.Transitions {
			events[string(event)] = string(next)
		}
		actual[string(state)] = events
	}
	got, err := json.Marshal(actual)
	require.NoError(t, err)
	require.JSONEq(t, string(want), string(got))
}
