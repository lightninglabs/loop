// Package liquidity is responsible for monitoring our node's liquidity. It is
// configured with a target and rule to inform how the node operator would like
// to manage liquidity. A rule describes how liquidity should be allocated,
// and a target describes what entity we apply this rule to.
//
// At present, the following targets are available:
// - Node: examine the liquidity balance of our node as a whole, considering all
//   channels.
// - Peer: examine the liquidity balance of the channels that we have per peer.
// - Channel: examine the liquidity balance of each channel.
//
// In addition to a target and rule, it has a set of parameters which determine
// how we asses liquidity:
// - Include private: whether to include private channels in our liquidity
//   calculations.
package liquidity

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

var (
	// ErrNoParameters is returned when a request is made to lookup manager
	// parameters, but none are set.
	ErrNoParameters = errors.New("no parameters set for manager")

	// ErrUnexpectedRule is returned when we are provided with a non-nil
	// rule for the nil target value.
	ErrUnexpectedRule = errors.New("rule not expected for target none")

	// ErrNoRule is returned when we are not provided with a rule and we
	// have a non-nil target.
	ErrNoRule = errors.New("targets must be set with a rule")

	// ErrSetTarget is returned when a request for suggestions is made but
	// no target/rule combination is set.
	ErrSetTarget = errors.New("target must be set to provide suggestions")

	// ErrShuttingDown is returned when a request is cancelled because
	// the manager is shutting down.
	ErrShuttingDown = errors.New("server shutting down")
)

// Config contains the external functionality required to run the liquidity
// manager.
type Config struct {
	// LoopOutRestrictions returns the restrictions placed on loop out swaps
	// by the server.
	LoopOutRestrictions func(ctx context.Context) (*Restrictions, error)

	// LoopInRestrictions returns the restrictions placed on loop in swaps
	// by the server.
	LoopInRestrictions func(ctx context.Context) (*Restrictions, error)

	// ListChannels provides a list of our currently open channels.
	ListChannels func(ctx context.Context) ([]lndclient.ChannelInfo, error)
}

// Target describes the target that a liquidity rule will be applied to.
type Target uint8

const (
	// TargetNone indicates that no mode has been set for the liquidity
	// manager.
	TargetNone Target = iota

	// TargetNode indicates that the liquidity manager will apply its rule
	// to all of the node's channels, examining the overall liquidity
	// balance of the node as one entity.
	TargetNode

	// TargetPeer indicates that the liquidity manager will apply its rule
	// per peer, examining the liquidity balance of the set of channels that
	// we have with each peer as one entity.
	TargetPeer

	// TargetChannel indicates that the liquidity manager will apply its
	// rule to individual channels, examining each channel's liquidity
	// balance.
	TargetChannel
)

// String returns the string representation of a mode.
func (t Target) String() string {
	switch t {
	case TargetNone:
		return "none"

	case TargetNode:
		return "node"

	case TargetPeer:
		return "peer"

	case TargetChannel:
		return "channel"

	default:
		return "unknown target"
	}
}

func (t Target) validateTarget() error {
	if t < TargetNone || t > TargetChannel {
		return fmt.Errorf("unknown target: %v", t)
	}

	return nil
}

// Parameters is a set of parameters provided by the user which guide how we
// assess liquidity.
type Parameters struct {
	// Rule is the rule that we apply when examining liquidity.
	Rule

	// Target is the entity that we apply our rule to.
	Target

	// ChannelRules contains custom rules for a specific channel. These
	// rules will overwrite the general rule/target if they are set, and
	// any custom peer rules that are set.
	ChannelRules map[lnwire.ShortChannelID]Rule

	// PeerRules contains custom rules for a specific peer. These rules will
	// overwrite the general rule/target if they are set.
	PeerRules map[route.Vertex]Rule

	// IncludePrivate indicates whether we should include private channels
	// in our balance calculations.
	IncludePrivate bool
}

// String returns the string representation of our parameters.
func (p *Parameters) String() string {
	return fmt.Sprintf("target: %v, rule: %v, include private: %v",
		p.Target, p.Rule, p.IncludePrivate)
}

// validate checks whether a set of parameters is valid.
func (p *Parameters) validate() error {
	if err := p.Target.validateTarget(); err != nil {
		return err
	}

	// If we have no target set, we expect our rule to be nil. If we have
	// a nil rule, we do not need to perform rule validation so we return.
	if p.Target == TargetNone {
		if p.Rule != nil {
			return ErrUnexpectedRule
		}

		return nil
	}

	// If we have a target set, we expect a rule to be provided.
	if p.Rule == nil {
		return ErrNoRule
	}

	return p.Rule.validate()
}

// Restrictions describe the restrictions placed on swaps.
type Restrictions struct {
	// MinimumAmount is the lower limit on swap amount, inclusive.
	MinimumAmount btcutil.Amount

	// MaximumAmount is the upper limit on swap amount, inclusive.
	MaximumAmount btcutil.Amount
}

// NewRestrictions creates a new set of restrictions.
func NewRestrictions(minimum, maximum btcutil.Amount) *Restrictions {
	return &Restrictions{
		MinimumAmount: minimum,
		MaximumAmount: maximum,
	}
}

// String returns the string representation of our restriction.
func (r *Restrictions) String() string {
	return fmt.Sprintf("%v-%v", r.MinimumAmount, r.MaximumAmount)
}

// Manager tracks monitors liquidity.
type Manager struct {
	started int32 // to be used atomically

	// cfg contains the external functionality we require to determine our
	// current liquidity balance.
	cfg *Config

	// params is the set of parameters we are currently using. These may be
	// updated at runtime.
	params *Parameters

	// getParams is a channel that takes requests to get our current set of
	// parameters.
	getParams chan getParametersRequest

	// setParams is a channel that takes requests to update our current set
	// of parameters.
	setParams chan setParamsRequest

	// suggestionRequests accepts requests for swaps that will help us reach
	// our configured thresholds.
	suggestionRequests chan suggestionRequest

	// done is closed when our main event loop is shutting down. This allows
	// us to cancel requests sent to our main event loop that cannot be
	// served.
	done chan struct{}
}

// getParametersRequest provides a request to get our currently set parameters.
type getParametersRequest struct {
	response chan *Parameters
}

// setParamsRequest contains a request to update our parameters.
type setParamsRequest struct {
	params Parameters
}

// suggestionRequest contains a request for a set of suggested swaps.
type suggestionRequest struct {
	ctx      context.Context
	response chan suggestionResponse
}

// suggestionResponse contains a set of recommended swaps, and any errors that
// occurred while trying to obtain them.
type suggestionResponse struct {
	err         error
	suggestions []SwapSuggestion
}

// SwapSuggestion contains the rule we used to determine whether we should
// perform any swaps, and the set of swaps we recommend.
type SwapSuggestion struct {
	// Rule provides the rule that we used to get swap recommendations.
	Rule Rule

	// Target provides the name of the target(s) that this rule was applied
	// to.
	Target Target

	// Suggestions contains a map of target ID to set of recommended swaps.
	Suggestions []SwapRecommendation
}

// NewManager creates a liquidity manager which has no parameters set.
func NewManager(cfg *Config) *Manager {
	return &Manager{
		cfg:                cfg,
		params:             nil,
		done:               make(chan struct{}),
		getParams:          make(chan getParametersRequest),
		setParams:          make(chan setParamsRequest),
		suggestionRequests: make(chan suggestionRequest),
	}
}

// Run starts the manager, failing if it has already been started. Note that
// this function will block, so should be run in a goroutine.
func (m *Manager) Run(ctx context.Context) error {
	if !atomic.CompareAndSwapInt32(&m.started, 0, 1) {
		return errors.New("manager already started")
	}

	return m.run(ctx)
}

// run is the main event loop for our liquidity manager. When it exits, it
// closes the done channel so that any pending requests sent into our request
// channel can be cancelled.
func (m *Manager) run(ctx context.Context) error {
	defer close(m.done)

	for {
		select {
		// Serve requests to get our current set of parameters.
		case getParams := <-m.getParams:
			getParams.response <- m.params

		// Serve requests to set our current set of parameters.
		case setParams := <-m.setParams:
			m.params = &setParams.params
			log.Infof("updated parameters to: %v", m.params)

		case request := <-m.suggestionRequests:
			channels, err := m.cfg.ListChannels(request.ctx)
			if err != nil {
				request.response <- suggestionResponse{
					err: err,
				}
				continue
			}

			swaps, err := m.suggestSwaps(request.ctx, channels)
			request.response <- suggestionResponse{
				suggestions: swaps,
				err:         err,
			}

		// Return a non-nil error if we receive the instruction to exit.
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// GetParameters serves a request to get our currently configured set of
// parameters. If no parameters are currently set, this function will fail.
func (m *Manager) GetParameters(ctx context.Context) (*Parameters, error) {
	// Create a request to get our current parameters, buffer the response
	// channel so that we can not read the response without blocking the
	// response (in the case of client cancellation).
	request := getParametersRequest{
		response: make(chan *Parameters, 1),
	}

	// Send our request to the main event loop.
	select {
	case m.getParams <- request:

	case <-ctx.Done():
		return nil, ctx.Err()

	case <-m.done:
		return nil, ErrShuttingDown
	}

	// Wait for a response.
	select {
	case params := <-request.response:
		if params == nil {
			return nil, ErrNoParameters
		}

		// Make a copy of the reference we were provided with so that
		// callers cannot mutate our current parameters.
		currentParams := *params
		return &currentParams, nil

	case <-ctx.Done():
		return nil, ctx.Err()
	}

}

// SetParameters sends a request to update our current parameters if they are
// valid.
func (m *Manager) SetParameters(ctx context.Context, params Parameters) error {
	if err := params.validate(); err != nil {
		return err
	}

	// Create a request to update our parameters.
	request := setParamsRequest{
		params: params,
	}

	select {
	case m.setParams <- request:
		return nil

	case <-m.done:
		return ErrShuttingDown

	case <-ctx.Done():
		return ctx.Err()
	}
}

// SuggestSwap queries the manager's main event loop for swap suggestions.
// Note that this function will block if the manager is not started.
func (m *Manager) SuggestSwap(ctx context.Context) ([]SwapSuggestion, error) {
	// Send a request to our main event loop to process the updates,
	// buffering the response channel so that the event loop cannot be
	// blocked by the client not consuming the request.
	responseChan := make(chan suggestionResponse, 1)
	select {
	case m.suggestionRequests <- suggestionRequest{
		ctx:      ctx,
		response: responseChan,
	}:

	case <-m.done:
		return nil, ErrShuttingDown

	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// Wait for a response from the main event loop, or client cancellation.
	select {
	case resp := <-responseChan:
		if resp.err != nil {
			return nil, resp.err
		}

		return resp.suggestions, nil

	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type ruleSet struct {
	balances []balances
	rule     Rule
}

// suggestSwaps divides a set of channels based on our current set of rules, and
// gets suggested swaps for our current set of rules.
func (m *Manager) suggestSwaps(ctx context.Context,
	channels []lndclient.ChannelInfo) ([]SwapSuggestion, error) {

	// If our parameters are nil, or we have no target set, fail because
	// we have no rules we can use to create our suggestions.
	if m.params == nil || m.params.Target == TargetNone {
		return nil, ErrSetTarget
	}

	// Run through all of our channels and create a set of balances. We do
	// not preallocate because we may skip over come channels if they are
	// private.
	targets := make(map[string]ruleSet)

	// Create a helper function which will add a fresh entry for our key
	// to our targets map (if it is not present), and otherwise append
	// another set of channel balances.
	addEntry := func(key string, channel balances, rule Rule) {
		value, ok := targets[key]
		if !ok {
			value = ruleSet{
				rule: rule,
			}
		}

		value.balances = append(value.balances, channel)
		targets[key] = value
	}

	for _, channel := range channels {
		// If the channel is private and we do not want to include
		// private channels, skip it.
		if channel.Private && !m.params.IncludePrivate {
			continue
		}

		chanID := lnwire.NewShortChanIDFromInt(channel.ChannelID)
		balances := balances{
			outgoing:  channel.LocalBalance,
			incoming:  channel.RemoteBalance,
			capacity:  channel.Capacity,
			channelID: chanID,
			pubkey:    channel.PubKeyBytes,
		}

		// If we have any custom channel rules set, we check whether
		// this channel has a specific rule set, and add it with this
		// custom rule to this set of targets if it does. We continue
		// so that it is not overwritten by peer-level or generic rules.
		if m.params.ChannelRules != nil {
			chanID := lnwire.NewShortChanIDFromInt(
				channel.ChannelID,
			)

			rule, ok := m.params.ChannelRules[chanID]
			if ok {
				addEntry(
					fmt.Sprintf("%v", channel.ChannelID),
					balances, rule,
				)

				continue
			}
		}

		// If we have any custom peer rules set, we check whether this
		// channel has a specific rule set for its peer, and add it
		// with this custom rule to our set of targets if it does. We
		// continue so that it is not overwritten by our generic rules.
		if m.params.PeerRules != nil {
			rule, ok := m.params.PeerRules[channel.PubKeyBytes]
			if ok {
				addEntry(
					channel.PubKeyBytes.String(), balances,
					rule,
				)

				continue
			}
		}

		switch m.params.Target {
		// For node, store all channels under the same key.
		case TargetNode:
			addEntry(
				TargetNode.String(), balances, m.params.Rule,
			)

		// For peers, store balances by pubkey.
		case TargetPeer:
			addEntry(
				channel.PubKeyBytes.String(), balances,
				m.params.Rule,
			)

		// For channels, store balances by channel ID.
		case TargetChannel:
			str := fmt.Sprintf("%v", channel.ChannelID)
			addEntry(str, balances, m.params.Rule)

		default:
			return nil, fmt.Errorf("cannot return suggestions "+
				"for target: %v", m.params.Target)
		}
	}

	// Get restrictions from the server.
	outRestrictions, err := m.cfg.LoopOutRestrictions(ctx)
	if err != nil {
		return nil, err
	}

	inRestrictions, err := m.cfg.LoopInRestrictions(ctx)
	if err != nil {
		return nil, err
	}

	var suggestions []SwapSuggestion
	for _, ruleSet := range targets {
		resp := SwapSuggestion{
			Rule:   m.params.Rule,
			Target: m.params.Target,
		}

		swaps, err := ruleSet.rule.getSwaps(
			ruleSet.balances, outRestrictions, inRestrictions,
		)
		if err != nil {
			return nil, err
		}

		resp.Suggestions = append(resp.Suggestions, swaps...)
		suggestions = append(suggestions, resp)
	}

	return suggestions, nil
}
