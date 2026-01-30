package script

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestNewEvaluator tests that an evaluator can be created.
func TestNewEvaluator(t *testing.T) {
	eval, err := NewEvaluator()
	require.NoError(t, err)
	require.NotNil(t, eval)
}

// TestEvalEmptyList tests evaluating a script that returns an empty list.
func TestEvalEmptyList(t *testing.T) {
	eval, err := NewEvaluator()
	require.NoError(t, err)

	script := `decisions = []`

	ctx := &AutoloopContext{
		Channels:    []ChannelInfo{},
		Peers:       []PeerInfo{},
		CurrentTime: time.Now(),
	}

	decisions, err := eval.Evaluate(script, ctx)
	require.NoError(t, err)
	require.Len(t, decisions, 0)
}

// TestEvalWithChannelData tests evaluating a script with channel data.
func TestEvalWithChannelData(t *testing.T) {
	eval, err := NewEvaluator()
	require.NoError(t, err)

	// Script that returns a loop out if total local > 100000.
	// Note: Starlark requires if statements to be in functions.
	script := `
def autoloop():
    if total_local > 100000:
        amount = min(total_local - 100000, restrictions.max_loop_out)
        return [loop_out(amount, [channels[0].channel_id])]
    return []

decisions = autoloop()
`

	ctx := &AutoloopContext{
		Channels: []ChannelInfo{
			{
				ChannelID:    123456,
				LocalBalance: 150000,
				Active:       true,
			},
		},
		TotalLocal: 150000,
		Restrictions: SwapRestrictions{
			MinLoopOut: 10000,
			MaxLoopOut: 1000000,
		},
		CurrentTime: time.Now(),
	}

	decisions, err := eval.Evaluate(script, ctx)
	require.NoError(t, err)
	require.Len(t, decisions, 1)
	require.Equal(t, SwapTypeLoopOut, decisions[0].Type)
	require.Equal(t, int64(50000), decisions[0].Amount)
	require.Equal(t, []uint64{123456}, decisions[0].ChannelIDs)
}

// TestEvalNoSwapNeeded tests when no swap is needed.
func TestEvalNoSwapNeeded(t *testing.T) {
	eval, err := NewEvaluator()
	require.NoError(t, err)

	script := `
def autoloop():
    if total_local > 100000:
        amount = min(total_local - 100000, restrictions.max_loop_out)
        return [loop_out(amount, [channels[0].channel_id])]
    return []

decisions = autoloop()
`

	ctx := &AutoloopContext{
		Channels: []ChannelInfo{
			{
				ChannelID:    123456,
				LocalBalance: 50000,
				Active:       true,
			},
		},
		TotalLocal: 50000,
		Restrictions: SwapRestrictions{
			MinLoopOut: 10000,
			MaxLoopOut: 1000000,
		},
		CurrentTime: time.Now(),
	}

	decisions, err := eval.Evaluate(script, ctx)
	require.NoError(t, err)
	require.Len(t, decisions, 0)
}

// TestLoopOutBuiltin tests the loop_out helper function.
func TestLoopOutBuiltin(t *testing.T) {
	eval, err := NewEvaluator()
	require.NoError(t, err)

	script := `decisions = [loop_out(50000, [123])]`

	ctx := &AutoloopContext{
		Channels: []ChannelInfo{
			{ChannelID: 123},
		},
		Restrictions: SwapRestrictions{
			MinLoopOut: 10000,
			MaxLoopOut: 1000000,
		},
		CurrentTime: time.Now(),
	}

	decisions, err := eval.Evaluate(script, ctx)
	require.NoError(t, err)
	require.Len(t, decisions, 1)
	require.Equal(t, SwapTypeLoopOut, decisions[0].Type)
	require.Equal(t, int64(50000), decisions[0].Amount)
	require.Equal(t, []uint64{123}, decisions[0].ChannelIDs)
}

// TestLoopInBuiltin tests the loop_in helper function.
func TestLoopInBuiltin(t *testing.T) {
	eval, err := NewEvaluator()
	require.NoError(t, err)

	script := `decisions = [loop_in(50000, "02abc123")]`

	ctx := &AutoloopContext{
		Peers: []PeerInfo{
			{Pubkey: "02abc123"},
		},
		Restrictions: SwapRestrictions{
			MinLoopIn: 10000,
			MaxLoopIn: 1000000,
		},
		CurrentTime: time.Now(),
	}

	decisions, err := eval.Evaluate(script, ctx)
	require.NoError(t, err)
	require.Len(t, decisions, 1)
	require.Equal(t, SwapTypeLoopIn, decisions[0].Type)
	require.Equal(t, int64(50000), decisions[0].Amount)
	require.Equal(t, "02abc123", decisions[0].PeerPubkey)
}

// TestChannelFiltering tests filtering channels with list comprehension.
func TestChannelFiltering(t *testing.T) {
	eval, err := NewEvaluator()
	require.NoError(t, err)

	script := `
def autoloop():
    eligible = [c for c in channels if c.active and not c.is_custom_channel]
    if len(eligible) > 0:
        return [loop_out(50000, [eligible[0].channel_id])]
    return []

decisions = autoloop()
`

	ctx := &AutoloopContext{
		Channels: []ChannelInfo{
			{
				ChannelID:       1,
				Active:          false,
				IsCustomChannel: false,
			},
			{
				ChannelID:       2,
				Active:          true,
				IsCustomChannel: true,
			},
			{
				ChannelID:       3,
				Active:          true,
				IsCustomChannel: false,
			},
		},
		Restrictions: SwapRestrictions{
			MinLoopOut: 10000,
			MaxLoopOut: 1000000,
		},
		CurrentTime: time.Now(),
	}

	decisions, err := eval.Evaluate(script, ctx)
	require.NoError(t, err)
	require.Len(t, decisions, 1)
	require.Equal(t, []uint64{3}, decisions[0].ChannelIDs)
}

// TestChannelSorting tests sorting channels by local balance.
func TestChannelSorting(t *testing.T) {
	eval, err := NewEvaluator()
	require.NoError(t, err)

	script := `
# Sort by local_balance descending
eligible = sorted(channels, key=lambda c: -c.local_balance)
decisions = [loop_out(50000, [eligible[0].channel_id])]
`

	ctx := &AutoloopContext{
		Channels: []ChannelInfo{
			{ChannelID: 1, LocalBalance: 100000},
			{ChannelID: 2, LocalBalance: 300000},
			{ChannelID: 3, LocalBalance: 200000},
		},
		Restrictions: SwapRestrictions{
			MinLoopOut: 10000,
			MaxLoopOut: 1000000,
		},
		CurrentTime: time.Now(),
	}

	decisions, err := eval.Evaluate(script, ctx)
	require.NoError(t, err)
	require.Len(t, decisions, 1)
	// Should pick channel 2 (highest local balance).
	require.Equal(t, []uint64{2}, decisions[0].ChannelIDs)
}

// TestValidateDecisions tests decision validation.
func TestValidateDecisions(t *testing.T) {
	ctx := &AutoloopContext{
		Channels: []ChannelInfo{
			{ChannelID: 123},
		},
		Peers: []PeerInfo{
			{Pubkey: "02abc"},
		},
		Restrictions: SwapRestrictions{
			MinLoopOut: 10000,
			MaxLoopOut: 1000000,
			MinLoopIn:  10000,
			MaxLoopIn:  1000000,
		},
	}

	tests := []struct {
		name      string
		decision  SwapDecision
		wantError bool
	}{
		{
			name: "valid loop out",
			decision: SwapDecision{
				Type:       SwapTypeLoopOut,
				Amount:     50000,
				ChannelIDs: []uint64{123},
			},
			wantError: false,
		},
		{
			name: "valid loop in",
			decision: SwapDecision{
				Type:       SwapTypeLoopIn,
				Amount:     50000,
				PeerPubkey: "02abc",
			},
			wantError: false,
		},
		{
			name: "invalid type",
			decision: SwapDecision{
				Type:   "invalid",
				Amount: 50000,
			},
			wantError: true,
		},
		{
			name: "amount too low",
			decision: SwapDecision{
				Type:       SwapTypeLoopOut,
				Amount:     100,
				ChannelIDs: []uint64{123},
			},
			wantError: true,
		},
		{
			name: "channel not found",
			decision: SwapDecision{
				Type:       SwapTypeLoopOut,
				Amount:     50000,
				ChannelIDs: []uint64{999},
			},
			wantError: true,
		},
		{
			name: "peer not found",
			decision: SwapDecision{
				Type:       SwapTypeLoopIn,
				Amount:     50000,
				PeerPubkey: "unknown",
			},
			wantError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateDecisions([]SwapDecision{tc.decision}, ctx)
			if tc.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestSortByPriority tests priority sorting.
func TestSortByPriority(t *testing.T) {
	decisions := []SwapDecision{
		{Type: SwapTypeLoopOut, Priority: 1},
		{Type: SwapTypeLoopIn, Priority: 3},
		{Type: SwapTypeLoopOut, Priority: 2},
	}

	SortByPriority(decisions)

	require.Equal(t, 3, decisions[0].Priority)
	require.Equal(t, 2, decisions[1].Priority)
	require.Equal(t, 1, decisions[2].Priority)
}

// TestDefFunction tests defining and calling a function.
func TestDefFunction(t *testing.T) {
	eval, err := NewEvaluator()
	require.NoError(t, err)

	script := `
def autoloop():
    if total_local > 100000:
        return [loop_out(total_local - 100000, [channels[0].channel_id])]
    return []

decisions = autoloop()
`

	ctx := &AutoloopContext{
		Channels: []ChannelInfo{
			{ChannelID: 123, LocalBalance: 150000},
		},
		TotalLocal: 150000,
		Restrictions: SwapRestrictions{
			MinLoopOut: 10000,
			MaxLoopOut: 1000000,
		},
		CurrentTime: time.Now(),
	}

	decisions, err := eval.Evaluate(script, ctx)
	require.NoError(t, err)
	require.Len(t, decisions, 1)
	require.Equal(t, int64(50000), decisions[0].Amount)
}

// TestAccessRestrictions tests accessing restrictions fields.
func TestAccessRestrictions(t *testing.T) {
	eval, err := NewEvaluator()
	require.NoError(t, err)

	// Note: Starlark doesn't support chained comparisons like Python.
	// Use explicit and instead.
	script := `
def autoloop():
    if restrictions.min_loop_out <= 50000 and 50000 <= restrictions.max_loop_out:
        return [loop_out(50000, [channels[0].channel_id])]
    return []

decisions = autoloop()
`

	ctx := &AutoloopContext{
		Channels: []ChannelInfo{
			{ChannelID: 123},
		},
		Restrictions: SwapRestrictions{
			MinLoopOut: 10000,
			MaxLoopOut: 1000000,
		},
		CurrentTime: time.Now(),
	}

	decisions, err := eval.Evaluate(script, ctx)
	require.NoError(t, err)
	require.Len(t, decisions, 1)
}

// TestInvalidScript tests that invalid scripts return errors.
func TestInvalidScript(t *testing.T) {
	eval, err := NewEvaluator()
	require.NoError(t, err)

	script := `this is not valid starlark`

	ctx := &AutoloopContext{CurrentTime: time.Now()}
	_, err = eval.Evaluate(script, ctx)
	require.Error(t, err)
}

// TestValidateScript tests script validation.
func TestValidateScript(t *testing.T) {
	tests := []struct {
		name      string
		script    string
		wantError bool
	}{
		{
			name:      "valid script",
			script:    `decisions = []`,
			wantError: false,
		},
		{
			name:      "invalid syntax",
			script:    `this is not valid`,
			wantError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateScript(tc.script)
			if tc.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
