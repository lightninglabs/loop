package loop

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightninglabs/loop/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/sweep"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
)

var (
	// MinLoopOutPreimageRevealDelta configures the minimum number of
	// remaining blocks before htlc expiry required to reveal preimage.
	MinLoopOutPreimageRevealDelta int32 = 20

	// DefaultSweepConfTarget is the default confirmation target we'll use
	// when sweeping on-chain HTLCs.
	DefaultSweepConfTarget int32 = 6

	// DefaultHtlcConfTarget is the default confirmation target we'll use
	// for on-chain htlcs published by the swap client for Loop In.
	DefaultHtlcConfTarget int32 = 6

	// DefaultSweepConfTargetDelta is the delta of blocks from a Loop Out
	// swap's expiration height at which we begin to use the default sweep
	// confirmation target.
	//
	// TODO(wilmer): tune?
	DefaultSweepConfTargetDelta = DefaultSweepConfTarget * 2

	// paymentTimeout is the timeout for the loop out payment loop as
	// communicated to lnd.
	paymentTimeout = time.Minute
)

const (
	// loopOutMaxShards defines that maximum number of shards that may be
	// used for a loop out swap.
	loopOutMaxShards = 5
)

// loopOutSwap contains all the in-memory state related to a pending loop out
// swap.
type loopOutSwap struct {
	swapKit

	loopdb.LoopOutContract

	swapPaymentChan chan lndclient.PaymentResult
	prePaymentChan  chan lndclient.PaymentResult
}

// executeConfig contains extra configuration to execute the swap.
type executeConfig struct {
	sweeper        *sweep.Sweeper
	statusChan     chan<- SwapInfo
	blockEpochChan <-chan interface{}
	timerFactory   func(d time.Duration) <-chan time.Time
}

// newLoopOutSwap initiates a new swap with the server and returns a
// corresponding swap object.
func newLoopOutSwap(globalCtx context.Context, cfg *swapConfig,
	currentHeight int32, request *OutRequest) (*loopOutSwap, error) {

	// Generate random preimage.
	var swapPreimage [32]byte
	if _, err := rand.Read(swapPreimage[:]); err != nil {
		log.Error("Cannot generate preimage")
	}
	swapHash := lntypes.Hash(sha256.Sum256(swapPreimage[:]))

	// Derive a receiver key for this swap.
	keyDesc, err := cfg.lnd.WalletKit.DeriveNextKey(
		globalCtx, swap.KeyFamily,
	)
	if err != nil {
		return nil, err
	}
	var receiverKey [33]byte
	copy(receiverKey[:], keyDesc.PubKey.SerializeCompressed())

	// Post the swap parameters to the swap server. The response contains
	// the server revocation key and the swap and prepay invoices.
	log.Infof("Initiating swap request at height %v", currentHeight)

	// The swap deadline will be given to the server for it to use as the
	// latest swap publication time.
	swapResp, err := cfg.server.NewLoopOutSwap(
		globalCtx, swapHash, request.Amount, receiverKey,
		request.SwapPublicationDeadline,
	)
	if err != nil {
		return nil, fmt.Errorf("cannot initiate swap: %v", err)
	}

	err = validateLoopOutContract(
		cfg.lnd, currentHeight, request, swapHash, swapResp,
	)
	if err != nil {
		return nil, err
	}

	// Instantiate a struct that contains all required data to start the
	// swap.
	initiationTime := time.Now()

	contract := loopdb.LoopOutContract{
		SwapInvoice:             swapResp.swapInvoice,
		DestAddr:                request.DestAddr,
		MaxSwapRoutingFee:       request.MaxSwapRoutingFee,
		SweepConfTarget:         request.SweepConfTarget,
		UnchargeChannel:         request.LoopOutChannel,
		PrepayInvoice:           swapResp.prepayInvoice,
		MaxPrepayRoutingFee:     request.MaxPrepayRoutingFee,
		SwapPublicationDeadline: request.SwapPublicationDeadline,
		SwapContract: loopdb.SwapContract{
			InitiationHeight: currentHeight,
			InitiationTime:   initiationTime,
			ReceiverKey:      receiverKey,
			SenderKey:        swapResp.senderKey,
			Preimage:         swapPreimage,
			AmountRequested:  request.Amount,
			CltvExpiry:       swapResp.expiry,
			MaxMinerFee:      request.MaxMinerFee,
			MaxSwapFee:       request.MaxSwapFee,
		},
	}

	swapKit, err := newSwapKit(
		swapHash, swap.TypeOut, cfg, &contract.SwapContract,
		swap.HtlcP2WSH,
	)
	if err != nil {
		return nil, err
	}

	swapKit.lastUpdateTime = initiationTime

	swap := &loopOutSwap{
		LoopOutContract: contract,
		swapKit:         *swapKit,
	}

	// Persist the data before exiting this function, so that the caller
	// can trust that this swap will be resumed on restart.
	err = cfg.store.CreateLoopOut(swapHash, &swap.LoopOutContract)
	if err != nil {
		return nil, fmt.Errorf("cannot store swap: %v", err)
	}

	return swap, nil
}

// resumeLoopOutSwap returns a swap object representing a pending swap that has
// been restored from the database.
func resumeLoopOutSwap(reqContext context.Context, cfg *swapConfig,
	pend *loopdb.LoopOut) (*loopOutSwap, error) {

	hash := lntypes.Hash(sha256.Sum256(pend.Contract.Preimage[:]))

	log.Infof("Resuming loop out swap %v", hash)

	swapKit, err := newSwapKit(
		hash, swap.TypeOut, cfg, &pend.Contract.SwapContract,
		swap.HtlcP2WSH,
	)
	if err != nil {
		return nil, err
	}

	swap := &loopOutSwap{
		LoopOutContract: *pend.Contract,
		swapKit:         *swapKit,
	}

	lastUpdate := pend.LastUpdate()
	if lastUpdate == nil {
		swap.lastUpdateTime = pend.Contract.InitiationTime
	} else {
		swap.state = lastUpdate.State
		swap.lastUpdateTime = lastUpdate.Time
	}

	return swap, nil
}

// execute starts/resumes the swap. It is a thin wrapper around
// executeAndFinalize to conveniently handle the error case.
func (s *loopOutSwap) execute(mainCtx context.Context,
	cfg *executeConfig, height int32) error {

	s.executeConfig = *cfg
	s.height = height

	// Execute swap.
	err := s.executeAndFinalize(mainCtx)

	// If an unexpected error happened, report a temporary failure.
	// Otherwise for example a connection error could lead to abandoning
	// the swap permanently and losing funds.
	if err != nil {
		s.log.Errorf("Swap error: %v", err)

		s.state = loopdb.StateFailTemporary

		// If we cannot send out this update, there is nothing we can
		// do.
		_ = s.sendUpdate(mainCtx)
	}

	return err
}

// executeAndFinalize executes a swap and awaits the definitive outcome of the
// offchain payments. When this method returns, the swap outcome is final.
func (s *loopOutSwap) executeAndFinalize(globalCtx context.Context) error {

	// Announce swap by sending out an initial update.
	err := s.sendUpdate(globalCtx)
	if err != nil {
		return err
	}

	// Execute swap. When this call returns, the swap outcome is final, but
	// it may be that there are still off-chain payments pending.
	err = s.executeSwap(globalCtx)
	if err != nil {
		return err
	}

	// Sanity check.
	if s.state.Type() == loopdb.StateTypePending {
		return fmt.Errorf("swap in non-final state %v", s.state)
	}

	// Wait until all offchain payments have completed. If payments have
	// already completed early, their channels have been set to nil.
	s.log.Infof("Wait for server pulling off-chain payment(s)")
	for s.swapPaymentChan != nil || s.prePaymentChan != nil {
		select {
		case result := <-s.swapPaymentChan:
			s.swapPaymentChan = nil
			if result.Err != nil {
				// Server didn't pull the swap payment.
				s.log.Infof("Swap payment failed: %v",
					result.Err)

				continue
			}
			s.cost.Server += result.PaidAmt
			s.cost.Offchain += result.PaidFee

		case result := <-s.prePaymentChan:
			s.prePaymentChan = nil
			if result.Err != nil {
				// Server didn't pull the prepayment.
				s.log.Infof("Prepayment failed: %v",
					result.Err)

				continue
			}
			s.cost.Server += result.PaidAmt
			s.cost.Offchain += result.PaidFee

		case <-globalCtx.Done():
			return globalCtx.Err()
		}
	}

	// Mark swap completed in store.
	s.log.Infof("Swap completed: %v "+
		"(final cost: server %v, onchain %v, offchain %v)",
		s.state,
		s.cost.Server,
		s.cost.Onchain,
		s.cost.Offchain,
	)

	return s.persistState(globalCtx)
}

// executeSwap executes the swap, but returns as soon as the swap outcome is
// final. At that point, there may still be pending off-chain payment(s).
func (s *loopOutSwap) executeSwap(globalCtx context.Context) error {
	// We always pay both invoices (again). This is currently the only way
	// to sort of resume payments.
	//
	// TODO: We shouldn't pay the invoices if it is already too late to
	// start the swap. But because we don't know if we already fired the
	// payments in a previous run, we cannot just abandon here.
	s.payInvoices(globalCtx)

	// Wait for confirmation of the on-chain htlc by watching for a tx
	// producing the swap script output.
	txConf, err := s.waitForConfirmedHtlc(globalCtx)
	if err != nil {
		return err
	}

	// If no error and no confirmation, the swap is aborted without an
	// error. The swap state has been updated to a final state.
	if txConf == nil {
		return nil
	}

	// TODO: Off-chain payments can be canceled here. Most probably the HTLC
	// is accepted by the server, but in case there are not for whatever
	// reason, we don't need to have mission control start another payment
	// attempt.

	// Retrieve outpoint for sweep.
	htlcOutpoint, htlcValue, err := swap.GetScriptOutput(
		txConf.Tx, s.htlc.PkScript,
	)
	if err != nil {
		return err
	}

	s.log.Infof("Htlc value: %v", htlcValue)

	// Verify amount if preimage hasn't been revealed yet.
	if s.state != loopdb.StatePreimageRevealed && htlcValue < s.AmountRequested {
		log.Warnf("Swap amount too low, expected %v but received %v",
			s.AmountRequested, htlcValue)

		s.state = loopdb.StateFailInsufficientValue
		return nil
	}

	// Try to spend htlc and continue (rbf) until a spend has confirmed.
	spendDetails, err := s.waitForHtlcSpendConfirmed(globalCtx,
		func() error {
			return s.sweep(globalCtx, *htlcOutpoint, htlcValue)
		},
	)
	if err != nil {
		return err
	}

	// Inspect witness stack to see if it is a success transaction. We
	// don't just try to match with the hash of our sweep tx, because it
	// may be swept by a different (fee) sweep tx from a previous run.
	htlcInput, err := swap.GetTxInputByOutpoint(
		spendDetails.SpendingTx, htlcOutpoint,
	)
	if err != nil {
		return err
	}

	sweepSuccessful := s.htlc.IsSuccessWitness(htlcInput.Witness)
	if sweepSuccessful {
		s.cost.Server -= htlcValue

		s.cost.Onchain = htlcValue -
			btcutil.Amount(spendDetails.SpendingTx.TxOut[0].Value)

		s.state = loopdb.StateSuccess
	} else {
		s.state = loopdb.StateFailSweepTimeout
	}

	return nil
}

// persistState updates the swap state and sends out an update notification.
func (s *loopOutSwap) persistState(ctx context.Context) error {
	updateTime := time.Now()

	s.lastUpdateTime = updateTime

	// Update state in store.
	err := s.store.UpdateLoopOut(
		s.hash, updateTime,
		loopdb.SwapStateData{
			State: s.state,
			Cost:  s.cost,
		},
	)
	if err != nil {
		return err
	}

	// Send out swap update
	return s.sendUpdate(ctx)
}

// payInvoices pays both swap invoices.
func (s *loopOutSwap) payInvoices(ctx context.Context) {
	// Pay the swap invoice.
	s.log.Infof("Sending swap payment %v", s.SwapInvoice)
	s.swapPaymentChan = s.payInvoice(
		ctx, s.SwapInvoice, s.MaxSwapRoutingFee,
		s.LoopOutContract.UnchargeChannel,
	)

	// Pay the prepay invoice.
	s.log.Infof("Sending prepayment %v", s.PrepayInvoice)
	s.prePaymentChan = s.payInvoice(
		ctx, s.PrepayInvoice, s.MaxPrepayRoutingFee,
		nil,
	)
}

// payInvoice pays a single invoice.
func (s *loopOutSwap) payInvoice(ctx context.Context, invoice string,
	maxFee btcutil.Amount,
	outgoingChannel *uint64) chan lndclient.PaymentResult {

	resultChan := make(chan lndclient.PaymentResult)

	go func() {
		var result lndclient.PaymentResult

		status, err := s.payInvoiceAsync(
			ctx, invoice, maxFee, outgoingChannel,
		)
		if err != nil {
			result.Err = err
		} else {
			result.Preimage = status.Preimage
			result.PaidFee = status.Fee.ToSatoshis()
			result.PaidAmt = status.Value.ToSatoshis()
		}

		select {
		case resultChan <- result:
		case <-ctx.Done():
		}
	}()

	return resultChan
}

// payInvoiceAsync is the asynchronously executed part of paying an invoice.
func (s *loopOutSwap) payInvoiceAsync(ctx context.Context,
	invoice string, maxFee btcutil.Amount, outgoingChannel *uint64) (
	*lndclient.PaymentStatus, error) {

	// Extract hash from payment request. Unfortunately the request
	// components aren't available directly.
	chainParams := s.lnd.ChainParams
	hash, _, err := swap.DecodeInvoice(chainParams, invoice)
	if err != nil {
		return nil, err
	}

	req := lndclient.SendPaymentRequest{
		MaxFee:          maxFee,
		Invoice:         invoice,
		OutgoingChannel: outgoingChannel,
		Timeout:         paymentTimeout,
		MaxParts:        loopOutMaxShards,
	}

	// Lookup state of the swap payment.
	paymentStateCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	payStatusChan, payErrChan, err := s.lnd.Router.SendPayment(
		paymentStateCtx, req,
	)
	if err != nil {
		return nil, err
	}

	for {
		select {
		// Payment advanced to the next state.
		case payState := <-payStatusChan:
			s.log.Infof("Payment %v: %v", hash, payState)

			switch payState.State {
			case lnrpc.Payment_SUCCEEDED:
				return &payState, nil

			case lnrpc.Payment_FAILED:
				return nil, errors.New("payment failed")

			case lnrpc.Payment_IN_FLIGHT:
				// Continue waiting for final state.

			default:
				return nil, errors.New("unknown payment state")
			}

		// Abort the swap in case of an error. An unknown payment error
		// from TrackPayment is no longer expected here.
		case err := <-payErrChan:
			if err != channeldb.ErrAlreadyPaid {
				return nil, err
			}

			payStatusChan, payErrChan, err =
				s.lnd.Router.TrackPayment(paymentStateCtx, hash)
			if err != nil {
				return nil, err
			}

		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// waitForConfirmedHtlc waits for a confirmed htlc to appear on the chain. In
// case we haven't revealed the preimage yet, it also monitors block height and
// off-chain payment failure.
func (s *loopOutSwap) waitForConfirmedHtlc(globalCtx context.Context) (
	*chainntnfs.TxConfirmation, error) {

	// Wait for confirmation of the on-chain htlc by watching for a tx
	// producing the swap script output.
	s.log.Infof(
		"Register conf ntfn for swap script on chain (hh=%v)",
		s.InitiationHeight,
	)

	ctx, cancel := context.WithCancel(globalCtx)
	defer cancel()
	htlcConfChan, htlcErrChan, err :=
		s.lnd.ChainNotifier.RegisterConfirmationsNtfn(
			ctx, nil, s.htlc.PkScript, 1,
			s.InitiationHeight,
		)
	if err != nil {
		return nil, err
	}

	var txConf *chainntnfs.TxConfirmation
	if s.state == loopdb.StateInitiated {
		// Check if it is already too late to start this swap. If we
		// already revealed the preimage, this check is irrelevant and
		// we need to sweep in any case.
		maxPreimageRevealHeight := s.CltvExpiry -
			MinLoopOutPreimageRevealDelta

		checkMaxRevealHeightExceeded := func() bool {
			s.log.Infof("Checking preimage reveal height %v "+
				"exceeded (height %v)",
				maxPreimageRevealHeight, s.height)

			if s.height <= maxPreimageRevealHeight {
				return false
			}

			s.log.Infof("Max preimage reveal height %v "+
				"exceeded (height %v)",
				maxPreimageRevealHeight, s.height)

			s.state = loopdb.StateFailTimeout

			return true
		}

		// First check, because after resume we may otherwise reveal the
		// preimage after the max height (depending on order in which
		// events are received in the select loop below).
		if checkMaxRevealHeightExceeded() {
			return nil, nil
		}
		s.log.Infof("Waiting for either htlc on-chain confirmation or " +
			" off-chain payment failure")
	loop:
		for {
			select {
			// If the swap payment fails, abandon the swap. We may
			// have lost the prepayment.
			case result := <-s.swapPaymentChan:
				s.swapPaymentChan = nil
				if result.Err != nil {
					s.state = loopdb.StateFailOffchainPayments
					s.log.Infof("Failed swap payment: %v",
						result.Err)

					return nil, nil
				}
				s.cost.Server += result.PaidAmt
				s.cost.Offchain += result.PaidFee

			// If the prepay fails, abandon the swap. Because we
			// didn't reveal the preimage, the swap payment will be
			// canceled or time out.
			case result := <-s.prePaymentChan:
				s.prePaymentChan = nil
				if result.Err != nil {
					s.state = loopdb.StateFailOffchainPayments
					s.log.Infof("Failed prepayment: %v",
						result.Err)

					return nil, nil
				}
				s.cost.Server += result.PaidAmt
				s.cost.Offchain += result.PaidFee

			// Unexpected error on the confirm channel happened,
			// abandon the swap.
			case err := <-htlcErrChan:
				return nil, err

			// Htlc got confirmed, continue to sweeping.
			case htlcConfNtfn := <-htlcConfChan:
				txConf = htlcConfNtfn
				break loop

			// New block is received. Recheck max reveal height.
			case notification := <-s.blockEpochChan:
				s.height = notification.(int32)

				log.Infof("Received block %v", s.height)

				if checkMaxRevealHeightExceeded() {
					return nil, nil
				}

			// Client quit.
			case <-globalCtx.Done():
				return nil, globalCtx.Err()
			}
		}

		s.log.Infof("Swap script confirmed on chain")

	} else {
		s.log.Infof("Retrieving htlc onchain")
		select {
		case err := <-htlcErrChan:
			return nil, err
		case htlcConfNtfn := <-htlcConfChan:
			txConf = htlcConfNtfn
		case <-globalCtx.Done():
			return nil, globalCtx.Err()
		}
	}

	s.log.Infof("Htlc tx %v at height %v", txConf.Tx.TxHash(),
		txConf.BlockHeight)

	return txConf, nil
}

// waitForHtlcSpendConfirmed waits for the htlc to be spent either by our own
// sweep or a server revocation tx. During this process, this function will try
// to spend the htlc every block by calling spendFunc.
//
// TODO: Improve retry/fee increase mechanism. Once in the mempool, server can
// sweep offchain. So we must make sure we sweep successfully before on-chain
// timeout.
func (s *loopOutSwap) waitForHtlcSpendConfirmed(globalCtx context.Context,
	spendFunc func() error) (*chainntnfs.SpendDetail, error) {

	// Register the htlc spend notification.
	ctx, cancel := context.WithCancel(globalCtx)
	defer cancel()
	spendChan, spendErr, err := s.lnd.ChainNotifier.RegisterSpendNtfn(
		ctx, nil, s.htlc.PkScript, s.InitiationHeight,
	)
	if err != nil {
		return nil, fmt.Errorf("register spend ntfn: %v", err)
	}

	timerChan := s.timerFactory(republishDelay)
	for {
		select {
		// Htlc spend, break loop.
		case spendDetails := <-spendChan:
			s.log.Infof("Htlc spend by tx: %v", spendDetails.SpenderTxHash)

			return spendDetails, nil

		// Spend notification error.
		case err := <-spendErr:
			return nil, err

		// New block arrived, update height and restart the republish
		// timer.
		case notification := <-s.blockEpochChan:
			s.height = notification.(int32)
			timerChan = s.timerFactory(republishDelay)

		// Some time after start or after arrival of a new block, try
		// to spend again.
		case <-timerChan:
			err := spendFunc()
			if err != nil {
				return nil, err
			}

		// Context canceled.
		case <-globalCtx.Done():
			return nil, globalCtx.Err()
		}
	}
}

// sweep tries to sweep the given htlc to a destination address. It takes into
// account the max miner fee and marks the preimage as revealed when it
// published the tx.
//
// TODO: Use lnd sweeper?
func (s *loopOutSwap) sweep(ctx context.Context,
	htlcOutpoint wire.OutPoint,
	htlcValue btcutil.Amount) error {

	witnessFunc := func(sig []byte) (wire.TxWitness, error) {
		return s.htlc.GenSuccessWitness(sig, s.Preimage)
	}

	// Calculate the transaction fee based on the confirmation target
	// required to sweep the HTLC before the timeout. We'll use the
	// confirmation target provided by the client unless we've come too
	// close to the expiration height, in which case we'll use the default
	// if it is better than what the client provided.
	confTarget := s.SweepConfTarget
	if s.CltvExpiry-s.height <= DefaultSweepConfTargetDelta &&
		confTarget > DefaultSweepConfTarget {
		confTarget = DefaultSweepConfTarget
	}
	fee, err := s.sweeper.GetSweepFee(
		ctx, s.htlc.AddSuccessToEstimator, s.DestAddr, confTarget,
	)
	if err != nil {
		return err
	}

	// Ensure it doesn't exceed our maximum fee allowed.
	if fee > s.MaxMinerFee {
		s.log.Warnf("Required fee %v exceeds max miner fee of %v",
			fee, s.MaxMinerFee)

		if s.state == loopdb.StatePreimageRevealed {
			// The currently required fee exceeds the max, but we
			// already revealed the preimage. The best we can do now
			// is to republish with the max fee.
			fee = s.MaxMinerFee
		} else {
			s.log.Warnf("Not revealing preimage")
			return nil
		}
	}

	// Create sweep tx.
	sweepTx, err := s.sweeper.CreateSweepTx(
		ctx, s.height, s.htlc, htlcOutpoint, s.ReceiverKey, witnessFunc,
		htlcValue, fee, s.DestAddr,
	)
	if err != nil {
		return err
	}

	// Before publishing the tx, already mark the preimage as revealed. This
	// is a precaution in case the publish call never returns and would
	// leave us thinking we didn't reveal yet.
	if s.state != loopdb.StatePreimageRevealed {
		s.state = loopdb.StatePreimageRevealed

		err := s.persistState(ctx)
		if err != nil {
			return err
		}
	}

	// Publish tx.
	s.log.Infof("Sweep on chain HTLC to address %v with fee %v (tx %v)",
		s.DestAddr, fee, sweepTx.TxHash())

	err = s.lnd.WalletKit.PublishTransaction(ctx, sweepTx)
	if err != nil {
		s.log.Warnf("Publish sweep: %v", err)
	}

	return nil
}

// validateLoopOutContract validates the contract parameters against our
// request.
func validateLoopOutContract(lnd *lndclient.LndServices,
	height int32, request *OutRequest, swapHash lntypes.Hash,
	response *newLoopOutResponse) error {

	// Check invoice amounts.
	chainParams := lnd.ChainParams

	swapInvoiceHash, swapInvoiceAmt, err := swap.DecodeInvoice(
		chainParams, response.swapInvoice,
	)
	if err != nil {
		return err
	}

	if swapInvoiceHash != swapHash {
		return fmt.Errorf(
			"cannot initiate swap, swap invoice hash %v not equal generated swap hash %v",
			swapInvoiceHash, swapHash)
	}

	_, prepayInvoiceAmt, err := swap.DecodeInvoice(
		chainParams, response.prepayInvoice,
	)
	if err != nil {
		return err
	}

	swapFee := swapInvoiceAmt + prepayInvoiceAmt - request.Amount
	if swapFee > request.MaxSwapFee {
		log.Warnf("Swap fee %v exceeding maximum of %v",
			swapFee, request.MaxSwapFee)

		return ErrSwapFeeTooHigh
	}

	if prepayInvoiceAmt > request.MaxPrepayAmount {
		log.Warnf("Prepay amount %v exceeding maximum of %v",
			prepayInvoiceAmt, request.MaxPrepayAmount)

		return ErrPrepayAmountTooHigh
	}

	if response.expiry-height < MinLoopOutPreimageRevealDelta {
		log.Warnf("Proposed expiry %v (delta %v) too soon",
			response.expiry, response.expiry-height)

		return ErrExpiryTooSoon
	}

	// Ensure the client has provided a sweep confirmation target that does
	// not exceed the height at which we revert back to using the default.
	if height+request.SweepConfTarget >= response.expiry-DefaultSweepConfTargetDelta {
		return ErrSweepConfTargetTooFar
	}

	return nil
}
