package loop

import (
	"context"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/zpay32"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	// DefaultMaxHopHints is set to 20 as that is the default set in LND
	DefaultMaxHopHints = 20
)

// SelectHopHints is a direct port of the SelectHopHints found in lnd. It was
// reimplemented because the current implementation in LND relies on internals
// not externalized through the API. Hopefully in the future SelectHopHints
// will be refactored to allow for custom data sources. It iterates through all
// the active and public channels available and returns eligible channels.
// Eligibility requirements are simple: does the channel have enough liquidity
// to fulfill the request and is the node whitelisted (if specified)
func SelectHopHints(ctx context.Context, lnd *lndclient.LndServices,
	amtMSat btcutil.Amount, numMaxHophints int,
	includeNodes map[route.Vertex]struct{}) ([][]zpay32.HopHint, error) {

	// Fetch all active and public channels.
	openChannels, err := lnd.Client.ListChannels(ctx, false, false)
	if err != nil {
		return nil, err
	}

	// We'll add our hop hints in two passes, first we'll add all channels
	// that are eligible to be hop hints, and also have a local balance
	// above the payment amount.
	var totalHintBandwidth btcutil.Amount

	// chanInfoCache is a simple cache for any information we retrieve
	// through GetChanInfo
	chanInfoCache := make(map[uint64]*lndclient.ChannelEdge)

	// skipCache is a simple cache which holds the indice of any
	// channel we've added to final hopHints
	skipCache := make(map[int]struct{})

	hopHints := make([][]zpay32.HopHint, 0, numMaxHophints)

	for i, channel := range openChannels {
		// In this first pass, we'll ignore all channels in
		// isolation that can't satisfy this payment.

		// Retrieve extra info for each channel not available in
		// listChannels
		chanInfo, err := lnd.Client.GetChanInfo(ctx, channel.ChannelID)
		if err != nil {
			return nil, err
		}

		// Cache the GetChanInfo result since it might be useful
		chanInfoCache[channel.ChannelID] = chanInfo

		// Skip if channel can't forward payment
		if channel.RemoteBalance < amtMSat {
			log.Debugf(
				"Skipping ChannelID: %v for hints as "+
					"remote balance (%v sats) "+
					"insufficient appears to be private",
				channel.ChannelID, channel.RemoteBalance,
			)
			continue
		}
		// If includeNodes is set, we'll only add channels with peers in
		// includeNodes. This is done to respect the last_hop parameter.
		if len(includeNodes) > 0 {
			if _, ok := includeNodes[channel.PubKeyBytes]; !ok {
				continue
			}
		}

		// Mark the index to skip so we can skip it on the next
		// iteration if needed. We'll skip all channels that make
		// it past this point as they'll likely belong to private
		// nodes or be selected.
		skipCache[i] = struct{}{}

		// We want to prevent leaking private nodes, which we define as
		// nodes with only private channels.
		//
		// GetNodeInfo will never return private channels, even if
		// they're somehow known to us. If there are any channels
		// returned, we can consider the node to be public.
		nodeInfo, err := lnd.Client.GetNodeInfo(
			ctx, channel.PubKeyBytes, true,
		)

		// If the error is node isn't found, just iterate. Otherwise,
		// fail.
		status, ok := status.FromError(err)
		if ok && status.Code() == codes.NotFound {
			log.Warnf("Skipping ChannelID: %v for hints as peer "+
				"(NodeID: %v) is not found: %v",
				channel.ChannelID, channel.PubKeyBytes.String(),
				err)
			continue
		} else if err != nil {
			return nil, err
		}

		if len(nodeInfo.Channels) == 0 {
			log.Infof(
				"Skipping ChannelID: %v for hints as peer "+
					"(NodeID: %v) appears to be private",
				channel.ChannelID, channel.PubKeyBytes.String(),
			)
			continue
		}

		nodeID, err := btcec.ParsePubKey(
			channel.PubKeyBytes[:], btcec.S256(),
		)
		if err != nil {
			return nil, err
		}

		// Now that we now this channel use usable, add it as a hop
		// hint and the indexes we'll use later.
		hopHints = append(hopHints, []zpay32.HopHint{{
			NodeID:      nodeID,
			ChannelID:   channel.ChannelID,
			FeeBaseMSat: uint32(chanInfo.Node2Policy.FeeBaseMsat),
			FeeProportionalMillionths: uint32(
				chanInfo.Node2Policy.FeeRateMilliMsat,
			),
			CLTVExpiryDelta: uint16(
				chanInfo.Node2Policy.TimeLockDelta),
		}})

		totalHintBandwidth += channel.RemoteBalance
	}

	// If we have enough hop hints at this point, then we'll exit early.
	// Otherwise, we'll continue to add more that may help out mpp users.
	if len(hopHints) >= numMaxHophints {
		return hopHints, nil
	}

	// In this second pass we'll add channels, and we'll either stop when
	// we have 20 hop hints, we've run through all the available channels,
	// or if the sum of available bandwidth in the routing hints exceeds 2x
	// the payment amount. We do 2x here to account for a margin of error
	// if some of the selected channels no longer become operable.
	hopHintFactor := btcutil.Amount(lnwire.MilliSatoshi(2))

	for i := 0; i < len(openChannels); i++ {
		// If we hit either of our early termination conditions, then
		// we'll break the loop here.
		if totalHintBandwidth > amtMSat*hopHintFactor ||
			len(hopHints) >= numMaxHophints {

			break
		}

		// Skip the channel if we already selected it.
		if _, ok := skipCache[i]; ok {
			continue
		}

		channel := openChannels[i]
		chanInfo := chanInfoCache[channel.ChannelID]

		nodeID, err := btcec.ParsePubKey(
			channel.PubKeyBytes[:], btcec.S256())
		if err != nil {
			continue
		}

		// Include the route hint in our set of options that will be
		// used when creating the invoice.
		hopHints = append(hopHints, []zpay32.HopHint{{
			NodeID:      nodeID,
			ChannelID:   channel.ChannelID,
			FeeBaseMSat: uint32(chanInfo.Node2Policy.FeeBaseMsat),
			FeeProportionalMillionths: uint32(
				chanInfo.Node2Policy.FeeRateMilliMsat,
			),
			CLTVExpiryDelta: uint16(
				chanInfo.Node2Policy.TimeLockDelta),
		}})

		// As we've just added a new hop hint, we'll accumulate it's
		// available balance now to update our tally.
		//
		// TODO(roasbeef): have a cut off based on min bandwidth?
		totalHintBandwidth += channel.RemoteBalance
	}

	return hopHints, nil
}
