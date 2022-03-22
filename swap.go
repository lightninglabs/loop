package loop

import (
	"context"
	"time"

	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/lntypes"
)

type swapKit struct {
	hash lntypes.Hash

	height int32

	log *swap.PrefixLog

	lastUpdateTime time.Time

	cost loopdb.SwapCost

	state loopdb.SwapState

	contract *loopdb.SwapContract

	swapType swap.Type

	swapConfig
}

func newSwapKit(hash lntypes.Hash, swapType swap.Type, cfg *swapConfig,
	contract *loopdb.SwapContract) *swapKit {

	log := &swap.PrefixLog{
		Hash:   hash,
		Logger: log,
	}

	return &swapKit{
		swapConfig: *cfg,
		hash:       hash,
		log:        log,
		state:      loopdb.StateInitiated,
		contract:   contract,
		swapType:   swapType,
	}
}

// GetHtlcScriptVersion returns the correct HTLC script version for the passed
// protocol version.
func GetHtlcScriptVersion(
	protocolVersion loopdb.ProtocolVersion) swap.ScriptVersion {

	if protocolVersion >= loopdb.ProtocolVersionHtlcV2 &&
		protocolVersion < loopdb.ProtocolVersionTaproot {
		// Protocol versions 5 through 9 only support v2 HTLCs

		return swap.HtlcV2

	} else if protocolVersion == loopdb.ProtocolVersionTaproot {
		// Only Protocol version 10 supports v3 HTLCs

		return swap.HtlcV3
	}

	// All other cases, including versions 0 through 4 and 11 to max int
	// should be considered legacy v1 HTLCs
	return swap.HtlcV1
}

// getHtlc composes and returns the on-chain swap script.
func (s *swapKit) getHtlc(outputType swap.HtlcOutputType) (*swap.Htlc, error) {
	return swap.NewHtlc(
		GetHtlcScriptVersion(s.contract.ProtocolVersion),
		s.contract.CltvExpiry, s.contract.SenderKey,
		s.contract.ReceiverKey, s.hash, outputType,
		s.swapConfig.lnd.ChainParams,
	)
}

// swapInfo constructs and returns a filled SwapInfo from
// the swapKit.
func (s *swapKit) swapInfo() *SwapInfo {
	return &SwapInfo{
		SwapContract: *s.contract,
		SwapHash:     s.hash,
		SwapType:     s.swapType,
		LastUpdate:   s.lastUpdateTime,
		SwapStateData: loopdb.SwapStateData{
			State: s.state,
			Cost:  s.cost,
		},
	}
}

type genericSwap interface {
	execute(mainCtx context.Context, cfg *executeConfig,
		height int32) error
}

type swapConfig struct {
	lnd    *lndclient.LndServices
	store  loopdb.SwapStore
	server swapServerClient
}

func newSwapConfig(lnd *lndclient.LndServices, store loopdb.SwapStore,
	server swapServerClient) *swapConfig {

	return &swapConfig{
		lnd:    lnd,
		store:  store,
		server: server,
	}
}
