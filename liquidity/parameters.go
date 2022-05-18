package liquidity

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

var (
	// defaultParameters contains the default parameters that we start our
	// liquidity manger with.
	defaultParameters = Parameters{
		AutoFeeBudget:   defaultBudget,
		MaxAutoInFlight: defaultMaxInFlight,
		ChannelRules:    make(map[lnwire.ShortChannelID]*SwapRule),
		PeerRules:       make(map[route.Vertex]*SwapRule),
		FailureBackOff:  defaultFailureBackoff,
		SweepConfTarget: defaultConfTarget,
		HtlcConfTarget:  defaultHtlcConfTarget,
		FeeLimit:        defaultFeePortion(),
	}
)

// Parameters is a set of parameters provided by the user which guide
// how we assess liquidity.
type Parameters struct {
	// Autoloop enables automatic dispatch of swaps.
	Autoloop bool

	// AutoFeeBudget is the total amount we allow to be spent on
	// automatically dispatched swaps. Once this budget has been used, we
	// will stop dispatching swaps until the budget is increased or the
	// start date is moved.
	AutoFeeBudget btcutil.Amount

	// AutoFeeStartDate is the date from which we will include automatically
	// dispatched swaps in our current budget, inclusive.
	AutoFeeStartDate time.Time

	// MaxAutoInFlight is the maximum number of in-flight automatically
	// dispatched swaps we allow.
	MaxAutoInFlight int

	// FailureBackOff is the amount of time that we require passes after a
	// channel has been part of a failed loop out swap before we suggest
	// using it again.
	// TODO(carla): add exponential backoff
	FailureBackOff time.Duration

	// SweepConfTarget is the number of blocks we aim to confirm our sweep
	// transaction in. This value affects the on chain fees we will pay.
	SweepConfTarget int32

	// HtlcConfTarget is the confirmation target that we use for publishing
	// loop in swap htlcs on chain.
	HtlcConfTarget int32

	// FeeLimit controls the fee limit we place on swaps.
	FeeLimit FeeLimit

	// ClientRestrictions are the restrictions placed on swap size by the
	// client.
	ClientRestrictions Restrictions

	// ChannelRules maps a short channel ID to a rule that describes how we
	// would like liquidity to be managed. These rules and PeerRules are
	// exclusively set to prevent overlap between peer and channel rules.
	ChannelRules map[lnwire.ShortChannelID]*SwapRule

	// PeerRules maps a peer's pubkey to a rule that applies to all the
	// channels that we have with the peer collectively. These rules and
	// ChannelRules are exclusively set to prevent overlap between peer
	// and channel rules map to avoid ambiguity.
	PeerRules map[route.Vertex]*SwapRule
}

// String returns the string representation of our parameters.
func (p Parameters) String() string {
	ruleList := make([]string, 0, len(p.ChannelRules)+len(p.PeerRules))

	for channel, rule := range p.ChannelRules {
		ruleList = append(
			ruleList, fmt.Sprintf("Channel: %v: %v", channel, rule),
		)
	}

	for peer, rule := range p.PeerRules {
		ruleList = append(
			ruleList, fmt.Sprintf("Peer: %v: %v", peer, rule),
		)
	}

	return fmt.Sprintf("rules: %v, failure backoff: %v, sweep "+
		"sweep conf target: %v, htlc conf target: %v,fees: %v, "+
		"auto budget: %v, budget start: %v, max auto in flight: %v, "+
		"minimum swap size=%v, maximum swap size=%v",
		strings.Join(ruleList, ","), p.FailureBackOff,
		p.SweepConfTarget, p.HtlcConfTarget, p.FeeLimit,
		p.AutoFeeBudget, p.AutoFeeStartDate, p.MaxAutoInFlight,
		p.ClientRestrictions.Minimum, p.ClientRestrictions.Maximum)
}

// haveRules returns a boolean indicating whether we have any rules configured.
func (p Parameters) haveRules() bool {
	if len(p.ChannelRules) != 0 {
		return true
	}

	if len(p.PeerRules) != 0 {
		return true
	}

	return false
}

// validate checks whether a set of parameters is valid. Our set of currently
// open channels are required to check that there is no overlap between the
// rules set on a per-peer level, and those set for specific channels. We can't
// allow both, because then we're trying to cater for two separate liquidity
// goals on the same channel. Since we use short channel ID, we don't need to
// worry about pending channels (users would need to work very hard to get the
// short channel ID for a pending channel). Likewise, we don't care about closed
// channels, since there is no action that may occur on them, and we want to
// allow peer-level rules to be set once a channel which had a specific rule
// has been closed. It takes the minimum confirmations we allow for sweep
// confirmation target as a parameter.
// TODO(carla): prune channels that have been closed from rules.
func (p Parameters) validate(minConfs int32, openChans []lndclient.ChannelInfo,
	server *Restrictions) error {

	// First, we check that the rules on a per peer and per channel do not
	// overlap, since this could lead to contractions.
	for _, channel := range openChans {
		// If we don't have a rule for the peer, there's no way we have
		// an overlap between this peer and the channel.
		_, ok := p.PeerRules[channel.PubKeyBytes]
		if !ok {
			continue
		}

		shortID := lnwire.NewShortChanIDFromInt(channel.ChannelID)
		_, ok = p.ChannelRules[shortID]
		if ok {
			log.Debugf("Rules for peer: %v and its channel: %v "+
				"can't both be set", channel.PubKeyBytes, shortID)

			return ErrExclusiveRules
		}
	}

	for channel, rule := range p.ChannelRules {
		if channel.ToUint64() == 0 {
			return ErrZeroChannelID
		}

		if rule.Type == swap.TypeIn {
			return errors.New("channel level rules not supported for " +
				"loop in swaps, only peer-level rules allowed")
		}

		if err := rule.validate(); err != nil {
			return fmt.Errorf("channel: %v has invalid rule: %v",
				channel.ToUint64(), err)
		}
	}

	for peer, rule := range p.PeerRules {
		if err := rule.validate(); err != nil {
			return fmt.Errorf("peer: %v has invalid rule: %v",
				peer, err)
		}
	}

	// Check that our confirmation target is above our required minimum.
	if p.SweepConfTarget < minConfs {
		return fmt.Errorf("confirmation target must be at least: %v",
			minConfs)
	}

	if p.HtlcConfTarget < 1 {
		return fmt.Errorf("htlc confirmation target must be > 0")
	}

	if err := p.FeeLimit.validate(); err != nil {
		return err
	}

	if p.AutoFeeBudget < 0 {
		return ErrNegativeBudget
	}

	if p.MaxAutoInFlight <= 0 {
		return ErrZeroInFlight
	}

	err := validateRestrictions(server, &p.ClientRestrictions)
	if err != nil {
		return err
	}

	return nil
}

// validateRestrictions checks that client restrictions fall within the server's
// restrictions.
func validateRestrictions(server, client *Restrictions) error {
	zeroMin := client.Minimum == 0
	zeroMax := client.Maximum == 0

	if zeroMin && zeroMax {
		return nil
	}

	// If we have a non-zero maximum, we need to ensure it is greater than
	// our minimum (which is fine if min is zero), and does not exceed the
	// server's maximum.
	if !zeroMax {
		if client.Minimum > client.Maximum {
			return ErrMinimumExceedsMaximumAmt
		}

		if client.Maximum > server.Maximum {
			return ErrMaxExceedsServer
		}
	}

	if zeroMin {
		return nil
	}

	// If the client set a minimum, ensure it is at least equal to the
	// server's limit.
	if client.Minimum < server.Minimum {
		return ErrMinLessThanServer
	}

	return nil
}

// cloneParameters creates a deep clone of a parameters struct so that callers
// cannot mutate our parameters. Although our parameters struct itself is not
// a reference, we still need to clone the contents of maps.
func cloneParameters(params Parameters) Parameters {
	paramCopy := params
	paramCopy.ChannelRules = make(
		map[lnwire.ShortChannelID]*SwapRule,
		len(params.ChannelRules),
	)

	for channel, rule := range params.ChannelRules {
		ruleCopy := *rule
		paramCopy.ChannelRules[channel] = &ruleCopy
	}

	paramCopy.PeerRules = make(
		map[route.Vertex]*SwapRule,
		len(params.PeerRules),
	)

	for peer, rule := range params.PeerRules {
		ruleCopy := *rule
		paramCopy.PeerRules[peer] = &ruleCopy
	}

	return paramCopy
}
