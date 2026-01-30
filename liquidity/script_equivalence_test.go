package liquidity

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/liquidity/script"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// EasyAutoloopStarlarkScript is a Starlark script that replicates easy-autoloop.
// Target: 100000 sats (embedded in script).
const EasyAutoloopStarlarkScript = `
# Easy autoloop equivalent script in Starlark
# Target: 100000 sats

def autoloop():
    target = 100000

    # Check if we're at or below target - no swap needed
    if total_local <= target:
        return []

    # Calculate amount to loop out (clamped to max)
    amount = min(total_local - target, restrictions.max_loop_out)
    if amount < restrictions.min_loop_out:
        return []

    # Filter eligible channels
    eligible = [
        c for c in channels
        if c.active
        and not c.is_custom_channel
        and not c.has_loop_out_swap
        and not c.recently_failed
        and c.local_balance >= restrictions.min_loop_out
    ]

    if len(eligible) == 0:
        return []

    # Sort by local balance descending, pick best
    eligible = sorted(eligible, key=lambda c: -c.local_balance)
    best = eligible[0]

    # Create swap decision
    swap_amount = min(best.local_balance, amount)
    return [loop_out(swap_amount, [best.channel_id])]

# Execute and store result.
decisions = autoloop()
`

// TestStarlarkEquivalentToEasyAutoloop validates that a Starlark script can
// produce the same swap decisions as the easy autoloop mode.
func TestStarlarkEquivalentToEasyAutoloop(t *testing.T) {
	testCases := []struct {
		name         string
		channels     []lndclient.ChannelInfo
		target       btcutil.Amount
		minSwap      btcutil.Amount
		maxSwap      btcutil.Amount
		expectSwap   bool
		expectAmount btcutil.Amount
		expectChanID uint64
	}{
		{
			name: "total below target - no swap",
			channels: []lndclient.ChannelInfo{
				{
					ChannelID:    1,
					PubKeyBytes:  route.Vertex{1},
					Capacity:     100000,
					LocalBalance: 40000,
					Active:       true,
				},
				{
					ChannelID:    2,
					PubKeyBytes:  route.Vertex{2},
					Capacity:     100000,
					LocalBalance: 50000,
					Active:       true,
				},
			},
			target:     100000,
			minSwap:    10000,
			maxSwap:    1000000,
			expectSwap: false,
		},
		{
			name: "total above target - swap from highest balance channel",
			channels: []lndclient.ChannelInfo{
				{
					ChannelID:    1,
					PubKeyBytes:  route.Vertex{1},
					Capacity:     200000,
					LocalBalance: 80000,
					Active:       true,
				},
				{
					ChannelID:    2,
					PubKeyBytes:  route.Vertex{2},
					Capacity:     200000,
					LocalBalance: 120000,
					Active:       true,
				},
			},
			target:       100000,
			minSwap:      10000,
			maxSwap:      1000000,
			expectSwap:   true,
			expectAmount: 100000, // min(120000, 200000-100000) = 100000
			expectChanID: 2,      // Channel 2 has highest local balance
		},
		{
			name: "inactive channel skipped",
			channels: []lndclient.ChannelInfo{
				{
					ChannelID:    1,
					PubKeyBytes:  route.Vertex{1},
					Capacity:     200000,
					LocalBalance: 180000,
					Active:       false, // Inactive
				},
				{
					ChannelID:    2,
					PubKeyBytes:  route.Vertex{2},
					Capacity:     200000,
					LocalBalance: 120000,
					Active:       true,
				},
			},
			target:       100000,
			minSwap:      10000,
			maxSwap:      1000000,
			expectSwap:   true,
			expectAmount: 120000, // min(120000, 300000-100000) = 120000
			expectChanID: 2,      // Channel 1 is inactive, pick 2
		},
		{
			name: "amount below minimum - no swap",
			channels: []lndclient.ChannelInfo{
				{
					ChannelID:    1,
					PubKeyBytes:  route.Vertex{1},
					Capacity:     200000,
					LocalBalance: 105000,
					Active:       true,
				},
			},
			target:     100000,
			minSwap:    10000,
			maxSwap:    1000000,
			expectSwap: false, // 105000 - 100000 = 5000 < minSwap
		},
		{
			name: "multiple channels - picks highest balance",
			channels: []lndclient.ChannelInfo{
				{
					ChannelID:    1,
					PubKeyBytes:  route.Vertex{1},
					Capacity:     300000,
					LocalBalance: 100000,
					Active:       true,
				},
				{
					ChannelID:    2,
					PubKeyBytes:  route.Vertex{2},
					Capacity:     300000,
					LocalBalance: 250000,
					Active:       true,
				},
				{
					ChannelID:    3,
					PubKeyBytes:  route.Vertex{3},
					Capacity:     300000,
					LocalBalance: 150000,
					Active:       true,
				},
			},
			target:       100000,
			minSwap:      10000,
			maxSwap:      1000000,
			expectSwap:   true,
			expectAmount: 250000, // min(250000, 500000-100000) = 250000
			expectChanID: 2,      // Channel 2 has highest balance
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Build script context.
			scriptCtx := buildTestScriptContext(
				tc.channels, tc.minSwap, tc.maxSwap,
			)

			// Evaluate Starlark script.
			eval, err := script.NewEvaluator()
			require.NoError(t, err)

			decisions, err := eval.Evaluate(
				EasyAutoloopStarlarkScript, scriptCtx,
			)
			require.NoError(t, err)

			if !tc.expectSwap {
				require.Len(t, decisions, 0,
					"expected no swap but got %d decisions",
					len(decisions))
				return
			}

			require.Len(t, decisions, 1,
				"expected 1 swap but got %d decisions",
				len(decisions))

			d := decisions[0]
			require.Equal(t, script.SwapTypeLoopOut, d.Type)
			require.Equal(t, int64(tc.expectAmount), d.Amount,
				"amount mismatch")
			require.Len(t, d.ChannelIDs, 1)
			require.Equal(t, tc.expectChanID, d.ChannelIDs[0],
				"channel ID mismatch")
		})
	}
}

// buildTestScriptContext creates a script context for testing.
func buildTestScriptContext(channels []lndclient.ChannelInfo,
	minSwap, maxSwap btcutil.Amount) *script.AutoloopContext {

	var totalLocal, totalRemote, totalCapacity int64
	channelInfos := make([]script.ChannelInfo, len(channels))

	for i, ch := range channels {
		totalLocal += int64(ch.LocalBalance)
		totalRemote += int64(ch.RemoteBalance)
		totalCapacity += int64(ch.Capacity)

		var localPercent, remotePercent float64
		if ch.Capacity > 0 {
			localPercent = float64(ch.LocalBalance) / float64(ch.Capacity) * 100
			remotePercent = float64(ch.RemoteBalance) / float64(ch.Capacity) * 100
		}

		channelInfos[i] = script.ChannelInfo{
			ChannelID:       ch.ChannelID,
			PeerPubkey:      hex.EncodeToString(ch.PubKeyBytes[:]),
			Capacity:        int64(ch.Capacity),
			LocalBalance:    int64(ch.LocalBalance),
			RemoteBalance:   int64(ch.RemoteBalance),
			Active:          ch.Active,
			Private:         ch.Private,
			LocalPercent:    localPercent,
			RemotePercent:   remotePercent,
			HasLoopOutSwap:  false,
			HasLoopInSwap:   false,
			RecentlyFailed:  false,
			IsCustomChannel: false,
		}
	}

	return &script.AutoloopContext{
		Channels:      channelInfos,
		Peers:         []script.PeerInfo{},
		TotalLocal:    totalLocal,
		TotalRemote:   totalRemote,
		TotalCapacity: totalCapacity,
		Restrictions: script.SwapRestrictions{
			MinLoopOut: int64(minSwap),
			MaxLoopOut: int64(maxSwap),
			MinLoopIn:  int64(minSwap),
			MaxLoopIn:  int64(maxSwap),
		},
		Budget: script.BudgetInfo{
			TotalBudget:     1000000,
			RemainingAmount: 1000000,
		},
		InFlight: script.InFlightInfo{
			MaxAllowed: 5,
		},
		CurrentTime: time.Now(),
	}
}

// TestScriptableAutoloopIntegration tests the full scriptable autoloop flow.
func TestScriptableAutoloopIntegration(t *testing.T) {
	ctx := context.Background()
	testClock := clock.NewTestClock(time.Now())

	// Create test channels.
	peer1Bytes := [33]byte{2, 1}
	peer2Bytes := [33]byte{2, 2}

	testChannels := []lndclient.ChannelInfo{
		{
			ChannelID:    123,
			PubKeyBytes:  peer1Bytes,
			Capacity:     500000,
			LocalBalance: 300000, // Higher balance
			Active:       true,
		},
		{
			ChannelID:    456,
			PubKeyBytes:  peer2Bytes,
			Capacity:     500000,
			LocalBalance: 200000,
			Active:       true,
		},
	}

	// Create mock config.
	lnd := test.NewMockLnd()
	lnd.Channels = testChannels

	cfg := &Config{
		Lnd:   &lnd.LndServices,
		Clock: testClock,
		ListLoopOut: func(context.Context) ([]*loopdb.LoopOut, error) {
			return nil, nil
		},
		ListLoopIn: func(context.Context) ([]*loopdb.LoopIn, error) {
			return nil, nil
		},
		Restrictions: func(ctx context.Context, swapType swap.Type,
			initiator string) (*Restrictions, error) {

			return &Restrictions{
				Minimum: 10000,
				Maximum: 1000000,
			}, nil
		},
		LoopOutQuote: func(ctx context.Context,
			req *loop.LoopOutQuoteRequest) (*loop.LoopOutQuote, error) {

			return &loop.LoopOutQuote{
				SwapFee:      100,
				PrepayAmount: 1000,
				MinerFee:     500,
			}, nil
		},
	}

	manager := NewManager(cfg)

	// Set scriptable autoloop parameters.
	manager.params.ScriptableAutoloop = true
	manager.params.ScriptableScript = EasyAutoloopStarlarkScript
	manager.params.AutoFeeBudget = 1000000
	manager.params.MaxAutoInFlight = 5

	// Build script context (this is what scriptableAutoLoop does internally).
	scriptCtx, err := manager.buildScriptContext(ctx)
	require.NoError(t, err)

	// Verify context was built correctly.
	require.Equal(t, int64(500000), scriptCtx.TotalLocal)
	require.Len(t, scriptCtx.Channels, 2)
	require.Equal(t, int64(10000), scriptCtx.Restrictions.MinLoopOut)
	require.Equal(t, int64(1000000), scriptCtx.Restrictions.MaxLoopOut)

	// Evaluate the Starlark script.
	eval, err := script.NewEvaluator()
	require.NoError(t, err)

	decisions, err := eval.Evaluate(EasyAutoloopStarlarkScript, scriptCtx)
	require.NoError(t, err)

	// Should have one swap decision since total local (500000) > target (100000).
	require.Len(t, decisions, 1)
	require.Equal(t, script.SwapTypeLoopOut, decisions[0].Type)
	// Amount should be min(300000, 500000-100000) = 300000.
	require.Equal(t, int64(300000), decisions[0].Amount)
	// Should pick channel 123 (highest local balance).
	require.Equal(t, []uint64{123}, decisions[0].ChannelIDs)
}

// TestScriptableParameterValidation tests parameter validation for
// scriptable mode.
func TestScriptableParameterValidation(t *testing.T) {
	testCases := []struct {
		name        string
		params      Parameters
		expectError string
	}{
		{
			name: "scriptable without script",
			params: Parameters{
				ScriptableAutoloop: true,
				ScriptableScript:   "",
			},
			expectError: "scriptable_script is required",
		},
		{
			name: "scriptable with easy autoloop",
			params: Parameters{
				ScriptableAutoloop: true,
				ScriptableScript:   "decisions = []",
				EasyAutoloop:       true,
			},
			expectError: "mutually exclusive",
		},
		{
			name: "scriptable with channel rules",
			params: Parameters{
				ScriptableAutoloop: true,
				ScriptableScript:   "decisions = []",
				ChannelRules: map[lnwire.ShortChannelID]*SwapRule{
					lnwire.NewShortChanIDFromInt(123): {
						ThresholdRule: &ThresholdRule{},
					},
				},
			},
			expectError: "cannot be used with channel/peer rules",
		},
		{
			name: "valid scriptable params",
			params: Parameters{
				ScriptableAutoloop: true,
				ScriptableScript:   "decisions = []",
				SweepConfTarget:    6,
				HtlcConfTarget:     6,
				MaxAutoInFlight:    1,
				FeeLimit:           defaultFeePortion(),
			},
			expectError: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.params.validate(1, nil, &Restrictions{})
			if tc.expectError != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.expectError)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestStarlarkAdvancedScript tests more advanced Starlark features.
func TestStarlarkAdvancedScript(t *testing.T) {
	// Test a script that uses multiple functions and loop in.
	advancedScript := `
def get_low_inbound_peers():
    """Find peers with less than 30% remote balance."""
    return [
        p for p in peers
        if p.remote_percent < 30 and not p.has_loop_in_swap
    ]

def autoloop():
    # First, handle loop outs for high local balance
    loop_out_decisions = []
    high_local = [c for c in channels if c.local_percent > 70 and c.active]
    for c in high_local[:1]:  # Limit to 1
        amt = min(c.local_balance // 2, restrictions.max_loop_out)
        if amt >= restrictions.min_loop_out:
            loop_out_decisions.append(loop_out(amt, [c.channel_id]))

    # Then, handle loop ins for low inbound peers
    loop_in_decisions = []
    low_inbound = get_low_inbound_peers()
    for p in low_inbound[:1]:  # Limit to 1
        amt = min(p.total_capacity // 4, restrictions.max_loop_in)
        if amt >= restrictions.min_loop_in:
            loop_in_decisions.append(loop_in(amt, p.pubkey))

    return loop_out_decisions + loop_in_decisions

decisions = autoloop()
`

	channelInfos := []script.ChannelInfo{
		{
			ChannelID:     1,
			PeerPubkey:    "02aabbcc",
			Capacity:      1000000,
			LocalBalance:  800000, // 80% local
			RemoteBalance: 200000,
			Active:        true,
			LocalPercent:  80,
			RemotePercent: 20,
		},
	}

	peerInfos := []script.PeerInfo{
		{
			Pubkey:        "02aabbcc",
			TotalCapacity: 1000000,
			TotalLocal:    800000,
			TotalRemote:   200000,
			ChannelCount:  1,
			ChannelIDs:    []uint64{1},
			LocalPercent:  80,
			RemotePercent: 20,
			HasLoopInSwap: false,
		},
	}

	ctx := &script.AutoloopContext{
		Channels:      channelInfos,
		Peers:         peerInfos,
		TotalLocal:    800000,
		TotalRemote:   200000,
		TotalCapacity: 1000000,
		Restrictions: script.SwapRestrictions{
			MinLoopOut: 10000,
			MaxLoopOut: 500000,
			MinLoopIn:  10000,
			MaxLoopIn:  500000,
		},
		Budget: script.BudgetInfo{
			TotalBudget:     1000000,
			RemainingAmount: 1000000,
		},
		InFlight: script.InFlightInfo{
			MaxAllowed: 5,
		},
		CurrentTime: time.Now(),
	}

	eval, err := script.NewEvaluator()
	require.NoError(t, err)

	decisions, err := eval.Evaluate(advancedScript, ctx)
	require.NoError(t, err)

	// Should have 1 loop out (high local balance) and 1 loop in (low remote).
	require.Len(t, decisions, 2)

	// First decision should be loop out.
	require.Equal(t, script.SwapTypeLoopOut, decisions[0].Type)
	require.Equal(t, int64(400000), decisions[0].Amount) // 800000 / 2

	// Second decision should be loop in.
	require.Equal(t, script.SwapTypeLoopIn, decisions[1].Type)
	require.Equal(t, int64(250000), decisions[1].Amount) // 1000000 / 4
	require.Equal(t, "02aabbcc", decisions[1].PeerPubkey)
}
