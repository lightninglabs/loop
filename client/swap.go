package client

import (
	"context"
	"time"

	"github.com/lightninglabs/loop/lndclient"
	"github.com/lightninglabs/loop/utils"
	"github.com/lightningnetwork/lnd/lntypes"
)

type swapKit struct {
	htlc *utils.Htlc
	hash lntypes.Hash

	height int32

	log *utils.SwapLog

	lastUpdateTime time.Time
	cost           SwapCost
	state          SwapState
	executeConfig
	swapConfig

	contract *SwapContract
	swapType SwapType
}

func newSwapKit(hash lntypes.Hash, swapType SwapType, cfg *swapConfig,
	contract *SwapContract) (*swapKit, error) {

	// Compose expected on-chain swap script
	htlc, err := utils.NewHtlc(
		contract.CltvExpiry, contract.SenderKey,
		contract.ReceiverKey, hash,
	)
	if err != nil {
		return nil, err
	}

	// Log htlc address for debugging.
	htlcAddress, err := htlc.Address(cfg.lnd.ChainParams)
	if err != nil {
		return nil, err
	}

	log := &utils.SwapLog{
		Hash:   hash,
		Logger: logger,
	}

	log.Infof("Htlc address: %v", htlcAddress)

	return &swapKit{
		swapConfig: *cfg,
		hash:       hash,
		log:        log,
		htlc:       htlc,
		state:      StateInitiated,
		contract:   contract,
		swapType:   swapType,
	}, nil
}

// sendUpdate reports an update to the swap state.
func (s *swapKit) sendUpdate(ctx context.Context) error {
	info := &SwapInfo{
		SwapContract: *s.contract,
		SwapHash:     s.hash,
		SwapType:     s.swapType,
		LastUpdate:   s.lastUpdateTime,
		State:        s.state,
	}

	s.log.Infof("state %v", info.State)

	select {
	case s.statusChan <- *info:
	case <-ctx.Done():
		return ctx.Err()
	}

	return nil
}

type genericSwap interface {
	execute(mainCtx context.Context, cfg *executeConfig,
		height int32) error
}

type swapConfig struct {
	lnd    *lndclient.LndServices
	store  swapClientStore
	server swapServerClient
}
