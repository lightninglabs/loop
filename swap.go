package loop

import (
	"context"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/lntypes"
)

type swapKit struct {
	hash lntypes.Hash

	height int32 //nolint:structcheck

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

	// If the swap was initiated before we had our v3 script, use v2.
	if protocolVersion < loopdb.ProtocolVersionHtlcV3 {
		return swap.HtlcV2
	}

	return swap.HtlcV3
}

// IsTaproot returns true if the swap referenced by the passed swap contract
// uses the v3 (taproot) htlc.
func IsTaprootSwap(swapContract *loopdb.SwapContract) bool {
	return GetHtlcScriptVersion(swapContract.ProtocolVersion) == swap.HtlcV3
}

// GetHtlc composes and returns the on-chain swap script.
func GetHtlc(hash lntypes.Hash, contract *loopdb.SwapContract,
	chainParams *chaincfg.Params) (*swap.Htlc, error) {

	switch GetHtlcScriptVersion(contract.ProtocolVersion) {
	case swap.HtlcV2:
		return swap.NewHtlcV2(
			contract.CltvExpiry, contract.SenderKey,
			contract.ReceiverKey, hash,
			chainParams,
		)

	case swap.HtlcV3:
		return swap.NewHtlcV3(
			contract.CltvExpiry, contract.SenderKey,
			contract.ReceiverKey, contract.SenderKey,
			contract.ReceiverKey, hash,
			chainParams,
		)
	}

	return nil, swap.ErrInvalidScriptVersion
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
