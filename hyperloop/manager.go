package hyperloop

import (
	"context"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	looprpc "github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntypes"
)

var (
	// defaultFSMObserverTimeout is the default timeout we'll wait for the fsm
	// to reach a certain state.
	defaultFSMObserverTimeout = time.Second * 15
)

// Config contains all the services that the reservation FSM needs to operate.
type Config struct {

	// Store is the store that is used to store the hyperloop.
	Store Store

	// Wallet handles the key derivation for the reservation.
	Wallet lndclient.WalletKitClient

	// ChainNotifier is used to subscribe to block notifications.
	ChainNotifier lndclient.ChainNotifierClient

	// Signer is used to sign messages.
	Signer lndclient.SignerClient

	// Router is used to pay the offchain payments.
	Router lndclient.RouterClient

	// HyperloopClient is the client used to communicate with the
	// swap server.
	HyperloopClient looprpc.HyperloopServerClient

	// ChainParams are the params for the bitcoin network.
	ChainParams *chaincfg.Params
}

type Manager struct {
	cfg    *Config
	runCtx context.Context

	activePendingHyperloop *HyperLoop
	pendingHyperloops      map[lntypes.Hash]*HyperLoop

	sync.Mutex
}

// NewManager creates a new hyperloop manager.
func NewManager(cfg *Config) *Manager {
	return &Manager{
		cfg:               cfg,
		pendingHyperloops: make(map[lntypes.Hash]*HyperLoop),
	}
}

// Run starts the hyperloop manager.
func (m *Manager) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	m.runCtx = runCtx

	for {
		select {
		case <-runCtx.Done():
			return nil
		}
	}
}

// NewHyperLoopOut creates a new hyperloop out swap. If we have a pending
// hyperloop, we'll use the same id and sweep address in order to batch
// a possible sweep.
func (m *Manager) NewHyperLoopOut(ctx context.Context, amt btcutil.Amount,
	customSweepAddr string) (*HyperLoop, error) {

	var (
		hyperloopID     ID
		sweepAddr       btcutil.Address
		publishDeadline time.Time
		err             error
	)

	// As we're only doing private hyperloops for now, we'll create an id.
	if m.activePendingHyperloop == nil {
		hyperloopID, err = NewHyperLoopId()
		if err != nil {
			return nil, err
		}

		publishDeadline = time.Now().Add(time.Minute * 30)

		// Create a sweep pk script.
		if customSweepAddr == "" {
			sweepAddr, err = m.cfg.Wallet.NextAddr(
				ctx, "", walletrpc.AddressType_TAPROOT_PUBKEY, false,
			)
			if err != nil {
				return nil, err
			}
		} else {
			sweepAddr, err = btcutil.DecodeAddress(customSweepAddr, nil)
			if err != nil {
				return nil, err
			}
		}
	} else {
		hyperloopID = m.activePendingHyperloop.ID
		publishDeadline = m.activePendingHyperloop.PublishTime

		if customSweepAddr == "" {
			sweepAddr = m.activePendingHyperloop.SweepAddr
		} else {
			addr, err := btcutil.DecodeAddress(customSweepAddr, nil)
			if err != nil {
				return nil, err
			}

			sweepAddr = addr
		}
	}

	req := &initHyperloopContext{
		hyperloopID: hyperloopID,
		swapAmt:     amt,
		sweepAddr:   sweepAddr,
		publishTime: publishDeadline,
	}

	// Create a new hyperloop fsm.
	hl, err := NewFSM(m.runCtx, m.cfg)
	if err != nil {
		return nil, err
	}

	err = hl.SendEvent(OnInit, req)
	if err != nil {
		return nil, err
	}

	err = hl.DefaultObserver.WaitForState(
		ctx, defaultFSMObserverTimeout, WaitForPublish,
		fsm.WithAbortEarlyOnErrorOption(),
	)
	if err != nil {
		return nil, err
	}

	return hl.hyperloop, nil
}
