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

// NewManager creates a liquidity manager which has no parameters set.
func NewManager(cfg *Config) *Manager {
	return &Manager{
		cfg:       cfg,
		params:    nil,
		done:      make(chan struct{}),
		getParams: make(chan getParametersRequest),
		setParams: make(chan setParamsRequest),
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
