// Package liquidity is responsible for monitoring our node's liquidity. It
// allows setting of a liquidity rule which describes the desired liquidity
// balance on a per-channel basis.
//
// Swap suggestions are limited to channels that are not currently being used
// for a pending swap. If we are currently processing an unrestricted swap (ie,
// a loop out with no outgoing channel targets set or a loop in with no last
// hop set), we will not suggest any swaps because these swaps will shift the
// balances of our channels in ways we can't predict.
//
// Fee restrictions are placed on swap suggestions to ensure that we only
// suggest swaps that fit the configured fee preferences.
// - Sweep Fee Rate Limit: the maximum sat/vByte fee estimate for our sweep
//   transaction to confirm within our configured number of confirmations
//   that we will suggest swaps for.
// - Maximum Swap Fee PPM: the maximum server fee, expressed as parts per
//   million of the full swap amount
// - Maximum Routing Fee PPM: the maximum off-chain routing fees for the swap
//   invoice, expressed as parts per million of the swap amount.
// - Maximum Prepay Routing Fee PPM: the maximum off-chain routing fees for the
//   swap prepayment, expressed as parts per million of the prepay amount.
// - Maximum Prepay: the maximum now-show fee, expressed in satoshis. This
//   amount is only payable in the case where the swap server broadcasts a htlc
//   and the client fails to sweep the preimage.
// - Maximum miner fee: the maximum miner fee we are willing to pay to sweep the
//   on chain htlc. Note that the client will use current fee estimates to
//   sweep, so this value acts more as a sanity check in the case of a large fee
//   spike.
//
// The maximum fee per-swap is calculated as follows:
// (swap amount * serverPPM/1e6) + miner fee + (swap amount * routingPPM/1e6)
// + (prepay amount * prepayPPM/1e6).
package liquidity

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/labels"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/ticker"
)

const (
	// defaultFailureBackoff is the default amount of time we backoff if
	// a channel is part of a temporarily failed swap.
	defaultFailureBackoff = time.Hour * 24

	// FeeBase is the base that we use to express fees.
	FeeBase = 1e6

	// defaultMaxInFlight is the default number of in-flight automatically
	// dispatched swaps we allow. Note that this does not enable automated
	// swaps itself (because we want non-zero values to be expressed in
	// suggestions as a dry-run).
	defaultMaxInFlight = 1

	// DefaultAutoloopTicker is the default amount of time between automated
	// swap checks.
	DefaultAutoloopTicker = time.Minute * 10

	// autoloopSwapInitiator is the value we send in the initiator field of
	// a swap request when issuing an automatic swap.
	autoloopSwapInitiator = "autoloop"
)

var (
	// defaultBudget is the default autoloop budget we set. This budget will
	// only be used for automatically dispatched swaps if autoloop is
	// explicitly enabled, so we are happy to set a non-zero value here. The
	// amount chosen simply uses the current defaults to provide budget for
	// a single swap. We don't have a swap amount to calculate our maximum
	// routing fee, so we use 0.16 BTC for now.
	defaultBudget = defaultMaximumMinerFee +
		ppmToSat(funding.MaxBtcFundingAmount, defaultSwapFeePPM) +
		ppmToSat(defaultMaximumPrepay, defaultPrepayRoutingFeePPM) +
		ppmToSat(funding.MaxBtcFundingAmount, defaultRoutingFeePPM)

	// defaultParameters contains the default parameters that we start our
	// liquidity manger with.
	defaultParameters = Parameters{
		AutoFeeBudget:   defaultBudget,
		MaxAutoInFlight: defaultMaxInFlight,
		ChannelRules:    make(map[lnwire.ShortChannelID]*ThresholdRule),
		PeerRules:       make(map[route.Vertex]*ThresholdRule),
		FailureBackOff:  defaultFailureBackoff,
		SweepConfTarget: loop.DefaultSweepConfTarget,
		FeeLimit:        defaultFeePortion(),
	}

	// ErrZeroChannelID is returned if we get a rule for a 0 channel ID.
	ErrZeroChannelID = fmt.Errorf("zero channel ID not allowed")

	// ErrNegativeBudget is returned if a negative swap budget is set.
	ErrNegativeBudget = errors.New("swap budget must be >= 0")

	// ErrZeroInFlight is returned is a zero in flight swaps value is set.
	ErrZeroInFlight = errors.New("max in flight swaps must be >=0")

	// ErrMinimumExceedsMaximumAmt is returned when the minimum configured
	// swap amount is more than the maximum.
	ErrMinimumExceedsMaximumAmt = errors.New("minimum swap amount " +
		"exceeds maximum")

	// ErrMaxExceedsServer is returned if the maximum swap amount set is
	// more than the server offers.
	ErrMaxExceedsServer = errors.New("maximum swap amount is more than " +
		"server maximum")

	// ErrMinLessThanServer is returned if the minimum swap amount set is
	// less than the server minimum.
	ErrMinLessThanServer = errors.New("minimum swap amount is less than " +
		"server minimum")

	// ErrNoRules is returned when no rules are set for swap suggestions.
	ErrNoRules = errors.New("no rules set for autoloop")

	// ErrExclusiveRules is returned when a set of rules that may not be
	// set together are specified.
	ErrExclusiveRules = errors.New("channel and peer rules must be " +
		"exclusive")
)

// Config contains the external functionality required to run the
// liquidity manager.
type Config struct {
	// AutoloopTicker determines how often we should check whether we want
	// to dispatch an automated swap. We use a force ticker so that we can
	// trigger autoloop in itests.
	AutoloopTicker *ticker.Force

	// Restrictions returns the restrictions that the server applies to
	// swaps.
	Restrictions func(ctx context.Context, swapType swap.Type) (
		*Restrictions, error)

	// Lnd provides us with access to lnd's rpc servers.
	Lnd *lndclient.LndServices

	// ListLoopOut returns all of the loop our swaps stored on disk.
	ListLoopOut func() ([]*loopdb.LoopOut, error)

	// ListLoopIn returns all of the loop in swaps stored on disk.
	ListLoopIn func() ([]*loopdb.LoopIn, error)

	// LoopOutQuote gets swap fee, estimated miner fee and prepay amount for
	// a loop out swap.
	LoopOutQuote func(ctx context.Context,
		request *loop.LoopOutQuoteRequest) (*loop.LoopOutQuote, error)

	// LoopOut dispatches a loop out.
	LoopOut func(ctx context.Context, request *loop.OutRequest) (
		*loop.LoopOutSwapInfo, error)

	// Clock allows easy mocking of time in unit tests.
	Clock clock.Clock

	// MinimumConfirmations is the minimum number of confirmations we allow
	// setting for sweep target.
	MinimumConfirmations int32
}

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

	// FeeLimit controls the fee limit we place on swaps.
	FeeLimit FeeLimit

	// ClientRestrictions are the restrictions placed on swap size by the
	// client.
	ClientRestrictions Restrictions

	// ChannelRules maps a short channel ID to a rule that describes how we
	// would like liquidity to be managed. These rules and PeerRules are
	// exclusively set to prevent overlap between peer and channel rules.
	ChannelRules map[lnwire.ShortChannelID]*ThresholdRule

	// PeerRules maps a peer's pubkey to a rule that applies to all the
	// channels that we have with the peer collectively. These rules and
	// ChannelRules are exclusively set to prevent overlap between peer
	// and channel rules map to avoid ambiguity.
	PeerRules map[route.Vertex]*ThresholdRule
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
		"sweep conf target: %v, fees: %v, auto budget: %v, budget "+
		"start: %v, max auto in flight: %v, minimum swap size=%v, "+
		"maximum swap size=%v", strings.Join(ruleList, ","),
		p.FailureBackOff, p.SweepConfTarget, p.FeeLimit,
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

// Manager contains a set of desired liquidity rules for our channel
// balances.
type Manager struct {
	// cfg contains the external functionality we require to determine our
	// current liquidity balance.
	cfg *Config

	// params is the set of parameters we are currently using. These may be
	// updated at runtime.
	params Parameters

	// paramsLock is a lock for our current set of parameters.
	paramsLock sync.Mutex
}

// Run periodically checks whether we should automatically dispatch a loop out.
// We run this loop even if automated swaps are not currently enabled rather
// than managing starting and stopping the ticker as our parameters are updated.
func (m *Manager) Run(ctx context.Context) error {
	m.cfg.AutoloopTicker.Resume()
	defer m.cfg.AutoloopTicker.Stop()

	for {
		select {
		case <-m.cfg.AutoloopTicker.Ticks():
			err := m.autoloop(ctx)
			switch err {
			case ErrNoRules:
				log.Debugf("No rules configured for autoloop")

			case nil:

			default:
				log.Errorf("autoloop failed: %v", err)
			}

		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// NewManager creates a liquidity manager which has no rules set.
func NewManager(cfg *Config) *Manager {
	return &Manager{
		cfg:    cfg,
		params: defaultParameters,
	}
}

// GetParameters returns a copy of our current parameters.
func (m *Manager) GetParameters() Parameters {
	m.paramsLock.Lock()
	defer m.paramsLock.Unlock()

	return cloneParameters(m.params)
}

// SetParameters updates our current set of parameters if the new parameters
// provided are valid.
func (m *Manager) SetParameters(ctx context.Context, params Parameters) error {
	restrictions, err := m.cfg.Restrictions(ctx, swap.TypeOut)
	if err != nil {
		return err
	}

	channels, err := m.cfg.Lnd.Client.ListChannels(ctx)
	if err != nil {
		return err
	}

	err = params.validate(m.cfg.MinimumConfirmations, channels, restrictions)
	if err != nil {
		return err
	}

	m.paramsLock.Lock()
	defer m.paramsLock.Unlock()

	m.params = cloneParameters(params)
	return nil
}

// cloneParameters creates a deep clone of a parameters struct so that callers
// cannot mutate our parameters. Although our parameters struct itself is not
// a reference, we still need to clone the contents of maps.
func cloneParameters(params Parameters) Parameters {
	paramCopy := params
	paramCopy.ChannelRules = make(
		map[lnwire.ShortChannelID]*ThresholdRule,
		len(params.ChannelRules),
	)

	for channel, rule := range params.ChannelRules {
		ruleCopy := *rule
		paramCopy.ChannelRules[channel] = &ruleCopy
	}

	paramCopy.PeerRules = make(
		map[route.Vertex]*ThresholdRule,
		len(params.PeerRules),
	)

	for peer, rule := range params.PeerRules {
		ruleCopy := *rule
		paramCopy.PeerRules[peer] = &ruleCopy
	}

	return paramCopy
}

// autoloop gets a set of suggested swaps and dispatches them automatically if
// we have automated looping enabled.
func (m *Manager) autoloop(ctx context.Context) error {
	suggestion, err := m.SuggestSwaps(ctx, true)
	if err != nil {
		return err
	}

	for _, swap := range suggestion.OutSwaps {
		// If we don't actually have dispatch of swaps enabled, log
		// suggestions.
		if !m.params.Autoloop {
			log.Debugf("recommended autoloop: %v sats over "+
				"%v", swap.Amount, swap.OutgoingChanSet)

			continue
		}

		// Create a copy of our range var so that we can reference it.
		swap := swap
		loopOut, err := m.cfg.LoopOut(ctx, &swap)
		if err != nil {
			return err
		}

		log.Infof("loop out automatically dispatched: hash: %v, "+
			"address: %v", loopOut.SwapHash,
			loopOut.HtlcAddressP2WSH)
	}

	return nil
}

// ForceAutoLoop force-ticks our auto-out ticker.
func (m *Manager) ForceAutoLoop(ctx context.Context) error {
	select {
	case m.cfg.AutoloopTicker.Force <- m.cfg.Clock.Now():
		return nil

	case <-ctx.Done():
		return ctx.Err()
	}
}

// Suggestions provides a set of suggested swaps, and the set of channels that
// were excluded from consideration.
type Suggestions struct {
	// OutSwaps is the set of loop out swaps that we suggest executing.
	OutSwaps []loop.OutRequest

	// DisqualifiedChans maps the set of channels that we do not recommend
	// swaps on to the reason that we did not recommend a swap.
	DisqualifiedChans map[lnwire.ShortChannelID]Reason

	// Disqualified peers maps the set of peers that we do not recommend
	// swaps for to the reason that they were excluded.
	DisqualifiedPeers map[route.Vertex]Reason
}

func newSuggestions() *Suggestions {
	return &Suggestions{
		DisqualifiedChans: make(map[lnwire.ShortChannelID]Reason),
		DisqualifiedPeers: make(map[route.Vertex]Reason),
	}
}

func (s *Suggestions) addSwap(swap swapSuggestion) error {
	out, ok := swap.(*loopOutSwapSuggestion)
	if !ok {
		return fmt.Errorf("unexpected swap type: %T", swap)
	}

	s.OutSwaps = append(s.OutSwaps, out.OutRequest)

	return nil
}

// singleReasonSuggestion is a helper function which returns a set of
// suggestions where all of our rules are disqualified due to a reason that
// applies to all of them (such as being out of budget).
func (m *Manager) singleReasonSuggestion(reason Reason) *Suggestions {
	resp := newSuggestions()

	for id := range m.params.ChannelRules {
		resp.DisqualifiedChans[id] = reason
	}

	for peer := range m.params.PeerRules {
		resp.DisqualifiedPeers[peer] = reason
	}

	return resp
}

// SuggestSwaps returns a set of swap suggestions based on our current liquidity
// balance for the set of rules configured for the manager, failing if there are
// no rules set. It takes an autoloop boolean that indicates whether the
// suggestions are being used for our internal autolooper. This boolean is used
// to determine the information we add to our swap suggestion and whether we
// return any suggestions.
func (m *Manager) SuggestSwaps(ctx context.Context, autoloop bool) (
	*Suggestions, error) {

	m.paramsLock.Lock()
	defer m.paramsLock.Unlock()

	// If we have no rules set, exit early to avoid unnecessary calls to
	// lnd and the server.
	if !m.params.haveRules() {
		return nil, ErrNoRules
	}

	// If our start date is in the future, we interpret this as meaning that
	// we should start using our budget at this date. This means that we
	// have no budget for the present, so we just return.
	if m.params.AutoFeeStartDate.After(m.cfg.Clock.Now()) {
		log.Debugf("autoloop fee budget start time: %v is in "+
			"the future", m.params.AutoFeeStartDate)

		return m.singleReasonSuggestion(ReasonBudgetNotStarted), nil
	}

	// Before we get any swap suggestions, we check what the current fee
	// estimate is to sweep within our target number of confirmations. If
	// This fee exceeds the fee limit we have set, we will not suggest any
	// swaps at present.
	estimate, err := m.cfg.Lnd.WalletKit.EstimateFee(
		ctx, m.params.SweepConfTarget,
	)
	if err != nil {
		return nil, err
	}

	if err := m.params.FeeLimit.mayLoopOut(estimate); err != nil {
		var reasonErr *reasonError
		if errors.As(err, &reasonErr) {
			return m.singleReasonSuggestion(reasonErr.reason), nil

		}

		return nil, err
	}

	// Get the current server side restrictions, combined with the client
	// set restrictions, if any.
	restrictions, err := m.getSwapRestrictions(ctx, swap.TypeOut)
	if err != nil {
		return nil, err
	}

	// List our current set of swaps so that we can determine which channels
	// are already being utilized by swaps. Note that these calls may race
	// with manual initiation of swaps.
	loopOut, err := m.cfg.ListLoopOut()
	if err != nil {
		return nil, err
	}

	loopIn, err := m.cfg.ListLoopIn()
	if err != nil {
		return nil, err
	}

	// Get a summary of our existing swaps so that we can check our autoloop
	// budget.
	summary, err := m.checkExistingAutoLoops(ctx, loopOut)
	if err != nil {
		return nil, err
	}

	if summary.totalFees() >= m.params.AutoFeeBudget {
		log.Debugf("autoloop fee budget: %v exhausted, %v spent on "+
			"completed swaps, %v reserved for ongoing swaps "+
			"(upper limit)",
			m.params.AutoFeeBudget, summary.spentFees,
			summary.pendingFees)

		return m.singleReasonSuggestion(ReasonBudgetElapsed), nil
	}

	// If we have already reached our total allowed number of in flight
	// swaps, we do not suggest any more at the moment.
	allowedSwaps := m.params.MaxAutoInFlight - summary.inFlightCount
	if allowedSwaps <= 0 {
		log.Debugf("%v autoloops allowed, %v in flight",
			m.params.MaxAutoInFlight, summary.inFlightCount)

		return m.singleReasonSuggestion(ReasonInFlight), nil
	}

	channels, err := m.cfg.Lnd.Client.ListChannels(ctx)
	if err != nil {
		return nil, err
	}

	peerChannels := make(map[route.Vertex]*balances)
	for _, channel := range channels {
		bal, ok := peerChannels[channel.PubKeyBytes]
		if !ok {
			bal = &balances{}
		}

		chanID := lnwire.NewShortChanIDFromInt(channel.ChannelID)
		bal.channels = append(bal.channels, chanID)
		bal.capacity += channel.Capacity
		bal.incoming += channel.RemoteBalance
		bal.outgoing += channel.LocalBalance
		bal.pubkey = channel.PubKeyBytes

		peerChannels[channel.PubKeyBytes] = bal
	}

	// Get a summary of the channels and peers that are not eligible due
	// to ongoing swaps.
	traffic := m.currentSwapTraffic(loopOut, loopIn)

	var (
		suggestions []swapSuggestion
		resp        = newSuggestions()
	)

	for peer, balances := range peerChannels {
		rule, haveRule := m.params.PeerRules[peer]
		if !haveRule {
			continue
		}

		suggestion, err := m.suggestSwap(
			ctx, traffic, balances, rule, restrictions, autoloop,
		)
		var reasonErr *reasonError
		if errors.As(err, &reasonErr) {
			resp.DisqualifiedPeers[peer] = reasonErr.reason
			continue
		}

		if err != nil {
			return nil, err
		}

		suggestions = append(suggestions, suggestion)
	}

	for _, channel := range channels {
		balance := newBalances(channel)

		channelID := lnwire.NewShortChanIDFromInt(channel.ChannelID)
		rule, ok := m.params.ChannelRules[channelID]
		if !ok {
			continue
		}

		suggestion, err := m.suggestSwap(
			ctx, traffic, balance, rule, restrictions, autoloop,
		)

		var reasonErr *reasonError
		if errors.As(err, &reasonErr) {
			resp.DisqualifiedChans[channelID] = reasonErr.reason
			continue
		}

		if err != nil {
			return nil, err
		}

		suggestions = append(suggestions, suggestion)
	}

	// If we have no swaps to execute after we have applied all of our
	// limits, just return our set of disqualified swaps.
	if len(suggestions) == 0 {
		return resp, nil
	}

	// Sort suggestions by amount in descending order.
	sort.SliceStable(suggestions, func(i, j int) bool {
		return suggestions[i].amount() > suggestions[j].amount()
	})

	// Run through our suggested swaps in descending order of amount and
	// return all of the swaps which will fit within our remaining budget.
	available := m.params.AutoFeeBudget - summary.totalFees()

	// setReason is a helper that adds a swap's channels to our disqualified
	// list with the reason provided.
	setReason := func(reason Reason, swap swapSuggestion) {
		for _, channel := range swap.channels() {
			_, ok := m.params.ChannelRules[channel]
			if !ok {
				continue
			}

			resp.DisqualifiedChans[channel] = reason
		}
	}

	for _, swap := range suggestions {
		swap := swap

		// If we do not have enough funds available, or we hit our
		// in flight limit, we record this value for the rest of the
		// swaps.
		var reason Reason
		switch {
		case available == 0:
			reason = ReasonBudgetInsufficient

		case len(resp.OutSwaps) == allowedSwaps:
			reason = ReasonInFlight
		}

		if reason != ReasonNone {
			setReason(reason, swap)
			continue
		}

		fees := swap.fees()

		// If the maximum fee we expect our swap to use is less than the
		// amount we have available, we add it to our set of swaps that
		// fall within the budget and decrement our available amount.
		if fees <= available {
			available -= fees

			if err := resp.addSwap(swap); err != nil {
				return nil, err
			}
		} else {
			setReason(ReasonBudgetInsufficient, swap)
		}
	}

	return resp, nil
}

// suggestSwap checks whether we can currently perform a swap, and creates a
// swap request for the rule provided.
func (m *Manager) suggestSwap(ctx context.Context, traffic *swapTraffic,
	balance *balances, rule *ThresholdRule, restrictions *Restrictions,
	autoloop bool) (swapSuggestion, error) {

	// Check whether we can perform a swap.
	err := traffic.maySwap(balance.pubkey, balance.channels)
	if err != nil {
		return nil, err
	}

	// We can have nil suggestions in the case where no action is
	// required, so we skip over them.
	amount := rule.swapAmount(balance, restrictions)
	if amount == 0 {
		return nil, newReasonError(ReasonLiquidityOk)
	}

	swap, err := m.loopOutSwap(ctx, amount, balance, autoloop)
	if err != nil {
		return nil, err
	}

	return &loopOutSwapSuggestion{
		OutRequest: *swap,
	}, nil
}

// loopOutSwap creates a loop out swap with the amount provided for the balance
// described by the balance set provided. A reason that indicates whether we
// can swap is returned. If this value is not ReasonNone, there is no possible
// swap and the loop out request returned will be nil.
func (m *Manager) loopOutSwap(ctx context.Context, amount btcutil.Amount,
	balance *balances, autoloop bool) (*loop.OutRequest, error) {

	quote, err := m.cfg.LoopOutQuote(
		ctx, &loop.LoopOutQuoteRequest{
			Amount:                  amount,
			SweepConfTarget:         m.params.SweepConfTarget,
			SwapPublicationDeadline: m.cfg.Clock.Now(),
		},
	)
	if err != nil {
		return nil, err
	}

	log.Debugf("quote for suggestion: %v, swap fee: %v, "+
		"miner fee: %v, prepay: %v", amount, quote.SwapFee,
		quote.MinerFee, quote.PrepayAmount)

	// Check that the estimated fees for the suggested swap are
	// below the fee limits configured by the manager.
	if err := m.params.FeeLimit.loopOutLimits(amount, quote); err != nil {
		return nil, err
	}

	outRequest, err := m.makeLoopOutRequest(
		ctx, amount, balance, quote, autoloop,
	)
	if err != nil {
		return nil, err
	}

	return &outRequest, nil
}

// getSwapRestrictions queries the server for its latest swap size restrictions,
// validates client restrictions (if present) against these values and merges
// the client's custom requirements with the server's limits to produce a single
// set of limitations for our swap.
func (m *Manager) getSwapRestrictions(ctx context.Context, swapType swap.Type) (
	*Restrictions, error) {

	restrictions, err := m.cfg.Restrictions(ctx, swapType)
	if err != nil {
		return nil, err
	}

	// It is possible that the server has updated its restrictions since
	// we validated our client restrictions, so we validate again to ensure
	// that our restrictions are within the server's bounds.
	err = validateRestrictions(restrictions, &m.params.ClientRestrictions)
	if err != nil {
		return nil, err
	}

	// If our minimum is more than the server's minimum, we set it.
	if m.params.ClientRestrictions.Minimum > restrictions.Minimum {
		restrictions.Minimum = m.params.ClientRestrictions.Minimum
	}

	// If our maximum set and is less than the server's maximum, we set it.
	if m.params.ClientRestrictions.Maximum != 0 &&
		m.params.ClientRestrictions.Maximum < restrictions.Maximum {

		restrictions.Maximum = m.params.ClientRestrictions.Maximum
	}

	return restrictions, nil
}

// makeLoopOutRequest creates a loop out request from a suggestion. Since we
// do not get any information about our off-chain routing fees when we request
// a quote, we just set our prepay and route maximum fees directly from the
// amounts we expect to route. The estimation we use elsewhere is the repo is
// route-independent, which is a very poor estimation so we don't bother with
// checking against this inaccurate constant. We use the exact prepay amount
// and swap fee given to us by the server, but use our maximum miner fee anyway
// to give us some leeway when performing the swap. We take an auto-out which
// determines whether we set a label identifying this swap as automatically
// dispatched, and decides whether we set a sweep address (we don't bother for
// non-auto requests, because the client api will set it anyway).
func (m *Manager) makeLoopOutRequest(ctx context.Context,
	amount btcutil.Amount, balance *balances, quote *loop.LoopOutQuote,
	autoloop bool) (loop.OutRequest, error) {

	prepayMaxFee, routeMaxFee, minerFee := m.params.FeeLimit.loopOutFees(
		amount, quote,
	)

	var chanSet loopdb.ChannelSet
	for _, channel := range balance.channels {
		chanSet = append(chanSet, channel.ToUint64())
	}

	// Create a request with our calculated routing fees. We can use the
	// swap fee, prepay amount and miner fee from the quote because we have
	// already validated them.
	request := loop.OutRequest{
		Amount:              amount,
		OutgoingChanSet:     chanSet,
		MaxPrepayRoutingFee: prepayMaxFee,
		MaxSwapRoutingFee:   routeMaxFee,
		MaxMinerFee:         minerFee,
		MaxSwapFee:          quote.SwapFee,
		MaxPrepayAmount:     quote.PrepayAmount,
		SweepConfTarget:     m.params.SweepConfTarget,
		Initiator:           autoloopSwapInitiator,
	}

	if autoloop {
		request.Label = labels.AutoloopLabel(swap.TypeOut)

		addr, err := m.cfg.Lnd.WalletKit.NextAddr(ctx)
		if err != nil {
			return loop.OutRequest{}, err
		}
		request.DestAddr = addr
	}

	return request, nil
}

// worstCaseOutFees calculates the largest possible fees for a loop out swap,
// comparing the fees for a successful swap to the cost when the client pays
// the prepay because they failed to sweep the on chain htlc. This is unlikely,
// because we expect clients to be online to sweep, but we want to account for
// every outcome so we include it.
func worstCaseOutFees(prepayRouting, swapRouting, swapFee, minerFee,
	prepayAmount btcutil.Amount) btcutil.Amount {

	var (
		successFees = prepayRouting + minerFee + swapFee + swapRouting
		noShowFees  = prepayRouting + prepayAmount
	)

	if noShowFees > successFees {
		return noShowFees
	}

	return successFees
}

// existingAutoLoopSummary provides a summary of the existing autoloops which
// were dispatched during our current budget period.
type existingAutoLoopSummary struct {
	// spentFees is the amount we have spent on completed swaps.
	spentFees btcutil.Amount

	// pendingFees is the worst-case amount of fees we could spend on in
	// flight autoloops.
	pendingFees btcutil.Amount

	// inFlightCount is the total number of automated swaps that are
	// currently in flight. Note that this may race with swap completion,
	// but not with initiation of new automated swaps, this is ok, because
	// it can only lead to dispatching fewer swaps than we could have (not
	// too many).
	inFlightCount int
}

// totalFees returns the total amount of fees that automatically dispatched
// swaps may consume.
func (e *existingAutoLoopSummary) totalFees() btcutil.Amount {
	return e.spentFees + e.pendingFees
}

// checkExistingAutoLoops calculates the total amount that has been spent by
// automatically dispatched swaps that have completed, and the worst-case fee
// total for our set of ongoing, automatically dispatched swaps as well as a
// current in-flight count.
func (m *Manager) checkExistingAutoLoops(ctx context.Context,
	loopOuts []*loopdb.LoopOut) (*existingAutoLoopSummary, error) {

	var summary existingAutoLoopSummary

	for _, out := range loopOuts {
		if out.Contract.Label != labels.AutoloopLabel(swap.TypeOut) {
			continue
		}

		// If we have a pending swap, we are uncertain of the fees that
		// it will end up paying. We use the worst-case estimate based
		// on the maximum values we set for each fee category. This will
		// likely over-estimate our fees (because we probably won't
		// spend our maximum miner amount). If a swap is not pending,
		// it has succeeded or failed so we just record our actual fees
		// for the swap provided that the swap completed after our
		// budget start date.
		if out.State().State.Type() == loopdb.StateTypePending {
			summary.inFlightCount++

			prepay, err := m.cfg.Lnd.Client.DecodePaymentRequest(
				ctx, out.Contract.PrepayInvoice,
			)
			if err != nil {
				return nil, err
			}

			summary.pendingFees += worstCaseOutFees(
				out.Contract.MaxPrepayRoutingFee,
				out.Contract.MaxSwapRoutingFee,
				out.Contract.MaxSwapFee,
				out.Contract.MaxMinerFee,
				mSatToSatoshis(prepay.Value),
			)
		} else if !out.LastUpdateTime().Before(m.params.AutoFeeStartDate) {
			summary.spentFees += out.State().Cost.Total()
		}
	}

	return &summary, nil
}

// currentSwapTraffic examines our existing swaps and returns a summary of the
// current activity which can be used to determine whether we should perform
// any swaps.
func (m *Manager) currentSwapTraffic(loopOut []*loopdb.LoopOut,
	loopIn []*loopdb.LoopIn) *swapTraffic {

	traffic := newSwapTraffic()

	// Failure cutoff is the most recent failure timestamp we will still
	// consider a channel eligible. Any channels involved in swaps that have
	// failed since this point will not be considered.
	failureCutoff := m.cfg.Clock.Now().Add(m.params.FailureBackOff * -1)

	for _, out := range loopOut {
		var (
			state   = out.State().State
			chanSet = out.Contract.OutgoingChanSet
		)

		// If a loop out swap failed due to off chain payment after our
		// failure cutoff, we add all of its channels to a set of
		// recently failed channels. It is possible that not all of
		// these channels were used for the swap, but we play it safe
		// and back off for all of them.
		//
		// We only backoff for off temporary failures. In the case of
		// chain payment failures, our swap failed to route and we do
		// not want to repeatedly try to route through bad channels
		// which remain unbalanced because they cannot route a swap, so
		// we backoff.
		if state == loopdb.StateFailOffchainPayments {
			failedAt := out.LastUpdate().Time

			if failedAt.After(failureCutoff) {
				for _, id := range chanSet {
					chanID := lnwire.NewShortChanIDFromInt(
						id,
					)

					traffic.failedLoopOut[chanID] = failedAt
				}
			}
		}

		// Skip completed swaps, they can't affect our channel balances.
		// Swaps that fail temporarily are considered to be in a pending
		// state, so we will also check that channels being used by
		// these swaps. This is important, because a temporarily failed
		// swap could be re-dispatched on restart, affecting our
		// balances.
		if state.Type() != loopdb.StateTypePending {
			continue
		}

		for _, id := range chanSet {
			chanID := lnwire.NewShortChanIDFromInt(id)
			traffic.ongoingLoopOut[chanID] = true
		}
	}

	for _, in := range loopIn {
		// Skip completed swaps, they can't affect our channel balances.
		if in.State().State.Type() != loopdb.StateTypePending {
			continue
		}

		// Skip over swaps that may come through any peer.
		if in.Contract.LastHop == nil {
			continue
		}

		traffic.ongoingLoopIn[*in.Contract.LastHop] = true
	}

	return traffic
}

// swapTraffic contains a summary of our current and previously failed swaps.
type swapTraffic struct {
	ongoingLoopOut map[lnwire.ShortChannelID]bool
	ongoingLoopIn  map[route.Vertex]bool
	failedLoopOut  map[lnwire.ShortChannelID]time.Time
}

func newSwapTraffic() *swapTraffic {
	return &swapTraffic{
		ongoingLoopOut: make(map[lnwire.ShortChannelID]bool),
		ongoingLoopIn:  make(map[route.Vertex]bool),
		failedLoopOut:  make(map[lnwire.ShortChannelID]time.Time),
	}
}

// maySwap returns a boolean that indicates whether we may perform a swap for a
// peer and its set of channels.
func (s *swapTraffic) maySwap(peer route.Vertex,
	channels []lnwire.ShortChannelID) error {

	for _, chanID := range channels {
		lastFail, recentFail := s.failedLoopOut[chanID]
		if recentFail {
			log.Debugf("Channel: %v not eligible for suggestions, was "+
				"part of a failed swap at: %v", chanID, lastFail)

			return newReasonError(ReasonFailureBackoff)
		}

		if s.ongoingLoopOut[chanID] {
			log.Debugf("Channel: %v not eligible for suggestions, "+
				"ongoing loop out utilizing channel", chanID)

			return newReasonError(ReasonLoopOut)
		}
	}

	if s.ongoingLoopIn[peer] {
		log.Debugf("Peer: %x not eligible for suggestions ongoing "+
			"loop in utilizing peer", peer)

		return newReasonError(ReasonLoopIn)
	}

	return nil
}

// satPerKwToSatPerVByte converts sat per kWeight to sat per vByte.
func satPerKwToSatPerVByte(satPerKw chainfee.SatPerKWeight) int64 {
	return int64(satPerKw.FeePerKVByte() / 1000)
}

// ppmToSat takes an amount and a measure of parts per million for the amount
// and returns the amount that the ppm represents.
func ppmToSat(amount btcutil.Amount, ppm uint64) btcutil.Amount {
	return btcutil.Amount(uint64(amount) * ppm / FeeBase)
}

func mSatToSatoshis(amount lnwire.MilliSatoshi) btcutil.Amount {
	return btcutil.Amount(amount / 1000)
}
