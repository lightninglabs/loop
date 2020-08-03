package loop

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcutil"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/labels"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// MaxLoopInAcceptDelta configures the maximum acceptable number of
	// remaining blocks until the on-chain htlc expires. This value is used
	// to decide whether we want to continue with the swap parameters as
	// proposed by the server. It is a protection to prevent the server from
	// getting us to lock up our funds to an arbitrary point in the future.
	MaxLoopInAcceptDelta = int32(1500)

	// MinLoopInPublishDelta defines the minimum number of remaining blocks
	// until on-chain htlc expiry required to proceed to publishing the htlc
	// tx. This value isn't critical, as we could even safely publish the
	// htlc after expiry. The reason we do implement this check is to
	// prevent us from publishing an htlc that the server surely wouldn't
	// follow up to.
	MinLoopInPublishDelta = int32(10)

	// TimeoutTxConfTarget defines the confirmation target for the loop in
	// timeout tx.
	TimeoutTxConfTarget = int32(2)
)

// loopInSwap contains all the in-memory state related to a pending loop in
// swap.
type loopInSwap struct {
	swapKit

	executeConfig

	loopdb.LoopInContract

	htlc *swap.Htlc

	htlcP2WSH *swap.Htlc

	htlcNP2WSH *swap.Htlc

	// htlcTxHash is the confirmed htlc tx id.
	htlcTxHash *chainhash.Hash

	timeoutAddr btcutil.Address

	wg sync.WaitGroup
}

// loopInInitResult contains information about a just-initiated loop in swap.
type loopInInitResult struct {
	swap          *loopInSwap
	serverMessage string
}

// newLoopInSwap initiates a new loop in swap.
func newLoopInSwap(globalCtx context.Context, cfg *swapConfig,
	currentHeight int32, request *LoopInRequest) (*loopInInitResult,
	error) {

	// Before we start, check that the label is valid.
	if err := labels.Validate(request.Label); err != nil {
		return nil, err
	}

	// Request current server loop in terms and use these to calculate the
	// swap fee that we should subtract from the swap amount in the payment
	// request that we send to the server.
	quote, err := cfg.server.GetLoopInQuote(globalCtx, request.Amount)
	if err != nil {
		return nil, fmt.Errorf("loop in terms: %v", err)
	}

	swapFee := quote.SwapFee

	if swapFee > request.MaxSwapFee {
		log.Warnf("Swap fee %v exceeding maximum of %v",
			swapFee, request.MaxSwapFee)

		return nil, ErrSwapFeeTooHigh
	}

	// Calculate the swap invoice amount. The prepay is added which
	// effectively forces the server to pay us back our prepayment on a
	// successful swap.
	swapInvoiceAmt := request.Amount - swapFee

	// Generate random preimage.
	var swapPreimage lntypes.Preimage
	if _, err := rand.Read(swapPreimage[:]); err != nil {
		log.Error("Cannot generate preimage")
	}
	swapHash := lntypes.Hash(sha256.Sum256(swapPreimage[:]))

	// Derive a sender key for this swap.
	keyDesc, err := cfg.lnd.WalletKit.DeriveNextKey(
		globalCtx, swap.KeyFamily,
	)
	if err != nil {
		return nil, err
	}
	var senderKey [33]byte
	copy(senderKey[:], keyDesc.PubKey.SerializeCompressed())

	// Create the swap invoice in lnd.
	_, swapInvoice, err := cfg.lnd.Client.AddInvoice(
		globalCtx, &invoicesrpc.AddInvoiceData{
			Preimage: &swapPreimage,
			Value:    lnwire.NewMSatFromSatoshis(swapInvoiceAmt),
			Memo:     "swap",
			Expiry:   3600 * 24 * 365,
		},
	)
	if err != nil {
		return nil, err
	}

	// Post the swap parameters to the swap server. The response contains
	// the server success key and the expiry height of the on-chain swap
	// htlc.
	log.Infof("Initiating swap request at height %v", currentHeight)
	swapResp, err := cfg.server.NewLoopInSwap(globalCtx, swapHash,
		request.Amount, senderKey, swapInvoice, request.LastHop,
	)
	if err != nil {
		return nil, fmt.Errorf("cannot initiate swap: %v", err)
	}

	// Validate the response parameters the prevent us continuing with a
	// swap that is based on parameters outside our allowed range.
	err = validateLoopInContract(cfg.lnd, currentHeight, request, swapResp)
	if err != nil {
		return nil, err
	}

	// Instantiate a struct that contains all required data to start the
	// swap.
	initiationTime := time.Now()

	contract := loopdb.LoopInContract{
		HtlcConfTarget: request.HtlcConfTarget,
		LastHop:        request.LastHop,
		ExternalHtlc:   request.ExternalHtlc,
		SwapContract: loopdb.SwapContract{
			InitiationHeight: currentHeight,
			InitiationTime:   initiationTime,
			ReceiverKey:      swapResp.receiverKey,
			SenderKey:        senderKey,
			Preimage:         swapPreimage,
			AmountRequested:  request.Amount,
			CltvExpiry:       swapResp.expiry,
			MaxMinerFee:      request.MaxMinerFee,
			MaxSwapFee:       request.MaxSwapFee,
			Label:            request.Label,
		},
	}

	swapKit := newSwapKit(
		swapHash, swap.TypeIn, cfg, &contract.SwapContract,
	)

	swapKit.lastUpdateTime = initiationTime

	swap := &loopInSwap{
		LoopInContract: contract,
		swapKit:        *swapKit,
	}

	if err := swap.initHtlcs(); err != nil {
		return nil, err
	}

	// Persist the data before exiting this function, so that the caller can
	// trust that this swap will be resumed on restart.
	err = cfg.store.CreateLoopIn(swapHash, &swap.LoopInContract)
	if err != nil {
		return nil, fmt.Errorf("cannot store swap: %v", err)
	}

	if swapResp.serverMessage != "" {
		swap.log.Infof("Server message: %v", swapResp.serverMessage)
	}

	return &loopInInitResult{
		swap:          swap,
		serverMessage: swapResp.serverMessage,
	}, nil
}

// resumeLoopInSwap returns a swap object representing a pending swap that has
// been restored from the database.
func resumeLoopInSwap(reqContext context.Context, cfg *swapConfig,
	pend *loopdb.LoopIn) (*loopInSwap, error) {

	hash := lntypes.Hash(sha256.Sum256(pend.Contract.Preimage[:]))

	log.Infof("Resuming loop in swap %v", hash)

	swapKit := newSwapKit(
		hash, swap.TypeIn, cfg, &pend.Contract.SwapContract,
	)

	swap := &loopInSwap{
		LoopInContract: *pend.Contract,
		swapKit:        *swapKit,
	}

	if err := swap.initHtlcs(); err != nil {
		return nil, err
	}

	lastUpdate := pend.LastUpdate()
	if lastUpdate == nil {
		swap.lastUpdateTime = pend.Contract.InitiationTime
	} else {
		swap.state = lastUpdate.State
		swap.lastUpdateTime = lastUpdate.Time
		swap.htlcTxHash = lastUpdate.HtlcTxHash
	}

	return swap, nil
}

// validateLoopInContract validates the contract parameters against our
// request.
func validateLoopInContract(lnd *lndclient.LndServices,
	height int32,
	request *LoopInRequest,
	response *newLoopInResponse) error {

	// Verify that we are not forced to publish an htlc that locks up our
	// funds for too long in case the server doesn't follow through.
	if response.expiry-height > MaxLoopInAcceptDelta {
		return ErrExpiryTooFar
	}

	return nil
}

// initHtlcs creates and updates the native and nested segwit htlcs
// of the loopInSwap.
func (s *loopInSwap) initHtlcs() error {
	htlcP2WSH, err := s.swapKit.getHtlc(swap.HtlcP2WSH)
	if err != nil {
		return err
	}

	htlcNP2WSH, err := s.swapKit.getHtlc(swap.HtlcNP2WSH)
	if err != nil {
		return err
	}

	// Log htlc addresses for debugging.
	s.swapKit.log.Infof("Htlc address (P2WSH): %v", htlcP2WSH.Address)
	s.swapKit.log.Infof("Htlc address (NP2WSH): %v", htlcNP2WSH.Address)

	s.htlcP2WSH = htlcP2WSH
	s.htlcNP2WSH = htlcNP2WSH

	return nil
}

// sendUpdate reports an update to the swap state.
func (s *loopInSwap) sendUpdate(ctx context.Context) error {
	info := s.swapInfo()
	s.log.Infof("Loop in swap state: %v", info.State)

	info.HtlcAddressP2WSH = s.htlcP2WSH.Address
	info.HtlcAddressNP2WSH = s.htlcNP2WSH.Address
	info.ExternalHtlc = s.ExternalHtlc

	select {
	case s.statusChan <- *info:
	case <-ctx.Done():
		return ctx.Err()
	}

	return nil
}

// execute starts/resumes the swap. It is a thin wrapper around executeSwap to
// conveniently handle the error case.
func (s *loopInSwap) execute(mainCtx context.Context,
	cfg *executeConfig, height int32) error {

	defer s.wg.Wait()

	s.executeConfig = *cfg
	s.height = height

	// Create context for our state subscription which we will cancel once
	// swap execution has completed, ensuring that we kill the subscribe
	// goroutine.
	subCtx, cancel := context.WithCancel(mainCtx)
	defer cancel()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		subscribeAndLogUpdates(
			subCtx, s.hash, s.log, s.server.SubscribeLoopInUpdates,
		)
	}()

	// Announce swap by sending out an initial update.
	err := s.sendUpdate(mainCtx)
	if err != nil {
		return err
	}

	// Execute the swap until it either reaches a final state or a temporary
	// error occurs.
	err = s.executeSwap(mainCtx)

	// Sanity check. If there is no error, the swap must be in a final
	// state.
	if err == nil && s.state.Type() == loopdb.StateTypePending {
		err = fmt.Errorf("swap in non-final state %v", s.state)
	}

	// If an unexpected error happened, report a temporary failure
	// but don't persist the error. Otherwise for example a
	// connection error could lead to abandoning the swap
	// permanently and losing funds.
	if err != nil {
		s.log.Errorf("Swap error: %v", err)
		s.setState(loopdb.StateFailTemporary)

		// If we cannot send out this update, there is nothing we can do.
		_ = s.sendUpdate(mainCtx)

		return err
	}

	s.log.Infof("Loop in swap completed: %v "+
		"(final cost: server %v, onchain %v, offchain %v)",
		s.state,
		s.cost.Server,
		s.cost.Onchain,
		s.cost.Offchain,
	)

	return nil
}

// executeSwap executes the swap.
func (s *loopInSwap) executeSwap(globalCtx context.Context) error {
	var err error

	// For loop in, the client takes the first step by publishing the
	// on-chain htlc. Only do this is we haven't already done so in a
	// previous run.
	if s.state == loopdb.StateInitiated {
		if s.ExternalHtlc {
			// If an external htlc was indicated, we can move to the
			// HtlcPublished state directly and wait for
			// confirmation.
			s.setState(loopdb.StateHtlcPublished)
			err = s.persistAndAnnounceState(globalCtx)
			if err != nil {
				return err
			}
		} else {
			published, err := s.publishOnChainHtlc(globalCtx)
			if err != nil {
				return err
			}
			if !published {
				return nil
			}
		}
	}

	// Wait for the htlc to confirm. After a restart this will pick up a
	// previously published tx.
	conf, err := s.waitForHtlcConf(globalCtx)
	if err != nil {
		return err
	}

	// Determine the htlc outpoint by inspecting the htlc tx.
	htlcOutpoint, htlcValue, err := swap.GetScriptOutput(
		conf.Tx, s.htlc.PkScript,
	)
	if err != nil {
		return err
	}

	// Verify that the confirmed (external) htlc value matches the swap
	// amount. Otherwise fail the swap immediately.
	if htlcValue != s.LoopInContract.AmountRequested {
		s.setState(loopdb.StateFailIncorrectHtlcAmt)
		return s.persistAndAnnounceState(globalCtx)
	}

	// TODO: Add miner fee of htlc tx to swap cost balance.

	// The server is expected to see the htlc on-chain and knowing that it
	// can sweep that htlc with the preimage, it should pay our swap
	// invoice, receive the preimage and sweep the htlc. We are waiting for
	// this to happen and simultaneously watch the htlc expiry height. When
	// the htlc expires, we will publish a timeout tx to reclaim the funds.
	err = s.waitForSwapComplete(globalCtx, htlcOutpoint, htlcValue)
	if err != nil {
		return err
	}

	// Persist swap outcome.
	if err := s.persistAndAnnounceState(globalCtx); err != nil {
		return err
	}

	return nil
}

// waitForHtlcConf watches the chain until the htlc confirms.
func (s *loopInSwap) waitForHtlcConf(globalCtx context.Context) (
	*chainntnfs.TxConfirmation, error) {

	// Register for confirmation of the htlc. It is essential to specify not
	// just the pk script, because an attacker may publish the same htlc
	// with a lower value and we don't want to follow through with that tx.
	// In the unlikely event that our call to SendOutputs crashes and we
	// restart, htlcTxHash will be nil at this point. Then only register
	// with PkScript and accept the risk that the call triggers on a
	// different htlc outpoint.
	s.log.Infof("Register for htlc conf (hh=%v, txid=%v)",
		s.InitiationHeight, s.htlcTxHash)

	if s.htlcTxHash == nil {
		s.log.Warnf("No htlc tx hash available, registering with " +
			"just the pkscript")
	}

	ctx, cancel := context.WithCancel(globalCtx)
	defer cancel()

	notifier := s.lnd.ChainNotifier

	confChanP2WSH, confErrP2WSH, err := notifier.RegisterConfirmationsNtfn(
		ctx, s.htlcTxHash, s.htlcP2WSH.PkScript, 1, s.InitiationHeight,
	)
	if err != nil {
		return nil, err
	}

	confChanNP2WSH, confErrNP2WSH, err := notifier.RegisterConfirmationsNtfn(
		ctx, s.htlcTxHash, s.htlcNP2WSH.PkScript, 1, s.InitiationHeight,
	)
	if err != nil {
		return nil, err
	}

	var conf *chainntnfs.TxConfirmation
	for conf == nil {
		select {

		// P2WSH htlc confirmed.
		case conf = <-confChanP2WSH:
			s.htlc = s.htlcP2WSH
			s.log.Infof("P2WSH htlc confirmed")

		// NP2WSH htlc confirmed.
		case conf = <-confChanNP2WSH:
			s.htlc = s.htlcNP2WSH
			s.log.Infof("NP2WSH htlc confirmed")

		// Conf ntfn error.
		case err := <-confErrP2WSH:
			return nil, err

		// Conf ntfn error.
		case err := <-confErrNP2WSH:
			return nil, err

		// Keep up with block height.
		case notification := <-s.blockEpochChan:
			s.height = notification.(int32)

		// Cancel.
		case <-globalCtx.Done():
			return nil, globalCtx.Err()
		}
	}

	// Store htlc tx hash for accounting purposes. Usually this call is a
	// no-op because the htlc tx hash was already known. Exceptions are:
	//
	// - Old pending swaps that were initiated before we persisted the htlc
	//   tx hash directly after publish.
	//
	// - Swaps that experienced a crash during their call to SendOutputs. In
	//   that case, we weren't able to record the tx hash.
	txHash := conf.Tx.TxHash()
	s.htlcTxHash = &txHash

	return conf, nil
}

// publishOnChainHtlc checks whether there are still enough blocks left and if
// so, it publishes the htlc and advances the swap state.
func (s *loopInSwap) publishOnChainHtlc(ctx context.Context) (bool, error) {
	var err error

	blocksRemaining := s.CltvExpiry - s.height
	s.log.Infof("Blocks left until on-chain expiry: %v", blocksRemaining)

	// Verify whether it still makes sense to publish the htlc.
	if blocksRemaining < MinLoopInPublishDelta {
		s.setState(loopdb.StateFailTimeout)
		return false, s.persistAndAnnounceState(ctx)
	}

	// Get fee estimate from lnd.
	feeRate, err := s.lnd.WalletKit.EstimateFee(
		ctx, s.LoopInContract.HtlcConfTarget,
	)
	if err != nil {
		return false, fmt.Errorf("estimate fee: %v", err)
	}

	// Transition to state HtlcPublished before calling SendOutputs to
	// prevent us from ever paying multiple times after a crash.
	s.setState(loopdb.StateHtlcPublished)
	err = s.persistAndAnnounceState(ctx)
	if err != nil {
		return false, err
	}

	s.log.Infof("Publishing on chain HTLC with fee rate %v", feeRate)

	// Internal loop-in is always P2WSH.
	tx, err := s.lnd.WalletKit.SendOutputs(ctx,
		[]*wire.TxOut{{
			PkScript: s.htlcP2WSH.PkScript,
			Value:    int64(s.LoopInContract.AmountRequested),
		}},
		feeRate,
	)
	if err != nil {
		return false, fmt.Errorf("send outputs: %v", err)
	}
	txHash := tx.TxHash()
	s.log.Infof("Published on chain HTLC tx %v", txHash)

	// Persist the htlc hash so that after a restart we are still waiting
	// for our own htlc. We don't need to announce to clients, because the
	// state remains unchanged.
	//
	// TODO(joostjager): Store tx hash before calling SendOutputs. This is
	// not yet possible with the current lnd api.
	s.htlcTxHash = &txHash
	s.lastUpdateTime = time.Now()
	if err := s.persistState(); err != nil {
		return false, fmt.Errorf("persist htlc tx: %v", err)
	}

	return true, nil

}

// waitForSwapComplete waits until a spending tx of the htlc gets confirmed and
// the swap invoice is either settled or canceled. If the htlc times out, the
// timeout tx will be published.
func (s *loopInSwap) waitForSwapComplete(ctx context.Context,
	htlcOutpoint *wire.OutPoint, htlcValue btcutil.Amount) error {

	// Register the htlc spend notification.
	rpcCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	spendChan, spendErr, err := s.lnd.ChainNotifier.RegisterSpendNtfn(
		rpcCtx, htlcOutpoint, s.htlc.PkScript, s.InitiationHeight,
	)
	if err != nil {
		return fmt.Errorf("register spend ntfn: %v", err)
	}

	// Register for swap invoice updates.
	rpcCtx, cancel = context.WithCancel(ctx)
	defer cancel()
	s.log.Infof("Subscribing to swap invoice %v", s.hash)
	swapInvoiceChan, swapInvoiceErr, err := s.lnd.Invoices.SubscribeSingleInvoice(
		rpcCtx, s.hash,
	)
	if err != nil {
		return fmt.Errorf("subscribe to swap invoice: %v", err)
	}

	// checkTimeout publishes the timeout tx if the contract has expired.
	checkTimeout := func() error {
		if s.height >= s.LoopInContract.CltvExpiry {
			return s.publishTimeoutTx(ctx, htlcOutpoint, htlcValue)
		}

		return nil
	}

	// Check timeout at current height. After a restart we may want to
	// publish the tx immediately.
	err = checkTimeout()
	if err != nil {
		return err
	}

	htlcSpend := false
	invoiceFinalized := false
	for !htlcSpend || !invoiceFinalized {
		select {
		// Spend notification error.
		case err := <-spendErr:
			return err

		// Receive block epochs and start publishing the timeout tx
		// whenever possible.
		case notification := <-s.blockEpochChan:
			s.height = notification.(int32)

			err := checkTimeout()
			if err != nil {
				return err
			}

		// The htlc spend is confirmed. Inspect the spending tx to
		// determine the final swap state.
		case spendDetails := <-spendChan:
			s.log.Infof("Htlc spend by tx: %v",
				spendDetails.SpenderTxHash)

			err := s.processHtlcSpend(
				ctx, spendDetails, htlcValue,
			)
			if err != nil {
				return err
			}

			htlcSpend = true

		// Swap invoice ntfn error.
		case err := <-swapInvoiceErr:
			return err

		// An update to the swap invoice occurred. Check the new state
		// and update the swap state accordingly.
		case update := <-swapInvoiceChan:
			s.log.Infof("Received swap invoice update: %v",
				update.State)

			switch update.State {

			// Swap invoice was paid, so update server cost balance.
			case channeldb.ContractSettled:
				s.cost.Server -= update.AmtPaid

				// If invoice settlement and htlc spend happen
				// in the expected order, move the swap to an
				// intermediate state that indicates that the
				// swap is complete from the user point of view,
				// but still incomplete with regards to
				// accounting data.
				if s.state == loopdb.StateHtlcPublished {
					s.setState(loopdb.StateInvoiceSettled)
					err := s.persistAndAnnounceState(ctx)
					if err != nil {
						return err
					}
				}

				invoiceFinalized = true

			// Canceled invoice has no effect on server cost
			// balance.
			case channeldb.ContractCanceled:
				invoiceFinalized = true
			}

		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return nil
}

func (s *loopInSwap) processHtlcSpend(ctx context.Context,
	spend *chainntnfs.SpendDetail, htlcValue btcutil.Amount) error {

	// Determine the htlc input of the spending tx and inspect the witness
	// to findout whether a success or a timeout tx spend the htlc.
	htlcInput := spend.SpendingTx.TxIn[spend.SpenderInputIndex]

	if s.htlc.IsSuccessWitness(htlcInput.Witness) {
		s.setState(loopdb.StateSuccess)

		// Server swept the htlc. The htlc value can be added to the
		// server cost balance.
		s.cost.Server += htlcValue
	} else {
		s.setState(loopdb.StateFailTimeout)

		// Now that the timeout tx confirmed, we can safely cancel the
		// swap invoice. We still need to query the final invoice state.
		// This is not a hodl invoice, so it may be that the invoice was
		// already settled. This means that the server didn't succeed in
		// sweeping the htlc after paying the invoice.
		err := s.lnd.Invoices.CancelInvoice(ctx, s.hash)
		if err != nil && err != channeldb.ErrInvoiceAlreadySettled {
			return err
		}

		// TODO: Add miner fee of timeout tx to swap cost balance.
	}

	return nil
}

// publishTimeoutTx publishes a timeout tx after the on-chain htlc has expired.
// The swap failed and we are reclaiming our funds.
func (s *loopInSwap) publishTimeoutTx(ctx context.Context,
	htlcOutpoint *wire.OutPoint, htlcValue btcutil.Amount) error {

	if s.timeoutAddr == nil {
		var err error
		s.timeoutAddr, err = s.lnd.WalletKit.NextAddr(ctx)
		if err != nil {
			return err
		}
	}

	// Calculate sweep tx fee
	fee, err := s.sweeper.GetSweepFee(
		ctx, s.htlc.AddTimeoutToEstimator, s.timeoutAddr,
		TimeoutTxConfTarget,
	)
	if err != nil {
		return err
	}

	witnessFunc := func(sig []byte) (wire.TxWitness, error) {
		return s.htlc.GenTimeoutWitness(sig)
	}

	timeoutTx, err := s.sweeper.CreateSweepTx(
		ctx, s.height, s.htlc, *htlcOutpoint, s.SenderKey, witnessFunc,
		htlcValue, fee, s.timeoutAddr,
	)
	if err != nil {
		return err
	}

	timeoutTxHash := timeoutTx.TxHash()
	s.log.Infof("Publishing timeout tx %v with fee %v to addr %v",
		timeoutTxHash, fee, s.timeoutAddr)

	err = s.lnd.WalletKit.PublishTransaction(ctx, timeoutTx)
	if err != nil {
		s.log.Warnf("publish timeout: %v", err)
	}

	return nil
}

// persistAndAnnounceState updates the swap state on disk and sends out an
// update notification.
func (s *loopInSwap) persistAndAnnounceState(ctx context.Context) error {
	// Update state in store.
	if err := s.persistState(); err != nil {
		return err
	}

	// Send out swap update
	return s.sendUpdate(ctx)
}

// persistState updates the swap state on disk.
func (s *loopInSwap) persistState() error {
	return s.store.UpdateLoopIn(
		s.hash, s.lastUpdateTime,
		loopdb.SwapStateData{
			State:      s.state,
			Cost:       s.cost,
			HtlcTxHash: s.htlcTxHash,
		},
	)
}

// setState updates the swap state and last update timestamp.
func (s *loopInSwap) setState(state loopdb.SwapState) {
	s.lastUpdateTime = time.Now()
	s.state = state
}
