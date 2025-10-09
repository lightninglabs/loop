package liquidity

import (
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// TestEasyAutoloopExcludedPeers ensures that peers listed in
// Parameters.EasyAutoloopExcludedPeers are not selected by
// pickEasyAutoloopChannel even if they would otherwise be preferred.
func TestEasyAutoloopExcludedPeers(t *testing.T) {
	// Two channels, peer1 has the higher local balance and would be picked
	// if not excluded.
	ch1 := lndclient.ChannelInfo{
		Active:        true,
		ChannelID:     lnwire.NewShortChanIDFromInt(11).ToUint64(),
		PubKeyBytes:   peer1,
		LocalBalance:  90000,
		RemoteBalance: 0,
		Capacity:      100000,
	}
	ch2 := lndclient.ChannelInfo{
		Active:        true,
		ChannelID:     lnwire.NewShortChanIDFromInt(22).ToUint64(),
		PubKeyBytes:   peer2,
		LocalBalance:  80000,
		RemoteBalance: 0,
		Capacity:      100000,
	}

	params := defaultParameters
	params.Autoloop = true
	params.EasyAutoloop = true
	params.EasyAutoloopTarget = 80000
	params.ClientRestrictions.Minimum = btcutil.Amount(1)
	params.ClientRestrictions.Maximum = btcutil.Amount(10000)
	// Exclude peer1, even though its channel has more local balance.
	params.EasyAutoloopExcludedPeers = []route.Vertex{peer1}

	c := newAutoloopTestCtx(
		t, params, []lndclient.ChannelInfo{ch1, ch2}, testRestrictions,
	)

	// Picking a channel should not pick the excluded peer's channel.
	picked := c.manager.pickEasyAutoloopChannel(
		[]lndclient.ChannelInfo{ch1, ch2}, &params.ClientRestrictions,
		nil, nil, 1,
	)
	require.NotNil(t, picked)
	require.Equal(
		t, ch2.ChannelID, picked.ChannelID,
		"should pick non-excluded peer's channel",
	)
}

// TestEasyAutoloopIncludeAllPeers simulates the --includealleasypeers flag by
// clearing the exclusion list and ensuring a previously excluded peer can be
// selected again.
func TestEasyAutoloopIncludeAllPeers(t *testing.T) {
	ch1 := lndclient.ChannelInfo{
		Active:        true,
		ChannelID:     lnwire.NewShortChanIDFromInt(33).ToUint64(),
		PubKeyBytes:   peer1,
		LocalBalance:  90000,
		RemoteBalance: 0,
		Capacity:      100000,
	}
	ch2 := lndclient.ChannelInfo{
		Active:        true,
		ChannelID:     lnwire.NewShortChanIDFromInt(44).ToUint64(),
		PubKeyBytes:   peer2,
		LocalBalance:  80000,
		RemoteBalance: 0,
		Capacity:      100000,
	}

	params := defaultParameters
	params.Autoloop = true
	params.EasyAutoloop = true
	params.EasyAutoloopTarget = 80000
	params.ClientRestrictions.Minimum = btcutil.Amount(1)
	params.ClientRestrictions.Maximum = btcutil.Amount(10000)
	params.EasyAutoloopExcludedPeers = []route.Vertex{peer1}

	c := newAutoloopTestCtx(
		t, params, []lndclient.ChannelInfo{ch1, ch2}, testRestrictions,
	)

	// With exclusion active, peer1 should not be picked.
	picked := c.manager.pickEasyAutoloopChannel(
		[]lndclient.ChannelInfo{ch1, ch2}, &params.ClientRestrictions,
		nil, nil, 1,
	)
	require.NotNil(t, picked)
	require.Equal(t, ch2.ChannelID, picked.ChannelID)

	// Simulate --includealleasypeers by clearing the exclusion list as the
	// CLI does before sending to the server.
	c.manager.params.EasyAutoloopExcludedPeers = nil

	picked = c.manager.pickEasyAutoloopChannel(
		[]lndclient.ChannelInfo{ch1, ch2}, &params.ClientRestrictions,
		nil, nil, 1,
	)
	require.NotNil(t, picked)
	require.Equal(
		t, ch1.ChannelID, picked.ChannelID,
		"after include-all, highest local balance should win again",
	)
}
