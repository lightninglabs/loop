package loopin

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	fsm2 "github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/labels"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	looprpc "github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/routing/route"
)

// Config contains the services required for the loop-in manager.
type Config struct {
	// StaticAddressServerClient is the client that is used to communicate
	// with the static address server.
	StaticAddressServerClient looprpc.StaticAddressServerClient

	// AddressManager gives the withdrawal manager access to static address
	// parameters.
	AddressManager AddressManager

	// DepositManager gives the withdrawal manager access to the deposits
	// enabling it to create and manage loop-ins.
	DepositManager DepositManager

	// LndClient is used to add invoices and select hop hints.
	LndClient lndclient.LightningClient

	// InvoicesClient is used to subscribe to invoice settlements and
	// cancel invoices.
	InvoicesClient lndclient.InvoicesClient

	// SwapClient is used to get loop in quotes.
	SwapClient *loop.Client

	// NodePubKey is used to get a loo-in quote.
	NodePubkey route.Vertex

	// WalletKit is the wallet client that is used to derive new keys from
	// lnd's wallet.
	WalletKit lndclient.WalletKitClient

	// ChainParams is the chain configuration(mainnet, testnet...) this
	// manager uses.
	ChainParams *chaincfg.Params

	// ChainNotifier is the chain notifier that is used to listen for new
	// blocks.
	ChainNotifier lndclient.ChainNotifierClient

	// Signer is the signer client that is used to sign transactions.
	Signer lndclient.SignerClient

	// Store is the database store that is used to store static address
	// loop-in related records.
	Store StaticAddressLoopInStore
}

// Manager manages the address state machines.
type Manager struct {
	cfg *Config

	runCtx context.Context

	sync.Mutex

	// initChan signals the daemon that the address manager has completed
	// its initialization.
	initChan chan struct{}

	// initiationHeight stores the currently best known block height.
	initiationHeight uint32

	// currentHeight stores the currently best known block height.
	currentHeight uint32
}

// NewManager creates a new deposit withdrawal manager.
func NewManager(cfg *Config) *Manager {
	return &Manager{
		cfg:      cfg,
		initChan: make(chan struct{}),
	}
}

// Run runs the static address loop-in manager.
func (m *Manager) Run(ctx context.Context, currentHeight uint32) error {
	m.runCtx = ctx

	m.Lock()
	m.currentHeight, m.initiationHeight = currentHeight, currentHeight
	m.Unlock()

	registerBlockNtfn := m.cfg.ChainNotifier.RegisterBlockEpochNtfn
	newBlockChan, newBlockErrChan, err := registerBlockNtfn(m.runCtx)
	if err != nil {
		return err
	}

	// Upon start of the loop-in manager we reinstate all previous loop-ins
	// that are not yet completed.
	err = m.recover()
	if err != nil {
		return err
	}

	// Communicate to the caller that the address manager has completed its
	// initialization.
	close(m.initChan)

	for {
		select {
		case height := <-newBlockChan:
			m.Lock()
			m.currentHeight = uint32(height)
			m.Unlock()

		case err = <-newBlockErrChan:
			return err

		case <-m.runCtx.Done():
			return m.runCtx.Err()
		}
	}
}

// recover stars a loop-in state machine for each non-final loop-in to pick up
// work where it was left off before the restart.
func (m *Manager) recover() error {
	log.Infof("Recovering static address loop-ins...")

	// Recover loop-ins.
	loopIns, err := m.cfg.Store.AllLoopIns(m.runCtx)
	if err != nil {
		return err
	}

	for _, loopIn := range loopIns {
		// If the current deposit is final it wasn't active when we
		// shut down the client last. So we don't need to start a fsm
		// for it.
		if loopIn.IsInFinalState() {
			continue
		}

		log.Debugf("Recovering loopIn %x", loopIn.SwapHash)

		// Retrieve all deposits regardless of deposit state.
		loopIn.Deposits, _ = m.cfg.DepositManager.AllStringOutpointsActiveDeposits( //nolint:lll
			loopIn.DepositOutpoints, fsm2.EmptyState,
		)

		loopIn.AddressParams, err = m.cfg.AddressManager.GetStaticAddressParameters( //nolint:lll
			m.runCtx,
		)
		if err != nil {
			return err
		}

		loopIn.Address, err = m.cfg.AddressManager.GetStaticAddress(
			m.runCtx,
		)
		if err != nil {
			return err
		}

		// Create a state machine for a given loop-in.
		recovery := true
		fsm, err := NewFSM(m.runCtx, loopIn, m.cfg, recovery)
		if err != nil {
			return err
		}

		// Send the OnRecover event to the state machine.
		go func() {
			err = fsm.SendEvent(OnRecover, nil)
			if err != nil {
				log.Errorf("Error sending OnStart event: %v",
					err)
			}
		}()
	}

	return nil
}

// WaitInitComplete waits until the static address loop-in manager has completed
// its setup.
func (m *Manager) WaitInitComplete() {
	defer log.Debugf("Static address loop-in manager initiation complete.")
	<-m.initChan
}

// InitiateLoopIn initiates a loop-in swap. It passes the request to the server
// along with all relevant loop-in information.
func (m *Manager) InitiateLoopIn(req *loop.StaticAddressLoopInRequest) error {
	loopIn := &StaticAddressLoopIn{}

	// Validate the loop-in request.
	if len(req.DepositOutpoints) == 0 {
		return fmt.Errorf("no deposit outpoints provided")
	}
	loopIn.DepositOutpoints = req.DepositOutpoints

	// Retrieve all deposits referenced by the outpoints and ensure that
	// they are in state Deposited.
	deposits, active := m.cfg.DepositManager.AllStringOutpointsActiveDeposits( //nolint:lll
		loopIn.DepositOutpoints, deposit.Deposited,
	)
	if !active {
		return fmt.Errorf("one or more deposits are not in state %s",
			deposit.Deposited)
	}
	loopIn.Deposits = deposits
	totalDepositAmount := loopIn.TotalDepositAmount()

	// Check that the label is valid.
	err := labels.Validate(req.Label)
	if err != nil {
		return fmt.Errorf("invalid label: %v", err)
	}
	loopIn.Label = req.Label
	loopIn.Initiator = req.Initiator

	// Private and route hints are mutually exclusive as setting private
	// means we retrieve our own route hints from the connected node.
	if len(req.RouteHints) != 0 && req.Private {
		return fmt.Errorf("private and route hints are mutually " +
			"exclusive")
	}

	// If private is set, we generate route hints.
	if req.Private {
		// If last_hop is set, we'll only add channels with peers set to
		// the last_hop parameter.
		includeNodes := make(map[route.Vertex]struct{})
		if req.LastHop != nil {
			includeNodes[*req.LastHop] = struct{}{}
		}

		// Because the Private flag is set, we'll generate our own set
		// of hop hints.
		req.RouteHints, err = loop.SelectHopHints(
			m.runCtx, m.cfg.LndClient, totalDepositAmount,
			loop.DefaultMaxHopHints, includeNodes,
		)
		if err != nil {
			return fmt.Errorf("unable to generate hop hints: %v",
				err)
		}
	}
	loopIn.RouteHints = req.RouteHints
	loopIn.Private = req.Private
	if req.LastHop != nil {
		loopIn.LastHop = req.LastHop[:]
	}

	// Request current server loop in terms and use these to calculate the
	// swap fee that we should subtract from the swap amount in the payment
	// request that we send to the server. We pass nil as optional route
	// hints as hop hint selection when generating invoices with private
	// channels is an LND side black box feature. Advanced users will quote
	// directly anyway and there they have the option to add specific route
	// hints.
	// The quote call will also request a probe from the server to ensure
	// feasibility of a loop-in for the totalDepositAmount.
	quote, err := m.cfg.SwapClient.Server.GetLoopInQuote(
		m.runCtx, totalDepositAmount, m.cfg.NodePubkey, req.LastHop,
		req.RouteHints, req.Initiator, uint32(len(loopIn.Deposits)),
	)
	if err != nil {
		return fmt.Errorf("unable to get loop in quote: %v", err)
	}

	// If the previously accepted quote fee is lower than what is quoted now
	// we abort the swap.
	if quote.SwapFee > req.MaxSwapFee {
		log.Warnf("Swap fee %v exceeding maximum of %v",
			quote.SwapFee, req.MaxSwapFee)

		return loop.ErrSwapFeeTooHigh
	}
	loopIn.QuotedSwapFee = quote.SwapFee
	loopIn.MaxSwapFee = req.MaxSwapFee

	m.Lock()
	loopIn.InitiationHeight = m.currentHeight
	m.Unlock()

	return m.startLoopInFsm(loopIn)
}

// startLoopInFsm initiates a loop-in state machine based on the user-provided
// swap information, sends that info to the server and waits for the server to
// return htlc signature information. It then creates the loop-in object in the
// database.
func (m *Manager) startLoopInFsm(loopIn *StaticAddressLoopIn) error {
	// Create a state machine for a given deposit.
	recovery := false
	fsm, err := NewFSM(m.runCtx, loopIn, m.cfg, recovery)
	if err != nil {
		return err
	}

	// Send the start event to the state machine.
	go func() {
		err := fsm.SendEvent(OnInitHtlc, nil)
		if err != nil {
			log.Errorf("Error sending OnNewRequest event: %v", err)
		}
	}()

	// If an error occurs before SignHtlcTx is reached we consider the swap
	// failed and abort early.
	err = fsm.DefaultObserver.WaitForState(
		m.runCtx, time.Minute, SignHtlcTx,
		fsm2.WithAbortEarlyOnErrorOption(),
	)
	if err != nil {
		return err
	}

	// Once the SignHtlcTx state is reached the server registered the
	// loop-in and assigned it an ID that we can store.
	err = m.cfg.Store.CreateLoopIn(m.runCtx, loopIn)
	if err != nil {
		return err
	}

	return nil
}
