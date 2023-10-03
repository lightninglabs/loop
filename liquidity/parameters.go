package liquidity

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"

	clientrpc "github.com/lightninglabs/loop/looprpc"
)

var (
	// defaultParameters contains the default parameters that we start our
	// liquidity manager with.
	defaultParameters = Parameters{
		AutoFeeBudget:             defaultBudget,
		AutoFeeRefreshPeriod:      defaultBudgetRefreshPeriod,
		AutoloopBudgetLastRefresh: time.Now(),
		DestAddr:                  nil,
		MaxAutoInFlight:           defaultMaxInFlight,
		ChannelRules:              make(map[lnwire.ShortChannelID]*SwapRule),
		PeerRules:                 make(map[route.Vertex]*SwapRule),
		FailureBackOff:            defaultFailureBackoff,
		SweepConfTarget:           defaultConfTarget,
		HtlcConfTarget:            defaultHtlcConfTarget,
		FeeLimit:                  defaultFeePortion(),
	}
)

const InfiniteDuration = (24 * 31 * 12 * 100) * time.Hour

// Parameters is a set of parameters provided by the user which guide
// how we assess liquidity.
type Parameters struct {
	// Autoloop enables automatic dispatch of swaps.
	Autoloop bool

	// DestAddr is the address to be used for sweeping the on-chain HTLC
	// that is related with a loop out.
	DestAddr btcutil.Address

	// An alternative destination address source for the swap. This field
	// represents the name of the account in the backing lnd instance.
	// Refer to lnd's wallet import functions for reference.
	Account string

	// The address type of the account specified in the account field.
	AccountAddrType walletrpc.AddressType

	// AutoFeeBudget is the total amount we allow to be spent on
	// automatically dispatched swaps. Once this budget has been used, we
	// will stop dispatching swaps until the budget is refreshed.
	AutoFeeBudget btcutil.Amount

	// AutoFeeRefreshPeriod is the amount of time that must pass before the
	// auto fee budget is refreshed.
	AutoFeeRefreshPeriod time.Duration

	// AutoloopBudgetLastRefresh is the last time at which we refreshed
	// our budget.
	AutoloopBudgetLastRefresh time.Time

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

	// CustomPaymentCheckInterval is an optional custom interval to use when
	// checking an autoloop loop out payments' payment status.
	CustomPaymentCheckInterval time.Duration

	// EasyAutoloop is a boolean that indicates whether we should use the
	// easy autoloop feature.
	EasyAutoloop bool

	// EasyAutoloopTarget is the target amount of liquidity that we want to
	// maintain in our channels.
	EasyAutoloopTarget btcutil.Amount
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
		"auto budget: %v, budget refresh: %v, max auto in flight: %v, "+
		"minimum swap size=%v, maximum swap size=%v",
		strings.Join(ruleList, ","), p.FailureBackOff,
		p.SweepConfTarget, p.HtlcConfTarget, p.FeeLimit,
		p.AutoFeeBudget, p.AutoFeeRefreshPeriod, p.MaxAutoInFlight,
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

	// Destination address and account cannot be set at the same time.
	if p.DestAddr != nil && len(p.DestAddr.String()) > 0 &&
		len(p.Account) > 0 {

		return ErrAmbiguousDestAddr
	}

	// If an account is specified the respective address type must be
	// specified as well, or both must be unset.
	if len(p.Account) == 0 !=
		(p.AccountAddrType == walletrpc.AddressType_UNKNOWN) {

		return ErrAccountAndAddrType
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

// rpcToFee converts the values provided over rpc to a fee limit interface,
// failing if an inconsistent set of fields are set.
func rpcToFee(req *clientrpc.LiquidityParameters) (FeeLimit, error) {
	// Check which fee limit type we have values set for. If any fields
	// relevant to our individual categories are set, we count that type
	// as set.
	isFeePPM := req.FeePpm != 0
	isCategories := req.MaxSwapFeePpm != 0 || req.MaxRoutingFeePpm != 0 ||
		req.MaxPrepayRoutingFeePpm != 0 || req.MaxMinerFeeSat != 0 ||
		req.MaxPrepaySat != 0 || req.SweepFeeRateSatPerVbyte != 0

	switch {
	case isFeePPM && isCategories:
		return nil, errors.New("set either fee ppm, or individual " +
			"fee categories")
	case isFeePPM:
		return NewFeePortion(req.FeePpm), nil

	case isCategories:
		satPerKVbyte := chainfee.SatPerKVByte(
			req.SweepFeeRateSatPerVbyte * 1000,
		)

		return NewFeeCategoryLimit(
			req.MaxSwapFeePpm,
			req.MaxRoutingFeePpm,
			req.MaxPrepayRoutingFeePpm,
			btcutil.Amount(req.MaxMinerFeeSat),
			btcutil.Amount(req.MaxPrepaySat),
			satPerKVbyte.FeePerKWeight(),
		), nil

	default:
		return nil, errors.New("no fee categories set")
	}
}

// rpcToRule switches on rpc rule type to convert to our rule interface.
func rpcToRule(rule *clientrpc.LiquidityRule) (*SwapRule, error) {
	swapType := swap.TypeOut
	if rule.SwapType == clientrpc.SwapType_LOOP_IN {
		swapType = swap.TypeIn
	}

	switch rule.Type {
	case clientrpc.LiquidityRuleType_UNKNOWN:
		return nil, fmt.Errorf("rule type field must be set")

	case clientrpc.LiquidityRuleType_THRESHOLD:
		return &SwapRule{
			ThresholdRule: NewThresholdRule(
				int(rule.IncomingThreshold),
				int(rule.OutgoingThreshold),
			),
			Type: swapType,
		}, nil

	default:
		return nil, fmt.Errorf("unknown rule: %T", rule)
	}
}

// rpcToParameters takes a `LiquidityParameters` and creates a `Parameters`
// from it.
func RpcToParameters(req *clientrpc.LiquidityParameters) (*Parameters,
	error) {

	feeLimit, err := rpcToFee(req)
	if err != nil {
		return nil, err
	}

	var destaddr btcutil.Address
	if len(req.AutoloopDestAddress) != 0 {
		if req.AutoloopDestAddress == "default" {
			destaddr = nil
		} else {
			destaddr, err = btcutil.DecodeAddress(
				req.AutoloopDestAddress, nil,
			)
			if err != nil {
				return nil, err
			}
		}
	}

	addrType := walletrpc.AddressType_UNKNOWN
	if req.AccountAddrType == clientrpc.AddressType_TAPROOT_PUBKEY {
		addrType = walletrpc.AddressType_TAPROOT_PUBKEY
	}

	params := &Parameters{
		FeeLimit:        feeLimit,
		SweepConfTarget: req.SweepConfTarget,
		FailureBackOff: time.Duration(req.FailureBackoffSec) *
			time.Second,
		Autoloop: req.Autoloop,
		AutoloopBudgetLastRefresh: time.Unix(
			int64(req.AutoloopBudgetLastRefresh), 0,
		),
		DestAddr:        destaddr,
		Account:         req.Account,
		AccountAddrType: addrType,
		AutoFeeBudget:   btcutil.Amount(req.AutoloopBudgetSat),
		MaxAutoInFlight: int(req.AutoMaxInFlight),
		ChannelRules: make(
			map[lnwire.ShortChannelID]*SwapRule,
		),
		PeerRules: make(
			map[route.Vertex]*SwapRule,
		),
		ClientRestrictions: Restrictions{
			Minimum: btcutil.Amount(req.MinSwapAmount),
			Maximum: btcutil.Amount(req.MaxSwapAmount),
		},
		HtlcConfTarget:     req.HtlcConfTarget,
		EasyAutoloop:       req.EasyAutoloop,
		EasyAutoloopTarget: btcutil.Amount(req.EasyAutoloopLocalTargetSat),
	}

	if req.AutoloopBudgetRefreshPeriodSec != 0 {
		params.AutoFeeRefreshPeriod =
			time.Duration(req.AutoloopBudgetRefreshPeriodSec) *
				time.Second
	}

	// If an old-style budget was written to storage then express it by
	// using the new auto budget parameters. If the newly added parameters
	// have the 0 default value, but a budget was defined that means the
	// client is using the old style budget parameters.
	if req.AutoloopBudgetRefreshPeriodSec == 0 &&
		req.AutoloopBudgetSat != 0 {

		params.AutoFeeRefreshPeriod = InfiniteDuration
		params.AutoloopBudgetLastRefresh = time.Unix(
			int64(req.AutoloopBudgetStartSec), 0)
	}

	for _, rule := range req.Rules {
		peerRule := rule.Pubkey != nil
		chanRule := rule.ChannelId != 0

		liquidityRule, err := rpcToRule(rule)
		if err != nil {
			return nil, err
		}

		switch {
		case peerRule && chanRule:
			return nil, fmt.Errorf("cannot set channel: %v and "+
				"peer: %v fields in rule", rule.ChannelId,
				rule.Pubkey)

		case peerRule:
			pubkey, err := route.NewVertexFromBytes(rule.Pubkey)
			if err != nil {
				return nil, err
			}

			if _, ok := params.PeerRules[pubkey]; ok {
				return nil, fmt.Errorf("multiple rules set "+
					"for peer: %v", pubkey)
			}

			params.PeerRules[pubkey] = liquidityRule

		case chanRule:
			shortID := lnwire.NewShortChanIDFromInt(rule.ChannelId)

			if _, ok := params.ChannelRules[shortID]; ok {
				return nil, fmt.Errorf("multiple rules set "+
					"for channel: %v", shortID)
			}

			params.ChannelRules[shortID] = liquidityRule

		default:
			return nil, errors.New("please set channel id or " +
				"pubkey for rule")
		}
	}

	return params, nil
}

// ParametersToRpc takes a `Parameters` and creates a `LiquidityParameters`
// from it.
func ParametersToRpc(cfg Parameters) (*clientrpc.LiquidityParameters,
	error) {

	totalRules := len(cfg.ChannelRules) + len(cfg.PeerRules)

	var destaddr string
	if cfg.DestAddr != nil {
		destaddr = cfg.DestAddr.String()
	}

	var addrType clientrpc.AddressType
	switch cfg.AccountAddrType {
	case walletrpc.AddressType_TAPROOT_PUBKEY:
		addrType = clientrpc.AddressType_TAPROOT_PUBKEY

	default:
		addrType = clientrpc.AddressType_ADDRESS_TYPE_UNKNOWN
	}

	rpcCfg := &clientrpc.LiquidityParameters{
		SweepConfTarget:   cfg.SweepConfTarget,
		FailureBackoffSec: uint64(cfg.FailureBackOff.Seconds()),
		Autoloop:          cfg.Autoloop,
		AutoloopBudgetSat: uint64(cfg.AutoFeeBudget),
		AutoloopBudgetRefreshPeriodSec: uint64(
			cfg.AutoFeeRefreshPeriod.Seconds(),
		),
		AutoloopBudgetLastRefresh: uint64(
			cfg.AutoloopBudgetLastRefresh.Unix(),
		),
		AutoMaxInFlight:     uint64(cfg.MaxAutoInFlight),
		AutoloopDestAddress: destaddr,
		Rules: make(
			[]*clientrpc.LiquidityRule, 0, totalRules,
		),
		MinSwapAmount: uint64(
			cfg.ClientRestrictions.Minimum,
		),
		MaxSwapAmount: uint64(
			cfg.ClientRestrictions.Maximum,
		),
		HtlcConfTarget:             cfg.HtlcConfTarget,
		EasyAutoloop:               cfg.EasyAutoloop,
		EasyAutoloopLocalTargetSat: uint64(cfg.EasyAutoloopTarget),
		Account:                    cfg.Account,
		AccountAddrType:            addrType,
	}

	switch f := cfg.FeeLimit.(type) {
	case *FeeCategoryLimit:
		satPerByte := f.SweepFeeRateLimit.FeePerKVByte() / 1000

		rpcCfg.SweepFeeRateSatPerVbyte = uint64(satPerByte)

		rpcCfg.MaxMinerFeeSat = uint64(f.MaximumMinerFee)
		rpcCfg.MaxSwapFeePpm = f.MaximumSwapFeePPM
		rpcCfg.MaxRoutingFeePpm = f.MaximumRoutingFeePPM
		rpcCfg.MaxPrepayRoutingFeePpm = f.MaximumPrepayRoutingFeePPM
		rpcCfg.MaxPrepaySat = uint64(f.MaximumPrepay)

	case *FeePortion:
		rpcCfg.FeePpm = f.PartsPerMillion

	default:
		return nil, fmt.Errorf("unknown fee limit: %T", cfg.FeeLimit)
	}

	for channel, rule := range cfg.ChannelRules {
		rpcRule := newRPCRule(channel.ToUint64(), nil, rule)
		rpcCfg.Rules = append(rpcCfg.Rules, rpcRule)
	}

	for peer, rule := range cfg.PeerRules {
		peer := peer
		rpcRule := newRPCRule(0, peer[:], rule)
		rpcCfg.Rules = append(rpcCfg.Rules, rpcRule)
	}

	return rpcCfg, nil
}

// newRPCRule is a helper function that creates a `LiquidityRule` based on the
// provided `SwapRule` for the given channelID or peer.
func newRPCRule(channelID uint64, peer []byte,
	rule *SwapRule) *clientrpc.LiquidityRule {

	rpcRule := &clientrpc.LiquidityRule{
		ChannelId:         channelID,
		Pubkey:            peer,
		Type:              clientrpc.LiquidityRuleType_THRESHOLD,
		IncomingThreshold: uint32(rule.MinimumIncoming),
		OutgoingThreshold: uint32(rule.MinimumOutgoing),
		SwapType:          clientrpc.SwapType_LOOP_OUT,
	}

	if rule.Type == swap.TypeIn {
		rpcRule.SwapType = clientrpc.SwapType_LOOP_IN
	}

	return rpcRule
}
