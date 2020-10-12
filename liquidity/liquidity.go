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
	"github.com/lightningnetwork/lnd"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

const (
	// defaultFailureBackoff is the default amount of time we backoff if
	// a channel is part of a temporarily failed swap.
	defaultFailureBackoff = time.Hour * 24

	// FeeBase is the base that we use to express fees.
	FeeBase = 1e6

	// defaultSwapFeePPM is the default limit we place on swap fees,
	// expressed as parts per million of swap volume, 0.5%.
	defaultSwapFeePPM = 5000

	// defaultRoutingFeePPM is the default limit we place on routing fees
	// for the swap invoice, expressed as parts per million of swap volume,
	// 1%.
	defaultRoutingFeePPM = 10000

	// defaultRoutingFeePPM is the default limit we place on routing fees
	// for the prepay invoice, expressed as parts per million of prepay
	// volume, 0.5%.
	defaultPrepayRoutingFeePPM = 5000

	// defaultMaximumMinerFee is the default limit we place on miner fees
	// per swap. We apply a multiplier to this default fee to guard against
	// the case where we have broadcast the preimage, then fees spike and
	// we need to sweep the preimage.
	defaultMaximumMinerFee = 15000 * 100

	// defaultMaximumPrepay is the default limit we place on prepay
	// invoices.
	defaultMaximumPrepay = 30000

	// defaultSweepFeeRateLimit is the default limit we place on estimated
	// sweep fees, (750 * 4 /1000 = 3 sat/vByte).
	defaultSweepFeeRateLimit = chainfee.SatPerKWeight(750)
)

var (
	// defaultBudget is the default autoloop budget we set. This budget will
	// only be used for automatically dispatched swaps if autoloop is
	// explicitly enabled, so we are happy to set a non-zero value here. The
	// amount chosen simply uses the current defaults to provide budget for
	// a single swap. We don't have a swap amount to calculate our maximum
	// routing fee, so we use 0.16 BTC for now.
	defaultBudget = defaultMaximumMinerFee +
		ppmToSat(lnd.MaxBtcFundingAmount, defaultSwapFeePPM) +
		ppmToSat(defaultMaximumPrepay, defaultPrepayRoutingFeePPM) +
		ppmToSat(lnd.MaxBtcFundingAmount, defaultRoutingFeePPM)

	// defaultParameters contains the default parameters that we start our
	// liquidity manger with.
	defaultParameters = Parameters{
		AutoFeeBudget:              defaultBudget,
		ChannelRules:               make(map[lnwire.ShortChannelID]*ThresholdRule),
		FailureBackOff:             defaultFailureBackoff,
		SweepFeeRateLimit:          defaultSweepFeeRateLimit,
		SweepConfTarget:            loop.DefaultSweepConfTarget,
		MaximumSwapFeePPM:          defaultSwapFeePPM,
		MaximumRoutingFeePPM:       defaultRoutingFeePPM,
		MaximumPrepayRoutingFeePPM: defaultPrepayRoutingFeePPM,
		MaximumMinerFee:            defaultMaximumMinerFee,
		MaximumPrepay:              defaultMaximumPrepay,
	}

	// ErrZeroChannelID is returned if we get a rule for a 0 channel ID.
	ErrZeroChannelID = fmt.Errorf("zero channel ID not allowed")

	// ErrInvalidSweepFeeRateLimit is returned if an invalid sweep fee limit
	// is set.
	ErrInvalidSweepFeeRateLimit = fmt.Errorf("sweep fee rate limit must "+
		"be > %v sat/vByte",
		satPerKwToSatPerVByte(chainfee.AbsoluteFeePerKwFloor))

	// ErrZeroMinerFee is returned if a zero maximum miner fee is set.
	ErrZeroMinerFee = errors.New("maximum miner fee must be non-zero")

	// ErrZeroSwapFeePPM is returned if a zero server fee ppm is set.
	ErrZeroSwapFeePPM = errors.New("swap fee PPM must be non-zero")

	// ErrZeroRoutingPPM is returned if a zero routing fee ppm is set.
	ErrZeroRoutingPPM = errors.New("routing fee PPM must be non-zero")

	// ErrZeroPrepayPPM is returned if a zero prepay routing fee ppm is set.
	ErrZeroPrepayPPM = errors.New("prepay routing fee PPM must be non-zero")

	// ErrZeroPrepay is returned if a zero maximum prepay is set.
	ErrZeroPrepay = errors.New("maximum prepay must be non-zero")

	// ErrNegativeBudget is returned if a negative swap budget is set.
	ErrNegativeBudget = errors.New("swap budget must be >= 0")
)

// Config contains the external functionality required to run the
// liquidity manager.
type Config struct {
	// LoopOutRestrictions returns the restrictions that the server applies
	// to loop out swaps.
	LoopOutRestrictions func(ctx context.Context) (*Restrictions, error)

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

	// Clock allows easy mocking of time in unit tests.
	Clock clock.Clock

	// MinimumConfirmations is the minimum number of confirmations we allow
	// setting for sweep target.
	MinimumConfirmations int32
}

// Parameters is a set of parameters provided by the user which guide
// how we assess liquidity.
type Parameters struct {
	// AutoFeeBudget is the total amount we allow to be spent on
	// automatically dispatched swaps. Once this budget has been used, we
	// will stop dispatching swaps until the budget is increased or the
	// start date is moved.
	AutoFeeBudget btcutil.Amount

	// AutoFeeStartDate is the date from which we will include automatically
	// dispatched swaps in our current budget, inclusive.
	AutoFeeStartDate time.Time

	// FailureBackOff is the amount of time that we require passes after a
	// channel has been part of a failed loop out swap before we suggest
	// using it again.
	// TODO(carla): add exponential backoff
	FailureBackOff time.Duration

	// SweepFeeRateLimit is the limit that we place on our estimated sweep
	// fee. A swap will not be suggested if estimated fee rate is above this
	// value.
	SweepFeeRateLimit chainfee.SatPerKWeight

	// SweepConfTarget is the number of blocks we aim to confirm our sweep
	// transaction in. This value affects the on chain fees we will pay.
	SweepConfTarget int32

	// MaximumPrepay is the maximum prepay amount we are willing to pay per
	// swap.
	MaximumPrepay btcutil.Amount

	// MaximumSwapFeePPM is the maximum server fee we are willing to pay per
	// swap expressed as parts per million of the swap volume.
	MaximumSwapFeePPM int

	// MaximumRoutingFeePPM is the maximum off-chain routing fee we
	// are willing to pay for off chain invoice routing fees per swap,
	// expressed as parts per million of the swap amount.
	MaximumRoutingFeePPM int

	// MaximumPrepayRoutingFeePPM is the maximum off-chain routing fee we
	// are willing to pay for off chain prepay routing fees per swap,
	// expressed as parts per million of the prepay amount.
	MaximumPrepayRoutingFeePPM int

	// MaximumMinerFee is the maximum on chain fee that we cap our miner
	// fee at in case where we need to claim on chain because we have
	// revealed the preimage, but fees have spiked. We will not initiate a
	// swap if we estimate that the sweep cost will be above our sweep
	// fee limit, and we use fee estimates at time of sweep to set our fees,
	// so this is just a sane cap covering the special case where we need to
	// sweep during a fee spike.
	MaximumMinerFee btcutil.Amount

	// ChannelRules maps a short channel ID to a rule that describes how we
	// would like liquidity to be managed.
	ChannelRules map[lnwire.ShortChannelID]*ThresholdRule
}

// String returns the string representation of our parameters.
func (p Parameters) String() string {
	channelRules := make([]string, 0, len(p.ChannelRules))

	for channel, rule := range p.ChannelRules {
		channelRules = append(
			channelRules, fmt.Sprintf("%v: %v", channel, rule),
		)
	}

	return fmt.Sprintf("channel rules: %v, failure backoff: %v, sweep "+
		"fee rate limit: %v, sweep conf target: %v, maximum prepay: "+
		"%v, maximum miner fee: %v, maximum swap fee ppm: %v, maximum "+
		"routing fee ppm: %v, maximum prepay routing fee ppm: %v, "+
		"auto budget: %v, budget start: %v",
		strings.Join(channelRules, ","), p.FailureBackOff,
		p.SweepFeeRateLimit, p.SweepConfTarget, p.MaximumPrepay,
		p.MaximumMinerFee, p.MaximumSwapFeePPM,
		p.MaximumRoutingFeePPM, p.MaximumPrepayRoutingFeePPM,
		p.AutoFeeBudget, p.AutoFeeStartDate)
}

// validate checks whether a set of parameters is valid. It takes the minimum
// confirmations we allow for sweep confirmation target as a parameter.
func (p Parameters) validate(minConfs int32) error {
	for channel, rule := range p.ChannelRules {
		if channel.ToUint64() == 0 {
			return ErrZeroChannelID
		}

		if err := rule.validate(); err != nil {
			return fmt.Errorf("channel: %v has invalid rule: %v",
				channel.ToUint64(), err)
		}
	}

	// Check that our sweep limit is above our minimum fee rate. We use
	// absolute fee floor rather than kw floor because we will allow users
	// to specify fee rate is sat/vByte and want to allow 1 sat/vByte.
	if p.SweepFeeRateLimit < chainfee.AbsoluteFeePerKwFloor {
		return ErrInvalidSweepFeeRateLimit
	}

	// Check that our confirmation target is above our required minimum.
	if p.SweepConfTarget < minConfs {
		return fmt.Errorf("confirmation target must be at least: %v",
			minConfs)
	}

	// Check that we have non-zero fee limits.
	if p.MaximumSwapFeePPM == 0 {
		return ErrZeroSwapFeePPM
	}

	if p.MaximumRoutingFeePPM == 0 {
		return ErrZeroRoutingPPM
	}

	if p.MaximumPrepayRoutingFeePPM == 0 {
		return ErrZeroPrepayPPM
	}

	if p.MaximumPrepay == 0 {
		return ErrZeroPrepay
	}

	if p.MaximumMinerFee == 0 {
		return ErrZeroMinerFee
	}

	if p.AutoFeeBudget < 0 {
		return ErrNegativeBudget
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
func (m *Manager) SetParameters(params Parameters) error {
	if err := params.validate(m.cfg.MinimumConfirmations); err != nil {
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

	return paramCopy
}

// SuggestSwaps returns a set of swap suggestions based on our current liquidity
// balance for the set of rules configured for the manager, failing if there are
// no rules set.
func (m *Manager) SuggestSwaps(ctx context.Context) (
	[]loop.OutRequest, error) {

	m.paramsLock.Lock()
	defer m.paramsLock.Unlock()

	// If we have no rules set, exit early to avoid unnecessary calls to
	// lnd and the server.
	if len(m.params.ChannelRules) == 0 {
		return nil, nil
	}

	// If our start date is in the future, we interpret this as meaning that
	// we should start using our budget at this date. This means that we
	// have no budget for the present, so we just return.
	if m.params.AutoFeeStartDate.After(m.cfg.Clock.Now()) {
		log.Debugf("autoloop fee budget start time: %v is in "+
			"the future", m.params.AutoFeeStartDate)

		return nil, nil
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

	if estimate > m.params.SweepFeeRateLimit {
		log.Debugf("Current fee estimate to sweep within: %v blocks "+
			"%v sat/vByte exceeds limit of: %v sat/vByte",
			m.params.SweepConfTarget,
			satPerKwToSatPerVByte(estimate),
			satPerKwToSatPerVByte(m.params.SweepFeeRateLimit))

		return nil, nil
	}

	// Get the current server side restrictions.
	outRestrictions, err := m.cfg.LoopOutRestrictions(ctx)
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

		return nil, nil
	}

	eligible, err := m.getEligibleChannels(ctx, loopOut, loopIn)
	if err != nil {
		return nil, err
	}

	var suggestions []loop.OutRequest
	for _, channel := range eligible {
		channelID := lnwire.NewShortChanIDFromInt(channel.ChannelID)
		rule, ok := m.params.ChannelRules[channelID]
		if !ok {
			continue
		}

		balance := newBalances(channel)

		suggestion := rule.suggestSwap(balance, outRestrictions)

		// We can have nil suggestions in the case where no action is
		// required, so we skip over them.
		if suggestion == nil {
			continue
		}

		// Get a quote for a swap of this amount.
		quote, err := m.cfg.LoopOutQuote(
			ctx, &loop.LoopOutQuoteRequest{
				Amount:                  suggestion.Amount,
				SweepConfTarget:         m.params.SweepConfTarget,
				SwapPublicationDeadline: m.cfg.Clock.Now(),
			},
		)
		if err != nil {
			return nil, err
		}

		log.Debugf("quote for suggestion: %v, swap fee: %v, "+
			"miner fee: %v, prepay: %v", suggestion, quote.SwapFee,
			quote.MinerFee, quote.PrepayAmount)

		// Check that the estimated fees for the suggested swap are
		// below the fee limits configured by the manager.
		err = m.checkFeeLimits(quote, suggestion.Amount)
		if err != nil {
			log.Infof("suggestion: %v expected fees too high: %v",
				suggestion, err)

			continue
		}

		outRequest := m.makeLoopOutRequest(suggestion, quote)
		suggestions = append(suggestions, outRequest)
	}

	// If we have no suggestions after we have applied all of our limits,
	// just return.
	if len(suggestions) == 0 {
		return nil, nil
	}

	// Sort suggestions by amount in descending order.
	sort.SliceStable(suggestions, func(i, j int) bool {
		return suggestions[i].Amount > suggestions[j].Amount
	})

	// Run through our suggested swaps in descending order of amount and
	// return all of the swaps which will fit within our remaining budget.
	var (
		available = m.params.AutoFeeBudget - summary.totalFees()
		inBudget  []loop.OutRequest
	)

	for _, swap := range suggestions {
		fees := worstCaseOutFees(
			swap.MaxPrepayRoutingFee, swap.MaxSwapRoutingFee,
			swap.MaxSwapFee, swap.MaxMinerFee, swap.MaxPrepayAmount,
		)

		// If the maximum fee we expect our swap to use is less than the
		// amount we have available, we add it to our set of swaps that
		// fall within the budget and decrement our available amount.
		if fees <= available {
			available -= fees
			inBudget = append(inBudget, swap)
		}

		// If we're out of budget, exit early.
		if available == 0 {
			break
		}
	}

	return inBudget, nil
}

// makeLoopOutRequest creates a loop out request from a suggestion. Since we
// do not get any information about our off-chain routing fees when we request
// a quote, we just set our prepay and route maximum fees directly from the
// amounts we expect to route. The estimation we use elsewhere is the repo is
// route-independent, which is a very poor estimation so we don't bother with
// checking against this inaccurate constant. We use the exact prepay amount
// and swap fee given to us by the server, but use our maximum miner fee anyway
// to give us some leeway when performing the swap.
func (m *Manager) makeLoopOutRequest(suggestion *LoopOutRecommendation,
	quote *loop.LoopOutQuote) loop.OutRequest {

	prepayMaxFee := ppmToSat(
		quote.PrepayAmount, m.params.MaximumPrepayRoutingFeePPM,
	)

	routeMaxFee := ppmToSat(
		suggestion.Amount, m.params.MaximumRoutingFeePPM,
	)

	return loop.OutRequest{
		Amount: suggestion.Amount,
		OutgoingChanSet: loopdb.ChannelSet{
			suggestion.Channel.ToUint64(),
		},
		MaxPrepayRoutingFee: prepayMaxFee,
		MaxSwapRoutingFee:   routeMaxFee,
		MaxMinerFee:         m.params.MaximumMinerFee,
		MaxSwapFee:          quote.SwapFee,
		MaxPrepayAmount:     quote.PrepayAmount,
		SweepConfTarget:     m.params.SweepConfTarget,
	}
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
}

// totalFees returns the total amount of fees that automatically dispatched
// swaps may consume.
func (e *existingAutoLoopSummary) totalFees() btcutil.Amount {
	return e.spentFees + e.pendingFees
}

// checkExistingAutoLoops calculates the total amount that has been spent by
// automatically dispatched swaps that have completed, and the worst-case fee
// total for our set of ongoing, automatically dispatched swaps.
func (m *Manager) checkExistingAutoLoops(ctx context.Context,
	loopOuts []*loopdb.LoopOut) (*existingAutoLoopSummary, error) {

	var summary existingAutoLoopSummary

	for _, out := range loopOuts {
		if out.Contract.Label != labels.AutoOutLabel() {
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

// getEligibleChannels takes lists of our existing loop out and in swaps, and
// gets a list of channels that are not currently being utilized for a swap.
// If an unrestricted swap is ongoing, we return an empty set of channels
// because we don't know which channels balances it will affect.
func (m *Manager) getEligibleChannels(ctx context.Context,
	loopOut []*loopdb.LoopOut, loopIn []*loopdb.LoopIn) (
	[]lndclient.ChannelInfo, error) {

	var (
		existingOut = make(map[lnwire.ShortChannelID]bool)
		existingIn  = make(map[route.Vertex]bool)
		failedOut   = make(map[lnwire.ShortChannelID]time.Time)
	)

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

					failedOut[chanID] = failedAt
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

		if len(chanSet) == 0 {
			log.Debugf("Ongoing unrestricted loop out: "+
				"%v, no suggestions at present", out.Hash)

			return nil, nil
		}

		for _, id := range chanSet {
			chanID := lnwire.NewShortChanIDFromInt(id)
			existingOut[chanID] = true
		}
	}

	for _, in := range loopIn {
		// Skip completed swaps, they can't affect our channel balances.
		if in.State().State.Type() != loopdb.StateTypePending {
			continue
		}

		if in.Contract.LastHop == nil {
			log.Debugf("Ongoing unrestricted loop in: "+
				"%v, no suggestions at present", in.Hash)

			return nil, nil
		}

		existingIn[*in.Contract.LastHop] = true
	}

	channels, err := m.cfg.Lnd.Client.ListChannels(ctx)
	if err != nil {
		return nil, err
	}

	// Run through our set of channels and skip over any channels that
	// are currently being utilized by a restricted swap (where restricted
	// means that a loop out limited channels, or a loop in limited last
	// hop).
	var eligible []lndclient.ChannelInfo
	for _, channel := range channels {
		shortID := lnwire.NewShortChanIDFromInt(channel.ChannelID)

		lastFail, recentFail := failedOut[shortID]
		if recentFail {
			log.Debugf("Channel: %v not eligible for "+
				"suggestions, was part of a failed swap at: %v",
				channel.ChannelID, lastFail)

			continue
		}

		if existingOut[shortID] {
			log.Debugf("Channel: %v not eligible for "+
				"suggestions, ongoing loop out utilizing "+
				"channel", channel.ChannelID)

			continue
		}

		if existingIn[channel.PubKeyBytes] {
			log.Debugf("Channel: %v not eligible for "+
				"suggestions, ongoing loop in utilizing "+
				"peer", channel.ChannelID)

			continue
		}

		eligible = append(eligible, channel)
	}

	return eligible, nil
}

// checkFeeLimits takes a set of fees for a swap and checks whether they exceed
// our swap limits.
func (m *Manager) checkFeeLimits(quote *loop.LoopOutQuote,
	swapAmt btcutil.Amount) error {

	maxFee := ppmToSat(swapAmt, m.params.MaximumSwapFeePPM)

	if quote.SwapFee > maxFee {
		return fmt.Errorf("quoted swap fee: %v > maximum swap fee: %v",
			quote.SwapFee, maxFee)
	}

	if quote.MinerFee > m.params.MaximumMinerFee {
		return fmt.Errorf("quoted miner fee: %v > maximum miner "+
			"fee: %v", quote.MinerFee, m.params.MaximumMinerFee)
	}

	if quote.PrepayAmount > m.params.MaximumPrepay {
		return fmt.Errorf("quoted prepay: %v > maximum prepay: %v",
			quote.PrepayAmount, m.params.MaximumPrepay)
	}

	return nil
}

// satPerKwToSatPerVByte converts sat per kWeight to sat per vByte.
func satPerKwToSatPerVByte(satPerKw chainfee.SatPerKWeight) int64 {
	return int64(satPerKw.FeePerKVByte() / 1000)
}

// ppmToSat takes an amount and a measure of parts per million for the amount
// and returns the amount that the ppm represents.
func ppmToSat(amount btcutil.Amount, ppm int) btcutil.Amount {
	return btcutil.Amount(uint64(amount) * uint64(ppm) / FeeBase)
}

func mSatToSatoshis(amount lnwire.MilliSatoshi) btcutil.Amount {
	return btcutil.Amount(amount / 1000)
}
