package loop

import (
	"context"
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/lndclient"
	mock_lnd "github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
)

var (
	chanID0 = lnwire.NewShortChanIDFromInt(10)
	chanID1 = lnwire.NewShortChanIDFromInt(11)
	chanID2 = lnwire.NewShortChanIDFromInt(12)
	chanID3 = lnwire.NewShortChanIDFromInt(13)
	chanID4 = lnwire.NewShortChanIDFromInt(14)
	chanID5 = lnwire.NewShortChanIDFromInt(15)

	// To generate a nodeID we'll have to perform a few steps.
	//
	//      Step 1: We generate the corresponding Y value to an
	//              arbitrary X value on the secp25k1 curve. This
	//              is done outside this function, and the output
	//              is converted to string with big.Int.Text(16)
	//              and converted to []bytes. btcec.decompressPoint
	//              is used here to generate this Y value
	//
	//      Step 2: Construct a btcec.PublicKey object with the
	//              aforementioned values
	//
	//      Step 3: Convert the pubkey to a Vertex by passing a
	//              compressed pubkey. This compression looses the
	//              Y value as it can be inferred.
	//
	// The Vertex object mainly contains the X value information,
	// and has the underlying []bytes type. We generate the Y
	// value information ourselves as that is returned in the
	// hophints, and we must ensure it's accuracy

	// Generate origin NodeID
	originYBytes, _ = hex.DecodeString(
		"bde70df51939b94c9c24979fa7dd04ebd9b" +
			"3572da7802290438af2a681895441",
	)
	pubKeyOrigin = &btcec.PublicKey{
		X:     big.NewInt(0),
		Y:     new(big.Int).SetBytes(originYBytes),
		Curve: btcec.S256(),
	}
	origin, _ = route.NewVertexFromBytes(pubKeyOrigin.SerializeCompressed())

	// Generate peer1 NodeID
	pubKey1YBytes, _ = hex.DecodeString(
		"598ec453728e0ffe0ae2f5e174243cf58f2" +
			"a3f2c83d2457b43036db568b11093",
	)
	pubKeyPeer1 = &btcec.PublicKey{
		X:     big.NewInt(4),
		Y:     new(big.Int).SetBytes(pubKey1YBytes),
		Curve: btcec.S256(),
	}
	peer1, _ = route.NewVertexFromBytes(pubKeyPeer1.SerializeCompressed())

	// Generate peer2 NodeID
	pubKey2YBytes, _ = hex.DecodeString(
		"bde70df51939b94c9c24979fa7dd04ebd" +
			"9b3572da7802290438af2a681895441",
	)
	pubKeyPeer2 = &btcec.PublicKey{
		X:     big.NewInt(1),
		Y:     new(big.Int).SetBytes(pubKey2YBytes),
		Curve: btcec.S256(),
	}
	peer2, _ = route.NewVertexFromBytes(pubKeyPeer2.SerializeCompressed())

	capacity = btcutil.Amount(10000)

	// Channel0 is a public active channel with peer1 with 10k remote
	// capacity.
	channel0 = lndclient.ChannelInfo{
		Active:        true,
		Private:       false,
		ChannelID:     chanID0.ToUint64(),
		PubKeyBytes:   peer1,
		LocalBalance:  0,
		RemoteBalance: capacity,
		Capacity:      capacity,
	}
	channelEdge0 = lndclient.ChannelEdge{
		ChannelID: chanID0.ToUint64(),
		ChannelPoint: "b121f1d368b8f60648970bc36b37e7b9700d" +
			"ed098c60b027e42e9c648e297501:0",
		Capacity: capacity,
		Node1:    peer1,
		Node2:    origin,
		Node1Policy: &lndclient.RoutingPolicy{
			FeeBaseMsat:      0,
			FeeRateMilliMsat: 0,
			TimeLockDelta:    140,
		},
		Node2Policy: &lndclient.RoutingPolicy{
			FeeBaseMsat:      0,
			FeeRateMilliMsat: 0,
			TimeLockDelta:    140,
		},
	}

	// Channel1 is a private active channel with peer1 with 10k remote
	// capacity.
	channel1 = lndclient.ChannelInfo{
		Active:        true,
		Private:       true,
		ChannelID:     chanID1.ToUint64(),
		PubKeyBytes:   peer1,
		LocalBalance:  0,
		RemoteBalance: capacity,
		Capacity:      capacity,
	}
	channelEdge1 = lndclient.ChannelEdge{
		ChannelID: chanID1.ToUint64(),
		ChannelPoint: "b121f1d368b8f60648970bc36b37e7b9700d" +
			"ed098c60b027e42e9c648e297502:0",
		Capacity: capacity,
		Node1:    peer1,
		Node2:    origin,
		Node1Policy: &lndclient.RoutingPolicy{
			FeeBaseMsat:      1,
			FeeRateMilliMsat: 1,
			TimeLockDelta:    141,
		},
		Node2Policy: &lndclient.RoutingPolicy{
			FeeBaseMsat:      0,
			FeeRateMilliMsat: 0,
			TimeLockDelta:    144,
		},
	}

	// Channel2 is a private active channel with peer2 with 10k remote
	// capacity.
	channel2 = lndclient.ChannelInfo{
		Active:        true,
		Private:       true,
		ChannelID:     chanID2.ToUint64(),
		PubKeyBytes:   peer2,
		LocalBalance:  0,
		RemoteBalance: capacity,
		Capacity:      capacity,
	}
	channelEdge2 = lndclient.ChannelEdge{
		ChannelID: chanID2.ToUint64(),
		ChannelPoint: "b121f1d368b8f60648970bc36b37e7b9700d" +
			"ed098c60b027e42e9c648e297502:0",
		Capacity: capacity,
		Node1:    origin,
		Node2:    peer2,
		Node1Policy: &lndclient.RoutingPolicy{
			FeeBaseMsat:      0,
			FeeRateMilliMsat: 0,
			TimeLockDelta:    144,
		},
		Node2Policy: &lndclient.RoutingPolicy{
			FeeBaseMsat:      2,
			FeeRateMilliMsat: 2,
			TimeLockDelta:    142,
		},
	}

	// Channel3 is a private inactive channel with peer2 with 0 remote
	// capacity.
	channel3 = lndclient.ChannelInfo{
		Active:        false,
		Private:       true,
		ChannelID:     chanID3.ToUint64(),
		PubKeyBytes:   peer2,
		LocalBalance:  capacity,
		RemoteBalance: 0,
		Capacity:      capacity,
	}
	channelEdge3 = lndclient.ChannelEdge{
		ChannelID: chanID3.ToUint64(),
		ChannelPoint: "b121f1d368b8f60648970bc36b37e7b9700d" +
			"ed098c60b027e42e9c648e297502:0",
		Capacity: capacity,
		Node1:    peer2,
		Node2:    origin,
		Node1Policy: &lndclient.RoutingPolicy{
			FeeBaseMsat:      3,
			FeeRateMilliMsat: 3,
			TimeLockDelta:    143,
		},
		Node2Policy: &lndclient.RoutingPolicy{
			FeeBaseMsat:      0,
			FeeRateMilliMsat: 0,
			TimeLockDelta:    144,
		},
	}

	// Channel4 is a private active channel with peer2 with 5k remote
	// capacity.
	channel4 = lndclient.ChannelInfo{
		Active:        true,
		Private:       true,
		ChannelID:     chanID4.ToUint64(),
		PubKeyBytes:   peer2,
		LocalBalance:  capacity / 2,
		RemoteBalance: capacity / 2,
		Capacity:      capacity,
	}
	channelEdge4 = lndclient.ChannelEdge{
		ChannelID: chanID4.ToUint64(),
		ChannelPoint: "6fe4408bba52c0a0ee15365e107105de" +
			"fabfc70c497556af69351c4cfbc167b:0",
		Capacity: capacity,
		Node1:    origin,
		Node2:    peer2,
		Node1Policy: &lndclient.RoutingPolicy{
			FeeBaseMsat:      0,
			FeeRateMilliMsat: 0,
			TimeLockDelta:    144,
		},
		Node2Policy: &lndclient.RoutingPolicy{
			FeeBaseMsat:      4,
			FeeRateMilliMsat: 4,
			TimeLockDelta:    144,
		},
	}

	// Channel5 is a public active channel with peer2 with 5k remote
	// capacity. It is useful to make peer2 public (give the testcase).
	channel5 = lndclient.ChannelInfo{
		Active:        true,
		Private:       false,
		ChannelID:     chanID5.ToUint64(),
		PubKeyBytes:   peer2,
		LocalBalance:  capacity / 2,
		RemoteBalance: capacity / 2,
		Capacity:      capacity,
	}
	channelEdge5 = lndclient.ChannelEdge{
		ChannelID: chanID5.ToUint64(),
		ChannelPoint: "abcde52c0a0ee15365e107105de" +
			"fabfc70c497556af69351c4cfbc167b:0",
		Capacity: capacity,
		Node1:    origin,
		Node2:    peer2,
		Node1Policy: &lndclient.RoutingPolicy{
			FeeBaseMsat:      0,
			FeeRateMilliMsat: 0,
			TimeLockDelta:    144,
		},
		Node2Policy: &lndclient.RoutingPolicy{
			FeeBaseMsat:      5,
			FeeRateMilliMsat: 5,
			TimeLockDelta:    145,
		},
	}
)

func TestSelectHopHints(t *testing.T) {
	tests := []struct {
		name             string
		channels         []lndclient.ChannelInfo
		channelEdges     map[uint64]*lndclient.ChannelEdge
		expectedHopHints [][]zpay32.HopHint
		amtMSat          btcutil.Amount
		numMaxHophints   int
		includeNodes     map[route.Vertex]struct{}
		expectedError    error
	}{
		// Chan2: private, active, remote balance OK
		// Chan3: private, !active
		// Chan4: private, active, small remote balance (second round).
		// Chan5: public, active => makes peer2 public.
		{
			name: "2 out of 4 selected",
			channels: []lndclient.ChannelInfo{
				channel2,
				channel3,
				channel4,
				channel5,
			},
			channelEdges: map[uint64]*lndclient.ChannelEdge{
				channel2.ChannelID: &channelEdge2,
				channel3.ChannelID: &channelEdge3,
				channel4.ChannelID: &channelEdge4,
				channel5.ChannelID: &channelEdge5,
			},
			expectedHopHints: [][]zpay32.HopHint{
				{{
					NodeID:                    pubKeyPeer2,
					ChannelID:                 channel2.ChannelID,
					FeeBaseMSat:               2,
					FeeProportionalMillionths: 2,
					CLTVExpiryDelta:           142,
				}},
				{{
					NodeID:                    pubKeyPeer2,
					ChannelID:                 channel4.ChannelID,
					FeeBaseMSat:               4,
					FeeProportionalMillionths: 4,
					CLTVExpiryDelta:           144,
				}},
			},
			amtMSat:        capacity,
			numMaxHophints: 20,
			includeNodes:   make(map[route.Vertex]struct{}),
			expectedError:  nil,
		},
		// A variation of the above test case but nodes are filtered so
		// we only add channel 1 (as channel 0 is public making peer1
		// public).
		{
			name: "1 out of 6 selected",
			channels: []lndclient.ChannelInfo{
				channel0,
				channel1,
				channel2,
				channel3,
				channel4,
				channel5,
			},
			channelEdges: map[uint64]*lndclient.ChannelEdge{
				channel0.ChannelID: &channelEdge0,
				channel1.ChannelID: &channelEdge1,
				channel2.ChannelID: &channelEdge2,
				channel3.ChannelID: &channelEdge3,
				channel4.ChannelID: &channelEdge4,
				channel5.ChannelID: &channelEdge5,
			},
			expectedHopHints: [][]zpay32.HopHint{
				{{
					NodeID:                    pubKeyPeer1,
					ChannelID:                 channel1.ChannelID,
					FeeBaseMSat:               1,
					FeeProportionalMillionths: 1,
					CLTVExpiryDelta:           141,
				}},
			},
			amtMSat:        capacity,
			numMaxHophints: 20,
			includeNodes: map[route.Vertex]struct{}{
				peer1: {},
			},
			expectedError: nil,
		},

		// Chan1: private, active, remote balance OK => node not public
		{
			name: "1 private chan",
			channels: []lndclient.ChannelInfo{
				channel1,
			},
			channelEdges: map[uint64]*lndclient.ChannelEdge{
				channel1.ChannelID: &channelEdge1,
			},
			expectedHopHints: [][]zpay32.HopHint{},
			amtMSat:          capacity,
			numMaxHophints:   20,
			includeNodes:     make(map[route.Vertex]struct{}),
			expectedError:    nil,
		},
		// Chan4: private, active, small remote balance (second round).
		// Chan5: public, active => makes peer2 public.
		{
			name: "1 out of 2 selected",
			channels: []lndclient.ChannelInfo{
				channel4,
				channel5,
			},
			channelEdges: map[uint64]*lndclient.ChannelEdge{
				channel4.ChannelID: &channelEdge4,
				channel5.ChannelID: &channelEdge5,
			},
			expectedHopHints: [][]zpay32.HopHint{
				{{
					NodeID:                    pubKeyPeer2,
					ChannelID:                 channel4.ChannelID,
					FeeBaseMSat:               4,
					FeeProportionalMillionths: 4,
					CLTVExpiryDelta:           144,
				}},
			},
			amtMSat:        capacity,
			numMaxHophints: 20,
			includeNodes:   make(map[route.Vertex]struct{}),
			expectedError:  nil,
		},
	}
	for _, test := range tests {
		test := test
		ctx := context.Background()

		lnd := mock_lnd.NewMockLnd()
		lnd.Channels = test.channels
		lnd.ChannelEdges = test.channelEdges
		t.Run(test.name, func(t *testing.T) {
			hopHints, err := SelectHopHints(
				ctx, &lnd.LndServices, test.amtMSat,
				test.numMaxHophints, test.includeNodes,
			)
			require.Equal(t, test.expectedError, err)
			require.Equal(t, test.expectedHopHints, hopHints)
		})
	}

}
