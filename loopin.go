package loop

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/lightninglabs/loop/swap"

	"github.com/btcsuite/btcutil"

	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"

	"github.com/lightninglabs/loop/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightningnetwork/lnd/lntypes"
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
)

// loopInSwap contains all the in-memory state related to a pending loop in
// swap.
type loopInSwap struct {
	swapKit

	loopdb.LoopInContract

	prePaymentChan chan lndclient.PaymentResult

	timeoutAddr btcutil.Address
}

// newLoopInSwap initiates a new loop in swap.
func newLoopInSwap(globalCtx context.Context, cfg *swapConfig,
	currentHeight int32, request *LoopInRequest) (*loopInSwap, error) {

	// Request current server loop in terms and use these to calculate the
	// swap fee that we should subtract from the swap amount in the payment
	// request that we send to the server.
	quote, err := cfg.server.GetLoopInTerms(globalCtx)
	if err != nil {
		return nil, fmt.Errorf("loop in terms: %v", err)
	}

	swapFee := swap.CalcFee(
		request.Amount, quote.SwapFeeBase, quote.SwapFeeRate,
	)

	if swapFee > request.MaxSwapFee {
		logger.Warnf("Swap fee %v exceeding maximum of %v",
			swapFee, request.MaxSwapFee)

		return nil, ErrSwapFeeTooHigh
	}

	// Calculate the swap invoice amount. The prepay is added which
	// effectively forces the server to pay us back our prepayment on a
	// successful swap.
	swapInvoiceAmt := request.Amount - swapFee + quote.PrepayAmt

	// Generate random preimage.
	var swapPreimage lntypes.Preimage
	if _, err := rand.Read(swapPreimage[:]); err != nil {
		logger.Error("Cannot generate preimage")
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
			Value:    swapInvoiceAmt,
			Memo:     "swap",
			Expiry:   3600 * 24 * 365,
		},
	)
	if err != nil {
		return nil, err
	}

	// Post the swap parameters to the swap server. The response contains
	// the server revocation key, the prepay invoice and the expiry height
	// of the on-chain swap htlc.
	logger.Infof("Initiating swap request at height %v", currentHeight)
	swapResp, err := cfg.server.NewLoopInSwap(globalCtx, swapHash,
		request.Amount, senderKey, swapInvoice,
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
		LoopInChannel:  request.LoopInChannel,
		SwapContract: loopdb.SwapContract{
			InitiationHeight:    currentHeight,
			InitiationTime:      initiationTime,
			PrepayInvoice:       swapResp.prepayInvoice,
			ReceiverKey:         swapResp.receiverKey,
			SenderKey:           senderKey,
			Preimage:            swapPreimage,
			AmountRequested:     request.Amount,
			CltvExpiry:          swapResp.expiry,
			MaxMinerFee:         request.MaxMinerFee,
			MaxSwapFee:          request.MaxSwapFee,
			MaxPrepayRoutingFee: request.MaxPrepayRoutingFee,
		},
	}

	swapKit, err := newSwapKit(
		swapHash, TypeIn, cfg, &contract.SwapContract,
	)
	if err != nil {
		return nil, err
	}

	swapKit.lastUpdateTime = initiationTime

	swap := &loopInSwap{
		LoopInContract: contract,
		swapKit:        *swapKit,
	}

	// Persist the data before exiting this function, so that the caller can
	// trust that this swap will be resumed on restart.
	err = cfg.store.CreateLoopIn(swapHash, &swap.LoopInContract)
	if err != nil {
		return nil, fmt.Errorf("cannot store swap: %v", err)
	}

	return swap, nil
}

// resumeLoopInSwap returns a swap object representing a pending swap that has
// been restored from the database.
func resumeLoopInSwap(reqContext context.Context, cfg *swapConfig,
	pend *loopdb.LoopIn) (*loopInSwap, error) {

	hash := lntypes.Hash(sha256.Sum256(pend.Contract.Preimage[:]))

	logger.Infof("Resuming loop in swap %v", hash)

	swapKit, err := newSwapKit(
		hash, TypeIn, cfg, &pend.Contract.SwapContract,
	)
	if err != nil {
		return nil, err
	}

	swap := &loopInSwap{
		LoopInContract: *pend.Contract,
		swapKit:        *swapKit,
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

// validateLoopInContract validates the contract parameters against our
// request.
func validateLoopInContract(lnd *lndclient.LndServices,
	height int32,
	request *LoopInRequest,
	response *newLoopInResponse) error {

	// Check prepay amount.
	chainParams := lnd.ChainParams

	prepayInvoiceAmt, err := swap.GetInvoiceAmt(
		chainParams, response.prepayInvoice,
	)
	if err != nil {
		return err
	}

	if prepayInvoiceAmt > request.MaxPrepayAmount {
		logger.Warnf("Prepay amount %v exceeding maximum of %v",
			prepayInvoiceAmt, request.MaxPrepayAmount)

		return ErrPrepayAmountTooHigh
	}

	// Verify that we are not forced to publish an htlc that locks up our
	// funds for too long in case the server doesn't follow through.
	if response.expiry-height > MaxLoopInAcceptDelta {
		return ErrExpiryTooFar
	}

	return nil
}

// execute starts/resumes the swap. It is a thin wrapper around executeSwap to
// conveniently handle the error case.
func (s *loopInSwap) execute(mainCtx context.Context,
	cfg *executeConfig, height int32) error {

	s.executeConfig = *cfg
	s.height = height

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
		s.state = loopdb.StateFailTemporary

		// If we cannot send out this update, there is nothing we can do.
		_ = s.sendUpdate(mainCtx)

		return err
	}

	s.log.Infof("Loop in swap completed: %v "+
		"(final cost: server %v, onchain %v)",
		s.state,
		s.cost.Server,
		s.cost.Onchain,
	)

	return nil
}

// waitPrepayPaid waits for the prepay payment to complete and adds the amount
// to the balance of payments to the server.
func (s *loopInSwap) waitPrepayPaid(ctx context.Context) error {
	s.log.Infof("Wait for prepay outcome")

	select {
	case result := <-s.prePaymentChan:
		if result.Err != nil {
			// Server didn't take the prepayment.
			s.log.Infof("Prepayment failed: %v",
				result.Err)
		} else {
			s.cost.Server += result.PaidAmt
		}
		return nil

	case <-ctx.Done():
		return ctx.Err()
	}
}

// executeSwap executes the swap.
func (s *loopInSwap) executeSwap(globalCtx context.Context) error {
	var err error

	// For loop in, the client takes the first step by publishing the
	// on-chain htlc. Only do this is we haven't already done so in a
	// previous run.
	if s.state == loopdb.StateInitiated {
		published, err := s.publishOnChainHtlc(globalCtx)
		if err != nil {
			return err
		}
		if !published {
			return nil
		}
	}

	// Wait for the htlc to confirm. After a restart this will pick up a
	// previously published tx.
	conf, err := s.waitForHtlcConf(globalCtx)
	if err != nil {
		return err
	}

	// Determine the htlc outpoint by inspecting the htlc tx.
	htlcOutpoint, _, err := swap.GetScriptOutput(
		conf.Tx, s.htlc.ScriptHash,
	)
	if err != nil {
		return err
	}

	// Now that the htlc is on the chain, the server needs to take action.
	// It requires us to pay the prepay before it will continue.
	if err := s.payPrepay(globalCtx); err != nil {
		return err
	}

	// The server is expected to see the htlc on-chain and knowing that it
	// can sweep that htlc with the preimage, it should pay our swap
	// invoice, receive the preimage and sweep the htlc. We are waiting for
	// this to happen and simultaneously watch the htlc expiry height. When
	// the htlc expires, we will publish a timeout tx to reclaim the funds.
	spend, err := s.waitForHtlcSpend(globalCtx, htlcOutpoint)
	if err != nil {
		return err
	}

	// Determine the htlc input of the spending tx and inspect the witness
	// to findout whether a success or a timeout tx spend the htlc.
	htlcInput := spend.SpendingTx.TxIn[spend.SpenderInputIndex]

	if s.htlc.IsSuccessWitness(htlcInput.Witness) {
		// The server swept the htlc. Swap invoice should have been
		// paid. We are waiting for this event to be sure of this.
		err := s.waitForSwapPaid(globalCtx)
		if err != nil {
			return err
		}

		s.state = loopdb.StateSuccess
	} else {
		s.state = loopdb.StateFailTimeout
	}

	// Wait for outcome of prepay payment (for accounting).
	if err := s.waitPrepayPaid(globalCtx); err != nil {
		return err
	}

	// Persist swap outcome.
	if err := s.persistState(globalCtx); err != nil {
		return err
	}

	return nil
}

// waitForHtlcConf watches the chain until the htlc confirms.
func (s *loopInSwap) waitForHtlcConf(globalCtx context.Context) (
	*chainntnfs.TxConfirmation, error) {

	ctx, cancel := context.WithCancel(globalCtx)
	defer cancel()
	confChan, confErr, err := s.lnd.ChainNotifier.RegisterConfirmationsNtfn(
		ctx, nil, s.htlc.ScriptHash, 1, s.InitiationHeight,
	)
	if err != nil {
		return nil, err
	}
	for {
		select {

		// Htlc confirmed.
		case conf := <-confChan:
			return conf, nil

		// Conf ntfn error.
		case err := <-confErr:
			return nil, err

		// Keep up with block height.
		case notification := <-s.blockEpochChan:
			s.height = notification.(int32)

		// Cancel.
		case <-globalCtx.Done():
			return nil, globalCtx.Err()
		}
	}
}

// publishOnChainHtlc checks whether there are still enough blocks left and if
// so, it publishes the htlc and advances the swap state.
func (s *loopInSwap) publishOnChainHtlc(ctx context.Context) (bool, error) {
	var err error

	blocksRemaining := s.CltvExpiry - s.height
	s.log.Infof("Blocks left until on-chain expiry: %v", blocksRemaining)

	// Verify whether it still makes sense to publish the htlc.
	if blocksRemaining < MinLoopInPublishDelta {
		s.state = loopdb.StateFailTimeout
		return false, s.persistState(ctx)
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
	s.state = loopdb.StateHtlcPublished
	err = s.persistState(ctx)
	if err != nil {
		return false, err
	}

	s.log.Infof("Publishing on chain HTLC with fee rate %v", feeRate)
	tx, err := s.lnd.WalletKit.SendOutputs(ctx,
		[]*wire.TxOut{{
			PkScript: s.htlc.ScriptHash,
			Value:    int64(s.LoopInContract.AmountRequested),
		}},
		feeRate,
	)
	if err != nil {
		return false, fmt.Errorf("send outputs: %v", err)
	}
	s.log.Infof("Published on chain HTLC tx %v", tx.TxHash())

	return true, nil

}

// payPrepay pays the prepay invoice obtained from the server. It stores the
// result channel for later use.
func (s *loopInSwap) payPrepay(ctx context.Context) error {
	// Pay the prepay invoice.
	s.log.Infof("Sending prepayment %v", s.PrepayInvoice)
	s.prePaymentChan = s.lnd.Client.PayInvoice(
		ctx, s.PrepayInvoice, s.MaxPrepayRoutingFee,
		nil,
	)

	return nil
}

// waitForHtlcSpend waits until a spending tx of the htlc gets confirmed and
// returns the spend details.
func (s *loopInSwap) waitForHtlcSpend(ctx context.Context,
	htlc *wire.OutPoint) (*chainntnfs.SpendDetail, error) {

	// Register the htlc spend notification.
	rpcCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	spendChan, spendErr, err := s.lnd.ChainNotifier.RegisterSpendNtfn(
		rpcCtx, nil, s.htlc.ScriptHash, s.InitiationHeight,
	)
	if err != nil {
		return nil, fmt.Errorf("register spend ntfn: %v", err)
	}

	for {
		select {
		// Spend notification error.
		case err := <-spendErr:
			return nil, err

		case notification := <-s.blockEpochChan:
			s.height = notification.(int32)

			if s.height >= s.LoopInContract.CltvExpiry {
				err := s.publishTimeoutTx(ctx, htlc)
				if err != nil {
					return nil, err
				}
			}

		// Htlc spend, break loop.
		case spendDetails := <-spendChan:
			s.log.Infof("Htlc spend by tx: %v",
				spendDetails.SpenderTxHash)

			return spendDetails, nil

		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// waitForSwapPaid waits until our swap invoice gets paid by the server.
func (s *loopInSwap) waitForSwapPaid(ctx context.Context) error {
	// Wait for swap invoice to be paid.
	rpcCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.log.Infof("Subscribing to swap invoice %v", s.hash)
	swapInvoiceChan, swapInvoiceErr, err := s.lnd.Invoices.SubscribeSingleInvoice(
		rpcCtx, s.hash,
	)
	if err != nil {
		return err
	}

	for {
		select {

		// Swap invoice ntfn error.
		case err := <-swapInvoiceErr:
			return err

		case state := <-swapInvoiceChan:
			s.log.Infof("Received swap invoice update: %v", state)
			if state != channeldb.ContractSettled {
				continue
			}

			return nil

		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// publishTimeoutTx publishes a timeout tx after the on-chain htlc has expired.
// The swap failed and we are reclaiming our funds.
func (s *loopInSwap) publishTimeoutTx(ctx context.Context,
	htlc *wire.OutPoint) error {

	if s.timeoutAddr == nil {
		var err error
		s.timeoutAddr, err = s.lnd.WalletKit.NextAddr(ctx)
		if err != nil {
			return err
		}
	}

	// Calculate sweep tx fee
	//
	// TODO: Configure timeout conf.
	fee, err := s.sweeper.GetSweepFee(
		ctx, s.htlc.MaxTimeoutWitnessSize, 2,
	)
	if err != nil {
		return err
	}

	witnessFunc := func(sig []byte) (wire.TxWitness, error) {
		return s.htlc.GenTimeoutWitness(sig)
	}

	timeoutTx, err := s.sweeper.CreateSweepTx(
		ctx, s.height, s.htlc, *htlc, s.SenderKey, witnessFunc,
		s.LoopInContract.AmountRequested, fee, s.timeoutAddr,
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

// persistState updates the swap state and sends out an update notification.
func (s *loopInSwap) persistState(ctx context.Context) error {
	updateTime := time.Now()

	s.lastUpdateTime = updateTime

	// Update state in store.
	err := s.store.UpdateLoopIn(s.hash, updateTime, s.state)
	if err != nil {
		return err
	}

	// Send out swap update
	return s.sendUpdate(ctx)
}
