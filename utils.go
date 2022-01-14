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

// chanCanBeHopHint checks whether the passed channel could be used as a private
// hophint.
func chanCanBeHopHint(chanInfo *lndclient.ChannelInfo) bool {
	return chanInfo.Private && chanInfo.Active
}

// chanRemotePolicy selectes the correct remote routing policy.
func chanRemotePolicy(remotePub route.Vertex,
	edgeInfo *lndclient.ChannelEdge) *lndclient.RoutingPolicy {

	if remotePub == edgeInfo.Node1 {
		return edgeInfo.Node1Policy
	}

	return edgeInfo.Node2Policy
}

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

	// skipCache is a simple cache which holds the indices of any channel
	// that we should skip when doing the second round of channel selection.
	skipCache := make(map[int]struct{})

	hopHints := make([][]zpay32.HopHint, 0, numMaxHophints)

	for i, channel := range openChannels {
		channel := channel

		// In this first pass, we'll ignore all channels in
		// isolation that can't satisfy this payment.

		// Skip public or inactive channels.
		if !chanCanBeHopHint(&channel) {
			log.Debugf("SelectHopHints: skipping ChannelID: %v, " +
				"as is not eligible for a private hop hint")
			skipCache[i] = struct{}{}
			continue
		}

		// If includeNodes is set, we'll only add channels with peers in
		// includeNodes. This is done to respect the last_hop parameter.
		if len(includeNodes) > 0 {
			if _, ok := includeNodes[channel.PubKeyBytes]; !ok {
				skipCache[i] = struct{}{}
				continue
			}
		}

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
			log.Warnf("SelectHopHints: skipping ChannelID: %v, "+
				"as peer (NodeID: %v) is not found: %v",
				channel.ChannelID, channel.PubKeyBytes.String(),
				err)
			continue
		} else if err != nil {
			return nil, err
		}

		if len(nodeInfo.Channels) == 0 {
			log.Infof(
				"SelectHopHints: skipping ChannelID: %v as "+
					"peer (NodeID: %v) appears to be private",
				channel.ChannelID, channel.PubKeyBytes.String(),
			)

			// Skip this channel since the remote node is private.
			skipCache[i] = struct{}{}
			continue
		}

		// Retrieve extra info for each channel not available in
		// listChannels.
		chanInfo, err := lnd.Client.GetChanInfo(ctx, channel.ChannelID)
		if err != nil {
			return nil, err
		}

		// Cache the GetChanInfo result since it might be useful
		chanInfoCache[channel.ChannelID] = chanInfo

		// Skip if channel can't forward payment
		if channel.RemoteBalance < amtMSat {
			log.Debugf(
				"SelectHopHints: skipping ChannelID: %v, as "+
					"the remote balance (%v sats) is "+
					"insufficient", channel.ChannelID,
				channel.RemoteBalance,
			)
			continue
		}

		// Now, we'll need to determine which is the correct policy.
		policy := chanRemotePolicy(channel.PubKeyBytes, chanInfo)
		if policy == nil {
			continue
		}

		nodePubKey, err := btcec.ParsePubKey(
			channel.PubKeyBytes[:], btcec.S256(),
		)
		if err != nil {
			return nil, err
		}

		// Now that we know this channel is usable, add it as a hop
		// hint and the indices we'll use later.
		hopHints = append(hopHints, []zpay32.HopHint{{
			NodeID:      nodePubKey,
			ChannelID:   channel.ChannelID,
			FeeBaseMSat: uint32(policy.FeeBaseMsat),
			FeeProportionalMillionths: uint32(
				policy.FeeRateMilliMsat,
			),
			CLTVExpiryDelta: uint16(policy.TimeLockDelta),
		}})

		totalHintBandwidth += channel.RemoteBalance

		// Mark the index to skip so we can skip it on the next
		// iteration.
		skipCache[i] = struct{}{}
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

		// Channels of private nodes, inactive, or public channels or
		// those that have already been selected can be skipped in this
		// iteration.
		if _, ok := skipCache[i]; ok {
			continue
		}

		channel := openChannels[i]
		chanInfo := chanInfoCache[channel.ChannelID]

		// Now, we'll need to determine which is the correct policy.
		policy := chanRemotePolicy(channel.PubKeyBytes, chanInfo)
		if policy == nil {
			continue
		}

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
			FeeBaseMSat: uint32(policy.FeeBaseMsat),
			FeeProportionalMillionths: uint32(
				policy.FeeRateMilliMsat,
			),
			CLTVExpiryDelta: uint16(policy.TimeLockDelta),
		}})

		// As we've just added a new hop hint, we'll accumulate it's
		// available balance now to update our tally.
		//
		// TODO(roasbeef): have a cut off based on min bandwidth?
		totalHintBandwidth += channel.RemoteBalance
	}

	return hopHints, nil
}
