package loopin

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/labels"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	looprpc "github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/routing/route"
)

// Config contains the services required for the loop-in manager.
type Config struct {
	// Server is the client that is used to communicate with the static
	// address server.
	Server looprpc.StaticAddressServerClient

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
	QuoteGetter QuoteGetter

	// NodePubKey is used to get a loo-in quote.
	NodePubkey route.Vertex

	// WalletKit is the wallet client that is used to derive new keys from
	// lnd's wallet.
	WalletKit lndclient.WalletKitClient

	// ChainParams is the chain configuration(mainnet, testnet...) this
	// manager uses.
	ChainParams *chaincfg.Params

	// Chain is the chain notifier that is used to listen for new
	// blocks.
	ChainNotifier lndclient.ChainNotifierClient

	// Signer is the signer client that is used to sign transactions.
	Signer lndclient.SignerClient

	// Store is the database store that is used to store static address
	// loop-in related records.
	Store StaticAddressLoopInStore

	// ValidateLoopInContract validates the contract parameters against our
	// request.
	ValidateLoopInContract ValidateLoopInContract
}

// Manager manages the address state machines.
type Manager struct {
	cfg *Config

	runCtx context.Context

	sync.Mutex

	// initChan signals the daemon that the address manager has completed
	// its initialization.
	initChan chan struct{}

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
	m.currentHeight = currentHeight
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

		log.Debugf("Recovering loopIn %x", loopIn.SwapHash[:])

		// Retrieve all deposits regardless of deposit state.
		// The second return value indicates true iff all returned
		// deposits are contained in the in-mem active deposits map. On
		// recovery that is never the case so we can ignore it here.
		loopIn.Deposits, _ = m.cfg.DepositManager.AllStringOutpointsActiveDeposits( //nolint:lll
			loopIn.DepositOutpoints, fsm.EmptyState,
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
	// Validate the loop-in request.
	if len(req.DepositOutpoints) == 0 {
		return fmt.Errorf("no deposit outpoints provided")
	}

	// Retrieve all deposits referenced by the outpoints and ensure that
	// they are in state Deposited.
	deposits, active := m.cfg.DepositManager.AllStringOutpointsActiveDeposits( //nolint:lll
		req.DepositOutpoints, deposit.Deposited,
	)
	if !active {
		return fmt.Errorf("one or more deposits are not in state %s",
			deposit.Deposited)
	}
	tmp := &StaticAddressLoopIn{
		Deposits: deposits,
	}
	totalDepositAmount := tmp.TotalDepositAmount()

	// Check that the label is valid.
	err := labels.Validate(req.Label)
	if err != nil {
		return fmt.Errorf("invalid label: %w", err)
	}

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
			return fmt.Errorf("unable to generate hop hints: %w",
				err)
		}
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
	numDeposits := uint32(len(deposits))
	quote, err := m.cfg.QuoteGetter.GetLoopInQuote(
		m.runCtx, totalDepositAmount, m.cfg.NodePubkey, req.LastHop,
		req.RouteHints, req.Initiator, numDeposits,
	)
	if err != nil {
		return fmt.Errorf("unable to get loop in quote: %w", err)
	}

	// If the previously accepted quote fee is lower than what is quoted now
	// we abort the swap.
	if quote.SwapFee > req.MaxSwapFee {
		log.Warnf("Swap fee %v exceeding maximum of %v",
			quote.SwapFee, req.MaxSwapFee)

		return loop.ErrSwapFeeTooHigh
	}

	paymentTimeoutSeconds := uint32(DefaultPaymentTimeoutSeconds)
	if req.PaymentTimeoutSeconds != 0 {
		paymentTimeoutSeconds = req.PaymentTimeoutSeconds
	}

	swap := &StaticAddressLoopIn{
		DepositOutpoints:      req.DepositOutpoints,
		Deposits:              deposits,
		Label:                 req.Label,
		Initiator:             req.Initiator,
		RouteHints:            req.RouteHints,
		QuotedSwapFee:         quote.SwapFee,
		MaxSwapFee:            req.MaxSwapFee,
		PaymentTimeoutSeconds: paymentTimeoutSeconds,
	}
	if req.LastHop != nil {
		swap.LastHop = req.LastHop[:]
	}

	m.Lock()
	swap.InitiationHeight = m.currentHeight
	m.Unlock()

	return m.startLoopInFsm(swap)
}

// startLoopInFsm initiates a loop-in state machine based on the user-provided
// swap information, sends that info to the server and waits for the server to
// return htlc signature information. It then creates the loop-in object in the
// database.
func (m *Manager) startLoopInFsm(loopIn *StaticAddressLoopIn) error {
	// Create a state machine for a given deposit.
	recovery := false
	loopInFsm, err := NewFSM(m.runCtx, loopIn, m.cfg, recovery)
	if err != nil {
		return err
	}

	// Send the start event to the state machine.
	go func() {
		err := loopInFsm.SendEvent(OnInitHtlc, nil)
		if err != nil {
			log.Errorf("Error sending OnNewRequest event: %v", err)
		}
	}()

	// If an error occurs before SignHtlcTx is reached we consider the swap
	// failed and abort early.
	err = loopInFsm.DefaultObserver.WaitForState(
		m.runCtx, time.Minute, SignHtlcTx,
		fsm.WithAbortEarlyOnErrorOption(),
	)
	if err != nil {
		return err
	}

	return nil
}

// GetAllSwaps returns all static address loop-in swaps from the database store.
func (m *Manager) GetAllSwaps() ([]*StaticAddressLoopIn, error) {
	swaps, err := m.cfg.Store.AllLoopIns(m.runCtx)
	if err != nil {
		return nil, err
	}

	allDeposits, err := m.cfg.DepositManager.GetAllDeposits()
	if err != nil {
		return nil, err
	}

	var depositLookup = make(map[string]*deposit.Deposit)
	for i, d := range allDeposits {
		depositLookup[d.OutPoint.String()] = allDeposits[i]
	}

	for i, s := range swaps {
		var deposits []*deposit.Deposit
		for _, outpoint := range s.DepositOutpoints {
			if d, ok := depositLookup[outpoint]; ok {
				deposits = append(deposits, d)
			}
		}

		swaps[i].Deposits = deposits
	}

	return swaps, nil
}
