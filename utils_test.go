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
	chanID1 = lnwire.NewShortChanIDFromInt(1)
	chanID2 = lnwire.NewShortChanIDFromInt(2)
	chanID3 = lnwire.NewShortChanIDFromInt(3)
	chanID4 = lnwire.NewShortChanIDFromInt(4)

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

	// Construct channel1 which will be returned my listChannels and
	// channelEdge1 which will be returned by getChanInfo
	chan1Capacity = btcutil.Amount(10000)
	channel1      = lndclient.ChannelInfo{
		Active:        true,
		ChannelID:     chanID1.ToUint64(),
		PubKeyBytes:   peer1,
		LocalBalance:  10000,
		RemoteBalance: 0,
		Capacity:      chan1Capacity,
	}
	channelEdge1 = lndclient.ChannelEdge{
		ChannelID: chanID1.ToUint64(),
		ChannelPoint: "b121f1d368b8f60648970bc36b37e7b9700d" +
			"ed098c60b027e42e9c648e297502:0",
		Capacity: chan1Capacity,
		Node1:    origin,
		Node2:    peer1,
		Node1Policy: &lndclient.RoutingPolicy{
			FeeBaseMsat:      0,
			FeeRateMilliMsat: 0,
			TimeLockDelta:    144,
		},
		Node2Policy: &lndclient.RoutingPolicy{
			FeeBaseMsat:      0,
			FeeRateMilliMsat: 0,
			TimeLockDelta:    144,
		},
	}

	// Construct channel2 which will be returned my listChannels and
	// channelEdge2 which will be returned by getChanInfo
	chan2Capacity = btcutil.Amount(10000)
	channel2      = lndclient.ChannelInfo{
		Active:        true,
		ChannelID:     chanID1.ToUint64(),
		PubKeyBytes:   peer2,
		LocalBalance:  0,
		RemoteBalance: 10000,
		Capacity:      chan1Capacity,
	}
	channelEdge2 = lndclient.ChannelEdge{
		ChannelID: chanID2.ToUint64(),
		ChannelPoint: "b121f1d368b8f60648970bc36b37e7b9700d" +
			"ed098c60b027e42e9c648e297502:0",
		Capacity: chan2Capacity,
		Node1:    origin,
		Node2:    peer2,
		Node1Policy: &lndclient.RoutingPolicy{
			FeeBaseMsat:      0,
			FeeRateMilliMsat: 0,
			TimeLockDelta:    144,
		},
		Node2Policy: &lndclient.RoutingPolicy{
			FeeBaseMsat:      0,
			FeeRateMilliMsat: 0,
			TimeLockDelta:    144,
		},
	}

	// Construct channel3 which will be returned my listChannels and
	// channelEdge3 which will be returned by getChanInfo
	chan3Capacity = btcutil.Amount(10000)
	channel3      = lndclient.ChannelInfo{
		Active:        true,
		ChannelID:     chanID3.ToUint64(),
		PubKeyBytes:   peer2,
		LocalBalance:  10000,
		RemoteBalance: 0,
		Capacity:      chan1Capacity,
	}
	channelEdge3 = lndclient.ChannelEdge{
		ChannelID: chanID3.ToUint64(),
		ChannelPoint: "b121f1d368b8f60648970bc36b37e7b9700d" +
			"ed098c60b027e42e9c648e297502:0",
		Capacity: chan3Capacity,
		Node1:    origin,
		Node2:    peer2,
		Node1Policy: &lndclient.RoutingPolicy{
			FeeBaseMsat:      0,
			FeeRateMilliMsat: 0,
			TimeLockDelta:    144,
		},
		Node2Policy: &lndclient.RoutingPolicy{
			FeeBaseMsat:      0,
			FeeRateMilliMsat: 0,
			TimeLockDelta:    144,
		},
	}

	// Construct channel4 which will be returned my listChannels and
	// channelEdge4 which will be returned by getChanInfo
	chan4Capacity = btcutil.Amount(10000)
	channel4      = lndclient.ChannelInfo{
		Active:        true,
		ChannelID:     chanID4.ToUint64(),
		PubKeyBytes:   peer2,
		LocalBalance:  10000,
		RemoteBalance: 0,
		Capacity:      chan4Capacity,
	}
	channelEdge4 = lndclient.ChannelEdge{
		ChannelID: chanID4.ToUint64(),
		ChannelPoint: "6fe4408bba52c0a0ee15365e107105de" +
			"fabfc70c497556af69351c4cfbc167b:0",
		Capacity: chan1Capacity,
		Node1:    origin,
		Node2:    peer2,
		Node1Policy: &lndclient.RoutingPolicy{
			FeeBaseMsat:      0,
			FeeRateMilliMsat: 0,
			TimeLockDelta:    144,
		},
		Node2Policy: &lndclient.RoutingPolicy{
			FeeBaseMsat:      0,
			FeeRateMilliMsat: 0,
			TimeLockDelta:    144,
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
		// 3 inputs set assumes the host node has 3 channels to chose
		// from. Only channel 2 with peer 2 is ideal, however we should
		// still include the other 2 after in the order they were
		// provided just in case
		{
			name: "3 inputs set",
			channels: []lndclient.ChannelInfo{
				channel2,
				channel3,
				channel4,
			},
			channelEdges: map[uint64]*lndclient.ChannelEdge{
				channel2.ChannelID: &channelEdge2,
				channel3.ChannelID: &channelEdge3,
				channel4.ChannelID: &channelEdge4,
			},
			expectedHopHints: [][]zpay32.HopHint{
				{{
					NodeID:                    pubKeyPeer2,
					ChannelID:                 channel2.ChannelID,
					FeeBaseMSat:               0,
					FeeProportionalMillionths: 0,
					CLTVExpiryDelta:           144,
				}},
				{{
					NodeID:                    pubKeyPeer2,
					ChannelID:                 channel3.ChannelID,
					FeeBaseMSat:               0,
					FeeProportionalMillionths: 0,
					CLTVExpiryDelta:           144,
				}},
				{{
					NodeID:                    pubKeyPeer2,
					ChannelID:                 channel4.ChannelID,
					FeeBaseMSat:               0,
					FeeProportionalMillionths: 0,
					CLTVExpiryDelta:           144,
				}},
			},
			amtMSat:        chan1Capacity,
			numMaxHophints: 20,
			includeNodes:   make(map[route.Vertex]struct{}),
			expectedError:  nil,
		},
		{
			name: "invalid set",
			channels: []lndclient.ChannelInfo{
				channel1,
			},
			channelEdges: map[uint64]*lndclient.ChannelEdge{
				channel1.ChannelID: &channelEdge1,
			},
			expectedHopHints: [][]zpay32.HopHint{
				{{
					NodeID:                    pubKeyPeer1,
					ChannelID:                 channel1.ChannelID,
					FeeBaseMSat:               0,
					FeeProportionalMillionths: 0,
					CLTVExpiryDelta:           144,
				}},
			}, amtMSat: chan1Capacity,
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
