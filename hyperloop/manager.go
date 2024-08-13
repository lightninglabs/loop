package hyperloop

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
)

var (
	// defaultFSMObserverTimeout is the default timeout we'll wait for the
	// fsm to reach a certain state.
	defaultFSMObserverTimeout = time.Second * 15
)

// newHyperloopRequest is a request to create a new hyperloop.
type newHyperloopRequest struct {
	amt             btcutil.Amount
	customSweepAddr string
	respChan        chan *Hyperloop
	errChan         chan error
}

// Config contains all the services that the reservation FSM needs to operate.
type Config struct {
	// Store is the store that is used to store the hyperloop.
	// TODO: implement the store.
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
	HyperloopClient swapserverrpc.HyperloopServerClient

	// ChainParams are the params for the bitcoin network.
	ChainParams *chaincfg.Params
}

type Manager struct {
	cfg *Config

	pendingHyperloops map[ID][]*FSM
	hyperloopRequests chan *newHyperloopRequest

	sync.Mutex
}

// NewManager creates a new hyperloop manager.
func NewManager(cfg *Config) *Manager {
	return &Manager{
		cfg:               cfg,
		pendingHyperloops: make(map[ID][]*FSM),
		hyperloopRequests: make(chan *newHyperloopRequest),
	}
}

// Run starts the hyperloop manager.
func (m *Manager) Run(ctx context.Context, initialBlockHeight int32) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Subscribe to blockheight.
	blockChan, errChan, err := m.cfg.ChainNotifier.RegisterBlockEpochNtfn(
		runCtx,
	)
	if err != nil {
		return err
	}

	blockHeight := initialBlockHeight

	for {
		select {
		case blockHeight = <-blockChan:

		case req := <-m.hyperloopRequests:
			hyperloop, err := m.newHyperLoopOut(
				runCtx, req.amt, req.customSweepAddr,
				blockHeight,
			)
			if err != nil {
				log.Errorf("unable to create hyperloop: %v",
					err)
				req.errChan <- err
			} else {
				req.respChan <- hyperloop
			}

		case err := <-errChan:
			log.Errorf("unable to get block height: %v", err)
			return err

		case <-runCtx.Done():
			return nil
		}
	}
}

// RequestNewHyperloop requests a new hyperloop. If we have a pending hyperloop,
// we'll use the same id and sweep address in order to batch a possible sweep.
func (m *Manager) RequestNewHyperloop(ctx context.Context, amt btcutil.Amount,
	customSweepAddr string) (*Hyperloop, error) {

	req := &newHyperloopRequest{
		amt:             amt,
		customSweepAddr: customSweepAddr,
		respChan:        make(chan *Hyperloop),
		errChan:         make(chan error),
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case m.hyperloopRequests <- req:
	}

	var (
		hyperloop *Hyperloop
		err       error
	)

	select {
	case <-ctx.Done():
		return nil, ctx.Err()

	case hyperloop = <-req.respChan:

	case err = <-req.errChan:
	}

	if err != nil {
		return nil, err
	}

	return hyperloop, err
}

// newHyperLoopOut creates a new hyperloop out swap. If we have a pending
// hyperloop, we'll use the same id and sweep address in order to batch
// a possible sweep.
func (m *Manager) newHyperLoopOut(ctx context.Context, amt btcutil.Amount,
	customSweepAddr string, blockheight int32) (*Hyperloop, error) {

	var (
		sweepAddr       btcutil.Address
		publishDeadline time.Time
		err             error
	)

	// For now we'll set the publish deadline to 30 minutes from now.
	publishDeadline = time.Now().Add(time.Minute * 30)

	// Create a sweep pk script.
	if customSweepAddr == "" {
		sweepAddr, err = m.cfg.Wallet.NextAddr(
			ctx, "", walletrpc.AddressType_TAPROOT_PUBKEY,
			false,
		)
		if err != nil {
			return nil, err
		}
	} else {
		sweepAddr, err = btcutil.DecodeAddress(
			customSweepAddr, m.cfg.ChainParams,
		)
		if err != nil {
			return nil, err
		}
	}

	req := &initHyperloopContext{
		swapAmt:          amt,
		sweepAddr:        sweepAddr,
		publishTime:      publishDeadline,
		initiationHeight: blockheight,
		// We'll only do private hyperloops for now.
		private: true,
	}

	// Create a new hyperloop fsm.
	hl, err := NewFSM(m.cfg, m)
	if err != nil {
		return nil, err
	}

	go func() {
		err = hl.SendEvent(ctx, OnStart, req)
		if err != nil {
			log.Errorf("unable to send event to hyperloop fsm: %v",
				err)
		}
	}()

	err = hl.DefaultObserver.WaitForState(
		ctx, defaultFSMObserverTimeout, WaitForPublish,
		fsm.WithAbortEarlyOnErrorOption(),
	)
	if err != nil {
		return nil, err
	}

	m.Lock()
	m.pendingHyperloops[hl.hyperloop.ID] = append(
		m.pendingHyperloops[hl.hyperloop.ID], hl,
	)
	m.Unlock()

	hyperloop := hl.GetVal()
	return hyperloop, nil
}

// fetchHyperLoopTotalSweepAmt returns the total amount that will be swept in
// the hyperloop for the given hyperloop id and sweep address.
func (m *Manager) fetchHyperLoopTotalSweepAmt(hyperloopID ID,
	sweepAddr btcutil.Address) (btcutil.Amount, error) {

	m.Lock()
	defer m.Unlock()
	if hyperloops, ok := m.pendingHyperloops[hyperloopID]; ok {
		var totalAmt btcutil.Amount
		for _, hyperloop := range hyperloops {
			hl := hyperloop.GetVal()
			if hl.SweepAddr.String() == sweepAddr.String() {
				totalAmt += hl.Amt
			}
		}

		return totalAmt, nil
	}

	return 0, errors.New("hyperloop not found")
}
