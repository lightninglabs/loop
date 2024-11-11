package loop

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/sweep"
	"github.com/lightninglabs/loop/sweepbatcher"
	"github.com/lightninglabs/loop/utils"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
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

	// MinLoopOutPreimageRevealDelta configures the minimum number of
	// remaining blocks before htlc expiry required to reveal preimage.
	MinLoopOutPreimageRevealDelta = 20

	// DefaultSweepConfTarget is the default confirmation target we'll use
	// when sweeping on-chain HTLCs.
	DefaultSweepConfTarget = 9

	// DefaultHtlcConfTarget is the default confirmation target we'll use
	// for on-chain htlcs published by the swap client for Loop In.
	DefaultHtlcConfTarget = 6

	// DefaultSweepConfTargetDelta is the delta of blocks from a Loop Out
	// swap's expiration height at which we begin to cap the sweep
	// confirmation target with urgentSweepConfTarget and multiply feerate
	// by factor urgentSweepConfTargetFactor.
	DefaultSweepConfTargetDelta = 10

	// urgentSweepConfTarget is the confirmation target we'll use when the
	// loop-out swap is about to expire (<= DefaultSweepConfTargetDelta
	// blocks to expire).
	urgentSweepConfTarget = 3

	// urgentSweepConfTargetFactor is the factor we apply to feerate of
	// loop-out sweep if it is about to expire.
	urgentSweepConfTargetFactor = 1.1
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

	// prepayAmount holds the amount of the prepay invoice. We use this
	// to calculate the total cost of the swap.
	prepayAmount btcutil.Amount

	swapPaymentChan chan paymentResult
	prePaymentChan  chan paymentResult

	wg sync.WaitGroup
}

// executeConfig contains extra configuration to execute the swap.
type executeConfig struct {
	sweeper             *sweep.Sweeper
	batcher             *sweepbatcher.Batcher
	statusChan          chan<- SwapInfo
	blockEpochChan      <-chan interface{}
	timerFactory        func(time.Duration) <-chan time.Time
	loopOutMaxParts     uint32
	totalPaymentTimeout time.Duration
	maxPaymentRetries   int
	cancelSwap          func(context.Context, *outCancelDetails) error
	verifySchnorrSig    func(pubKey *btcec.PublicKey, hash, sig []byte) error
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
		IsExternalAddr:          request.IsExternalAddr,
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
		PaymentTimeout:  request.PaymentTimeout,
	}

	swapKit := newSwapKit(
		swapHash, swap.TypeOut, cfg, &contract.SwapContract,
	)

	swapKit.lastUpdateTime = initiationTime

	// Create the htlc.
	htlc, err := utils.GetHtlc(
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
	paymentAddr, err := utils.ObtainSwapPaymentAddr(
		contract.SwapInvoice, cfg.lnd.ChainParams,
	)
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
	htlc, err := utils.GetHtlc(
		swapKit.hash, swapKit.contract, swapKit.lnd.ChainParams,
	)
	if err != nil {
		return nil, err
	}

	// Log htlc address for debugging.
	swapKit.log.Infof("Htlc address: %v", htlc.Address)

	// Obtain the payment addr since we'll need it later for routing plugin
	// recommendation and possibly for cancel.
	paymentAddr, err := utils.ObtainSwapPaymentAddr(
		pend.Contract.SwapInvoice, cfg.lnd.ChainParams,
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

// sendUpdate reports an update to the swap state.
func (s *loopOutSwap) sendUpdate(ctx context.Context) error {
	info := s.swapInfo()
	s.log.Infof("Loop out swap state: %v", info.State)

	if s.htlc.OutputType == swap.HtlcP2WSH {
		info.HtlcAddressP2WSH = s.htlc.Address
	} else {
		info.HtlcAddressP2TR = s.htlc.Address
	}

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

			err := s.handlePaymentResult(result, true)
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

			err := s.handlePaymentResult(result, false)
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

// handlePaymentResult processes the result of a payment attempt. If the
// payment was successful and this is the main swap payment, the cost of the
// swap is updated.
func (s *loopOutSwap) handlePaymentResult(result paymentResult,
	swapPayment bool) error {

	switch {
	// If our result has a non-nil error, our status will be nil. In this
	// case the payment failed so we do not need to take any action.
	case result.err != nil:
		return nil

	case result.status.State == lnrpc.Payment_SUCCEEDED:
		// Update the cost of the swap if this is the main swap payment.
		if swapPayment {
			// The client pays for the swap with the swap invoice,
			// so we can calculate the total cost of the swap by
			// subtracting the amount requested from the total
			// amount that we actually paid (which is the sum of
			// the swap invoice amount and the prepay invoice
			// amount).
			s.cost.Server += s.prepayAmount +
				result.status.Value.ToSatoshis() -
				s.AmountRequested
		}

		// On top of the swap cost we also pay for routing which
		// is reflected in the fee. We add the off-chain fee for both
		// the swap payment and the prepay.
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
	// Decode the prepay invoice so we can ensure that we account for the
	// prepay amount when calculating the final costs of the swap.
	_, _, _, amt, err := swap.DecodeInvoice(
		s.lnd.ChainParams, s.PrepayInvoice,
	)
	if err != nil {
		return err
	}
	s.prepayAmount = amt

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
	spend, err := s.waitForHtlcSpendConfirmedV2(
		globalCtx, *htlcOutpoint, htlcValue,
	)
	if err != nil {
		return err
	}

	// If spend details are nil, we resolved the swap without waiting for
	// its spend, so we can exit.
	if spend == nil {
		return nil
	}

	// Inspect witness stack to see if it is a success transaction. We
	// don't just try to match with the hash of our sweep tx, because it
	// may be swept by a different (fee) sweep tx from a previous run.
	htlcInput, err := swap.GetTxInputByOutpoint(
		spend.Tx, htlcOutpoint,
	)
	if err != nil {
		return err
	}

	sweepSuccessful := s.htlc.IsSuccessWitness(htlcInput.Witness)
	if sweepSuccessful {
		s.cost.Onchain = spend.OnChainFeePortion
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
		s.LoopOutContract.OutgoingChanSet,
		s.LoopOutContract.PaymentTimeout, pluginType, true,
	)

	// Pay the prepay invoice. Won't use the routing plugin here as the
	// prepay is trivially small and shouldn't normally need any help. We
	// are sending it over the same channel as the loop out payment.
	s.log.Infof("Sending prepayment %v", s.PrepayInvoice)
	s.prePaymentChan = s.payInvoice(
		ctx, s.PrepayInvoice, s.MaxPrepayRoutingFee,
		s.LoopOutContract.OutgoingChanSet,
		s.LoopOutContract.PaymentTimeout, RoutingPluginNone, false,
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
	paymentTimeout time.Duration, pluginType RoutingPluginType,
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
			ctx, invoice, maxFee, outgoingChanIds, paymentTimeout,
			pluginType, reportPluginResult,
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
	outgoingChanIds loopdb.ChannelSet, paymentTimeout time.Duration,
	pluginType RoutingPluginType, reportPluginResult bool) (
	*lndclient.PaymentStatus, error) {

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
	totalPaymentTimeout := s.executeConfig.totalPaymentTimeout

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

		// If not set, default to the per payment timeout to the total
		// payment timeout divied by the configured maximum retries.
		if paymentTimeout == 0 {
			paymentTimeout = totalPaymentTimeout /
				time.Duration(maxRetries)
		}

		// If the payment timeout is too long, we need to adjust the
		// number of retries to ensure we don't exceed the total
		// payment timeout.
		if paymentTimeout*time.Duration(maxRetries) >
			totalPaymentTimeout {

			maxRetries = int(totalPaymentTimeout / paymentTimeout)
			s.log.Infof("Adjusted max routing plugin retries to "+
				"%v to stay within total payment timeout",
				maxRetries)
		}
		defer ReleaseRoutingPlugin(ctx)
	} else if paymentTimeout == 0 {
		// If not set, default the payment timeout to the total payment
		// timeout.
		paymentTimeout = totalPaymentTimeout
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

				err := s.handlePaymentResult(result, true)
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

				err := s.handlePaymentResult(result, false)
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

// waitForHtlcSpendConfirmedV2 waits for the htlc to be spent either by our own
// sweep or a server revocation tx.
func (s *loopOutSwap) waitForHtlcSpendConfirmedV2(globalCtx context.Context,
	htlcOutpoint wire.OutPoint, htlcValue btcutil.Amount) (
	*sweepbatcher.SpendDetail, error) {

	spendChan := make(chan *sweepbatcher.SpendDetail)
	spendErrChan := make(chan error, 1)
	quitChan := make(chan bool, 1)

	defer func() {
		quitChan <- true
	}()

	notifier := sweepbatcher.SpendNotifier{
		SpendChan:    spendChan,
		SpendErrChan: spendErrChan,
		QuitChan:     quitChan,
	}

	sweepReq := sweepbatcher.SweepRequest{
		SwapHash: s.hash,
		Outpoint: htlcOutpoint,
		Value:    htlcValue,
		Notifier: &notifier,
	}

	// Register the htlc spend notification.
	ctx, cancel := context.WithCancel(globalCtx)
	defer cancel()

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
	)

	timerChan := s.timerFactory(repushDelay)

	for {
		select {
		// Htlc spend, break loop.
		case spend := <-spendChan:
			s.log.Infof("Htlc spend by tx: %v", spend.Tx.TxHash())

			return spend, nil

		// Spend notification error.
		case err := <-spendErrChan:
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

		// New block arrived, update height and try pushing preimage.
		case notification := <-s.blockEpochChan:
			s.height = notification.(int32)
			timerChan = s.timerFactory(repushDelay)

		case <-timerChan:
			// canSweep will return false if the preimage is
			// not revealed yet but the conf target is closer than
			// 20 blocks. In this case to be sure we won't attempt
			// to sweep at all and we won't reveal the preimage
			// either.
			canSweep := s.canSweep()
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

			// Send the sweep to the sweeper.
			err := s.batcher.AddSweep(&sweepReq)
			if err != nil {
				return nil, err
			}

			// Now that the sweep is taken care of, we can update
			// our state.
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

// canSweep will return false if the preimage is not revealed yet but the conf
// target is closer than 20 blocks (i.e. it is too late to reveal the preimage).
func (s *loopOutSwap) canSweep() bool {
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
		return false
	}

	return true
}
