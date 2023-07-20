package loop

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/labels"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/sweep"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/zpay32"
)

const (
	// loopInternalHops indicate the number of hops that a loop out swap
	// makes in the server's off-chain infrastructure. We are ok reporting
	// failure distances from the server up until this point, because every
	// swap takes these two hops, so surfacing this information does not
	// identify the client in any way. After this point, the client does not
	// report failure distances, so that sender-privacy is preserved.
	loopInternalHops = 2

	// We'll try to sweep with MuSig2 at most 10 times. If that fails we'll
	// fail back to using standard scriptspend sweep.
	maxMusigSweepRetries = 10
)

var (
	// MinLoopOutPreimageRevealDelta configures the minimum number of
	// remaining blocks before htlc expiry required to reveal preimage.
	MinLoopOutPreimageRevealDelta int32 = 20

	// DefaultSweepConfTarget is the default confirmation target we'll use
	// when sweeping on-chain HTLCs.
	DefaultSweepConfTarget int32 = 9

	// DefaultHtlcConfTarget is the default confirmation target we'll use
	// for on-chain htlcs published by the swap client for Loop In.
	DefaultHtlcConfTarget int32 = 6

	// DefaultSweepConfTargetDelta is the delta of blocks from a Loop Out
	// swap's expiration height at which we begin to use the default sweep
	// confirmation target.
	//
	// TODO(wilmer): tune?
	DefaultSweepConfTargetDelta = DefaultSweepConfTarget * 2
)

// loopOutSwap contains all the in-memory state related to a pending loop out
// swap.
type loopOutSwap struct {
	swapKit

	loopdb.LoopOutContract

	executeConfig

	htlc *swap.Htlc

	// htlcTxHash is the confirmed htlc tx id.
	htlcTxHash *chainhash.Hash

	swapInvoicePaymentAddr [32]byte

	swapPaymentChan chan paymentResult
	prePaymentChan  chan paymentResult

	wg sync.WaitGroup
}

// executeConfig contains extra configuration to execute the swap.
type executeConfig struct {
	sweeper            *sweep.Sweeper
	statusChan         chan<- SwapInfo
	blockEpochChan     <-chan interface{}
	timerFactory       func(time.Duration) <-chan time.Time
	loopOutMaxParts    uint32
	totalPaymentTimout time.Duration
	maxPaymentRetries  int
	cancelSwap         func(context.Context, *outCancelDetails) error
	verifySchnorrSig   func(pubKey *btcec.PublicKey, hash, sig []byte) error
}

// loopOutInitResult contains information about a just-initiated loop out swap.
type loopOutInitResult struct {
	swap          *loopOutSwap
	serverMessage string
}

// newLoopOutSwap initiates a new swap with the server and returns a
// corresponding swap object.
func newLoopOutSwap(globalCtx context.Context, cfg *swapConfig,
	currentHeight int32, request *OutRequest) (*loopOutInitResult, error) {

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
	log.Infof("Initiating swap request at height %v: amt=%v, expiry=%v",
		currentHeight, request.Amount, request.Expiry)

	// The swap deadline will be given to the server for it to use as the
	// latest swap publication time.
	swapResp, err := cfg.server.NewLoopOutSwap(
		globalCtx, swapHash, request.Amount, request.Expiry,
		receiverKey, request.SwapPublicationDeadline, request.Initiator,
	)
	if err != nil {
		return nil, wrapGrpcError("cannot initiate swap", err)
	}

	err = validateLoopOutContract(
		cfg.lnd, request, swapHash, swapResp,
	)
	if err != nil {
		return nil, err
	}

	// Check channel set for duplicates.
	chanSet, err := loopdb.NewChannelSet(request.OutgoingChanSet)
	if err != nil {
		return nil, err
	}

	// If a htlc confirmation target was not provided, we use the default
	// number of confirmations. We overwrite this value rather than failing
	// it because the field is a new addition to the rpc, and we don't want
	// to break older clients that are not aware of this new field.
	confs := uint32(request.HtlcConfirmations)
	if confs == 0 {
		confs = loopdb.DefaultLoopOutHtlcConfirmations
	}

	// Instantiate a struct that contains all required data to start the
	// swap.
	initiationTime := time.Now()

	contract := loopdb.LoopOutContract{
		SwapInvoice:             swapResp.swapInvoice,
		DestAddr:                request.DestAddr,
		MaxSwapRoutingFee:       request.MaxSwapRoutingFee,
		SweepConfTarget:         request.SweepConfTarget,
		HtlcConfirmations:       confs,
		PrepayInvoice:           swapResp.prepayInvoice,
		MaxPrepayRoutingFee:     request.MaxPrepayRoutingFee,
		SwapPublicationDeadline: request.SwapPublicationDeadline,
		SwapContract: loopdb.SwapContract{
			InitiationHeight: currentHeight,
			InitiationTime:   initiationTime,
			HtlcKeys: loopdb.HtlcKeys{
				SenderScriptKey:        swapResp.senderKey,
				SenderInternalPubKey:   swapResp.senderKey,
				ReceiverScriptKey:      receiverKey,
				ReceiverInternalPubKey: receiverKey,
				ClientScriptKeyLocator: keyDesc.KeyLocator,
			},
			Preimage:        swapPreimage,
			AmountRequested: request.Amount,
			CltvExpiry:      request.Expiry,
			MaxMinerFee:     request.MaxMinerFee,
			MaxSwapFee:      request.MaxSwapFee,
			Label:           request.Label,
			ProtocolVersion: loopdb.CurrentProtocolVersion(),
		},
		OutgoingChanSet: chanSet,
	}

	swapKit := newSwapKit(
		swapHash, swap.TypeOut, cfg, &contract.SwapContract,
	)

	swapKit.lastUpdateTime = initiationTime

	// Create the htlc.
	htlc, err := GetHtlc(
		swapKit.hash, swapKit.contract, swapKit.lnd.ChainParams,
	)
	if err != nil {
		return nil, err
	}

	// Log htlc address for debugging.
	swapKit.log.Infof("Htlc address (%s): %v", htlc.OutputType,
		htlc.Address)

	// Obtain the payment addr since we'll need it later for routing plugin
	// recommendation and possibly for cancel.
	paymentAddr, err := obtainSwapPaymentAddr(contract.SwapInvoice, cfg)
	if err != nil {
		return nil, err
	}

	swap := &loopOutSwap{
		LoopOutContract:        contract,
		swapKit:                *swapKit,
		htlc:                   htlc,
		swapInvoicePaymentAddr: *paymentAddr,
	}

	// Persist the data before exiting this function, so that the caller
	// can trust that this swap will be resumed on restart.
	err = cfg.store.CreateLoopOut(globalCtx, swapHash, &swap.LoopOutContract)
	if err != nil {
		return nil, fmt.Errorf("cannot store swap: %v", err)
	}

	if swapResp.serverMessage != "" {
		swap.log.Infof("Server message: %v", swapResp.serverMessage)
	}

	return &loopOutInitResult{
		swap:          swap,
		serverMessage: swapResp.serverMessage,
	}, nil
}

// resumeLoopOutSwap returns a swap object representing a pending swap that has
// been restored from the database.
func resumeLoopOutSwap(cfg *swapConfig, pend *loopdb.LoopOut,
) (*loopOutSwap, error) {

	hash := lntypes.Hash(sha256.Sum256(pend.Contract.Preimage[:]))

	log.Infof("Resuming loop out swap %v", hash)

	swapKit := newSwapKit(
		hash, swap.TypeOut, cfg, &pend.Contract.SwapContract,
	)

	// Create the htlc.
	htlc, err := GetHtlc(
		swapKit.hash, swapKit.contract, swapKit.lnd.ChainParams,
	)
	if err != nil {
		return nil, err
	}

	// Log htlc address for debugging.
	swapKit.log.Infof("Htlc address: %v", htlc.Address)

	// Obtain the payment addr since we'll need it later for routing plugin
	// recommendation and possibly for cancel.
	paymentAddr, err := obtainSwapPaymentAddr(
		pend.Contract.SwapInvoice, cfg,
	)
	if err != nil {
		return nil, err
	}

	// Create the swap.
	swap := &loopOutSwap{
		LoopOutContract:        *pend.Contract,
		swapKit:                *swapKit,
		htlc:                   htlc,
		swapInvoicePaymentAddr: *paymentAddr,
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

// obtainSwapPaymentAddr will retrieve the payment addr from the passed invoice.
func obtainSwapPaymentAddr(swapInvoice string, cfg *swapConfig) (
	*[32]byte, error) {

	swapPayReq, err := zpay32.Decode(
		swapInvoice, cfg.lnd.ChainParams,
	)
	if err != nil {
		return nil, err
	}

	if swapPayReq.PaymentAddr == nil {
		return nil, fmt.Errorf("expected payment address for invoice")
	}

	return swapPayReq.PaymentAddr, nil
}

// sendUpdate reports an update to the swap state.
func (s *loopOutSwap) sendUpdate(ctx context.Context) error {
	info := s.swapInfo()
	s.log.Infof("Loop out swap state: %v", info.State)

	info.HtlcAddressP2WSH = s.htlc.Address

	// In order to avoid potentially dangerous ownership sharing
	// we copy the outgoing channel set.
	if s.OutgoingChanSet != nil {
		outgoingChanSet := make(loopdb.ChannelSet, len(s.OutgoingChanSet))
		copy(outgoingChanSet[:], s.OutgoingChanSet[:])

		info.OutgoingChanSet = outgoingChanSet
	}

	select {
	case s.statusChan <- *info:
	case <-ctx.Done():
		return ctx.Err()
	}

	return nil
}

// execute starts/resumes the swap. It is a thin wrapper around
// executeAndFinalize to conveniently handle the error case.
func (s *loopOutSwap) execute(mainCtx context.Context,
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
			subCtx, s.hash, s.log, s.server.SubscribeLoopOutUpdates,
		)
	}()

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

			err := s.handlePaymentResult(result)
			if err != nil {
				return err
			}

			if result.failure() != nil {
				// Server didn't pull the swap payment.
				s.log.Infof("Swap payment failed: %v",
					result.failure())

				continue
			}

		case result := <-s.prePaymentChan:
			s.prePaymentChan = nil

			err := s.handlePaymentResult(result)
			if err != nil {
				return err
			}

			if result.failure() != nil {
				// Server didn't pull the prepayment.
				s.log.Infof("Prepayment failed: %v",
					result.failure())

				continue
			}

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

func (s *loopOutSwap) handlePaymentResult(result paymentResult) error {
	switch {
	// If our result has a non-nil error, our status will be nil. In this
	// case the payment failed so we do not need to take any action.
	case result.err != nil:
		return nil

	case result.status.State == lnrpc.Payment_SUCCEEDED:
		s.cost.Server += result.status.Value.ToSatoshis()
		s.cost.Offchain += result.status.Fee.ToSatoshis()

		return nil

	case result.status.State == lnrpc.Payment_FAILED:
		return nil

	default:
		return fmt.Errorf("unexpected state: %v", result.status.State)
	}
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
	spendDetails, err := s.waitForHtlcSpendConfirmed(
		globalCtx, *htlcOutpoint, htlcValue,
	)
	if err != nil {
		return err
	}

	// If spend details are nil, we resolved the swap without waiting for
	// its spend, so we can exit.
	if spendDetails == nil {
		return nil
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
		ctx, s.hash, updateTime,
		loopdb.SwapStateData{
			State:      s.state,
			Cost:       s.cost,
			HtlcTxHash: s.htlcTxHash,
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

	// Ask the server if it recommends using a routing plugin.
	pluginType, err := s.swapKit.server.RecommendRoutingPlugin(
		ctx, s.swapInfo().SwapHash, s.swapInvoicePaymentAddr,
	)
	if err != nil {
		s.log.Warnf("Server couldn't recommend routing plugin: %v", err)
		pluginType = RoutingPluginNone
	} else {
		s.log.Infof("Server recommended routing plugin: %v", pluginType)
	}

	// Use the recommended routing plugin.
	s.swapPaymentChan = s.payInvoice(
		ctx, s.SwapInvoice, s.MaxSwapRoutingFee,
		s.LoopOutContract.OutgoingChanSet, pluginType, true,
	)

	// Pay the prepay invoice. Won't use the routing plugin here as the
	// prepay is trivially small and shouldn't normally need any help.
	s.log.Infof("Sending prepayment %v", s.PrepayInvoice)
	s.prePaymentChan = s.payInvoice(
		ctx, s.PrepayInvoice, s.MaxPrepayRoutingFee,
		nil, RoutingPluginNone, false,
	)
}

// paymentResult contains the response for a failed or settled payment, and
// any errors that occurred if the payment unexpectedly failed.
type paymentResult struct {
	status lndclient.PaymentStatus
	err    error
}

// failure returns the error we encountered trying to dispatch a payment result,
// if any.
func (p paymentResult) failure() error {
	if p.err != nil {
		return p.err
	}

	if p.status.State == lnrpc.Payment_SUCCEEDED {
		return nil
	}

	return fmt.Errorf("payment failed: %v", p.status.FailureReason)
}

// payInvoice pays a single invoice.
func (s *loopOutSwap) payInvoice(ctx context.Context, invoice string,
	maxFee btcutil.Amount, outgoingChanIds loopdb.ChannelSet,
	pluginType RoutingPluginType,
	reportPluginResult bool) chan paymentResult {

	resultChan := make(chan paymentResult)
	sendResult := func(result paymentResult) {
		select {
		case resultChan <- result:
		case <-ctx.Done():
		}
	}

	go func() {
		var result paymentResult

		status, err := s.payInvoiceAsync(
			ctx, invoice, maxFee, outgoingChanIds, pluginType,
			reportPluginResult,
		)
		if err != nil {
			result.err = err
			sendResult(result)
			return
		}

		// If our payment failed or succeeded, our status should be
		// non-nil.
		switch status.State {
		case lnrpc.Payment_FAILED, lnrpc.Payment_SUCCEEDED:
			result.status = *status

		default:
			result.err = fmt.Errorf("unexpected payment state: %v",
				status.State)
		}

		sendResult(result)
	}()

	return resultChan
}

// payInvoiceAsync is the asynchronously executed part of paying an invoice.
func (s *loopOutSwap) payInvoiceAsync(ctx context.Context,
	invoice string, maxFee btcutil.Amount,
	outgoingChanIds loopdb.ChannelSet, pluginType RoutingPluginType,
	reportPluginResult bool) (*lndclient.PaymentStatus, error) {

	// Extract hash from payment request. Unfortunately the request
	// components aren't available directly.
	chainParams := s.lnd.ChainParams
	target, routeHints, hash, amt, err := swap.DecodeInvoice(
		chainParams, invoice,
	)
	if err != nil {
		return nil, err
	}

	maxRetries := 1
	paymentTimeout := s.executeConfig.totalPaymentTimout

	// Attempt to acquire and initialize the routing plugin.
	routingPlugin, err := AcquireRoutingPlugin(
		ctx, pluginType, *s.lnd, target, routeHints, amt,
	)
	if err != nil {
		return nil, err
	}

	if routingPlugin != nil {
		s.log.Infof("Acquired routing plugin %v for payment %v",
			pluginType, hash.String())

		maxRetries = s.executeConfig.maxPaymentRetries
		paymentTimeout /= time.Duration(maxRetries)
		defer ReleaseRoutingPlugin(ctx)
	}

	req := lndclient.SendPaymentRequest{
		MaxFee:          maxFee,
		Invoice:         invoice,
		OutgoingChanIds: outgoingChanIds,
		Timeout:         paymentTimeout,
		MaxParts:        s.executeConfig.loopOutMaxParts,
	}

	// Lookup state of the swap payment.
	payCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	start := time.Now()
	paymentStatus, attempts, err := s.sendPaymentWithRetry(
		payCtx, hash, &req, maxRetries, routingPlugin, pluginType,
	)

	dt := time.Since(start)
	paymentSuccess := err == nil &&
		paymentStatus.State == lnrpc.Payment_SUCCEEDED

	if reportPluginResult {
		// If the plugin couldn't be acquired then override the reported
		// plugin type to RoutingPluginNone.
		reportType := pluginType
		if routingPlugin == nil {
			reportType = RoutingPluginNone
		}

		if err := s.swapKit.server.ReportRoutingResult(
			ctx, s.swapInfo().SwapHash, s.swapInvoicePaymentAddr,
			reportType, paymentSuccess, int32(attempts),
			dt.Milliseconds(),
		); err != nil {
			s.log.Warnf("Failed to report routing result: %v", err)
		}
	}

	return paymentStatus, err
}

// sendPaymentWithRetry will send the payment, optionally with the passed
// routing plugin retrying at most maxRetries times.
func (s *loopOutSwap) sendPaymentWithRetry(ctx context.Context,
	hash lntypes.Hash, req *lndclient.SendPaymentRequest, maxRetries int,
	routingPlugin RoutingPlugin, pluginType RoutingPluginType) (
	*lndclient.PaymentStatus, int, error) {

	tryCount := 1
	for {
		s.log.Infof("Payment (%v) try count %v/%v (plugin=%v)",
			hash.String(), tryCount, maxRetries,
			pluginType.String())

		if routingPlugin != nil {
			if err := routingPlugin.BeforePayment(
				ctx, tryCount, maxRetries,
			); err != nil {
				return nil, tryCount, err
			}
		}

		var err error
		paymentStatus, err := s.awaitSendPayment(ctx, hash, req)
		if err != nil {
			return nil, tryCount, err
		}

		// Payment has succeeded, we can return here.
		if paymentStatus.State == lnrpc.Payment_SUCCEEDED {
			return paymentStatus, tryCount, nil
		}

		// Retry if the payment has timed out, or return here.
		if tryCount > maxRetries || paymentStatus.FailureReason !=
			lnrpc.PaymentFailureReason_FAILURE_REASON_TIMEOUT {

			return paymentStatus, tryCount, nil
		}

		tryCount++
	}
}

func (s *loopOutSwap) awaitSendPayment(ctx context.Context, hash lntypes.Hash,
	req *lndclient.SendPaymentRequest) (*lndclient.PaymentStatus, error) {

	payStatusChan, payErrChan, err := s.lnd.Router.SendPayment(ctx, *req)
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
				return &payState, nil

			case lnrpc.Payment_IN_FLIGHT:
				// Continue waiting for final state.

			default:
				return nil, errors.New("unknown payment state")
			}

		// Abort the swap in case of an error. An unknown
		// payment error from TrackPayment is no longer expected
		// here.
		case err := <-payErrChan:
			if err != channeldb.ErrAlreadyPaid {
				return nil, err
			}

			payStatusChan, payErrChan, err =
				s.lnd.Router.TrackPayment(ctx, hash)
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
		"Register %v conf ntfn for swap script on chain (hh=%v)",
		s.HtlcConfirmations, s.InitiationHeight,
	)

	// If we've revealed the preimage in a previous run, we expect to have
	// recorded the htlc tx hash. We use this to re-register for
	// confirmation, to be sure that we'll keep tracking the same htlc. For
	// older swaps, this field may not be populated even though the preimage
	// has already been revealed.
	if s.state == loopdb.StatePreimageRevealed && s.htlcTxHash == nil {
		s.log.Warnf("No htlc tx hash available, registering with " +
			"just the pkscript")
	}

	ctx, cancel := context.WithCancel(globalCtx)
	defer cancel()
	htlcConfChan, htlcErrChan, err :=
		s.lnd.ChainNotifier.RegisterConfirmationsNtfn(
			ctx, s.htlcTxHash, s.htlc.PkScript,
			int32(s.HtlcConfirmations), s.InitiationHeight,
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
			"off-chain payment failure")
	loop:
		for {
			select {
			// If the swap payment fails, abandon the swap. We may
			// have lost the prepayment.
			case result := <-s.swapPaymentChan:
				s.swapPaymentChan = nil

				err := s.handlePaymentResult(result)
				if err != nil {
					return nil, err
				}

				if result.failure() != nil {
					s.log.Infof("Failed swap payment: %v",
						result.failure())

					s.failOffChain(
						ctx, paymentTypeInvoice,
						result.status,
					)
					return nil, nil
				}

			// If the prepay fails, abandon the swap. Because we
			// didn't reveal the preimage, the swap payment will be
			// canceled or time out.
			case result := <-s.prePaymentChan:
				s.prePaymentChan = nil

				err := s.handlePaymentResult(result)
				if err != nil {
					return nil, err
				}

				if result.failure() != nil {
					s.log.Infof("Failed prepayment: %v",
						result.failure())

					s.failOffChain(
						ctx, paymentTypeInvoice,
						result.status,
					)

					return nil, nil
				}

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

	htlcTxHash := txConf.Tx.TxHash()
	s.log.Infof("Htlc tx %v at height %v", htlcTxHash, txConf.BlockHeight)

	s.htlcTxHash = &htlcTxHash

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
	htlcOutpoint wire.OutPoint, htlcValue btcutil.Amount) (
	*chainntnfs.SpendDetail, error) {

	// Register the htlc spend notification.
	ctx, cancel := context.WithCancel(globalCtx)
	defer cancel()
	spendChan, spendErr, err := s.lnd.ChainNotifier.RegisterSpendNtfn(
		ctx, &htlcOutpoint, s.htlc.PkScript, s.InitiationHeight,
	)
	if err != nil {
		return nil, fmt.Errorf("register spend ntfn: %v", err)
	}

	// Track our payment status so that we can detect whether our off chain
	// htlc is settled. We track this information to determine whether it is
	// necessary to continue trying to push our preimage to the server.
	trackChan, trackErrChan, err := s.lnd.Router.TrackPayment(
		ctx, s.hash,
	)
	if err != nil {
		return nil, fmt.Errorf("track payment: %v", err)
	}

	var (
		// paymentComplete tracks whether our payment is complete, and
		// is used to decide whether we need to push our preimage to
		// the server.
		paymentComplete bool
		// musigSweepTryCount tracts the number of cooperative, MuSig2
		// sweep attempts.
		musigSweepTryCount int
		// musigSweepSuccess tracks whether at least one MuSig2 sweep
		// txn was successfully published to the mempool.
		musigSweepSuccess bool
	)

	timerChan := s.timerFactory(republishDelay)
	for {
		select {
		// Htlc spend, break loop.
		case spendDetails := <-spendChan:
			s.log.Infof("Htlc spend by tx: %v",
				spendDetails.SpenderTxHash)

			return spendDetails, nil

		// Spend notification error.
		case err := <-spendErr:
			return nil, err

		// Receive status updates for our payment so that we can detect
		// whether we've successfully pushed our preimage.
		case status, ok := <-trackChan:
			// If our channel has been closed, indicating that the
			// server is finished providing updates because the
			// payment has reached a terminal state, we replace
			// the closed channel with nil so that we will no longer
			// listen on it.
			if !ok {
				trackChan = nil
				continue
			}

			if status.State == lnrpc.Payment_SUCCEEDED {
				s.log.Infof("Off chain payment succeeded")

				paymentComplete = true
			}

		// If we receive a track payment error that indicates that the
		// server stream is complete, we ignore it because we want to
		// continue this loop beyond the completion of the payment.
		case err, ok := <-trackErrChan:
			// If our channel has been closed, indicating that the
			// server is finished providing updates because the
			// payment has reached a terminal state, we replace
			// the closed channel with nil so that we will no longer
			// listen on it.
			if !ok {
				trackErrChan = nil
				continue
			}

			// Otherwise, if we receive a non-nil error, we return
			// it.
			if err != nil {
				return nil, err
			}

		// New block arrived, update height and restart the republish
		// timer.
		case notification := <-s.blockEpochChan:
			s.height = notification.(int32)
			timerChan = s.timerFactory(republishDelay)

		// Some time after start or after arrival of a new block, try
		// to spend again.
		case <-timerChan:
			if IsTaprootSwap(&s.SwapContract) {
				// sweepConfTarget will return false if the
				// preimage is not revealed yet but the conf
				// target is closer than 20 blocks. In this case
				// to be sure we won't attempt to sweep at all
				// and we won't reveal the preimage either.
				_, canSweep := s.sweepConfTarget()
				if !canSweep {
					s.log.Infof("Aborting swap, timed " +
						"out on-chain")

					s.state = loopdb.StateFailTimeout
					err := s.persistState(ctx)
					if err != nil {
						log.Warnf("unable to persist " +
							"state")
					}

					return nil, nil
				}

				// When using taproot HTLCs we're pushing the
				// preimage before attempting to sweep. This
				// way the server will know that the swap will
				// go through and we'll be able to MuSig2
				// cosign our sweep transaction. In the worst
				// case if the server is uncooperative for any
				// reason we can still sweep using scriptpath
				// spend.
				err = s.setStatePreimageRevealed(ctx)
				if err != nil {
					return nil, err
				}

				if !paymentComplete {
					// Push the preimage for as long as the
					// server is able to settle the swap
					// invoice. So that we can continue
					// with the MuSig2 sweep afterwards.
					s.pushPreimage(ctx)
				}

				// Now attempt to publish a MuSig2 sweep txn.
				// Only attempt at most maxMusigSweepRetires
				// times to still leave time for an emergency
				// script path sweep.
				if musigSweepTryCount < maxMusigSweepRetries {
					success := s.sweepMuSig2(
						ctx, htlcOutpoint, htlcValue,
					)
					if !success {
						musigSweepTryCount++
					} else {
						// Mark that we had a sweep
						// that was successful. There's
						// no need for the script spend
						// now we can just keep pushing
						// new sweeps to bump the fee.
						musigSweepSuccess = true
					}
				} else if !musigSweepSuccess {
					// Attempt to script path sweep. If the
					// sweep fails, we can't do any better
					// than go on and try again later as
					// the preimage is alredy revealed and
					// the server settled the swap payment.
					// From the server's point of view the
					// swap is succeeded at this point so
					// we are free to retry as long as we
					// want.
					err := s.sweep(
						ctx, htlcOutpoint, htlcValue,
					)
					if err != nil {
						log.Warnf("Failed to publish "+
							"non-cooperative "+
							"sweep: %v", err)
					}
				}

				// If the result of our spend func was that the
				// swap has reached a final state, then we
				// return nil spend details, because there is
				// no further action required for this swap.
				if s.state.Type() != loopdb.StateTypePending {
					return nil, nil
				}
			} else {
				err := s.sweep(ctx, htlcOutpoint, htlcValue)
				if err != nil {
					return nil, err
				}

				// If the result of our spend func was that the
				// swap has reached a final state, then we
				// return nil spend details, because there is no
				// further action required for this swap.
				if s.state.Type() != loopdb.StateTypePending {
					return nil, nil
				}

				// If our off chain payment is not yet complete,
				// we try to push our preimage to the server.
				if !paymentComplete {
					s.pushPreimage(ctx)
				}
			}

		// Context canceled.
		case <-globalCtx.Done():
			return nil, globalCtx.Err()
		}
	}
}

// pushPreimage pushes our preimage to the server if we have already revealed
// our preimage on chain with a sweep attempt.
func (s *loopOutSwap) pushPreimage(ctx context.Context) {
	// If we have not yet revealed our preimage through a sweep, we do not
	// push the preimage because we may choose to never sweep if fees are
	// too high.
	if s.state != loopdb.StatePreimageRevealed {
		return
	}

	s.log.Infof("Pushing preimage to server")

	// Push the preimage to the server, just log server errors since we rely
	// on our payment state rather than the server response to judge the
	// outcome of our preimage push.
	if err := s.server.PushLoopOutPreimage(ctx, s.Preimage); err != nil {
		s.log.Warnf("Could not push preimage: %v", err)
	}
}

// failOffChain updates a swap's state when it has failed due to a routing
// failure and notifies the server of the failure.
func (s *loopOutSwap) failOffChain(ctx context.Context, paymentType paymentType,
	status lndclient.PaymentStatus) {

	// Set our state to failed off chain timeout.
	s.state = loopdb.StateFailOffchainPayments

	details := &outCancelDetails{
		hash:        s.hash,
		paymentAddr: s.swapInvoicePaymentAddr,
		metadata: routeCancelMetadata{
			paymentType:   paymentType,
			failureReason: status.FailureReason,
		},
	}

	for _, htlc := range status.Htlcs {
		if htlc.Status != lnrpc.HTLCAttempt_FAILED {
			continue
		}

		if htlc.Route == nil {
			continue
		}

		if len(htlc.Route.Hops) == 0 {
			continue
		}

		if htlc.Failure == nil {
			continue
		}

		failureIdx := htlc.Failure.FailureSourceIndex
		hops := uint32(len(htlc.Route.Hops))

		// We really don't expect a failure index that is greater than
		// our number of hops. This is because failure index is zero
		// based, where a value of zero means that the payment failed
		// at the client's node, and a value = len(hops) means that it
		// failed at the last node in the route. We don't want to
		// underflow so we check and log a warning if this happens.
		if failureIdx > hops {
			s.log.Warnf("Htlc attempt failure index > hops",
				failureIdx, hops)

			continue
		}

		// Add the number of hops from the server that we failed at
		// to the set of attempts that we will report to the server.
		distance := hops - failureIdx

		// In the case that our swap failed in the network at large,
		// rather than the loop server's internal infrastructure, we
		// don't want to disclose and information about distance from
		// the server, so we set maxUint32 to represent failure in
		// "the network at large" rather than due to the server's
		// liquidity.
		if distance > loopInternalHops {
			distance = math.MaxUint32
		}

		details.metadata.attempts = append(
			details.metadata.attempts, distance,
		)
	}

	s.log.Infof("Canceling swap: %v payment failed: %v, %v attempts",
		paymentType, details.metadata.failureReason,
		len(details.metadata.attempts))

	// Report to server, it's not critical if this doesn't go through.
	if err := s.cancelSwap(ctx, details); err != nil {
		s.log.Warnf("Could not report failure: %v", err)
	}
}

// createMuSig2SweepTxn creates a taproot keyspend sweep transaction and
// attempts to cooperate with the server to create a MuSig2 signature witness.
func (s *loopOutSwap) createMuSig2SweepTxn(
	ctx context.Context, htlcOutpoint wire.OutPoint,
	htlcValue btcutil.Amount, fee btcutil.Amount) (*wire.MsgTx, error) {

	// First assemble our taproot keyspend sweep transaction and get the
	// sig hash.
	sweepTx, sweepTxPsbt, sigHash, err := s.sweeper.CreateUnsignedTaprootKeySpendSweepTx(
		ctx, uint32(s.height), s.htlc, htlcOutpoint, htlcValue, fee,
		s.DestAddr,
	)
	if err != nil {
		return nil, err
	}

	var (
		signers       [][]byte
		muSig2Version input.MuSig2Version
	)

	// Depending on the MuSig2 version we either pass 32 byte Schnorr
	// public keys or normal 33 byte public keys.
	if s.ProtocolVersion >= loopdb.ProtocolVersionMuSig2 {
		muSig2Version = input.MuSig2Version100RC2
		signers = [][]byte{
			s.HtlcKeys.SenderInternalPubKey[:],
			s.HtlcKeys.ReceiverInternalPubKey[:],
		}
	} else {
		muSig2Version = input.MuSig2Version040
		signers = [][]byte{
			s.HtlcKeys.SenderInternalPubKey[1:],
			s.HtlcKeys.ReceiverInternalPubKey[1:],
		}
	}

	htlcScript, ok := s.htlc.HtlcScript.(*swap.HtlcScriptV3)
	if !ok {
		return nil, fmt.Errorf("non taproot htlc")
	}

	// Now we're creating a local MuSig2 session using the receiver key's
	// key locator and the htlc's root hash.
	musig2SessionInfo, err := s.lnd.Signer.MuSig2CreateSession(
		ctx, muSig2Version, &s.HtlcKeys.ClientScriptKeyLocator, signers,
		lndclient.MuSig2TaprootTweakOpt(htlcScript.RootHash[:], false),
	)
	if err != nil {
		return nil, err
	}

	// With the session active, we can now send the server our public nonce
	// and the sig hash, so that it can create it's own MuSig2 session and
	// return the server side nonce and partial signature.
	serverNonce, serverSig, err := s.swapKit.server.MuSig2SignSweep(
		ctx, s.SwapContract.ProtocolVersion, s.hash,
		s.swapInvoicePaymentAddr, musig2SessionInfo.PublicNonce[:],
		sweepTxPsbt,
	)
	if err != nil {
		return nil, err
	}

	var serverPublicNonce [musig2.PubNonceSize]byte
	copy(serverPublicNonce[:], serverNonce)

	// Register the server's nonce before attempting to create our partial
	// signature.
	haveAllNonces, err := s.lnd.Signer.MuSig2RegisterNonces(
		ctx, musig2SessionInfo.SessionID,
		[][musig2.PubNonceSize]byte{serverPublicNonce},
	)
	if err != nil {
		return nil, err
	}

	// Sanity check that we have all the nonces.
	if !haveAllNonces {
		return nil, fmt.Errorf("invalid MuSig2 session: nonces missing")
	}

	var digest [32]byte
	copy(digest[:], sigHash)

	// Since our MuSig2 session has all nonces, we can now create the local
	// partial signature by signing the sig hash.
	_, err = s.lnd.Signer.MuSig2Sign(
		ctx, musig2SessionInfo.SessionID, digest, false,
	)
	if err != nil {
		return nil, err
	}

	// Now combine the partial signatures to use the final combined
	// signature in the sweep transaction's witness.
	haveAllSigs, finalSig, err := s.lnd.Signer.MuSig2CombineSig(
		ctx, musig2SessionInfo.SessionID, [][]byte{serverSig},
	)
	if err != nil {
		return nil, err
	}

	if !haveAllSigs {
		return nil, fmt.Errorf("failed to combine signatures")
	}

	// To be sure that we're good, parse and validate that the combined
	// signature is indeed valid for the sig hash and the internal pubkey.
	err = s.executeConfig.verifySchnorrSig(
		htlcScript.TaprootKey, sigHash, finalSig,
	)
	if err != nil {
		return nil, err
	}

	// Now that we know the signature is correct, we can fill it in to our
	// witness.
	sweepTx.TxIn[0].Witness = wire.TxWitness{
		finalSig,
	}

	return sweepTx, nil
}

// sweepConfTarget returns the confirmation target for the htlc sweep or false
// if we're too late.
func (s *loopOutSwap) sweepConfTarget() (int32, bool) {
	remainingBlocks := s.CltvExpiry - s.height
	blocksToLastReveal := remainingBlocks - MinLoopOutPreimageRevealDelta
	preimageRevealed := s.state == loopdb.StatePreimageRevealed

	// If we have not revealed our preimage, and we don't have time left
	// to sweep the swap, we abandon the swap because we can no longer
	// sweep the success path (without potentially having to compete with
	// the server's timeout sweep), and we have not had any coins pulled
	// off-chain.
	if blocksToLastReveal <= 0 && !preimageRevealed {
		s.log.Infof("Preimage can no longer be safely revealed: "+
			"expires at: %v, current height: %v", s.CltvExpiry,
			s.height)

		s.state = loopdb.StateFailTimeout
		return 0, false
	}

	// Calculate the transaction fee based on the confirmation target
	// required to sweep the HTLC before the timeout. We'll use the
	// confirmation target provided by the client unless we've come too
	// close to the expiration height, in which case we'll use the default
	// if it is better than what the client provided.
	confTarget := s.SweepConfTarget
	if remainingBlocks <= DefaultSweepConfTargetDelta &&
		confTarget > DefaultSweepConfTarget {

		confTarget = DefaultSweepConfTarget
	}

	return confTarget, true
}

// clampSweepFee will clamp the passed in sweep fee to the maximum configured
// miner fee. Returns false if sweeping should not continue. Note that in the
// MuSig2 case we always continue as the preimage is revealed to the server
// before cooperatively signing the sweep transaction.
func (s *loopOutSwap) clampSweepFee(fee btcutil.Amount) (btcutil.Amount, bool) {
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
			return 0, false
		}
	}

	return fee, true
}

// sweepMuSig2 attempts to sweep the on-chain HTLC using MuSig2. If anything
// fails, we'll log it but will simply return to allow further retries. Since
// the preimage is revealed by the time we attempt to MuSig2 sweep, we'll need
// to fall back to a script spend sweep if all MuSig2 sweep attempts fail (for
// example the server could be down due to maintenance or any other issue
// making the cooperative sweep fail).
func (s *loopOutSwap) sweepMuSig2(ctx context.Context,
	htlcOutpoint wire.OutPoint, htlcValue btcutil.Amount) bool {

	addInputToEstimator := func(e *input.TxWeightEstimator) error {
		e.AddTaprootKeySpendInput(txscript.SigHashDefault)
		return nil
	}

	confTarget, _ := s.sweepConfTarget()
	fee, err := s.sweeper.GetSweepFee(
		ctx, addInputToEstimator, s.DestAddr, confTarget,
	)
	if err != nil {
		s.log.Warnf("Failed to estimate fee MuSig2 sweep txn: %v", err)
		return false
	}

	fee, _ = s.clampSweepFee(fee)

	// Now attempt the co-signing of the txn.
	sweepTx, err := s.createMuSig2SweepTxn(
		ctx, htlcOutpoint, htlcValue, fee,
	)
	if err != nil {
		s.log.Warnf("Failed to create MuSig2 sweep txn: %v", err)
		return false
	}

	// Finally, try publish the txn.
	s.log.Infof("Sweep on chain HTLC using MuSig2 to address %v "+
		"fee %v (tx %v)", s.DestAddr, fee, sweepTx.TxHash())

	err = s.lnd.WalletKit.PublishTransaction(
		ctx, sweepTx,
		labels.LoopOutSweepSuccess(swap.ShortHash(&s.hash)),
	)
	if err != nil {
		var sweepTxBuf bytes.Buffer
		if err := sweepTx.Serialize(&sweepTxBuf); err != nil {
			s.log.Warnf("Unable to serialize sweep txn: %v", err)
		}

		s.log.Warnf("Publish of MuSig2 sweep failed: %v. Raw tx: %x",
			err, sweepTxBuf.Bytes())

		return false
	}

	return true
}

func (s *loopOutSwap) setStatePreimageRevealed(ctx context.Context) error {
	if s.state != loopdb.StatePreimageRevealed {
		s.state = loopdb.StatePreimageRevealed

		err := s.persistState(ctx)
		if err != nil {
			return err
		}
	}

	return nil
}

// sweep tries to sweep the given htlc to a destination address. It takes into
// account the max miner fee and unless the preimage is already revealed
// (MuSig2 case), marks the preimage as revealed when it published the tx. If
// the preimage has not yet been revealed, and the time during which we can
// safely reveal it has passed, the swap will be marked as failed, and the
// function will return.
func (s *loopOutSwap) sweep(ctx context.Context, htlcOutpoint wire.OutPoint,
	htlcValue btcutil.Amount) error {

	confTarget, canSweep := s.sweepConfTarget()
	if !canSweep {
		return nil
	}

	fee, err := s.sweeper.GetSweepFee(
		ctx, s.htlc.AddSuccessToEstimator, s.DestAddr, confTarget,
	)
	if err != nil {
		return err
	}

	fee, canSweep = s.clampSweepFee(fee)
	if !canSweep {
		return nil
	}

	witnessFunc := func(sig []byte) (wire.TxWitness, error) {
		return s.htlc.GenSuccessWitness(sig, s.Preimage)
	}

	// Retrieve the full script required to unlock the output.
	redeemScript := s.htlc.SuccessScript()

	// Create sweep tx.
	sweepTx, err := s.sweeper.CreateSweepTx(
		ctx, s.height, s.htlc.SuccessSequence(), s.htlc,
		htlcOutpoint, s.contract.HtlcKeys.ReceiverScriptKey,
		redeemScript, witnessFunc, htlcValue, fee, s.DestAddr,
	)
	if err != nil {
		return err
	}

	// Before publishing the tx, already mark the preimage as revealed. This
	// is a precaution in case the publish call never returns and would
	// leave us thinking we didn't reveal yet.
	err = s.setStatePreimageRevealed(ctx)
	if err != nil {
		return err
	}

	// Publish tx.
	s.log.Infof("Sweep on chain HTLC to address %v with fee %v (tx %v)",
		s.DestAddr, fee, sweepTx.TxHash())

	err = s.lnd.WalletKit.PublishTransaction(
		ctx, sweepTx,
		labels.LoopOutSweepSuccess(swap.ShortHash(&s.hash)),
	)
	if err != nil {
		var sweepTxBuf bytes.Buffer
		if err := sweepTx.Serialize(&sweepTxBuf); err != nil {
			s.log.Warnf("Unable to serialize sweep txn: %v", err)
		}

		s.log.Warnf("Publish sweep failed: %v. Raw tx: %x",
			err, sweepTxBuf.Bytes())
	}

	return nil
}

// validateLoopOutContract validates the contract parameters against our
// request.
func validateLoopOutContract(lnd *lndclient.LndServices, request *OutRequest,
	swapHash lntypes.Hash, response *newLoopOutResponse) error {

	// Check invoice amounts.
	chainParams := lnd.ChainParams

	_, _, swapInvoiceHash, swapInvoiceAmt, err := swap.DecodeInvoice(
		chainParams, response.swapInvoice,
	)
	if err != nil {
		return err
	}

	if swapInvoiceHash != swapHash {
		return fmt.Errorf(
			"cannot initiate swap, swap invoice hash %v not equal "+
				"generated swap hash %v", swapInvoiceHash, swapHash)
	}

	_, _, _, prepayInvoiceAmt, err := swap.DecodeInvoice(
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

	return nil
}
