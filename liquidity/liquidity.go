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
		AutoFeeBudget:              defaultBudget,
		MaxAutoInFlight:            defaultMaxInFlight,
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

	// ClientRestrictions are the restrictions placed on swap size by the
	// client.
	ClientRestrictions Restrictions

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
		"auto budget: %v, budget start: %v, max auto in flight: %v, "+
		"minimum swap size=%v, maximum swap size=%v",
		strings.Join(channelRules, ","), p.FailureBackOff,
		p.SweepFeeRateLimit, p.SweepConfTarget, p.MaximumPrepay,
		p.MaximumMinerFee, p.MaximumSwapFeePPM,
		p.MaximumRoutingFeePPM, p.MaximumPrepayRoutingFeePPM,
		p.AutoFeeBudget, p.AutoFeeStartDate, p.MaxAutoInFlight,
		p.ClientRestrictions.Minimum, p.ClientRestrictions.Maximum)
}

// validate checks whether a set of parameters is valid. It takes the minimum
// confirmations we allow for sweep confirmation target as a parameter.
func (p Parameters) validate(minConfs int32, server *Restrictions) error {
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

	err = params.validate(m.cfg.MinimumConfirmations, restrictions)
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

	return paramCopy
}

// autoloop gets a set of suggested swaps and dispatches them automatically if
// we have automated looping enabled.
func (m *Manager) autoloop(ctx context.Context) error {
	swaps, err := m.SuggestSwaps(ctx, true)
	if err != nil {
		return err
	}

	for _, swap := range swaps {
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

// SuggestSwaps returns a set of swap suggestions based on our current liquidity
// balance for the set of rules configured for the manager, failing if there are
// no rules set. It takes an autoloop boolean that indicates whether the
// suggestions are being used for our internal autolooper. This boolean is used
// to determine the information we add to our swap suggestion and whether we
// return any suggestions.
func (m *Manager) SuggestSwaps(ctx context.Context, autoloop bool) (
	[]loop.OutRequest, error) {

	m.paramsLock.Lock()
	defer m.paramsLock.Unlock()

	// If we have no rules set, exit early to avoid unnecessary calls to
	// lnd and the server.
	if len(m.params.ChannelRules) == 0 {
		return nil, ErrNoRules
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

		return nil, nil
	}

	// If we have already reached our total allowed number of in flight
	// swaps, we do not suggest any more at the moment.
	allowedSwaps := m.params.MaxAutoInFlight - summary.inFlightCount
	if allowedSwaps <= 0 {
		log.Debugf("%v autoloops allowed, %v in flight",
			m.params.MaxAutoInFlight, summary.inFlightCount)
		return nil, nil
	}

	channels, err := m.cfg.Lnd.Client.ListChannels(ctx)
	if err != nil {
		return nil, err
	}

	// Get a summary of the channels and peers that are not eligible due
	// to ongoing swaps.
	traffic := m.currentSwapTraffic(loopOut, loopIn)

	var suggestions []loop.OutRequest

	for _, channel := range channels {
		balance := newBalances(channel)

		rule, ok := m.params.ChannelRules[balance.channelID]
		if !ok {
			continue
		}

		if !traffic.maySwap(channel.PubKeyBytes, balance.channelID) {
			continue
		}

		// We can have nil suggestions in the case where no action is
		// required, so we skip over them.
		suggestion := rule.suggestSwap(balance, restrictions)
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

		outRequest, err := m.makeLoopOutRequest(
			ctx, suggestion, quote, autoloop,
		)
		if err != nil {
			return nil, err
		}
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

		// If we're out of budget, or we have hit the max number of
		// swaps that we want to dispatch at one time, exit early.
		if available == 0 || allowedSwaps == len(inBudget) {
			break
		}
	}

	return inBudget, nil
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
	suggestion *LoopOutRecommendation, quote *loop.LoopOutQuote,
	autoloop bool) (loop.OutRequest, error) {

	prepayMaxFee := ppmToSat(
		quote.PrepayAmount, m.params.MaximumPrepayRoutingFeePPM,
	)

	routeMaxFee := ppmToSat(
		suggestion.Amount, m.params.MaximumRoutingFeePPM,
	)

	request := loop.OutRequest{
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
	chanID lnwire.ShortChannelID) bool {

	lastFail, recentFail := s.failedLoopOut[chanID]
	if recentFail {
		log.Debugf("Channel: %v not eligible for suggestions, was "+
			"part of a failed swap at: %v", chanID, lastFail)

		return false
	}

	if s.ongoingLoopOut[chanID] {
		log.Debugf("Channel: %v not eligible for suggestions, "+
			"ongoing loop out utilizing channel", chanID)

		return false
	}

	if s.ongoingLoopIn[peer] {
		log.Debugf("Peer: %x not eligible for suggestions ongoing "+
			"loop in utilizing peer", peer)

		return false
	}

	return true
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
