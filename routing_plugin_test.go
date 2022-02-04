package loop

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

var (
	alice    = route.Vertex{1}
	bob      = route.Vertex{2}
	charlie  = route.Vertex{3}
	dave     = route.Vertex{4}
	eugene   = route.Vertex{5}
	loopNode = route.Vertex{99}

	privFrank, _ = btcec.NewPrivateKey(btcec.S256())
	frankPubKey  = privFrank.PubKey()
	frank        = route.NewVertex(frankPubKey)

	privGeorge, _ = btcec.NewPrivateKey(btcec.S256())
	georgePubKey  = privGeorge.PubKey()
	george        = route.NewVertex(georgePubKey)
)

// testChan holds simplified test data for channels.
type testChan struct {
	nodeID1  route.Vertex
	nodeID2  route.Vertex
	chanID   uint64
	capacity int64
	feeBase1 int64
	feeRate1 int64
	feeBase2 int64
	feeRate2 int64
}

// makeTestNetwork is a helper creating mocked network data from test inputs.
func makeTestNetwork(channels []testChan) ([]lndclient.ChannelInfo,
	map[uint64]*lndclient.ChannelEdge) {

	chanInfos := make([]lndclient.ChannelInfo, len(channels))
	edges := make(map[uint64]*lndclient.ChannelEdge, len(channels))
	for i, ch := range channels {
		chanInfos[i] = lndclient.ChannelInfo{
			ChannelID: ch.chanID,
		}

		edges[ch.chanID] = &lndclient.ChannelEdge{
			ChannelID: ch.chanID,
			Capacity:  btcutil.Amount(ch.capacity),
			Node1:     ch.nodeID1,
			Node2:     ch.nodeID2,
			Node1Policy: &lndclient.RoutingPolicy{
				FeeBaseMsat:      ch.feeBase1,
				FeeRateMilliMsat: ch.feeRate1,
			},
			Node2Policy: &lndclient.RoutingPolicy{
				FeeBaseMsat:      ch.feeBase2,
				FeeRateMilliMsat: ch.feeRate2,
			},
		}
	}

	return chanInfos, edges
}

func TestRoutingPluginAcquireRelease(t *testing.T) {
	mockLnd := test.NewMockLnd()

	//       _____Bob_____
	//      /             \
	// Alice               Dave---Loop
	//      \___       ___/
	//          Charlie
	//
	channels := []testChan{
		{alice, bob, 1, 1000, 1000, 1, 1000, 1},
		{alice, charlie, 2, 1000, 1000, 1, 1000, 1},
		{bob, dave, 3, 1000, 1000, 1, 1000, 1},
		{charlie, dave, 4, 1000, 1000, 100, 1000, 1},
		{dave, loopNode, 5, 1000, 1000, 1, 1000, 1},
	}

	mockLnd.Channels, mockLnd.ChannelEdges = makeTestNetwork(channels)
	lnd := lndclient.LndServices{
		Client: mockLnd.Client,
		Router: mockLnd.Router,
	}

	target := loopNode
	amt := btcutil.Amount(50)
	ctx := context.TODO()

	// RoutingPluginNone returns nil.
	plugin, err := AcquireRoutingPlugin(
		ctx, RoutingPluginNone, lnd, target, nil, amt,
	)
	require.Nil(t, plugin)
	require.NoError(t, err)

	// Attempting to acquire RoutingPluginNone again still returns nil.
	plugin, err = AcquireRoutingPlugin(
		ctx, RoutingPluginNone, lnd, target, nil, amt,
	)
	require.Nil(t, plugin)
	require.NoError(t, err)

	// Call ReleaseRoutingPlugin twice to ensure we can call it even when no
	// plugin is acquired.
	ReleaseRoutingPlugin(ctx)
	ReleaseRoutingPlugin(ctx)
}
