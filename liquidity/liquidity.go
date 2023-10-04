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
//   - Sweep Fee Rate Limit: the maximum sat/vByte fee estimate for our sweep
//     transaction to confirm within our configured number of confirmations
//     that we will suggest swaps for.
//   - Maximum Swap Fee PPM: the maximum server fee, expressed as parts per
//     million of the full swap amount
//   - Maximum Routing Fee PPM: the maximum off-chain routing fees for the swap
//     invoice, expressed as parts per million of the swap amount.
//   - Maximum Prepay Routing Fee PPM: the maximum off-chain routing fees for the
//     swap prepayment, expressed as parts per million of the prepay amount.
//   - Maximum Prepay: the maximum now-show fee, expressed in satoshis. This
//     amount is only payable in the case where the swap server broadcasts a htlc
//     and the client fails to sweep the preimage.
//   - Maximum miner fee: the maximum miner fee we are willing to pay to sweep the
//     on chain htlc. Note that the client will use current fee estimates to
//     sweep, so this value acts more as a sanity check in the case of a large fee
//     spike.
//
// The maximum fee per-swap is calculated as follows:
// (swap amount * serverPPM/1e6) + miner fee + (swap amount * routingPPM/1e6)
// + (prepay amount * prepayPPM/1e6).
package liquidity

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/labels"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/ticker"
	"google.golang.org/protobuf/proto"

	clientrpc "github.com/lightninglabs/loop/looprpc"
)

const (
	// defaultFailureBackoff is the default amount of time we backoff if
	// a channel is part of a temporarily failed swap.
	defaultFailureBackoff = time.Hour * 24

	// defaultAmountBackoff is the default backoff we apply to the amount
	// of a loop out swap that failed the off-chain payments.
	defaultAmountBackoff = float64(0.25)

	// defaultAmountBackoffRetry is the default number of times we will
	// perform an amount backoff to a loop out swap before we give up.
	defaultAmountBackoffRetry = 5

	// defaultSwapWaitTimeout is the default maximum amount of time we
	// wait for a swap to reach a terminal state.
	defaultSwapWaitTimeout = time.Hour * 24

	// defaultPaymentCheckInterval is the default time that passes between
	// checks for loop out payments status.
	defaultPaymentCheckInterval = time.Second * 2

	// defaultConfTarget is the default sweep target we use for loop outs.
	// We get our inbound liquidity quickly using preimage push, so we can
	// use a long conf target without worrying about ux impact.
	defaultConfTarget = 100

	// FeeBase is the base that we use to express fees.
	FeeBase = 1e6

	// defaultMaxInFlight is the default number of in-flight automatically
	// dispatched swaps we allow. Note that this does not enable automated
	// swaps itself (because we want non-zero values to be expressed in
	// suggestions as a dry-run).
	defaultMaxInFlight = 1

	// DefaultAutoloopTicker is the default amount of time between automated
	// swap checks.
	DefaultAutoloopTicker = time.Minute * 20

	// autoloopSwapInitiator is the value we send in the initiator field of
	// a swap request when issuing an automatic swap.
	autoloopSwapInitiator = "autoloop"

	// We use a static fee rate to estimate our sweep fee, because we
	// can't realistically estimate what our fee estimate will be by the
	// time we reach timeout. We set this to a high estimate so that we can
	// account for worst-case fees, (1250 * 4 / 1000) = 50 sat/byte.
	defaultLoopInSweepFee = chainfee.SatPerKWeight(1250)
)

var (
	// defaultHtlcConfTarget is the default confirmation target we use for
	// loop in swap htlcs, set to the same default at the client.
	defaultHtlcConfTarget = loop.DefaultHtlcConfTarget

	// defaultBudget is the default autoloop budget we set. This budget will
	// only be used for automatically dispatched swaps if autoloop is
	// explicitly enabled, so we are happy to set a non-zero value here. The
	// amount chosen simply uses the current defaults to provide budget for
	// a single swap. We don't have a swap amount so we just use our max
	// funding amount.
	defaultBudget = ppmToSat(funding.MaxBtcFundingAmount, defaultFeePPM)

	// defaultBudgetRefreshPeriod is the default amount of time we wait for
	// the autoloop budget to be refreshed.
	defaultBudgetRefreshPeriod = time.Hour * 24 * 7

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

	// ErrAmbiguousDestAddr is returned when a destination address and
	// a extended public key account is set.
	ErrAmbiguousDestAddr = errors.New("ambiguous destination address")

	// ErrAccountAndAddrType indicates if an account is set but the
	// account address type is not or vice versa.
	ErrAccountAndAddrType = errors.New("account and address type have " +
		"to be both either set or unset")
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
	Restrictions func(ctx context.Context, swapType swap.Type,
		initiator string) (*Restrictions, error)

	// Lnd provides us with access to lnd's rpc servers.
	Lnd *lndclient.LndServices

	// ListLoopOut returns all of the loop our swaps stored on disk.
	ListLoopOut func(context.Context) ([]*loopdb.LoopOut, error)

	// GetLoopOut returns a single loop out swap based on the provided swap
	// hash.
	GetLoopOut func(ctx context.Context, hash lntypes.Hash) (*loopdb.LoopOut, error)

	// ListLoopIn returns all of the loop in swaps stored on disk.
	ListLoopIn func(ctx context.Context) ([]*loopdb.LoopIn, error)

	// LoopOutQuote gets swap fee, estimated miner fee and prepay amount for
	// a loop out swap.
	LoopOutQuote func(ctx context.Context,
		request *loop.LoopOutQuoteRequest) (*loop.LoopOutQuote, error)

	// LoopInQuote provides a quote for a loop in swap.
	LoopInQuote func(ctx context.Context,
		request *loop.LoopInQuoteRequest) (*loop.LoopInQuote, error)

	// LoopOut dispatches a loop out.
	LoopOut func(ctx context.Context, request *loop.OutRequest) (
		*loop.LoopOutSwapInfo, error)

	// LoopIn dispatches a loop in swap.
	LoopIn func(ctx context.Context,
		request *loop.LoopInRequest) (*loop.LoopInSwapInfo, error)

	// LoopInTerms returns the terms for a loop in swap.
	LoopInTerms func(ctx context.Context,
		initiator string) (*loop.LoopInTerms, error)

	// LoopOutTerms returns the terms for a loop out swap.
	LoopOutTerms func(ctx context.Context,
		initiator string) (*loop.LoopOutTerms, error)

	// Clock allows easy mocking of time in unit tests.
	Clock clock.Clock

	// MinimumConfirmations is the minimum number of confirmations we allow
	// setting for sweep target.
	MinimumConfirmations int32

	// PutLiquidityParams writes the serialized `Parameters` into db.
	//
	// NOTE: the params are encoded using `proto.Marshal` over an RPC
	// request.
	PutLiquidityParams func(ctx context.Context, params []byte) error

	// FetchLiquidityParams reads the serialized `Parameters` from db.
	//
	// NOTE: the params are decoded using `proto.Unmarshal` over a
	// serialized RPC request.
	FetchLiquidityParams func(ctx context.Context) ([]byte, error)
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

	// activeStickyLoops is a counter that helps us keep track of currently
	// active sticky loops. We use this to ensure we don't dispatch more
	// than the max configured loops at a time.
	activeStickyLoops int

	// activeStickyLock is a lock to ensure atomic access to the
	// activeStickyLoops counter.
	activeStickyLock sync.Mutex
}

// Run periodically checks whether we should automatically dispatch a loop out.
// We run this loop even if automated swaps are not currently enabled rather
// than managing starting and stopping the ticker as our parameters are updated.
func (m *Manager) Run(ctx context.Context) error {
	m.cfg.AutoloopTicker.Resume()
	defer m.cfg.AutoloopTicker.Stop()

	// Before we start the main loop, load the params from db.
	req, err := m.loadParams(ctx)
	if err != nil {
		return err
	}

	// Set the params if there's one.
	if req != nil {
		if err := m.SetParameters(ctx, req); err != nil {
			return err
		}
	}

	for {
		select {
		case <-m.cfg.AutoloopTicker.Ticks():
			if m.params.EasyAutoloop {
				err := m.easyAutoLoop(ctx)
				if err != nil {
					log.Errorf("easy autoloop failed: %v",
						err)
				}
			} else {
				err := m.autoloop(ctx)
				switch err {
				case ErrNoRules:
					log.Debugf("no rules configured for " +
						"autoloop")

				case nil:

				default:
					log.Errorf("autoloop failed: %v", err)
				}
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

// SetParameters takes an RPC request and calls the internal method to set
// parameters for the manager.
func (m *Manager) SetParameters(ctx context.Context,
	req *clientrpc.LiquidityParameters) error {

	params, err := RpcToParameters(req)
	if err != nil {
		return err
	}

	if err := m.setParameters(ctx, *params); err != nil {
		return err
	}

	// Save the params on disk.
	//
	// NOTE: alternatively we can save the bytes in memory and persist them
	// on disk during shutdown to save us some IO cost from hitting the db.
	// Since setting params is NOT a frequent action, it's should put
	// little pressure on our db. Only when performance becomes an issue,
	// we can then apply the alternative.
	return m.saveParams(ctx, req)
}

// SetParameters updates our current set of parameters if the new parameters
// provided are valid.
func (m *Manager) setParameters(ctx context.Context,
	params Parameters) error {

	restrictions, err := m.cfg.Restrictions(
		ctx, swap.TypeOut, getInitiator(m.params),
	)
	if err != nil {
		return err
	}

	channels, err := m.cfg.Lnd.Client.ListChannels(ctx, false, false)
	if err != nil {
		return err
	}

	err = params.validate(
		m.cfg.MinimumConfirmations, channels, restrictions,
	)
	if err != nil {
		return err
	}

	m.paramsLock.Lock()
	defer m.paramsLock.Unlock()

	m.params = cloneParameters(params)

	return nil
}

// saveParams marshals an RPC request and saves it to db.
func (m *Manager) saveParams(ctx context.Context, req proto.Message) error {
	// Marshal the params.
	paramsBytes, err := proto.Marshal(req)
	if err != nil {
		return err
	}

	// Save the params on disk.
	if err := m.cfg.PutLiquidityParams(ctx, paramsBytes); err != nil {
		return fmt.Errorf("failed to save params: %v", err)
	}

	return nil
}

// loadParams unmarshals a serialized RPC request from db and returns the RPC
// request.
func (m *Manager) loadParams(ctx context.Context) (
	*clientrpc.LiquidityParameters, error) {

	paramsBytes, err := m.cfg.FetchLiquidityParams(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read params: %v", err)
	}

	// Return early if there's nothing saved.
	if paramsBytes == nil {
		return nil, nil
	}

	// Unmarshal the params.
	req := &clientrpc.LiquidityParameters{}
	err = proto.Unmarshal(paramsBytes, req)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal params: %v", err)
	}

	return req, nil
}

// autoloop gets a set of suggested swaps and dispatches them automatically if
// we have automated looping enabled.
func (m *Manager) autoloop(ctx context.Context) error {
	// First check if we should refresh our budget before calculating any
	// swaps for autoloop.
	m.refreshAutoloopBudget(ctx)

	suggestion, err := m.SuggestSwaps(ctx)
	if err != nil {
		return err
	}

	for _, swap := range suggestion.OutSwaps {
		// If we don't actually have dispatch of swaps enabled, log
		// suggestions.
		if !m.params.Autoloop {
			log.Debugf("recommended autoloop out: %v sats over "+
				"%v", swap.Amount, swap.OutgoingChanSet)

			continue
		}

		// Create a copy of our range var so that we can reference it.
		swap := swap

		go m.dispatchStickyLoopOut(
			ctx, swap, defaultAmountBackoffRetry,
			defaultAmountBackoff,
		)
	}

	for _, in := range suggestion.InSwaps {
		// If we don't actually have dispatch of swaps enabled, log
		// suggestions.
		if !m.params.Autoloop {
			log.Debugf("recommended autoloop in: %v sats over "+
				"%v", in.Amount, in.LastHop)

			continue
		}

		in := in
		loopIn, err := m.cfg.LoopIn(ctx, &in)
		if err != nil {
			return err
		}

		log.Infof("loop in automatically dispatched: hash: %v, "+
			"address: p2wsh(%v), p2tr(%v)", loopIn.SwapHash,
			loopIn.HtlcAddressP2WSH, loopIn.HtlcAddressP2TR)
	}

	return nil
}

// easyAutoLoop is the main entry point for the easy auto loop functionality.
// This function will try to dispatch a swap in order to meet the easy autoloop
// requirements. For easyAutoloop to work there needs to be an
// EasyAutoloopTarget defined in the parameters. Easy autoloop also uses the
// configured max inflight swaps and budget rules defined in the parameters.
func (m *Manager) easyAutoLoop(ctx context.Context) error {
	if !m.params.Autoloop {
		return nil
	}

	// First check if we should refresh our budget before calculating any
	// swaps for autoloop.
	m.refreshAutoloopBudget(ctx)

	// Dispatch the best easy autoloop swap.
	err := m.dispatchBestEasyAutoloopSwap(ctx)
	if err != nil {
		return err
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

// dispatchBestEasyAutoloopSwap tries to dispatch a swap to bring the total
// local balance back to the target.
func (m *Manager) dispatchBestEasyAutoloopSwap(ctx context.Context) error {
	// Retrieve existing swaps.
	loopOut, err := m.cfg.ListLoopOut(ctx)
	if err != nil {
		return err
	}

	loopIn, err := m.cfg.ListLoopIn(ctx)
	if err != nil {
		return err
	}

	// Get a summary of our existing swaps so that we can check our autoloop
	// budget.
	summary := m.checkExistingAutoLoops(ctx, loopOut, loopIn)

	err = m.checkSummaryBudget(summary)
	if err != nil {
		return err
	}

	_, err = m.checkSummaryInflight(summary)
	if err != nil {
		return err
	}

	// Get all channels in order to calculate current total local balance.
	channels, err := m.cfg.Lnd.Client.ListChannels(ctx, false, false)
	if err != nil {
		return err
	}

	localTotal := btcutil.Amount(0)
	for _, channel := range channels {
		localTotal += channel.LocalBalance
	}

	// Since we're only autolooping-out we need to check if we are below
	// the target, meaning that we already meet the requirements.
	if localTotal <= m.params.EasyAutoloopTarget {
		log.Debugf("total local balance %v below target %v",
			localTotal, m.params.EasyAutoloopTarget)
		return nil
	}

	restrictions, err := m.cfg.Restrictions(
		ctx, swap.TypeOut, getInitiator(m.params),
	)
	if err != nil {
		return err
	}

	// Calculate the amount that we want to loop out. If it exceeds the max
	// allowed clamp it to max.
	amount := localTotal - m.params.EasyAutoloopTarget
	if amount > restrictions.Maximum {
		amount = restrictions.Maximum
	}

	// If the amount we want to loop out is less than the minimum we can't
	// proceed with a swap, so we return early.
	if amount < restrictions.Minimum {
		log.Debugf("easy autoloop: swap amount is below minimum swap "+
			"size, minimum=%v, need to swap %v",
			restrictions.Minimum, amount)
		return nil
	}

	log.Debugf("easy autoloop: local_total=%v, target=%v, "+
		"attempting to loop out %v", localTotal,
		m.params.EasyAutoloopTarget, amount)

	// Start building that swap.
	builder := newLoopOutBuilder(m.cfg)

	channel := m.pickEasyAutoloopChannel(
		channels, restrictions, loopOut, loopIn,
	)
	if channel == nil {
		return fmt.Errorf("no eligible channel for easy autoloop")
	}

	log.Debugf("easy autoloop: picked channel %v with local balance %v",
		channel.ChannelID, channel.LocalBalance)

	swapAmt, err := btcutil.NewAmount(
		math.Min(channel.LocalBalance.ToBTC(), amount.ToBTC()),
	)
	if err != nil {
		return err
	}

	// If no fee is set, override our current parameters in order to use the
	// default percent limit of easy-autoloop.
	easyParams := m.params

	switch feeLimit := easyParams.FeeLimit.(type) {
	case *FeePortion:
		if feeLimit.PartsPerMillion == 0 {
			easyParams.FeeLimit = &FeePortion{
				PartsPerMillion: defaultFeePPM,
			}
		}
	default:
		easyParams.FeeLimit = &FeePortion{
			PartsPerMillion: defaultFeePPM,
		}
	}

	// Set the swap outgoing channel to the chosen channel.
	outgoing := []lnwire.ShortChannelID{
		lnwire.NewShortChanIDFromInt(channel.ChannelID),
	}

	suggestion, err := builder.buildSwap(
		ctx, channel.PubKeyBytes, outgoing, swapAmt, easyParams,
	)
	if err != nil {
		return err
	}

	var swp loop.OutRequest
	if t, ok := suggestion.(*loopOutSwapSuggestion); ok {
		swp = t.OutRequest
	} else {
		return fmt.Errorf("unexpected swap suggestion type: %T", t)
	}

	// Dispatch a sticky loop out.
	go m.dispatchStickyLoopOut(
		ctx, swp, defaultAmountBackoffRetry, defaultAmountBackoff,
	)

	return nil
}

// Suggestions provides a set of suggested swaps, and the set of channels that
// were excluded from consideration.
type Suggestions struct {
	// OutSwaps is the set of loop out swaps that we suggest executing.
	OutSwaps []loop.OutRequest

	// InSwaps is the set of loop in swaps that we suggest executing.
	InSwaps []loop.LoopInRequest

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
	switch t := swap.(type) {
	case *loopOutSwapSuggestion:
		s.OutSwaps = append(s.OutSwaps, t.OutRequest)

	case *loopInSwapSuggestion:
		s.InSwaps = append(s.InSwaps, t.LoopInRequest)

	default:
		return fmt.Errorf("unexpected swap type: %T", swap)
	}

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
func (m *Manager) SuggestSwaps(ctx context.Context) (
	*Suggestions, error) {

	m.paramsLock.Lock()
	defer m.paramsLock.Unlock()

	// If we have no rules set, exit early to avoid unnecessary calls to
	// lnd and the server.
	if !m.params.haveRules() {
		return nil, ErrNoRules
	}

	// Get restrictions placed on swaps by the server.
	outRestrictions, err := m.getSwapRestrictions(ctx, swap.TypeOut)
	if err != nil {
		return nil, err
	}

	inRestrictions, err := m.getSwapRestrictions(ctx, swap.TypeIn)
	if err != nil {
		return nil, err
	}

	// List our current set of swaps so that we can determine which channels
	// are already being utilized by swaps. Note that these calls may race
	// with manual initiation of swaps.
	loopOut, err := m.cfg.ListLoopOut(ctx)
	if err != nil {
		return nil, err
	}

	loopIn, err := m.cfg.ListLoopIn(ctx)
	if err != nil {
		return nil, err
	}

	// Get a summary of our existing swaps so that we can check our autoloop
	// budget.
	summary := m.checkExistingAutoLoops(ctx, loopOut, loopIn)

	err = m.checkSummaryBudget(summary)
	if err != nil {
		return m.singleReasonSuggestion(ReasonBudgetElapsed), nil
	}

	allowedSwaps, err := m.checkSummaryInflight(summary)
	if err != nil {
		return m.singleReasonSuggestion(ReasonInFlight), nil
	}

	channels, err := m.cfg.Lnd.Client.ListChannels(ctx, false, false)
	if err != nil {
		return nil, err
	}

	// Collect a map of channel IDs to peer pubkeys, and a set of per-peer
	// balances which we will use for peer-level liquidity rules.
	channelPeers := make(map[uint64]route.Vertex)
	peerChannels := make(map[route.Vertex]*balances)
	for _, channel := range channels {
		channelPeers[channel.ChannelID] = channel.PubKeyBytes

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
			ctx, traffic, balances, rule, outRestrictions,
			inRestrictions,
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
			ctx, traffic, balance, rule, outRestrictions,
			inRestrictions,
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
		for _, peer := range swap.peers(channelPeers) {
			_, ok := m.params.PeerRules[peer]
			if !ok {
				continue
			}

			resp.DisqualifiedPeers[peer] = reason
		}

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
			refreshTime := m.params.AutoFeeRefreshPeriod -
				time.Since(m.params.AutoloopBudgetLastRefresh)

			log.Infof("Swap fee exceeds budget, remaining budget: "+
				"%v, swap fee %v, next budget refresh: %v",
				available, fees, refreshTime)
			setReason(ReasonBudgetInsufficient, swap)
		}
	}

	return resp, nil
}

// suggestSwap checks whether we can currently perform a swap, and creates a
// swap request for the rule provided.
func (m *Manager) suggestSwap(ctx context.Context, traffic *swapTraffic,
	balance *balances, rule *SwapRule, outRestrictions *Restrictions,
	inRestrictions *Restrictions) (swapSuggestion, error) {

	var (
		builder      swapBuilder
		restrictions *Restrictions
	)

	// Get an appropriate builder and set of restrictions based on our swap
	// type.
	switch rule.Type {
	case swap.TypeOut:
		builder = newLoopOutBuilder(m.cfg)
		restrictions = outRestrictions

	case swap.TypeIn:
		builder = newLoopInBuilder(m.cfg)
		restrictions = inRestrictions

	default:
		return nil, fmt.Errorf("unsupported swap type: %v", rule.Type)
	}

	// Before we get any swap suggestions, we check what the current fee
	// estimate is to sweep within our target number of confirmations. If
	// This fee exceeds the fee limit we have set, we will not suggest any
	// swaps at present.
	if err := builder.maySwap(ctx, m.params); err != nil {
		return nil, err
	}

	// First, check whether this peer/channel combination is already in use
	// for our swap.
	err := builder.inUse(traffic, balance.pubkey, balance.channels)
	if err != nil {
		return nil, err
	}

	// Next, get the amount that we need to swap for this entity, skipping
	// over it if no change in liquidity is required.
	amount := rule.swapAmount(balance, restrictions, rule.Type)
	if amount == 0 {
		return nil, newReasonError(ReasonLiquidityOk)
	}

	return builder.buildSwap(
		ctx, balance.pubkey, balance.channels, amount, m.params,
	)
}

// getSwapRestrictions queries the server for its latest swap size restrictions,
// validates client restrictions (if present) against these values and merges
// the client's custom requirements with the server's limits to produce a single
// set of limitations for our swap.
func (m *Manager) getSwapRestrictions(ctx context.Context, swapType swap.Type) (
	*Restrictions, error) {

	restrictions, err := m.cfg.Restrictions(
		ctx, swapType, getInitiator(m.params),
	)
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

// worstCaseOutFees calculates the largest possible fees for a loop out swap,
// comparing the fees for a successful swap to the cost when the client pays
// the prepay because they failed to sweep the on chain htlc. This is unlikely,
// because we expect clients to be online to sweep, but we want to account for
// every outcome so we include it.
func worstCaseOutFees(prepayRouting, swapRouting, swapFee,
	minerFee btcutil.Amount) btcutil.Amount {

	return prepayRouting + minerFee + swapFee + swapRouting
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
func (m *Manager) checkExistingAutoLoops(_ context.Context,
	loopOuts []*loopdb.LoopOut,
	loopIns []*loopdb.LoopIn) *existingAutoLoopSummary {

	var summary existingAutoLoopSummary

	for _, out := range loopOuts {
		if !isAutoloopLabel(out.Contract.Label) {
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

			summary.pendingFees += worstCaseOutFees(
				out.Contract.MaxPrepayRoutingFee,
				out.Contract.MaxSwapRoutingFee,
				out.Contract.MaxSwapFee,
				out.Contract.MaxMinerFee,
			)
		} else if out.LastUpdateTime().After(
			m.params.AutoloopBudgetLastRefresh,
		) {

			summary.spentFees += out.State().Cost.Total()
		}
	}

	for _, in := range loopIns {
		if !isAutoloopLabel(in.Contract.Label) {
			continue
		}

		pending := in.State().State.Type() == loopdb.StateTypePending
		inBudget := !in.LastUpdateTime().
			Before(m.params.AutoloopBudgetLastRefresh)

		// If an autoloop is in a pending state, we always count it in
		// our current budget, and record the worst-case fees for it,
		// because we do not know how it will resolve.
		if pending {
			summary.inFlightCount++
			summary.pendingFees += worstCaseInFees(
				in.Contract.MaxMinerFee, in.Contract.MaxSwapFee,
				defaultLoopInSweepFee,
			)
		} else if inBudget {
			summary.spentFees += in.State().Cost.Total()
		}
	}

	return &summary
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
		// Skip over swaps that may come through any peer.
		if in.Contract.LastHop == nil {
			continue
		}

		pubkey := *in.Contract.LastHop

		switch {
		// Include any pending swaps in our ongoing set of swaps. Swaps
		// that reached InvoiceSettled are not considered ongoing since
		// from the client's perspective the swap is complete. This
		// consideration allows the client to dispatch the next autoloop
		// in once an invoice for a previous swap is settled.
		case in.State().State.Type() == loopdb.StateTypePending &&
			in.State().State != loopdb.StateInvoiceSettled:

			traffic.ongoingLoopIn[pubkey] = true

		// If a swap failed with an on-chain timeout, the server could
		// not route to us. We add it to our backoff list so that
		// there's some time for routing conditions to improve.
		case in.State().State == loopdb.StateFailTimeout:
			failedAt := in.LastUpdate().Time

			if failedAt.After(failureCutoff) {
				traffic.failedLoopIn[pubkey] = failedAt
			}
		}
	}

	return traffic
}

// refreshAutoloopBudget checks whether the elapsed time since our last autoloop
// budget refresh is greater than our configured refresh period. If so, the last
// refresh timestamp.
func (m *Manager) refreshAutoloopBudget(ctx context.Context) {
	if time.Since(m.params.AutoloopBudgetLastRefresh) >
		m.params.AutoFeeRefreshPeriod {

		log.Debug("Refreshing autoloop budget")
		m.params.AutoloopBudgetLastRefresh = m.cfg.Clock.Now()

		paramsRpc, err := ParametersToRpc(m.params)
		if err != nil {
			log.Errorf("Error converting parameters to rpc: %v",
				err)
			return
		}

		err = m.saveParams(ctx, paramsRpc)
		if err != nil {
			log.Errorf("Error saving parameters: %v", err)
		}
	}
}

// dispatchStickyLoopOut attempts to dispatch a loop out swap that will
// automatically retry its execution with an amount based backoff.
func (m *Manager) dispatchStickyLoopOut(ctx context.Context,
	out loop.OutRequest, retryCount uint16, amountBackoff float64) {

	// Check our sticky loop counter to decide whether we should continue
	// executing this loop.
	m.activeStickyLock.Lock()
	if m.activeStickyLoops >= m.params.MaxAutoInFlight {
		m.activeStickyLock.Unlock()
		return
	}

	m.activeStickyLoops += 1
	m.activeStickyLock.Unlock()

	// No matter the outcome, decrease the counter upon exiting sticky loop.
	defer func() {
		m.activeStickyLock.Lock()
		m.activeStickyLoops -= 1
		m.activeStickyLock.Unlock()
	}()

	for i := 0; i < int(retryCount); i++ {
		// Dispatch the swap.
		swap, err := m.cfg.LoopOut(ctx, &out)
		if err != nil {
			log.Errorf("unable to dispatch loop out, amt: %v, "+
				"err: %v", out.Amount, err)
			return
		}

		log.Infof("loop out automatically dispatched: hash: %v, "+
			"address: %v, amount %v", swap.SwapHash,
			swap.HtlcAddress, out.Amount)

		updates := make(chan *loopdb.SwapState, 1)

		// Monitor the swap state and write the desired update to the
		// update channel. We do not want to read all of the swap state
		// updates, just the one that will help us assume the state of
		// the off-chain payment.
		go m.waitForSwapPayment(
			ctx, swap.SwapHash, updates, defaultSwapWaitTimeout,
		)

		select {
		case <-ctx.Done():
			return

		case update := <-updates:
			if update == nil {
				// If update is nil then no update occurred
				// within the defined timeout period. It's
				// better to return and not attempt a retry.
				log.Debug(
					"No payment update received for swap "+
						"%v, skipping amount backoff",
					swap.SwapHash,
				)

				return
			}

			if *update == loopdb.StateFailOffchainPayments {
				// Save the old amount so we can log it.
				oldAmt := out.Amount

				// If we failed to pay the server, we will
				// decrease the amount of the swap and try
				// again.
				out.Amount -= btcutil.Amount(
					float64(out.Amount) * amountBackoff,
				)

				log.Infof("swap %v: amount backoff old amount="+
					"%v, new amount=%v", swap.SwapHash,
					oldAmt, out.Amount)

				continue
			} else {
				// If the update channel did not return an
				// off-chain payment failure we won't retry.
				return
			}
		}
	}
}

// waitForSwapPayment waits for a swap to progress beyond the stage of
// forwarding the payment to the server through the network. It returns the
// final update on the outcome through a channel.
func (m *Manager) waitForSwapPayment(ctx context.Context, swapHash lntypes.Hash,
	updateChan chan *loopdb.SwapState, timeout time.Duration) {

	startTime := time.Now()
	var (
		swap     *loopdb.LoopOut
		err      error
		interval time.Duration
	)

	if m.params.CustomPaymentCheckInterval != 0 {
		interval = m.params.CustomPaymentCheckInterval
	} else {
		interval = defaultPaymentCheckInterval
	}

	for time.Since(startTime) < timeout {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}

		swap, err = m.cfg.GetLoopOut(ctx, swapHash)
		if err != nil {
			log.Errorf(
				"Error getting swap with hash %x: %v", swapHash,
				err,
			)
			continue
		}

		// If no update has occurred yet, continue in order to wait.
		update := swap.LastUpdate()
		if update == nil {
			continue
		}

		// Write the update if the swap has reached a state the helps
		// us determine whether the off-chain payment successfully
		// reached the destination.
		switch update.State {
		case loopdb.StateFailInsufficientValue:
			fallthrough
		case loopdb.StateSuccess:
			fallthrough
		case loopdb.StateFailSweepTimeout:
			fallthrough
		case loopdb.StateFailTimeout:
			fallthrough
		case loopdb.StatePreimageRevealed:
			fallthrough
		case loopdb.StateFailOffchainPayments:
			updateChan <- &update.State
			return
		}
	}

	// If no update occurred within the defined timeout we return an empty
	// update to the channel, causing the sticky loop out to not retry
	// anymore.
	updateChan <- nil
}

// pickEasyAutoloopChannel picks a channel to be used for an easy autoloop swap.
// This function prioritizes channels with high local balance but also consults
// previous failures and ongoing swaps to avoid temporary channel failures or
// swap conflicts.
func (m *Manager) pickEasyAutoloopChannel(channels []lndclient.ChannelInfo,
	restrictions *Restrictions, loopOut []*loopdb.LoopOut,
	loopIn []*loopdb.LoopIn) *lndclient.ChannelInfo {

	traffic := m.currentSwapTraffic(loopOut, loopIn)

	// Sort the candidate channels based on descending local balance. We
	// want to prioritize picking a channel with the highest possible local
	// balance.
	sort.Slice(channels, func(i, j int) bool {
		return channels[i].LocalBalance > channels[j].LocalBalance
	})

	// Check each channel, since channels are already sorted we return the
	// first channel that passes all checks.
	for _, channel := range channels {
		shortChanID := lnwire.NewShortChanIDFromInt(channel.ChannelID)

		if !channel.Active {
			log.Debugf("Channel %v cannot be used for easy "+
				"autoloop: inactive", channel.ChannelID)
			continue
		}

		lastFail, recentFail := traffic.failedLoopOut[shortChanID]
		if recentFail {
			log.Debugf("Channel %v cannot be used for easy "+
				"autoloop: last failed swap was at %v",
				channel.ChannelID, lastFail)
			continue
		}

		if traffic.ongoingLoopOut[shortChanID] {
			log.Debugf("Channel %v cannot be used for easy "+
				"autoloop: ongoing swap", channel.ChannelID)
			continue
		}

		if channel.LocalBalance < restrictions.Minimum {
			log.Debugf("Channel %v cannot be used for easy "+
				"autoloop: insufficient local balance %v,"+
				"minimum is %v, skipping remaining channels",
				channel.ChannelID, channel.LocalBalance,
				restrictions.Minimum)
			return nil
		}

		return &channel
	}

	return nil
}

func (m *Manager) numActiveStickyLoops() int {
	m.activeStickyLock.Lock()
	defer m.activeStickyLock.Unlock()

	return m.activeStickyLoops
}

func (m *Manager) checkSummaryBudget(summary *existingAutoLoopSummary) error {
	if summary.totalFees() >= m.params.AutoFeeBudget {
		return fmt.Errorf("autoloop fee budget: %v exhausted, %v spent on "+
			"completed swaps, %v reserved for ongoing swaps "+
			"(upper limit)",
			m.params.AutoFeeBudget, summary.spentFees,
			summary.pendingFees)
	}

	return nil
}

func (m *Manager) checkSummaryInflight(
	summary *existingAutoLoopSummary) (int, error) {
	// If we have already reached our total allowed number of in flight
	// swaps we return early.
	allowedSwaps := m.params.MaxAutoInFlight - summary.inFlightCount
	if allowedSwaps <= 0 {
		return 0, fmt.Errorf("%v autoloops allowed, %v in flight",
			m.params.MaxAutoInFlight, summary.inFlightCount)
	}

	return allowedSwaps, nil
}

func getInitiator(params Parameters) string {
	if params.EasyAutoloop {
		return "easy-autoloop"
	}

	return "autoloop"
}

// isAutoloopLabel is a helper function that returns a flag indicating whether
// the provided label corresponds to an autoloop swap.
func isAutoloopLabel(label string) bool {
	switch label {
	case labels.AutoloopLabel(swap.TypeOut):
		return true

	case labels.AutoloopLabel(swap.TypeIn):
		return true

	case labels.EasyAutoloopLabel(swap.TypeOut):
		return true

	case labels.EasyAutoloopLabel(swap.TypeIn):
		return true
	}

	return false
}

// swapTraffic contains a summary of our current and previously failed swaps.
type swapTraffic struct {
	ongoingLoopOut map[lnwire.ShortChannelID]bool
	ongoingLoopIn  map[route.Vertex]bool
	failedLoopOut  map[lnwire.ShortChannelID]time.Time
	failedLoopIn   map[route.Vertex]time.Time
}

func newSwapTraffic() *swapTraffic {
	return &swapTraffic{
		ongoingLoopOut: make(map[lnwire.ShortChannelID]bool),
		ongoingLoopIn:  make(map[route.Vertex]bool),
		failedLoopOut:  make(map[lnwire.ShortChannelID]time.Time),
		failedLoopIn:   make(map[route.Vertex]time.Time),
	}
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
