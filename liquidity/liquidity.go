// Package liquidity is responsible for monitoring our node's liquidity. It
// allows setting of a liquidity rule which describes the desired liquidity
// balance on a per-channel basis.
package liquidity

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lnwire"
)

const (
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
	// per swap.
	defaultMaximumMinerFee = 15000

	// defaultMaximumPrepay is the default limit we place on prepay
	// invoices.
	defaultMaximumPrepay = 30000
)

var (
	// defaultParameters contains the default parameters that we start our
	// liquidity manger with.
	defaultParameters = Parameters{
		ChannelRules: make(map[lnwire.ShortChannelID]*ThresholdRule),
	}

	// ErrZeroChannelID is returned if we get a rule for a 0 channel ID.
	ErrZeroChannelID = fmt.Errorf("zero channel ID not allowed")
)

// Config contains the external functionality required to run the
// liquidity manager.
type Config struct {
	// LoopOutRestrictions returns the restrictions that the server applies
	// to loop out swaps.
	LoopOutRestrictions func(ctx context.Context) (*Restrictions, error)

	// Lnd provides us with access to lnd's rpc servers.
	Lnd *lndclient.LndServices

	// Clock allows easy mocking of time in unit tests.
	Clock clock.Clock
}

// Parameters is a set of parameters provided by the user which guide
// how we assess liquidity.
type Parameters struct {
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

	return fmt.Sprintf("channel rules: %v",
		strings.Join(channelRules, ","))
}

// validate checks whether a set of parameters is valid.
func (p Parameters) validate() error {
	for channel, rule := range p.ChannelRules {
		if channel.ToUint64() == 0 {
			return ErrZeroChannelID
		}

		if err := rule.validate(); err != nil {
			return fmt.Errorf("channel: %v has invalid rule: %v",
				channel.ToUint64(), err)
		}
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
	if err := params.validate(); err != nil {
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
	paramCopy := Parameters{
		ChannelRules: make(map[lnwire.ShortChannelID]*ThresholdRule,
			len(params.ChannelRules)),
	}

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

	channels, err := m.cfg.Lnd.Client.ListChannels(ctx)
	if err != nil {
		return nil, err
	}

	// Get the current server side restrictions.
	outRestrictions, err := m.cfg.LoopOutRestrictions(ctx)
	if err != nil {
		return nil, err
	}

	var suggestions []loop.OutRequest
	for _, channel := range channels {
		channelID := lnwire.NewShortChanIDFromInt(channel.ChannelID)
		rule, ok := m.params.ChannelRules[channelID]
		if !ok {
			continue
		}

		balance := newBalances(channel)

		suggestion := rule.suggestSwap(balance, outRestrictions)

		// We can have nil suggestions in the case where no action is
		// required, so only add non-nil suggestions.
		if suggestion != nil {
			outRequest := makeLoopOutRequest(suggestion)
			suggestions = append(suggestions, outRequest)
		}
	}

	return suggestions, nil
}

// makeLoopOutRequest creates a loop out request from a suggestion, setting fee
// limits defined by our default fee values.
func makeLoopOutRequest(suggestion *LoopOutRecommendation) loop.OutRequest {
	prepayMaxFee := ppmToSat(
		defaultMaximumPrepay, defaultPrepayRoutingFeePPM,
	)

	routeMaxFee := ppmToSat(suggestion.Amount, defaultRoutingFeePPM)
	maxSwapFee := ppmToSat(suggestion.Amount, defaultSwapFeePPM)

	return loop.OutRequest{
		Amount: suggestion.Amount,
		OutgoingChanSet: loopdb.ChannelSet{
			suggestion.Channel.ToUint64(),
		},
		MaxPrepayRoutingFee: prepayMaxFee,
		MaxSwapRoutingFee:   routeMaxFee,
		MaxMinerFee:         defaultMaximumMinerFee,
		MaxSwapFee:          maxSwapFee,
		MaxPrepayAmount:     defaultMaximumPrepay,
		SweepConfTarget:     loop.DefaultSweepConfTarget,
	}
}

// ppmToSat takes an amount and a measure of parts per million for the amount
// and returns the amount that the ppm represents.
func ppmToSat(amount btcutil.Amount, ppm int) btcutil.Amount {
	return btcutil.Amount(uint64(amount) * uint64(ppm) / FeeBase)
}
