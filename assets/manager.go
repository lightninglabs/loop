package assets

import (
	"context"
	"sync"
	"time"

	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	loop_rpc "github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/loop/utils"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightningnetwork/lnd/lntypes"
)

const (
	// ClientKeyFamily is the key family for the assets swap client.
	ClientKeyFamily = 696969
)

// Config holds the configuration for the assets swap manager.
type Config struct {
	AssetClient *TapdClient
	Wallet      lndclient.WalletKitClient
	// ExchangeRateProvider is the exchange rate provider.
	ExchangeRateProvider *FixedExchangeRateProvider
	Signer               lndclient.SignerClient
	ChainNotifier        lndclient.ChainNotifierClient
	Router               lndclient.RouterClient
	LndClient            lndclient.LightningClient
	Store                *PostgresStore
	ServerClient         loop_rpc.AssetsSwapServerClient
}

// AssetsSwapManager handles the lifecycle of asset swaps.
type AssetsSwapManager struct {
	cfg *Config

	expiryManager *utils.ExpiryManager
	txConfManager *utils.TxSubscribeConfirmationManager

	blockHeight    int32
	runCtx         context.Context
	activeSwapOuts map[lntypes.Hash]*OutFSM

	sync.Mutex
}

// NewAssetSwapServer creates a new assets swap manager.
func NewAssetSwapServer(config *Config) *AssetsSwapManager {
	return &AssetsSwapManager{
		cfg: config,

		activeSwapOuts: make(map[lntypes.Hash]*OutFSM),
	}
}

// Run is the main loop for the assets swap manager.
func (m *AssetsSwapManager) Run(ctx context.Context, blockHeight int32) error {
	m.runCtx = ctx
	m.blockHeight = blockHeight

	// Get our tapd client info.
	tapdInfo, err := m.cfg.AssetClient.GetInfo(
		ctx, &taprpc.GetInfoRequest{},
	)
	if err != nil {
		return err
	}
	log.Infof("Tapd info: %v", tapdInfo)

	// Create our subscriptionManagers.
	m.expiryManager = utils.NewExpiryManager(m.cfg.ChainNotifier)
	m.txConfManager = utils.NewTxSubscribeConfirmationManager(
		m.cfg.ChainNotifier,
	)

	// Start the expiry manager.
	errChan := make(chan error, 1)
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := m.expiryManager.Start(ctx, blockHeight)
		if err != nil {
			log.Errorf("Expiry manager failed: %v", err)
			errChan <- err
		}
	}()

	// Recover all the active asset swap outs from the database.
	err = m.recoverSwapOuts(ctx)
	if err != nil {
		return err
	}

	for {
		select {
		case err := <-errChan:
			return err

		case <-ctx.Done():
			wg.Wait()
			return nil
		}
	}
}

// NewSwapOut creates a new asset swap out using the amount and asset id.
// It will wait for the fsm to be in the payprepay state before returning.
func (m *AssetsSwapManager) NewSwapOut(ctx context.Context,
	amt uint64, asset []byte) (*OutFSM, error) {

	// Create a new out fsm.
	outFSM := NewOutFSM(m.getFSMOutConfig())

	// Send the initial event to the fsm.
	err := outFSM.SendEvent(
		m.runCtx, OnRequestAssetOut, &InitSwapOutContext{
			Amount:          amt,
			AssetId:         asset,
			BlockHeightHint: uint32(m.blockHeight),
		},
	)
	if err != nil {
		return nil, err
	}
	// Check if the fsm has an error.
	if outFSM.LastActionError != nil {
		return nil, outFSM.LastActionError
	}

	// Wait for the fsm to be in the state we expect.
	err = outFSM.DefaultObserver.WaitForState(
		ctx, time.Second*15, PayPrepay,
		fsm.WithAbortEarlyOnErrorOption(),
	)
	if err != nil {
		return nil, err
	}

	// Add the swap to the active swap outs.
	m.Lock()
	m.activeSwapOuts[outFSM.SwapOut.SwapHash] = outFSM
	m.Unlock()

	return outFSM, nil
}

// recoverSwapOuts recovers all the active asset swap outs from the database.
func (m *AssetsSwapManager) recoverSwapOuts(ctx context.Context) error {
	// Fetch all the active asset swap outs from the database.
	activeSwapOuts, err := m.cfg.Store.GetActiveAssetOuts(ctx)
	if err != nil {
		return err
	}

	for _, swapOut := range activeSwapOuts {
		log.Debugf("Recovering asset out %v with state %v",
			swapOut.SwapHash, swapOut.State)

		swapOutFSM := NewOutFSMFromSwap(
			m.getFSMOutConfig(), swapOut,
		)

		m.Lock()
		m.activeSwapOuts[swapOut.SwapHash] = swapOutFSM
		m.Unlock()

		// As SendEvent can block, we'll start a goroutine to process
		// the event.
		go func() {
			err := swapOutFSM.SendEvent(ctx, OnRecover, nil)
			if err != nil {
				log.Errorf("FSM %v Error sending recover "+
					"event %v, state: %v",
					swapOutFSM.SwapOut.SwapHash,
					err, swapOutFSM.SwapOut.State)
			}
		}()
	}

	return nil
}

// getFSMOutConfig returns a fsmconfig from the manager.
func (m *AssetsSwapManager) getFSMOutConfig() *FSMConfig {
	return &FSMConfig{
		TapdClient:            m.cfg.AssetClient,
		AssetClient:           m.cfg.ServerClient,
		BlockHeightSubscriber: m.expiryManager,
		TxConfSubscriber:      m.txConfManager,
		ExchangeRateProvider:  m.cfg.ExchangeRateProvider,
		Wallet:                m.cfg.Wallet,

		Store:  m.cfg.Store,
		Signer: m.cfg.Signer,
	}
}

// ListSwapOuts lists all the asset swap outs in the database.
func (m *AssetsSwapManager) ListSwapOuts(ctx context.Context) ([]*SwapOut,
	error) {

	return m.cfg.Store.GetAllAssetOuts(ctx)
}
