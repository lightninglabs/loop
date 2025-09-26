package loopin

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/labels"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/routing/route"
)

const (
	// SwapNotFinishedMsg is the message that is sent to the server if a
	// swap is not considered finished yet.
	SwapNotFinishedMsg = "swap not finished yet"
)

var (
	dustLimit = lnwallet.DustLimitForSize(input.P2TRSize)
)

// Config contains the services required for the loop-in manager.
type Config struct {
	// Server is the client that is used to communicate with the static
	// address server.
	Server swapserverrpc.StaticAddressServerClient

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

	// NotificationManager is the manager that handles the notification
	// subscriptions.
	NotificationManager NotificationManager

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
func NewManager(cfg *Config, currentHeight uint32) *Manager {
	m := &Manager{
		cfg:           cfg,
		newLoopInChan: make(chan *newSwapRequest),
		exitChan:      make(chan struct{}),
		errChan:       make(chan error),
		activeLoopIns: make(map[lntypes.Hash]*FSM),
	}
	m.currentHeight.Store(currentHeight)

	return m
}

// Run runs the static address loop-in manager.
func (m *Manager) Run(ctx context.Context, initChan chan struct{}) error {
	registerBlockNtfn := m.cfg.ChainNotifier.RegisterBlockEpochNtfn
	newBlockChan, newBlockErrChan, err := registerBlockNtfn(ctx)
	if err != nil {
		log.Errorf("unable to register for block notifications: %v",
			err)

		return err
	}

	// Upon start of the loop-in manager we reinstate all previous loop-ins
	// that are not yet completed.
	err = m.recoverLoopIns(ctx)
	if err != nil {
		log.Errorf("unable to recover loop-ins: %v", err)

		return err
	}

	// Register for notifications of loop-in sweep requests.
	sweepReqs := m.cfg.NotificationManager.
		SubscribeStaticLoopInSweepRequests(ctx)

	// Communicate to the caller that the address manager has completed its
	// initialization.
	close(initChan)

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
				// Notify subroutines that the main loop has
				// been canceled.
				close(m.exitChan)

				return ctx.Err()
			}

		case sweepReq, ok := <-sweepReqs:
			if !ok {
				// The channel has been closed, we'll stop the
				// loop-in manager.
				log.Debugf("Stopping loop-in manager " +
					"(ntfnChan closed)")

				close(m.exitChan)

				return fmt.Errorf("ntfnChan closed")
			}

			err = m.handleLoopInSweepReq(ctx, sweepReq)
			if err != nil {
				log.Errorf("Error handling loop-in sweep "+
					"request: %v", err)
			}

		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// notifyNotFinished notifies the server that a swap is not finished by
// sending the defined error message.
func (m *Manager) notifyNotFinished(ctx context.Context, swapHash lntypes.Hash,
	txId chainhash.Hash) error {

	_, err := m.cfg.Server.PushStaticAddressSweeplessSigs(
		ctx, &swapserverrpc.PushStaticAddressSweeplessSigsRequest{
			SwapHash:     swapHash[:],
			Txid:         txId[:],
			ErrorMessage: SwapNotFinishedMsg,
		})

	return err
}

// handleLoopInSweepReq handles a loop-in sweep request from the server.
// It first checks if the requested loop-in is finished as expected and if
// yes will send signature to the server for the provided psbt.
func (m *Manager) handleLoopInSweepReq(ctx context.Context,
	req *swapserverrpc.ServerStaticLoopInSweepNotification) error {

	// First we'll check if the loop-ins are known to us and in
	// the expected state.
	swapHash, err := lntypes.MakeHash(req.SwapHash)
	if err != nil {
		return err
	}

	// Fetch the loop-in from the store.
	loopIn, err := m.cfg.Store.GetLoopInByHash(ctx, swapHash)
	if err != nil {
		return err
	}

	loopIn.AddressParams, err =
		m.cfg.AddressManager.GetStaticAddressParameters(ctx)

	if err != nil {
		return err
	}

	loopIn.Address, err = m.cfg.AddressManager.GetStaticAddress(ctx)
	if err != nil {
		return err
	}

	ignoreUnknownOutpoints := false
	deposits, err := m.cfg.DepositManager.DepositsForOutpoints(
		ctx, loopIn.DepositOutpoints, ignoreUnknownOutpoints,
	)
	if err != nil {
		return err
	}
	loopIn.Deposits = deposits

	reader := bytes.NewReader(req.SweepTxPsbt)
	sweepPacket, err := psbt.NewFromRawBytes(reader, false)
	if err != nil {
		return err
	}

	sweepTx := sweepPacket.UnsignedTx

	// If the loop-in is not in the Succeeded state we return an
	// error.
	if !loopIn.IsInState(Succeeded) {
		// We'll notify the server that we don't consider the swap
		// finished yet, so it can retry later.
		_ = m.notifyNotFinished(ctx, swapHash, sweepTx.TxHash())
		return fmt.Errorf("loop-in %v not in Succeeded state",
			swapHash)
	}

	// Perform a sanity check on the number of unsigned tx inputs and
	// prevout info.
	if len(sweepTx.TxIn) != len(req.PrevoutInfo) {
		return fmt.Errorf("expected %v inputs, got %v",
			len(req.PrevoutInfo), len(sweepTx.TxIn))
	}

	// If the user selected an amount that is less than the total deposit
	// amount we'll check that the server sends us the correct change amount
	// back to our static address.
	err = m.checkChange(ctx, sweepTx, loopIn.AddressParams)
	if err != nil {
		return err
	}

	// Check if all the deposits requested are part of the loop-in and
	// find them in the requested sweep.
	depositToIdxMap, err := mapDepositsToIndices(req, loopIn, sweepTx)
	if err != nil {
		return err
	}

	prevoutMap := make(map[wire.OutPoint]*wire.TxOut, len(req.PrevoutInfo))

	// Set all the prevouts in the prevout map.
	for _, prevout := range req.PrevoutInfo {
		txid, err := chainhash.NewHash(prevout.TxidBytes)
		if err != nil {
			return err
		}

		prevoutMap[wire.OutPoint{
			Hash:  *txid,
			Index: prevout.OutputIndex,
		}] = &wire.TxOut{
			Value:    int64(prevout.Value),
			PkScript: prevout.PkScript,
		}
	}

	prevOutputFetcher := txscript.NewMultiPrevOutFetcher(
		prevoutMap,
	)

	sigHashes := txscript.NewTxSigHashes(
		sweepPacket.UnsignedTx, prevOutputFetcher,
	)

	// We'll now sign for every deposit that is part of the loop-in.
	responseMap := make(
		map[string]*swapserverrpc.ClientSweeplessSigningInfo,
		len(req.DepositToNonces),
	)

	for depositOutpoint, nonce := range req.DepositToNonces {
		taprootSigHash, err := txscript.CalcTaprootSignatureHash(
			sigHashes, txscript.SigHashDefault,
			sweepPacket.UnsignedTx,
			depositToIdxMap[depositOutpoint], prevOutputFetcher,
		)
		if err != nil {
			return err
		}

		var (
			serverNonce [musig2.PubNonceSize]byte
			sigHash     [32]byte
		)

		copy(serverNonce[:], nonce)
		musig2Session, err := loopIn.createMusig2Session(
			ctx, m.cfg.Signer,
		)
		if err != nil {
			return err
		}
		// We'll clean up the session if we don't get to signing.
		defer func() {
			err = m.cfg.Signer.MuSig2Cleanup(
				context.WithoutCancel(ctx),
				musig2Session.SessionID,
			)
			if err != nil {
				log.Errorf("Error cleaning up musig2 session: "+
					" %v", err)
			}
		}()

		haveAllNonces, err := m.cfg.Signer.MuSig2RegisterNonces(
			ctx, musig2Session.SessionID,
			[][musig2.PubNonceSize]byte{serverNonce},
		)
		if err != nil {
			return err
		}

		if !haveAllNonces {
			return fmt.Errorf("expected all nonces to be " +
				"registered")
		}

		copy(sigHash[:], taprootSigHash)

		// Since our MuSig2 session has all nonces, we can now create
		// the local partial signature by signing the sig hash.
		sig, err := m.cfg.Signer.MuSig2Sign(
			ctx, musig2Session.SessionID, sigHash, false,
		)
		if err != nil {
			return err
		}

		signingInfo := &swapserverrpc.ClientSweeplessSigningInfo{
			Nonce: musig2Session.PublicNonce[:],
			Sig:   sig,
		}
		responseMap[depositOutpoint] = signingInfo
	}

	txHash := sweepTx.TxHash()

	_, err = m.cfg.Server.PushStaticAddressSweeplessSigs(
		ctx, &swapserverrpc.PushStaticAddressSweeplessSigsRequest{
			SwapHash:    loopIn.SwapHash[:],
			Txid:        txHash[:],
			SigningInfo: responseMap,
		},
	)
	return err
}

// checkChange ensures that the server sends us the correct change amount
// back to our static address. An edge case arises if a batch contains two
// swaps with identical change outputs. The client needs to ensure that any
// swap referenced by the inputs has a respective change output in the batch.
func (m *Manager) checkChange(ctx context.Context,
	sweepTx *wire.MsgTx, changeAddr *address.Parameters) error {

	prevOuts := make([]string, len(sweepTx.TxIn))
	for i, in := range sweepTx.TxIn {
		prevOuts[i] = in.PreviousOutPoint.String()
	}

	ignoreUnknownOutpoints := true
	deposits, err := m.cfg.DepositManager.DepositsForOutpoints(
		ctx, prevOuts, ignoreUnknownOutpoints,
	)
	if err != nil {
		return err
	}

	depositIDs := make([]deposit.ID, len(deposits))
	for i, d := range deposits {
		depositIDs[i] = d.ID
	}

	swapHashes, err := m.cfg.Store.SwapHashesForDepositIDs(ctx, depositIDs)
	if err != nil {
		return err
	}

	var expectedChange btcutil.Amount
	for swapHash := range swapHashes {
		loopIn, err := m.cfg.Store.GetLoopInByHash(ctx, swapHash)
		if err != nil {
			return err
		}

		totalDepositAmount := loopIn.TotalDepositAmount()
		changeAmt := totalDepositAmount - loopIn.SelectedAmount
		if changeAmt > 0 && changeAmt < totalDepositAmount {
			log.Debugf("expected change output to our "+
				"static address, total_deposit_amount=%v, "+
				"selected_amount=%v, "+
				"expected_change_amount=%v ",
				totalDepositAmount, loopIn.SelectedAmount,
				changeAmt)

			expectedChange += changeAmt
		}
	}

	if expectedChange == 0 {
		return nil
	}

	for _, out := range sweepTx.TxOut {
		if out.Value == int64(expectedChange) &&
			bytes.Equal(out.PkScript, changeAddr.PkScript) {

			// We found the expected change output.
			return nil
		}
	}

	return fmt.Errorf("couldn't find expected change of %v "+
		"satoshis sent to our static address", expectedChange)
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
		go func(fsm *FSM, swapHash lntypes.Hash) {
			err := fsm.SendEvent(ctx, OnRecover, nil)
			if err != nil {
				log.Errorf("Error sending OnStart event: %v",
					err)
			}

			m.activeLoopIns[swapHash] = fsm
		}(fsm, loopIn.SwapHash)
	}

	return nil
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

	var (
		err               error
		selectedOutpoints = req.DepositOutpoints
		selectedDeposits  []*deposit.Deposit
	)

	// Determine which deposits to use for the loop-in swap. If none are
	// selected by the client, we will coin-select them based on the amount.
	switch {
	case len(selectedOutpoints) == 0 && req.SelectedAmount == 0:
		return nil, fmt.Errorf("neither deposit outpoints nor amount " +
			"provided")

	case len(selectedOutpoints) > 0:
		// Retrieve all deposits referenced by the outpoints and ensure
		// that they are in state Deposited.
		var active bool
		selectedDeposits, active = m.cfg.DepositManager.
			AllStringOutpointsActiveDeposits(
				selectedOutpoints, deposit.Deposited,
			)
		if !active {
			return nil, fmt.Errorf("one or more deposits are not in "+
				"state %s", deposit.Deposited)
		}

	case len(selectedOutpoints) == 0:
		// If an amount was provided, we'll coin-select deposits to
		// cover for the amount.
		allDeposits, err := m.cfg.DepositManager.
			GetActiveDepositsInState(deposit.Deposited)
		if err != nil {
			return nil, fmt.Errorf("unable to retrieve all "+
				"deposits: %w", err)
		}

		// TODO(hieblmi): add params to deposit for multi-address
		//      support.
		params, err := m.cfg.AddressManager.GetStaticAddressParameters(
			ctx,
		)
		if err != nil {
			return nil, fmt.Errorf("unable to retrieve static "+
				"address parameters: %w", err)
		}

		selectedDeposits, err = SelectDeposits(
			req.SelectedAmount, allDeposits, params.Expiry,
			m.currentHeight.Load(),
		)
		if err != nil {
			return nil, fmt.Errorf("unable to select deposits: %w",
				err)
		}

		selectedOutpoints = make([]string, 0, len(selectedDeposits))
		for _, deposit := range selectedDeposits {
			selectedOutpoints = append(selectedOutpoints,
				deposit.String())
		}
	}

	// Calculate the total deposit amount and check if the selected amount
	// would leave a dust output.
	swapAmount, err := DeduceSwapAmount(
		sumOfDeposits(selectedDeposits), req.SelectedAmount,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to determine swap amount: %w",
			err)
	}

	// Check that the label is valid.
	err = labels.Validate(req.Label)
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
			ctx, m.cfg.LndClient, swapAmount,
			loop.DefaultMaxHopHints, includeNodes,
		)
		if err != nil {
			return nil, fmt.Errorf("unable to generate hop "+
				"hints: %w", err)
		}
	}

	// Request the current server loop in terms and use these to calculate
	// the swap fee that we should subtract from the swap amount in the
	// payment request that we send to the server. We pass nil as optional
	// route hints as hop hint selection when generating invoices with
	// private channels is an LND side black box feature. Advanced users
	// will quote directly anyway, and there they are able to add specific
	// route hints.
	// The quote call will also request a probe from the server to ensure
	// feasibility of a loop-in for the selected.
	numDeposits := uint32(len(selectedDeposits))
	quote, err := m.cfg.QuoteGetter.GetLoopInQuote(
		ctx, swapAmount, m.cfg.NodePubkey, req.LastHop, req.RouteHints,
		req.Initiator, numDeposits,
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
		SelectedAmount:        req.SelectedAmount,
		DepositOutpoints:      selectedOutpoints,
		Deposits:              selectedDeposits,
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

// GetAllSwaps returns all static address loop-in swaps from the database store.
func (m *Manager) GetAllSwaps(ctx context.Context) ([]*StaticAddressLoopIn,
	error) {

	swaps, err := m.cfg.Store.GetStaticAddressLoopInSwapsByStates(
		ctx, AllStates,
	)
	if err != nil {
		return nil, err
	}

	allDeposits, err := m.cfg.DepositManager.GetAllDeposits(ctx)
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

// SelectDeposits sorts the deposits by amount in descending order, then by
// blocks-until-expiry in ascending order. It then selects the deposits that
// are needed to cover the amount requested without leaving a dust change. It
// returns an error if the sum of deposits minus dust is less than the requested
// amount.
func SelectDeposits(targetAmount btcutil.Amount, deposits []*deposit.Deposit,
	csvExpiry uint32, blockHeight uint32) ([]*deposit.Deposit, error) {

	// Sort the deposits by amount in descending order, then by
	// blocks-until-expiry in ascending order.
	sort.Slice(deposits, func(i, j int) bool {
		if deposits[i].Value == deposits[j].Value {
			iExp := uint32(deposits[i].ConfirmationHeight) +
				csvExpiry - blockHeight
			jExp := uint32(deposits[j].ConfirmationHeight) +
				csvExpiry - blockHeight

			return iExp < jExp
		}
		return deposits[i].Value > deposits[j].Value
	})

	// Select the deposits that are needed to cover the swap amount without
	// leaving a dust change.
	var selectedDeposits []*deposit.Deposit
	var selectedAmount btcutil.Amount
	for _, deposit := range deposits {
		selectedDeposits = append(selectedDeposits, deposit)
		selectedAmount += deposit.Value
		if selectedAmount == targetAmount {
			return selectedDeposits, nil
		}
		if selectedAmount > targetAmount {
			if selectedAmount-targetAmount >= dustLimit {
				return selectedDeposits, nil
			}
		}
	}

	return nil, fmt.Errorf("not enough deposits to cover "+
		"requested amount or prevent dust change, have %d but need %d",
		selectedAmount, targetAmount)
}

// DeduceSwapAmount calculates the swap amount based on the selected amount and
// the total deposit amount. It checks if the selected amount leaves a dust
// change output or exceeds the total deposits value. Note that if the selected
// amount is 0, the swap amount is the total deposit value. If the selected
// amount is equal to the total deposit value, the total deposit value will be
// swapped.
func DeduceSwapAmount(totalDepositAmount btcutil.Amount,
	selectedAmount btcutil.Amount) (btcutil.Amount, error) {

	// If the selected amount leaves a dust change output or exceeds the
	// total deposits value, we return an error.
	swapAmount := selectedAmount
	remainingAmount := totalDepositAmount - selectedAmount
	switch {
	case selectedAmount < 0:
		return 0, fmt.Errorf("selected amount %v is negative",
			selectedAmount)

	case selectedAmount > 0 && selectedAmount < dustLimit:
		return 0, fmt.Errorf("selected amount %v is dust, "+
			"need at least %v", selectedAmount, dustLimit)

	case totalDepositAmount < dustLimit:
		return 0, fmt.Errorf("total deposit value %v is dust, "+
			"need at least %v", totalDepositAmount, dustLimit)

	case remainingAmount < 0:
		return 0, fmt.Errorf("selected amount %v exceeds total "+
			"deposit value %v", selectedAmount, totalDepositAmount)

	case remainingAmount > 0 && remainingAmount < dustLimit:
		return 0, fmt.Errorf("selected amount %v leaves dust change "+
			"%v", selectedAmount, remainingAmount)

	default:
		// If the remaining amount is 0 or equal or greater than the
		// dust limit, we can proceed with the swap.
	}

	// If the client didn't select an amount, we quote for the total
	// deposits value.
	if selectedAmount == 0 {
		swapAmount = totalDepositAmount
	}

	return swapAmount, nil
}

// mapDepositsToIndices maps the deposit outpoints to their respective indices
// in the sweep transaction.
func mapDepositsToIndices(
	req *swapserverrpc.ServerStaticLoopInSweepNotification,
	loopIn *StaticAddressLoopIn, sweepTx *wire.MsgTx) (map[string]int,
	error) {

	depositToIdxMap := make(map[string]int)
	for reqOutpoint := range req.DepositToNonces {
		hasDeposit := false
		for _, depositOutpoint := range loopIn.DepositOutpoints {
			if depositOutpoint == reqOutpoint {
				hasDeposit = true
				break
			}
		}
		if !hasDeposit {
			return nil, fmt.Errorf("deposit outpoint not part of " +
				"loop-in")
		}

		foundDepositInTx := false

		for i, txIn := range sweepTx.TxIn {
			if txIn.PreviousOutPoint.String() == reqOutpoint {
				// Check that the deposit does not exist in the
				// map yet.
				if _, ok := depositToIdxMap[reqOutpoint]; ok {
					return nil, fmt.Errorf("deposit "+
						"outpoint %v already part of "+
						"sweep tx", reqOutpoint)
				}

				depositToIdxMap[reqOutpoint] = i
				foundDepositInTx = true
				break
			}
		}

		if !foundDepositInTx {
			return nil, fmt.Errorf("deposit outpoint %v not part "+
				"of sweep tx", reqOutpoint)
		}
	}
	return depositToIdxMap, nil
}

func sumOfDeposits(deposits []*deposit.Deposit) btcutil.Amount {
	sum := btcutil.Amount(0)
	for _, d := range deposits {
		sum += d.Value
	}

	return sum
}
