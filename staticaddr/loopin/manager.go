package loopin

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/labels"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	looprpc "github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/lntypes"
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

	// MaxStaticAddrHtlcFeePercentage is the percentage of the swap amount
	// that we allow the server to charge for the htlc transaction.
	// Although highly unlikely, this is a defense against the server
	// publishing the htlc without paying the swap invoice, forcing us to
	// sweep the timeout path.
	MaxStaticAddrHtlcFeePercentage float64

	// MaxStaticAddrHtlcBackupFeePercentage is the percentage of the swap
	// amount that we allow the server to charge for the htlc backup
	// transactions. This is a defense against the server publishing the
	// htlc backup without paying the swap invoice, forcing us to sweep the
	// timeout path. This value is elevated compared to
	// MaxStaticAddrHtlcFeePercentage since it serves the server as backup
	// transaction in case of fee spikes.
	MaxStaticAddrHtlcBackupFeePercentage float64
}

// newSwapRequest is used to send a loop-in request to the manager main loop.
type newSwapRequest struct {
	loopInRequest *loop.StaticAddressLoopInRequest
	respChan      chan *newSwapResponse
}

// newSwapResponse is used to return the loop-in swap and error to the server.
type newSwapResponse struct {
	loopIn *StaticAddressLoopIn
	err    error
}

// Manager manages the address state machines.
type Manager struct {
	cfg *Config

	// initChan signals the daemon that the address manager has completed
	// its initialization.
	initChan chan struct{}

	// newLoopInChan receives swap requests from the server and initiates
	// loop-in swaps.
	newLoopInChan chan *newSwapRequest

	// exitChan signals the manager's subroutines that the main looop ctx
	// has been canceled.
	exitChan chan struct{}

	// errChan forwards errors from the loop-in manager to the server.
	errChan chan error

	// currentHeight stores the currently best known block height.
	currentHeight atomic.Uint32

	activeLoopIns map[lntypes.Hash]*FSM
}

// NewManager creates a new deposit withdrawal manager.
func NewManager(cfg *Config) *Manager {
	return &Manager{
		cfg:           cfg,
		initChan:      make(chan struct{}),
		newLoopInChan: make(chan *newSwapRequest),
		exitChan:      make(chan struct{}),
		errChan:       make(chan error),
		activeLoopIns: make(map[lntypes.Hash]*FSM),
	}
}

// Run runs the static address loop-in manager.
func (m *Manager) Run(ctx context.Context, currentHeight uint32) error {
	m.currentHeight.Store(currentHeight)

	registerBlockNtfn := m.cfg.ChainNotifier.RegisterBlockEpochNtfn
	newBlockChan, newBlockErrChan, err := registerBlockNtfn(ctx)
	if err != nil {
		return err
	}

	// Upon start of the loop-in manager we reinstate all previous loop-ins
	// that are not yet completed.
	err = m.recoverLoopIns(ctx)
	if err != nil {
		return err
	}

	// Communicate to the caller that the address manager has completed its
	// initialization.
	close(m.initChan)

	var loopIn *StaticAddressLoopIn
	for {
		select {
		case height := <-newBlockChan:
			m.currentHeight.Store(uint32(height))

		case err = <-newBlockErrChan:
			return err

		case request := <-m.newLoopInChan:
			loopIn, err = m.initiateLoopIn(
				ctx, request.loopInRequest,
			)
			if err != nil {
				log.Errorf("Error initiating loop-in swap: %v",
					err)
			}

			// We forward the initialized loop-in and error to
			// DeliverLoopInRequest.
			resp := &newSwapResponse{
				loopIn: loopIn,
				err:    err,
			}
			select {
			case request.respChan <- resp:

			case <-ctx.Done():
				// Noify subroutines that the main loop has been
				// canceled.
				close(m.exitChan)

				return ctx.Err()
			}

		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// recover stars a loop-in state machine for each non-final loop-in to pick up
// work where it was left off before the restart.
func (m *Manager) recoverLoopIns(ctx context.Context) error {
	log.Infof("Recovering static address loop-ins...")

	// Recover loop-ins.
	// Recover pending static address loop-ins.
	pendingLoopIns, err := m.cfg.Store.GetStaticAddressLoopInSwapsByStates(
		ctx, PendingStates,
	)
	if err != nil {
		return err
	}

	for _, loopIn := range pendingLoopIns {
		log.Debugf("Recovering loopIn %x", loopIn.SwapHash[:])

		// Retrieve all deposits regardless of deposit state. If any of
		// the deposits is not active in the in-mem map of the deposits
		// manager we log it, but continue to recover the loop-in.
		var allActive bool
		loopIn.Deposits, allActive =
			m.cfg.DepositManager.AllStringOutpointsActiveDeposits(
				loopIn.DepositOutpoints, fsm.EmptyState,
			)

		if !allActive {
			log.Errorf("one or more deposits are not active")
		}

		loopIn.AddressParams, err =
			m.cfg.AddressManager.GetStaticAddressParameters(ctx)

		if err != nil {
			return err
		}

		loopIn.Address, err = m.cfg.AddressManager.GetStaticAddress(
			ctx,
		)
		if err != nil {
			return err
		}

		// Create a state machine for a given loop-in.
		var (
			recovery = true
			fsm      *FSM
		)
		fsm, err = NewFSM(ctx, loopIn, m.cfg, recovery)
		if err != nil {
			return err
		}

		// Send the OnRecover event to the state machine.
		swapHash := loopIn.SwapHash
		go func() {
			err = fsm.SendEvent(ctx, OnRecover, nil)
			if err != nil {
				log.Errorf("Error sending OnStart event: %v",
					err)
			}

			m.activeLoopIns[swapHash] = fsm
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

// DeliverLoopInRequest forwards a loop-in request from the server to the
// manager run loop to initiate a new loop-in swap.
func (m *Manager) DeliverLoopInRequest(ctx context.Context,
	req *loop.StaticAddressLoopInRequest) (*StaticAddressLoopIn, error) {

	request := &newSwapRequest{
		loopInRequest: req,
		respChan:      make(chan *newSwapResponse),
	}

	// Send the new loop-in request to the manager run loop.
	select {
	case m.newLoopInChan <- request:

	case <-m.exitChan:
		return nil, fmt.Errorf("loop-in manager has been canceled")

	case <-ctx.Done():
		return nil, fmt.Errorf("context canceled while initiating " +
			"a loop-in swap")
	}

	// Wait for the response from the manager run loop.
	select {
	case resp := <-request.respChan:
		return resp.loopIn, resp.err

	case <-m.exitChan:
		return nil, fmt.Errorf("loop-in manager has been canceled")

	case <-ctx.Done():
		return nil, fmt.Errorf("context canceled while waiting for " +
			"loop-in swap response")
	}
}

// initiateLoopIn initiates a loop-in swap. It passes the request to the server
// along with all relevant loop-in information.
func (m *Manager) initiateLoopIn(ctx context.Context,
	req *loop.StaticAddressLoopInRequest) (*StaticAddressLoopIn, error) {

	// Validate the loop-in request.
	if len(req.DepositOutpoints) == 0 {
		return nil, fmt.Errorf("no deposit outpoints provided")
	}

	// Retrieve all deposits referenced by the outpoints and ensure that
	// they are in state Deposited.
	deposits, active := m.cfg.DepositManager.AllStringOutpointsActiveDeposits( //nolint:lll
		req.DepositOutpoints, deposit.Deposited,
	)
	if !active {
		return nil, fmt.Errorf("one or more deposits are not in "+
			"state %s", deposit.Deposited)
	}

	// Calculate the total deposit amount.
	tmp := &StaticAddressLoopIn{
		Deposits: deposits,
	}
	totalDepositAmount := tmp.TotalDepositAmount()

	// Check that the label is valid.
	err := labels.Validate(req.Label)
	if err != nil {
		return nil, fmt.Errorf("invalid label: %w", err)
	}

	// Private and route hints are mutually exclusive as setting private
	// means we retrieve our own route hints from the connected node.
	if len(req.RouteHints) != 0 && req.Private {
		return nil, fmt.Errorf("private and route hints are mutually " +
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
			ctx, m.cfg.LndClient, totalDepositAmount,
			loop.DefaultMaxHopHints, includeNodes,
		)
		if err != nil {
			return nil, fmt.Errorf("unable to generate hop "+
				"hints: %w", err)
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
		ctx, totalDepositAmount, m.cfg.NodePubkey, req.LastHop,
		req.RouteHints, req.Initiator, numDeposits,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to get loop in quote: %w", err)
	}

	// If the previously accepted quote fee is lower than what is quoted now
	// we abort the swap.
	if quote.SwapFee > req.MaxSwapFee {
		log.Warnf("Swap fee %v exceeding maximum of %v",
			quote.SwapFee, req.MaxSwapFee)

		return nil, loop.ErrSwapFeeTooHigh
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
		InitiationTime:        time.Now(),
		RouteHints:            req.RouteHints,
		QuotedSwapFee:         quote.SwapFee,
		MaxSwapFee:            req.MaxSwapFee,
		PaymentTimeoutSeconds: paymentTimeoutSeconds,
	}
	if req.LastHop != nil {
		swap.LastHop = req.LastHop[:]
	}

	swap.InitiationHeight = m.currentHeight.Load()

	return m.startLoopInFsm(ctx, swap)
}

// startLoopInFsm initiates a loop-in state machine based on the user-provided
// swap information, sends that info to the server and waits for the server to
// return htlc signature information. It then creates the loop-in object in the
// database.
func (m *Manager) startLoopInFsm(ctx context.Context,
	loopIn *StaticAddressLoopIn) (*StaticAddressLoopIn, error) {

	// Create a state machine for a given deposit.
	recovery := false
	loopInFsm, err := NewFSM(ctx, loopIn, m.cfg, recovery)
	if err != nil {
		return nil, err
	}

	// Send the start event to the state machine.
	go func() {
		err = loopInFsm.SendEvent(ctx, OnInitHtlc, nil)
		if err != nil {
			log.Errorf("Error sending OnNewRequest event: %v", err)
		}
	}()

	// If an error occurs before SignHtlcTx is reached we consider the swap
	// failed and abort early.
	err = loopInFsm.DefaultObserver.WaitForState(
		ctx, time.Minute, SignHtlcTx,
		fsm.WithAbortEarlyOnErrorOption(),
	)
	if err != nil {
		return nil, err
	}

	m.activeLoopIns[loopIn.SwapHash] = loopInFsm

	return loopIn, nil
}
