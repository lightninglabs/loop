package loop

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
)

var (
	alice    = route.Vertex{1}
	bob      = route.Vertex{2}
	charlie  = route.Vertex{3}
	dave     = route.Vertex{4}
	eugene   = route.Vertex{5}
	loopNode = route.Vertex{99}

	privFrank, _ = btcec.NewPrivateKey()
	frankPubKey  = privFrank.PubKey()
	frank        = route.NewVertex(frankPubKey)

	privGeorge, _ = btcec.NewPrivateKey()
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

// TestLowHighRoutingPlugin tests that the low-high routing plugin does indeed
// gradually change MC settings in favour of more expensive inbound channels
// towards the Loop server.
func TestLowHighRoutingPlugin(t *testing.T) {
	target := loopNode
	amt := btcutil.Amount(50)
	testTime := time.Now().UTC()

	tests := []struct {
		name                        string
		channels                    []testChan
		routeHints                  [][]zpay32.HopHint
		initError                   error
		missionControlState         [][]lndclient.MissionControlEntry
		restoredMissionControlState []lndclient.MissionControlEntry
	}{
		{
			name: "degenerate network 1",
			//
			// Alice --- Loop
			//
			channels: []testChan{
				{alice, loopNode, 1, 1000, 1000, 1, 1000, 1},
			},
			initError: ErrRoutingPluginNotApplicable,
			missionControlState: [][]lndclient.MissionControlEntry{
				// The original MC state we start with.
				{
					{
						NodeFrom:    alice,
						NodeTo:      loopNode,
						SuccessTime: testTime,
						SuccessAmt:  10000,
					},
				},
			},
		},
		{
			name: "degenerate network 2",
			//
			//  Alice --- Bob --- Loop
			//
			channels: []testChan{
				// Alice - Bob
				{alice, bob, 1, 1000, 1000, 1, 1000, 1},
				// Bob - Loop
				{bob, loopNode, 2, 1000, 1000, 1, 1000, 1},
			},
			initError: ErrRoutingPluginNotApplicable,
			missionControlState: [][]lndclient.MissionControlEntry{
				// The original MC state we start with.
				{
					{},
				},
			},
		},
		{
			name: "degenrate network 3",
			//
			//       _____Bob_____
			//      /             \
			// Alice               Dave---Loop
			//      \___
			//          Charlie
			//
			channels: []testChan{
				{alice, bob, 1, 1000, 1000, 1, 1000, 1},
				{alice, charlie, 2, 1000, 1000, 1, 1000, 1},
				// Bob - Dave (cheap)
				{bob, dave, 3, 1000, 1000, 1, 1000, 1},
				{dave, loopNode, 5, 1000, 1000, 1, 1000, 1},
			},
			initError: ErrRoutingPluginNotApplicable,
			missionControlState: [][]lndclient.MissionControlEntry{
				// The original MC state we start with.
				{
					{
						NodeFrom:    bob,
						NodeTo:      dave,
						FailTime:    time.Time{},
						FailAmt:     0,
						SuccessTime: testTime,
						SuccessAmt:  10000,
					},
				},
			},
			restoredMissionControlState: []lndclient.MissionControlEntry{
				{
					NodeFrom:    bob,
					NodeTo:      dave,
					FailTime:    time.Time{},
					FailAmt:     0,
					SuccessTime: testTime,
					SuccessAmt:  10000,
				},
			},
		},
		{ // nolint: dupl
			name: "fork before loop node 1",
			//
			//       _____Bob_____
			//      /             \
			// Alice               Dave---Loop
			//      \___       ___/
			//          Charlie
			//
			channels: []testChan{
				{alice, bob, 1, 1000, 1000, 1, 1000, 1},
				{alice, charlie, 2, 1000, 1000, 1, 1000, 1},
				// Bob - Dave (cheap)
				{bob, dave, 3, 1000, 1000, 1, 1000, 1},
				// Charlie - Dave (expensive)
				{charlie, dave, 4, 1000, 1000, 100, 1000, 1},
				{dave, loopNode, 5, 1000, 1000, 1, 1000, 1},
			},
			initError: nil,
			missionControlState: [][]lndclient.MissionControlEntry{
				// The original MC state we start with.
				{
					{
						NodeFrom:    bob,
						NodeTo:      dave,
						FailTime:    time.Time{},
						FailAmt:     0,
						SuccessTime: testTime,
						SuccessAmt:  10000,
					},
				},
				// MC state set on the second attempt.
				{
					// Discourage Bob - Dave
					{
						NodeFrom: bob,
						NodeTo:   dave,
						FailTime: testTime,
						FailAmt:  1,
					},
					// Encourage Charlie - Dave
					{
						NodeFrom:    charlie,
						NodeTo:      dave,
						SuccessTime: testTime,
						SuccessAmt:  1000000,
					},
				},
			},
			restoredMissionControlState: []lndclient.MissionControlEntry{
				{
					NodeFrom:    bob,
					NodeTo:      dave,
					FailTime:    time.Time{},
					FailAmt:     0,
					SuccessTime: testTime,
					SuccessAmt:  10000,
				},
				{
					NodeFrom:    charlie,
					NodeTo:      dave,
					FailTime:    testTime,
					FailAmt:     1000001,
					SuccessTime: testTime,
					SuccessAmt:  1000000,
				},
			},
		},
		{ // nolint: dupl
			name: "fork before loop node 1 with equal inbound fees",
			//
			//       _____Bob_____
			//      /             \
			// Alice               Dave---Loop
			//      \___       ___/
			//          Charlie
			//
			channels: []testChan{
				{alice, bob, 1, 999, 1000, 1, 1000, 1},
				{alice, charlie, 2, 9999, 1000, 1, 1000, 1},
				// Bob - Dave (expensive)
				{bob, dave, 3, 999, 1000, 100, 1000, 1},
				// Charlie - Dave (expensive)
				{charlie, dave, 4, 999, 1000, 100, 1000, 1},
				{dave, loopNode, 5, 999, 1000, 1, 1000, 1},
			},
			initError: nil,
			missionControlState: [][]lndclient.MissionControlEntry{
				// The original MC state we start with.
				{
					{
						NodeFrom:    dave,
						NodeTo:      loopNode,
						SuccessTime: testTime,
						SuccessAmt:  10000,
					},
				},
				// MC state on the second attempt encourages
				// both inbound peers to make sure we do try
				// to route through both.
				{
					{
						NodeFrom:    dave,
						NodeTo:      loopNode,
						SuccessTime: testTime,
						SuccessAmt:  10000,
					},
					{
						NodeFrom:    bob,
						NodeTo:      dave,
						SuccessTime: testTime,
						SuccessAmt:  999000,
					},
					{
						NodeFrom:    charlie,
						NodeTo:      dave,
						SuccessTime: testTime,
						SuccessAmt:  999000,
					},
				},
			},
			restoredMissionControlState: []lndclient.MissionControlEntry{
				{
					NodeFrom:    dave,
					NodeTo:      loopNode,
					SuccessTime: testTime,
					SuccessAmt:  10000,
				},
				{
					NodeFrom:    bob,
					NodeTo:      dave,
					SuccessTime: testTime,
					SuccessAmt:  999000,
					FailTime:    testTime,
					FailAmt:     999001,
				},
				{
					NodeFrom:    charlie,
					NodeTo:      dave,
					SuccessTime: testTime,
					SuccessAmt:  999000,
					FailTime:    testTime,
					FailAmt:     999001,
				},
			},
		},
		{
			name: "fork before loop node 2",
			//
			//       _____Bob_____
			//      /             \
			// Alice               Eugene---Frank---George---Loop
			//     |\___       ___//
			//     |    Charlie   /
			//      \            /
			//       \___    ___/
			//           Dave
			//
			channels: []testChan{
				{alice, bob, 1, 1000, 1000, 1, 1000, 1},
				{alice, charlie, 2, 1000, 1000, 1, 1000, 1},
				{alice, dave, 3, 1000, 1000, 1, 1000, 1},
				// Bob - Eugene (cheap)
				{bob, eugene, 4, 1000, 1000, 1, 1000, 1},
				// Charlie - Eugene (more expensive)
				{charlie, eugene, 5, 1000, 1000, 2, 1000, 1},
				// Dave - Eugene (most expensive)
				{dave, eugene, 6, 1000, 1001, 2, 1000, 1},
				{eugene, frank, 7, 1000, 1000, 1, 1000, 1},
			},
			// Private channels: Frank - George - Loop
			routeHints: [][]zpay32.HopHint{{
				{
					NodeID:                    frankPubKey,
					ChannelID:                 8,
					FeeBaseMSat:               1000,
					FeeProportionalMillionths: 1,
				},
				{
					NodeID:                    georgePubKey,
					ChannelID:                 9,
					FeeBaseMSat:               1000,
					FeeProportionalMillionths: 1,
				},
			}},
			initError: nil,
			missionControlState: [][]lndclient.MissionControlEntry{
				// The original MC state we start with.
				{
					{
						NodeFrom:    charlie,
						NodeTo:      eugene,
						SuccessTime: testTime,
						SuccessAmt:  10000,
					},
				},
				// MC state set on the second attempt.
				{
					// Discourage Bob - Eugene
					{
						NodeFrom: bob,
						NodeTo:   eugene,
						FailTime: testTime,
						FailAmt:  1,
					},
					// Encourage Charlie - Eugene
					{
						NodeFrom:    charlie,
						NodeTo:      eugene,
						SuccessTime: testTime,
						SuccessAmt:  1000000,
					},
					// Encourage Dave - Eugene
					{
						NodeFrom:    dave,
						NodeTo:      eugene,
						SuccessTime: testTime,
						SuccessAmt:  1000000,
					},
				},
				// MC state set on the third attempt.
				{
					// Discourage Bob - Eugene
					{
						NodeFrom: bob,
						NodeTo:   eugene,
						FailTime: testTime,
						FailAmt:  1,
					},
					// Discourage Charlie - Eugene
					{
						NodeFrom: charlie,
						NodeTo:   eugene,
						FailTime: testTime,
						FailAmt:  1,
					},
					// Encourage Dave - Eugene
					{
						NodeFrom:    dave,
						NodeTo:      eugene,
						SuccessTime: testTime,
						SuccessAmt:  1000000,
					},
				},
			},
			restoredMissionControlState: []lndclient.MissionControlEntry{
				{
					NodeFrom:    bob,
					NodeTo:      eugene,
					FailTime:    testTime,
					FailAmt:     1000001,
					SuccessTime: testTime,
					SuccessAmt:  1000000,
				},
				{
					NodeFrom:    charlie,
					NodeTo:      eugene,
					FailTime:    time.Time{},
					FailAmt:     0,
					SuccessTime: testTime,
					SuccessAmt:  10000,
				},
				{
					NodeFrom:    dave,
					NodeTo:      eugene,
					FailTime:    testTime,
					FailAmt:     1000001,
					SuccessTime: testTime,
					SuccessAmt:  1000000,
				},
			},
		},
		{
			name: "fork before loop node 3",
			//
			//       _____Bob_____
			//      /             \
			// Alice               Eugene---Frank---George---Loop
			//     |\___       ___/                /
			//     |    Charlie                   /
			//      \                            /
			//       \___    ___________________/
			//           Dave
			//
			channels: []testChan{
				// Alice - Bob
				{alice, bob, 1, 1000, 1000, 1, 1000, 1},
				// Alice - Charlie
				{alice, charlie, 2, 1000, 1000, 1, 1000, 1},
				// Alice - Dave
				{alice, dave, 3, 1000, 1000, 1, 1000, 1},
				// Bob - Eugene
				{bob, eugene, 4, 1000, 1000, 1, 1000, 1},
				// Charlie - Eugene
				{charlie, eugene, 5, 1000, 1000, 2, 1000, 1},
				// Dave - George (expensive)
				{dave, george, 6, 1000, 1001, 2, 1000, 1},
				// Eugene - Frank
				{eugene, frank, 7, 1000, 1000, 1, 1000, 1},
				// Frank - George (cheap)
				{frank, george, 8, 1000, 1000, 1, 1000, 1},
				// George - Loop
				{george, loopNode, 9, 1000, 1000, 1, 1000, 1},
			},
			initError: nil,
			missionControlState: [][]lndclient.MissionControlEntry{
				// The original MC state we start with.
				{
					{
						NodeFrom:    charlie,
						NodeTo:      eugene,
						SuccessTime: testTime,
						SuccessAmt:  10000,
					},
				},
				// MC state set on the second attempt.
				{
					{
						NodeFrom:    charlie,
						NodeTo:      eugene,
						SuccessTime: testTime,
						SuccessAmt:  10000,
					},
					// Discourage Frank - George
					{
						NodeFrom: frank,
						NodeTo:   george,
						FailTime: testTime,
						FailAmt:  1,
					},
					// Encourage Dave - George
					{
						NodeFrom:    dave,
						NodeTo:      george,
						SuccessTime: testTime,
						SuccessAmt:  1000000,
					},
				},
			},
			restoredMissionControlState: []lndclient.MissionControlEntry{
				{
					NodeFrom:    charlie,
					NodeTo:      eugene,
					SuccessTime: testTime,
					SuccessAmt:  10000,
				},
				{
					NodeFrom:    frank,
					NodeTo:      george,
					FailTime:    testTime,
					FailAmt:     1000001,
					SuccessTime: testTime,
					SuccessAmt:  1000000,
				},
				{
					NodeFrom:    dave,
					NodeTo:      george,
					FailTime:    testTime,
					FailAmt:     1000001,
					SuccessTime: testTime,
					SuccessAmt:  1000000,
				},
			},
		},
	}

	for _, tc := range tests {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			mockLnd := test.NewMockLnd()

			mockLnd.Channels, mockLnd.ChannelEdges =
				makeTestNetwork(tc.channels)

			lnd := lndclient.LndServices{
				Client: mockLnd.Client,
				Router: mockLnd.Router,
			}

			testClock := clock.NewTestClock(testTime)
			plugin := makeRoutingPlugin(
				RoutingPluginLowHigh, lnd, testClock,
			)
			require.NotNil(t, plugin)

			// Set start state for MC.
			mockLnd.MissionControlState = tc.missionControlState[0]

			// Initialize the routing plugin.
			require.Equal(
				t, tc.initError,
				plugin.Init(
					context.TODO(), target, tc.routeHints,
					amt,
				),
			)

			if tc.initError != nil {
				// Make sure that MC state is untouched.
				require.Equal(
					t, tc.missionControlState[0],
					mockLnd.MissionControlState,
				)

				return
			}

			maxAttempts := len(tc.missionControlState)
			for i, expectedState := range tc.missionControlState {
				// Check that after each step, MC state is what
				// we expect it to be.
				require.NoError(
					t, plugin.BeforePayment(
						context.TODO(),
						i+1, maxAttempts,
					),
				)

				require.ElementsMatch(
					t, expectedState,
					mockLnd.MissionControlState,
				)
			}

			// Make sure we covered all inbound channels.
			require.Error(
				t, ErrRoutingPluginNoMoreRetries,
				plugin.BeforePayment(
					context.TODO(), maxAttempts, maxAttempts,
				),
			)

			// Deinitialize the routing plugin.
			require.NoError(t, plugin.Done(context.TODO()))

			// Make sure that MC state is reset after Done() is
			// called.
			require.ElementsMatch(
				t, tc.restoredMissionControlState,
				mockLnd.MissionControlState,
			)
		})
	}
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

	// RoutingPluginNone returns nil.
	plugin2, err := AcquireRoutingPlugin(
		ctx, RoutingPluginNone, lnd, target, nil, amt,
	)
	require.Nil(t, plugin2)
	require.NoError(t, err)

	// Acquire is successful.
	plugin, err = AcquireRoutingPlugin(
		ctx, RoutingPluginLowHigh, lnd, target, nil, amt,
	)
	require.NotNil(t, plugin)
	require.NoError(t, err)

	// Plugin already acquired, above.
	plugin2, err = AcquireRoutingPlugin(
		ctx, RoutingPluginLowHigh, lnd, target, nil, amt,
	)
	require.Nil(t, plugin2)
	require.NoError(t, err)

	// Release acruired plugin.
	ReleaseRoutingPlugin(ctx)

	// Acquire is successful.
	plugin2, err = AcquireRoutingPlugin(
		ctx, RoutingPluginLowHigh, lnd, target, nil, amt,
	)
	require.NotNil(t, plugin2)
	require.NoError(t, err)
}
