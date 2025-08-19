package loop

import (
	"context"
	"time"

	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/assets"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/utils"
	"github.com/lightningnetwork/lnd/lntypes"
)

type swapKit struct {
	swapConfig

	hash lntypes.Hash

	height int32 //nolint:structcheck

	log *swap.PrefixLog

	lastUpdateTime time.Time

	cost loopdb.SwapCost

	state loopdb.SwapState

	contract *loopdb.SwapContract

	swapType swap.Type
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

// IsTaproot returns true if the swap referenced by the passed swap contract
// uses the v3 (taproot) htlc.
func IsTaprootSwap(swapContract *loopdb.SwapContract) bool {
	return utils.GetHtlcScriptVersion(swapContract.ProtocolVersion) == swap.HtlcV3
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
	assets *assets.TapdClient
}

func newSwapConfig(lnd *lndclient.LndServices, store loopdb.SwapStore,
	server swapServerClient, assets *assets.TapdClient) *swapConfig {

	return &swapConfig{
		lnd:    lnd,
		store:  store,
		server: server,
		assets: assets,
	}
}
