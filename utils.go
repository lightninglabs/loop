package loop

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/zpay32"
)

var (
	// DefaultMaxHopHints is set to 20 as that is the default set in LND.
	DefaultMaxHopHints = 20

	// hopHintFactor is factor by which we scale the total amount of
	// inbound capacity we want our hop hints to represent, allowing us to
	// have some leeway if peers go offline.
	hopHintFactor = 2
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

// getAlias tries to get the ShortChannelId from the passed ChannelId and
// aliasCache.
func getAlias(aliasCache map[lnwire.ChannelID]lnwire.ShortChannelID,
	channelID lnwire.ChannelID) (lnwire.ShortChannelID, error) {

	if channelID, ok := aliasCache[channelID]; ok {
		return channelID, nil
	}

	return lnwire.ShortChannelID{}, fmt.Errorf("can't find channelId")
}

// SelectHopHints calls into LND's exposed SelectHopHints prefiltered to the
// includeNodes map (unless it's empty).
func SelectHopHints(ctx context.Context, lnd *lndclient.LndServices,
	amt btcutil.Amount, numMaxHophints int,
	includeNodes map[route.Vertex]struct{}) ([][]zpay32.HopHint, error) {

	aliasCache := make(map[lnwire.ChannelID]lnwire.ShortChannelID)

	// Fetch all active and public channels.
	channels, err := lnd.Client.ListChannels(ctx, false, false)
	if err != nil {
		return nil, err
	}

	openChannels := []*HopHintInfo{}
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

		remotePubkey, err := btcec.ParsePubKey(channel.PubKeyBytes[:])
		if err != nil {
			return nil, err
		}

		openChannels = append(
			openChannels, &HopHintInfo{
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

		channelID := lnwire.NewChanIDFromOutPoint(outPoint)
		scID := lnwire.NewShortChanIDFromInt(channel.ChannelID)
		aliasCache[channelID] = scID
	}

	cfg := &SelectHopHintsCfg{
		IsPublicNode: func(pubKey [33]byte) (bool, error) {
			return isPublicNode(ctx, lnd, pubKey)
		},
		FetchChannelEdgesByID: func(chanID uint64) (
			*channeldb.ChannelEdgeInfo, *channeldb.ChannelEdgePolicy,
			*channeldb.ChannelEdgePolicy, error) {

			return fetchChannelEdgesByID(ctx, lnd, chanID)
		},
		GetAlias: func(id lnwire.ChannelID) (
			lnwire.ShortChannelID, error) {

			return getAlias(aliasCache, id)
		},
	}

	routeHints := invoicesrpcSelectHopHints(
		lnwire.MilliSatoshi(amt*1000), cfg, openChannels, numMaxHophints,
	)

	return routeHints, nil
}

// chanCanBeHopHint returns true if the target channel is eligible to be a hop
// hint.
func chanCanBeHopHint(channel *HopHintInfo, cfg *SelectHopHintsCfg) (
	*channeldb.ChannelEdgePolicy, bool) {

	// Since we're only interested in our private channels, we'll skip
	// public ones.
	if channel.IsPublic {
		return nil, false
	}

	// Make sure the channel is active.
	if !channel.IsActive {
		log.Debugf("Skipping channel %v due to not "+
			"being eligible to forward payments",
			channel.ShortChannelID)
		return nil, false
	}

	// To ensure we don't leak unadvertised nodes, we'll make sure our
	// counterparty is publicly advertised within the network.  Otherwise,
	// we'll end up leaking information about nodes that intend to stay
	// unadvertised, like in the case of a node only having private
	// channels.
	var remotePub [33]byte
	copy(remotePub[:], channel.RemotePubkey.SerializeCompressed())
	isRemoteNodePublic, err := cfg.IsPublicNode(remotePub)
	if err != nil {
		log.Errorf("Unable to determine if node %x "+
			"is advertised: %v", remotePub, err)
		return nil, false
	}

	if !isRemoteNodePublic {
		log.Debugf("Skipping channel %v due to "+
			"counterparty %x being unadvertised",
			channel.ShortChannelID, remotePub)
		return nil, false
	}

	// Fetch the policies for each end of the channel.
	info, p1, p2, err := cfg.FetchChannelEdgesByID(channel.ShortChannelID)
	if err != nil {
		// In the case of zero-conf channels, it may be the case that
		// the alias SCID was deleted from the graph, and replaced by
		// the confirmed SCID. Check the Graph for the confirmed SCID.
		confirmedScid := channel.ConfirmedScidZC
		info, p1, p2, err = cfg.FetchChannelEdgesByID(confirmedScid)
		if err != nil {
			log.Errorf("Unable to fetch the routing policies for "+
				"the edges of the channel %v: %v",
				channel.ShortChannelID, err)
			return nil, false
		}
	}

	// Now, we'll need to determine which is the correct policy for HTLCs
	// being sent from the remote node.
	var remotePolicy *channeldb.ChannelEdgePolicy
	if bytes.Equal(remotePub[:], info.NodeKey1Bytes[:]) {
		remotePolicy = p1
	} else {
		remotePolicy = p2
	}

	return remotePolicy, true
}

// addHopHint creates a hop hint out of the passed channel and channel policy.
// The new hop hint is appended to the passed slice.
func addHopHint(hopHints *[][]zpay32.HopHint,
	channel *HopHintInfo, chanPolicy *channeldb.ChannelEdgePolicy,
	aliasScid lnwire.ShortChannelID) {

	hopHint := zpay32.HopHint{
		NodeID:      channel.RemotePubkey,
		ChannelID:   channel.ShortChannelID,
		FeeBaseMSat: uint32(chanPolicy.FeeBaseMSat),
		FeeProportionalMillionths: uint32(
			chanPolicy.FeeProportionalMillionths,
		),
		CLTVExpiryDelta: chanPolicy.TimeLockDelta,
	}

	var defaultScid lnwire.ShortChannelID
	if aliasScid != defaultScid {
		hopHint.ChannelID = aliasScid.ToUint64()
	}

	*hopHints = append(*hopHints, []zpay32.HopHint{hopHint})
}

// HopHintInfo contains the channel information required to create a hop hint.
type HopHintInfo struct {
	// IsPublic indicates whether a channel is advertised to the network.
	IsPublic bool

	// IsActive indicates whether the channel is online and available for
	// use.
	IsActive bool

	// FundingOutpoint is the funding txid:index for the channel.
	FundingOutpoint wire.OutPoint

	// RemotePubkey is the public key of the remote party that this channel
	// is in.
	RemotePubkey *btcec.PublicKey

	// RemoteBalance is the remote party's balance (our current incoming
	// capacity).
	RemoteBalance lnwire.MilliSatoshi

	// ShortChannelID is the short channel ID of the channel.
	ShortChannelID uint64

	// ConfirmedScidZC is the confirmed SCID of a zero-conf channel. This
	// may be used for looking up a channel in the graph.
	ConfirmedScidZC uint64

	// ScidAliasFeature denotes whether the channel has negotiated the
	// option-scid-alias feature bit.
	ScidAliasFeature bool
}

// SelectHopHintsCfg contains the dependencies required to obtain hop hints
// for an invoice.
type SelectHopHintsCfg struct {
	// IsPublicNode is returns a bool indicating whether the node with the
	// given public key is seen as a public node in the graph from the
	// graph's source node's point of view.
	IsPublicNode func(pubKey [33]byte) (bool, error)

	// FetchChannelEdgesByID attempts to lookup the two directed edges for
	// the channel identified by the channel ID.
	FetchChannelEdgesByID func(chanID uint64) (*channeldb.ChannelEdgeInfo,
		*channeldb.ChannelEdgePolicy, *channeldb.ChannelEdgePolicy,
		error)

	// GetAlias allows the peer's alias SCID to be retrieved for private
	// option_scid_alias channels.
	GetAlias func(lnwire.ChannelID) (lnwire.ShortChannelID, error)
}

// sufficientHints checks whether we have sufficient hop hints, based on the
// following criteria:
//   - Hop hint count: limit to a set number of hop hints, regardless of whether
//     we've reached our invoice amount or not.
//   - Total incoming capacity: limit to our invoice amount * scaling factor to
//     allow for some of our links going offline.
//
// We limit our number of hop hints like this to keep our invoice size down,
// and to avoid leaking all our private channels when we don't need to.
func sufficientHints(numHints, maxHints, scalingFactor int, amount,
	totalHintAmount lnwire.MilliSatoshi) bool {

	if numHints >= maxHints {
		log.Debug("Reached maximum number of hop hints")
		return true
	}

	requiredAmount := amount * lnwire.MilliSatoshi(scalingFactor)
	if totalHintAmount >= requiredAmount {
		log.Debugf("Total hint amount: %v has reached target hint "+
			"bandwidth: %v (invoice amount: %v * factor: %v)",
			totalHintAmount, requiredAmount, amount,
			scalingFactor)

		return true
	}

	return false
}

// SelectHopHints will select up to numMaxHophints from the set of passed open
// channels. The set of hop hints will be returned as a slice of functional
// options that'll append the route hint to the set of all route hints.
//
// TODO(sputn1ck): remove when https://github.com/lightningnetwork/lnd/pull/7065
// is merged to a new lnd release.
func invoicesrpcSelectHopHints(amtMSat lnwire.MilliSatoshi, cfg *SelectHopHintsCfg,
	openChannels []*HopHintInfo,
	numMaxHophints int) [][]zpay32.HopHint {

	// We'll add our hop hints in two passes, first we'll add all channels
	// that are eligible to be hop hints, and also have a local balance
	// above the payment amount.
	var totalHintBandwidth lnwire.MilliSatoshi
	hopHintChans := make(map[wire.OutPoint]struct{})
	hopHints := make([][]zpay32.HopHint, 0, numMaxHophints)
	for _, channel := range openChannels {
		channel := channel

		enoughHopHints := sufficientHints(
			len(hopHints), numMaxHophints, hopHintFactor, amtMSat,
			totalHintBandwidth,
		)
		if enoughHopHints {
			log.Debugf("First pass of hop selection has " +
				"sufficient hints")

			return hopHints
		}

		// If this channel can't be a hop hint, then skip it.
		edgePolicy, canBeHopHint := chanCanBeHopHint(channel, cfg)
		if edgePolicy == nil || !canBeHopHint {
			continue
		}

		// Similarly, in this first pass, we'll ignore all channels in
		// isolation can't satisfy this payment.
		if channel.RemoteBalance < amtMSat {
			continue
		}

		// Lookup and see if there is an alias SCID that exists.
		chanID := lnwire.NewChanIDFromOutPoint(
			&channel.FundingOutpoint,
		)
		alias, _ := cfg.GetAlias(chanID)

		// If this is a channel where the option-scid-alias feature bit
		// was negotiated and the alias is not yet assigned, we cannot
		// issue an invoice. Doing so might expose the confirmed SCID
		// of a private channel.
		if channel.ScidAliasFeature {
			var defaultScid lnwire.ShortChannelID
			if alias == defaultScid {
				continue
			}
		}

		// Now that we now this channel use usable, add it as a hop
		// hint and the indexes we'll use later.
		addHopHint(&hopHints, channel, edgePolicy, alias)

		hopHintChans[channel.FundingOutpoint] = struct{}{}
		totalHintBandwidth += channel.RemoteBalance
	}

	// In this second pass we'll add channels, and we'll either stop when
	// we have 20 hop hints, we've run through all the available channels,
	// or if the sum of available bandwidth in the routing hints exceeds 2x
	// the payment amount. We do 2x here to account for a margin of error
	// if some of the selected channels no longer become operable.
	for i := 0; i < len(openChannels); i++ {
		enoughHopHints := sufficientHints(
			len(hopHints), numMaxHophints, hopHintFactor, amtMSat,
			totalHintBandwidth,
		)
		if enoughHopHints {
			log.Debugf("Second pass of hop selection has " +
				"sufficient hints")

			return hopHints
		}

		channel := openChannels[i]

		// Skip the channel if we already selected it.
		if _, ok := hopHintChans[channel.FundingOutpoint]; ok {
			continue
		}

		// If the channel can't be a hop hint, then we'll skip it.
		// Otherwise, we'll use the policy information to populate the
		// hop hint.
		remotePolicy, canBeHopHint := chanCanBeHopHint(channel, cfg)
		if !canBeHopHint || remotePolicy == nil {
			continue
		}

		// Lookup and see if there's an alias SCID that exists.
		chanID := lnwire.NewChanIDFromOutPoint(
			&channel.FundingOutpoint,
		)
		alias, _ := cfg.GetAlias(chanID)

		// If this is a channel where the option-scid-alias feature bit
		// was negotiated and the alias is not yet assigned, we cannot
		// issue an invoice. Doing so might expose the confirmed SCID
		// of a private channel.
		if channel.ScidAliasFeature {
			var defaultScid lnwire.ShortChannelID
			if alias == defaultScid {
				continue
			}
		}

		// Include the route hint in our set of options that will be
		// used when creating the invoice.
		addHopHint(&hopHints, channel, remotePolicy, alias)

		// As we've just added a new hop hint, we'll accumulate it's
		// available balance now to update our tally.
		//
		// TODO(roasbeef): have a cut off based on min bandwidth?
		totalHintBandwidth += channel.RemoteBalance
	}

	return hopHints
}
