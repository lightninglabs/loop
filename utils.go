package loop

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/zpay32"
)

var (
	// DefaultMaxHopHints is set to 20 as that is the default set in LND.
	DefaultMaxHopHints = 20
)

// isPublicNode checks if a node is public, by simply checking if there's any
// channels reported to the node.
func isPublicNode(ctx context.Context, lnd *lndclient.LndServices,
	pubKey [33]byte) (bool, error) {

	// GetNodeInfo doesn't report our private channels with the queried node
	// so we can use it to determine if the node is considered public.
	nodeInfo, err := lnd.Client.GetNodeInfo(
		ctx, pubKey, true,
	)

	if err != nil {
		return false, err
	}

	return (nodeInfo.ChannelCount > 0), nil
}

// fetchChannelEdgesByID fetches the edge info for the passed channel and
// returns the channeldb structs filled with the data that is needed for
// LND's SelectHopHints implementation.
func fetchChannelEdgesByID(ctx context.Context, lnd *lndclient.LndServices,
	chanID uint64) (*channeldb.ChannelEdgeInfo, *channeldb.ChannelEdgePolicy,
	*channeldb.ChannelEdgePolicy, error) {

	chanInfo, err := lnd.Client.GetChanInfo(ctx, chanID)
	if err != nil {
		return nil, nil, nil, err
	}

	edgeInfo := &channeldb.ChannelEdgeInfo{
		ChannelID:     chanID,
		NodeKey1Bytes: chanInfo.Node1,
		NodeKey2Bytes: chanInfo.Node2,
	}

	policy1 := &channeldb.ChannelEdgePolicy{
		FeeBaseMSat: lnwire.MilliSatoshi(
			chanInfo.Node1Policy.FeeBaseMsat,
		),
		FeeProportionalMillionths: lnwire.MilliSatoshi(
			chanInfo.Node1Policy.FeeRateMilliMsat,
		),
		TimeLockDelta: uint16(chanInfo.Node1Policy.TimeLockDelta),
	}

	policy2 := &channeldb.ChannelEdgePolicy{
		FeeBaseMSat: lnwire.MilliSatoshi(
			chanInfo.Node2Policy.FeeBaseMsat,
		),
		FeeProportionalMillionths: lnwire.MilliSatoshi(
			chanInfo.Node2Policy.FeeRateMilliMsat,
		),
		TimeLockDelta: uint16(chanInfo.Node2Policy.TimeLockDelta),
	}

	return edgeInfo, policy1, policy2, nil
}

// parseOutPoint attempts to parse an outpoint from the passed in string.
func parseOutPoint(s string) (*wire.OutPoint, error) {
	split := strings.Split(s, ":")
	if len(split) != 2 {
		return nil, fmt.Errorf("expecting outpoint to be in format "+
			"of txid:index: %s", s)
	}

	index, err := strconv.ParseInt(split[1], 10, 32)
	if err != nil {
		return nil, fmt.Errorf("unable to decode output index: %v", err)
	}

	txid, err := chainhash.NewHashFromStr(split[0])
	if err != nil {
		return nil, fmt.Errorf("unable to parse hex string: %v", err)
	}

	return &wire.OutPoint{
		Hash:  *txid,
		Index: uint32(index),
	}, nil
}

// SelectHopHints calls into LND's exposed SelectHopHints prefiltered to the
// includeNodes map (unless it's empty).
func SelectHopHints(ctx context.Context, lnd *lndclient.LndServices,
	amt btcutil.Amount, numMaxHophints int,
	includeNodes map[route.Vertex]struct{}) ([][]zpay32.HopHint, error) {

	cfg := &invoicesrpc.SelectHopHintsCfg{
		IsPublicNode: func(pubKey [33]byte) (bool, error) {
			return isPublicNode(ctx, lnd, pubKey)
		},
		FetchChannelEdgesByID: func(chanID uint64) (
			*channeldb.ChannelEdgeInfo, *channeldb.ChannelEdgePolicy,
			*channeldb.ChannelEdgePolicy, error) {

			return fetchChannelEdgesByID(ctx, lnd, chanID)
		},
	}
	// Fetch all active and public channels.
	channels, err := lnd.Client.ListChannels(ctx, false, false)
	if err != nil {
		return nil, err
	}

	openChannels := []*invoicesrpc.HopHintInfo{}
	for _, channel := range channels {
		if len(includeNodes) > 0 {
			if _, ok := includeNodes[channel.PubKeyBytes]; !ok {
				continue
			}
		}

		outPoint, err := parseOutPoint(channel.ChannelPoint)
		if err != nil {
			return nil, err
		}

		remotePubkey, err := btcec.ParsePubKey(
			channel.PubKeyBytes[:], btcec.S256(),
		)
		if err != nil {
			return nil, err
		}

		openChannels = append(
			openChannels, &invoicesrpc.HopHintInfo{
				IsPublic:        !channel.Private,
				IsActive:        channel.Active,
				FundingOutpoint: *outPoint,
				RemotePubkey:    remotePubkey,
				RemoteBalance: lnwire.MilliSatoshi(
					channel.RemoteBalance * 1000,
				),
				ShortChannelID: channel.ChannelID,
			},
		)
	}

	routeHints := invoicesrpc.SelectHopHints(
		lnwire.MilliSatoshi(amt*1000), cfg, openChannels, numMaxHophints,
	)

	return routeHints, nil
}
