package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/loop/sweep"
	"github.com/lightninglabs/loop/utils"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/routing/route"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const loopInSweepConfTarget = int32(2)

// loopInSwap holds all server-side state for a standard Loop In. The initDone
// barrier makes NewLoopInSwap idempotent even when duplicate requests arrive
// while the first request is still carrying out the probe payment.
type loopInSwap struct {
	mu sync.Mutex

	request          loopInRequestFingerprint
	hash             lntypes.Hash
	amount           btcutil.Amount
	swapInvoice      string
	lastHop          *route.Vertex
	initiationHeight int32
	expiry           int32

	senderScriptKey     [btcec.PubKeyBytesLenCompressed]byte
	senderInternalKey   [btcec.PubKeyBytesLenCompressed]byte
	receiverScriptKey   *serverKey
	receiverInternalKey *serverKey
	htlc                *swap.Htlc

	updates *updateHub

	initDone chan struct{}
	initErr  error
	response *swapserverrpc.ServerLoopInResponse

	clientInternalKeyPushed bool
}

// loopInRequestFingerprint records every request field that can change the
// swap contract or either Lightning payment. It is captured before the hash is
// reserved so concurrent replays can be checked without waiting for the first
// probe to finish.
type loopInRequestFingerprint struct {
	amount            uint64
	swapInvoice       string
	probeInvoice      string
	senderKey         []byte
	senderInternalKey []byte
	lastHop           []byte
	protocol          swapserverrpc.ProtocolVersion
}

func newLoopInRequestFingerprint(
	req *swapserverrpc.ServerLoopInRequest) loopInRequestFingerprint {

	return loopInRequestFingerprint{
		amount:            req.GetAmt(),
		swapInvoice:       req.GetSwapInvoice(),
		probeInvoice:      req.GetProbeInvoice(),
		senderKey:         bytes.Clone(req.GetSenderKey()),
		senderInternalKey: bytes.Clone(req.GetSenderInternalPubkey()),
		lastHop:           bytes.Clone(req.GetLastHop()),
		protocol:          req.GetProtocolVersion(),
	}
}

func (f loopInRequestFingerprint) matches(
	req *swapserverrpc.ServerLoopInRequest) bool {

	return f.amount == req.GetAmt() &&
		f.swapInvoice == req.GetSwapInvoice() &&
		f.probeInvoice == req.GetProbeInvoice() &&
		bytes.Equal(f.senderKey, req.GetSenderKey()) &&
		bytes.Equal(f.senderInternalKey, req.GetSenderInternalPubkey()) &&
		bytes.Equal(f.lastHop, req.GetLastHop()) &&
		f.protocol == req.GetProtocolVersion()
}

func validateLoopInProtocol(version swapserverrpc.ProtocolVersion) error {
	if version != swapserverrpc.ProtocolVersion_MUSIG2 {
		return status.Errorf(
			codes.InvalidArgument,
			"standard Loop In requires protocol %d, got %d",
			swapserverrpc.ProtocolVersion_MUSIG2, version,
		)
	}

	return nil
}

func loopInAmount(raw uint64) (btcutil.Amount, error) {
	if raw > math.MaxInt64 {
		return 0, status.Error(codes.InvalidArgument, "amount overflows int64")
	}

	return btcutil.Amount(raw), nil
}

func cloneLoopInResponse(
	response *swapserverrpc.ServerLoopInResponse) *swapserverrpc.ServerLoopInResponse {

	if response == nil {
		return nil
	}

	return &swapserverrpc.ServerLoopInResponse{
		ReceiverKey: bytes.Clone(response.ReceiverKey),
		ReceiverInternalPubkey: bytes.Clone(
			response.ReceiverInternalPubkey,
		),
		Expiry:        response.Expiry,
		ServerMessage: response.ServerMessage,
	}
}

func (s *loopInSwap) completeInit(
	response *swapserverrpc.ServerLoopInResponse, err error) {

	s.mu.Lock()
	s.response = cloneLoopInResponse(response)
	s.initErr = err
	close(s.initDone)
	s.mu.Unlock()
}

func (s *loopInSwap) waitForInit(ctx context.Context) (
	*swapserverrpc.ServerLoopInResponse, error) {

	select {
	case <-s.initDone:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return cloneLoopInResponse(s.response), s.initErr
}

// LoopInTerms returns the amount range supported by the disposable server.
func (s *Server) LoopInTerms(_ context.Context,
	req *swapserverrpc.ServerLoopInTermsRequest) (
	*swapserverrpc.ServerLoopInTerms, error) {

	if err := validateLoopInProtocol(req.GetProtocolVersion()); err != nil {
		return nil, err
	}

	return &swapserverrpc.ServerLoopInTerms{
		MinSwapAmount: uint64(s.cfg.MinSwapAmount),
		MaxSwapAmount: uint64(s.cfg.MaxSwapAmount),
	}, nil
}

// Probe validates the proposed route endpoint. The authoritative reachability
// test is the hold-invoice payment performed by NewLoopInSwap, because this
// quote-time RPC does not carry an invoice that can be paid atomically.
func (s *Server) Probe(_ context.Context,
	req *swapserverrpc.ServerProbeRequest) (
	*swapserverrpc.ServerProbeResponse, error) {

	if err := validateLoopInProtocol(req.GetProtocolVersion()); err != nil {
		return nil, err
	}

	amount, err := loopInAmount(req.GetAmt())
	if err != nil {
		return nil, err
	}
	if err := s.validateAmount(amount); err != nil {
		return nil, err
	}
	if _, err := parseKey("probe target", req.GetTarget()); err != nil {
		return nil, err
	}
	if len(req.GetLastHop()) != 0 {
		if _, err := parseKey("last hop", req.GetLastHop()); err != nil {
			return nil, err
		}
	}

	return &swapserverrpc.ServerProbeResponse{}, nil
}

// LoopInQuote returns a deterministic fee. A zero amount requests a quote for
// the maximum supported amount, as specified by the server protocol.
func (s *Server) LoopInQuote(_ context.Context,
	req *swapserverrpc.ServerLoopInQuoteRequest) (
	*swapserverrpc.ServerLoopInQuoteResponse, error) {

	if err := validateLoopInProtocol(req.GetProtocolVersion()); err != nil {
		return nil, err
	}

	if _, err := parseKey("payment destination", req.GetPubkey()); err != nil {
		return nil, err
	}
	if len(req.GetLastHop()) != 0 {
		if _, err := parseKey("last hop", req.GetLastHop()); err != nil {
			return nil, err
		}
	}

	amount := s.cfg.MaxSwapAmount
	if req.GetAmt() != 0 {
		var err error
		amount, err = loopInAmount(req.GetAmt())
		if err != nil {
			return nil, err
		}
	}
	if err := s.validateAmount(amount); err != nil {
		return nil, err
	}

	return &swapserverrpc.ServerLoopInQuoteResponse{
		SwapFee:   int64(s.swapFee(amount)),
		CltvDelta: s.cfg.LoopInCltvDelta,
	}, nil
}

// NewLoopInSwap validates the contract and pays the client's hold invoice as
// a real Lightning payment. It only returns after lnd reports the payment in
// flight and the client cancels it, proving that the actual swap invoice is
// reachable without disclosing the swap preimage.
func (s *Server) NewLoopInSwap(ctx context.Context,
	req *swapserverrpc.ServerLoopInRequest) (
	*swapserverrpc.ServerLoopInResponse, error) {

	hash, err := parseHash(req.GetSwapHash())
	if err != nil {
		return nil, err
	}

	// Reserve the hash before any blocking work. All duplicate calls wait on
	// the same initialization result and never repeat the probe payment or
	// start a second chain watcher.
	s.mu.Lock()
	if existing, ok := s.loopIns[hash]; ok {
		s.mu.Unlock()
		if !existing.request.matches(req) {
			return nil, status.Error(
				codes.AlreadyExists,
				"Loop In swap hash already exists with different parameters",
			)
		}

		return existing.waitForInit(ctx)
	}
	if err := validateLoopInProtocol(req.GetProtocolVersion()); err != nil {
		s.mu.Unlock()
		return nil, err
	}

	loopIn := &loopInSwap{
		request:  newLoopInRequestFingerprint(req),
		hash:     hash,
		updates:  newUpdateHub(),
		initDone: make(chan struct{}),
	}
	s.loopIns[hash] = loopIn
	s.mu.Unlock()

	failInit := func(err error) (*swapserverrpc.ServerLoopInResponse, error) {
		loopIn.updates.finish(
			swapserverrpc.ServerSwapState_SERVER_FAILED_INITIALIZATION,
		)
		loopIn.completeInit(nil, err)
		return nil, err
	}

	amount, err := loopInAmount(req.GetAmt())
	if err != nil {
		return failInit(err)
	}
	if err := s.validateAmount(amount); err != nil {
		return failInit(err)
	}

	senderScriptPubKey, err := parseKey("sender key", req.GetSenderKey())
	if err != nil {
		return failInit(err)
	}
	senderInternalPubKey, err := parseKey(
		"sender internal pubkey", req.GetSenderInternalPubkey(),
	)
	if err != nil {
		return failInit(err)
	}

	var lastHop *route.Vertex
	if len(req.GetLastHop()) != 0 {
		if _, err := parseKey("last hop", req.GetLastHop()); err != nil {
			return failInit(err)
		}

		vertex, err := route.NewVertexFromBytes(req.GetLastHop())
		if err != nil {
			return failInit(status.Error(
				codes.InvalidArgument, err.Error(),
			))
		}
		lastHop = &vertex
	}

	invoiceAmount := amount - s.swapFee(amount)
	swapInvoice, err := s.validateInvoice(
		req.GetSwapInvoice(), hash, invoiceAmount,
	)
	if err != nil {
		return failInit(err)
	}

	probeHash := lntypes.Hash(sha256.Sum256(hash[:]))
	probeHash[0] ^= 1
	probeInvoice, err := s.validateInvoice(
		req.GetProbeInvoice(), probeHash, invoiceAmount,
	)
	if err != nil {
		return failInit(status.Errorf(
			codes.InvalidArgument, "invalid probe invoice: %v", err,
		))
	}
	if !bytes.Equal(
		swapInvoice.Destination.SerializeCompressed(),
		probeInvoice.Destination.SerializeCompressed(),
	) {

		return failInit(status.Error(
			codes.InvalidArgument,
			"swap and probe invoices have different destinations",
		))
	}

	receiverScriptKey, err := s.deriveKey(ctx, swap.KeyFamily)
	if err != nil {
		return failInit(status.Errorf(
			codes.Internal, "derive receiver script key: %v", err,
		))
	}
	receiverInternalKey, err := s.deriveKey(ctx, swap.KeyFamily)
	if err != nil {
		return failInit(status.Errorf(
			codes.Internal, "derive receiver internal key: %v", err,
		))
	}

	height, err := s.currentHeight(ctx)
	if err != nil {
		return failInit(status.Errorf(
			codes.Unavailable, "get block height: %v", err,
		))
	}
	expiry := height + s.cfg.LoopInCltvDelta

	copy(loopIn.senderScriptKey[:], senderScriptPubKey.SerializeCompressed())
	copy(loopIn.senderInternalKey[:], senderInternalPubKey.SerializeCompressed())
	loopIn.amount = amount
	loopIn.swapInvoice = req.GetSwapInvoice()
	loopIn.lastHop = lastHop
	loopIn.initiationHeight = height
	loopIn.expiry = expiry
	loopIn.receiverScriptKey = receiverScriptKey
	loopIn.receiverInternalKey = receiverInternalKey

	contract := &loopdb.SwapContract{
		AmountRequested: amount,
		HtlcKeys: loopdb.HtlcKeys{
			SenderScriptKey:        loopIn.senderScriptKey,
			SenderInternalPubKey:   loopIn.senderInternalKey,
			ReceiverScriptKey:      keyBytes(receiverScriptKey.pubKey),
			ReceiverInternalPubKey: keyBytes(receiverInternalKey.pubKey),
		},
		CltvExpiry:       expiry,
		InitiationHeight: height,
		ProtocolVersion:  loopdb.ProtocolVersionMuSig2,
	}
	loopIn.htlc, err = utils.GetHtlc(hash, contract, s.cfg.Lnd.ChainParams)
	if err != nil {
		return failInit(status.Errorf(
			codes.Internal, "construct Loop In HTLC: %v", err,
		))
	}

	// This is deliberately part of the unary RPC. The client has already
	// subscribed to the hold invoice and will cancel it after it becomes
	// accepted; waiting for the final failed payment is the acknowledgement.
	if err := s.probeLoopInInvoice(
		ctx, req.GetProbeInvoice(), lastHop,
	); err != nil {
		return failInit(status.Errorf(
			codes.FailedPrecondition, "probe payment failed: %v", err,
		))
	}

	response := &swapserverrpc.ServerLoopInResponse{
		ReceiverKey:            receiverScriptKey.pubKey.SerializeCompressed(),
		ReceiverInternalPubkey: receiverInternalKey.pubKey.SerializeCompressed(),
		Expiry:                 expiry,
		ServerMessage:          "regtest Loop In accepted; waiting for the HTLC",
	}

	loopIn.updates.publish(swapserverrpc.ServerSwapState_SERVER_INITIATED)
	s.goSwap(func(runCtx context.Context) {
		s.runLoopInSwap(runCtx, loopIn)
	})
	loopIn.completeInit(response, nil)

	return cloneLoopInResponse(response), nil
}

// probeLoopInInvoice performs the real hold-invoice/cancellation handshake.
func (s *Server) probeLoopInInvoice(ctx context.Context, invoice string,
	lastHop *route.Vertex) error {

	statusChan, errChan, err := s.cfg.Lnd.Router.SendPayment(
		ctx, lndclient.SendPaymentRequest{
			Invoice:       invoice,
			MaxFee:        s.cfg.MaxSwapAmount,
			Timeout:       s.cfg.PaymentTimeout,
			LastHopPubkey: lastHop,
			MaxParts:      10,
			Cancelable:    true,
		},
	)
	if err != nil {
		return err
	}

	accepted := false
	for statusChan != nil || errChan != nil {
		select {
		case payment, ok := <-statusChan:
			if !ok {
				statusChan = nil
				continue
			}

			switch payment.State {
			case lnrpc.Payment_IN_FLIGHT:
				accepted = true

			case lnrpc.Payment_FAILED:
				if !accepted {
					return fmt.Errorf(
						"probe failed before acceptance: %v",
						payment.FailureReason,
					)
				}

				// CancelInvoice at the receiving lnd fails the held
				// HTLC with FailIncorrectDetails. Other terminal
				// reasons (notably NO_ROUTE) do not prove that the
				// receiver accepted and canceled this probe.
				if payment.FailureReason !=
					lnrpc.PaymentFailureReason_FAILURE_REASON_INCORRECT_PAYMENT_DETAILS { //nolint:lll

					return fmt.Errorf(
						"probe ended with unexpected failure reason: %v",
						payment.FailureReason,
					)
				}

				return nil

			case lnrpc.Payment_SUCCEEDED:
				return errors.New("probe invoice unexpectedly settled")
			}

		case err, ok := <-errChan:
			if !ok {
				errChan = nil
				continue
			}
			if err != nil {
				return err
			}

		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return errors.New("probe payment stream closed before cancellation")
}

func (s *Server) runLoopInSwap(ctx context.Context, loopIn *loopInSwap) {
	confirmation, err := s.waitForLoopInHtlc(ctx, loopIn)
	if err != nil {
		s.failRunningLoopIn(
			ctx, loopIn,
			swapserverrpc.ServerSwapState_SERVER_UNEXPECTED_FAILURE,
			"wait for HTLC confirmation", err,
		)
		return
	}

	outpoint, htlcAmount, terminalState, err := confirmedLoopInOutput(
		confirmation, loopIn.htlc.PkScript, loopIn.amount,
	)
	if err != nil {
		s.failRunningLoopIn(ctx, loopIn, terminalState,
			"validate confirmed HTLC", err)
		return
	}

	loopIn.updates.publish(
		swapserverrpc.ServerSwapState_SERVER_HTLC_CONFIRMED,
	)

	paymentCtx, cancelPayment := context.WithTimeout(
		ctx, s.cfg.PaymentTimeout,
	)
	payment, err := s.payLoopInInvoice(
		paymentCtx, loopIn.swapInvoice, loopIn.lastHop,
	)
	cancelPayment()
	if err != nil {
		s.failRunningLoopIn(
			ctx, loopIn,
			swapserverrpc.ServerSwapState_SERVER_FAILED_OFF_CHAIN_TIMEOUT,
			"pay swap invoice", err,
		)
		return
	}
	if payment.Preimage.Hash() != loopIn.hash {
		s.failRunningLoopIn(
			ctx, loopIn,
			swapserverrpc.ServerSwapState_SERVER_UNEXPECTED_FAILURE,
			"pay swap invoice", errors.New("lnd returned the wrong preimage"),
		)
		return
	}

	sweepTx, err := s.createLoopInSuccessSweep(
		ctx, loopIn, *outpoint, htlcAmount, payment.Preimage,
	)
	if err != nil {
		s.failRunningLoopIn(
			ctx, loopIn,
			swapserverrpc.ServerSwapState_SERVER_UNEXPECTED_FAILURE,
			"create success sweep", err,
		)
		return
	}

	if err := s.publishAndConfirmLoopInSweep(ctx, loopIn, sweepTx); err != nil {
		s.failRunningLoopIn(
			ctx, loopIn,
			swapserverrpc.ServerSwapState_SERVER_UNEXPECTED_FAILURE,
			"publish success sweep", err,
		)
		return
	}

	loopIn.updates.finish(swapserverrpc.ServerSwapState_SERVER_SUCCESS)
}

func (s *Server) waitForLoopInHtlc(ctx context.Context,
	loopIn *loopInSwap) (*chainntnfs.TxConfirmation, error) {

	confChan, errChan, err := s.cfg.Lnd.ChainNotifier.RegisterConfirmationsNtfn(
		ctx, nil, loopIn.htlc.PkScript, 1, loopIn.initiationHeight,
	)
	if err != nil {
		return nil, err
	}

	for confChan != nil || errChan != nil {
		select {
		case confirmation, ok := <-confChan:
			if !ok {
				confChan = nil
				continue
			}
			if confirmation == nil || confirmation.Tx == nil {
				return nil, errors.New("empty HTLC confirmation")
			}

			return confirmation, nil

		case err, ok := <-errChan:
			if !ok {
				errChan = nil
				continue
			}
			if err != nil {
				return nil, err
			}

		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	return nil, errors.New("HTLC confirmation stream closed")
}

func confirmedLoopInOutput(confirmation *chainntnfs.TxConfirmation,
	pkScript []byte, expectedAmount btcutil.Amount) (*wire.OutPoint,
	btcutil.Amount, swapserverrpc.ServerSwapState, error) {

	var (
		outputIndex uint32
		outputValue btcutil.Amount
		matches     int
	)
	for index, output := range confirmation.Tx.TxOut {
		if !bytes.Equal(output.PkScript, pkScript) {
			continue
		}

		matches++
		outputIndex = uint32(index)
		outputValue = btcutil.Amount(output.Value)
	}

	switch {
	case matches == 0:
		return nil, 0,
			swapserverrpc.ServerSwapState_SERVER_UNEXPECTED_FAILURE,
			errors.New("confirmation does not contain the swap script")

	case matches > 1:
		return nil, 0,
			swapserverrpc.ServerSwapState_SERVER_FAILED_MULTIPLE_SWAP_SCRIPTS,
			fmt.Errorf("confirmed transaction contains %d swap outputs", matches)

	case outputValue != expectedAmount:
		return nil, 0,
			swapserverrpc.ServerSwapState_SERVER_FAILED_INVALID_HTLC_AMOUNT,
			fmt.Errorf("HTLC amount %d does not match %d", outputValue,
				expectedAmount)
	}

	return &wire.OutPoint{
		Hash:  confirmation.Tx.TxHash(),
		Index: outputIndex,
	}, outputValue, swapserverrpc.ServerSwapState_SERVER_HTLC_CONFIRMED, nil
}

func (s *Server) payLoopInInvoice(ctx context.Context, invoice string,
	lastHop *route.Vertex) (lndclient.PaymentStatus, error) {

	statusChan, errChan, err := s.cfg.Lnd.Router.SendPayment(
		ctx, lndclient.SendPaymentRequest{
			Invoice:       invoice,
			MaxFee:        s.cfg.MaxSwapAmount,
			Timeout:       s.cfg.PaymentTimeout,
			LastHopPubkey: lastHop,
			MaxParts:      10,
			Cancelable:    true,
		},
	)
	if err != nil {
		return lndclient.PaymentStatus{}, err
	}

	for statusChan != nil || errChan != nil {
		select {
		case payment, ok := <-statusChan:
			if !ok {
				statusChan = nil
				continue
			}

			switch payment.State {
			case lnrpc.Payment_SUCCEEDED:
				return payment, nil

			case lnrpc.Payment_FAILED:
				return payment, fmt.Errorf(
					"payment failed: %v", payment.FailureReason,
				)
			}

		case err, ok := <-errChan:
			if !ok {
				errChan = nil
				continue
			}
			if err != nil {
				return lndclient.PaymentStatus{}, err
			}

		case <-ctx.Done():
			return lndclient.PaymentStatus{}, ctx.Err()
		}
	}

	return lndclient.PaymentStatus{}, errors.New("payment stream closed")
}

func (s *Server) createLoopInSuccessSweep(ctx context.Context,
	loopIn *loopInSwap, outpoint wire.OutPoint, amount btcutil.Amount,
	preimage lntypes.Preimage) (*wire.MsgTx, error) {

	destination, err := s.cfg.Lnd.WalletKit.NextAddr(
		ctx, "", walletrpc.AddressType_WITNESS_PUBKEY_HASH, false,
	)
	if err != nil {
		return nil, err
	}

	sweeper := sweep.Sweeper{Lnd: s.cfg.Lnd}
	fee, err := sweeper.GetSweepFee(
		ctx, loopIn.htlc.AddSuccessToEstimator, destination,
		loopInSweepConfTarget,
		fmt.Sprintf("regtest-loop-in-%s", swap.ShortHash(&loopIn.hash)),
	)
	if err != nil {
		return nil, err
	}
	if fee >= amount {
		return nil, fmt.Errorf("success sweep fee %d exceeds HTLC amount %d",
			fee, amount)
	}

	height, err := s.currentHeight(ctx)
	if err != nil {
		return nil, err
	}
	witness := func(signature []byte) (wire.TxWitness, error) {
		return loopIn.htlc.GenSuccessWitness(signature, preimage)
	}

	return sweeper.CreateSweepTx(
		ctx, height, loopIn.htlc.SuccessSequence(), loopIn.htlc,
		outpoint, keyBytes(loopIn.receiverScriptKey.pubKey),
		loopIn.htlc.SuccessScript(), witness, amount, fee, destination,
	)
}

func (s *Server) publishAndConfirmLoopInSweep(ctx context.Context,
	loopIn *loopInSwap, sweepTx *wire.MsgTx) error {

	if len(sweepTx.TxOut) != 1 {
		return fmt.Errorf("success sweep has %d outputs", len(sweepTx.TxOut))
	}

	txid := sweepTx.TxHash()
	height, err := s.currentHeight(ctx)
	if err != nil {
		return err
	}
	confChan, errChan, err := s.cfg.Lnd.ChainNotifier.RegisterConfirmationsNtfn(
		ctx, cloneHash(txid), sweepTx.TxOut[0].PkScript, 1, height,
	)
	if err != nil {
		return err
	}

	label := fmt.Sprintf(
		"loopserver-regtest -- InSweepSuccess(swap=%s)",
		swap.ShortHash(&loopIn.hash),
	)
	if err := s.cfg.Lnd.WalletKit.PublishTransaction(
		ctx, sweepTx, label,
	); err != nil {
		return err
	}

	for confChan != nil || errChan != nil {
		select {
		case confirmation, ok := <-confChan:
			if !ok {
				confChan = nil
				continue
			}
			if confirmation == nil || confirmation.Tx == nil {
				return errors.New("empty sweep confirmation")
			}
			if confirmation.Tx.TxHash() != txid {
				return fmt.Errorf("confirmed unexpected sweep transaction %v",
					confirmation.Tx.TxHash())
			}

			return nil

		case err, ok := <-errChan:
			if !ok {
				errChan = nil
				continue
			}
			if err != nil {
				return err
			}

		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return errors.New("sweep confirmation stream closed")
}

func (s *Server) failRunningLoopIn(ctx context.Context, loopIn *loopInSwap,
	state swapserverrpc.ServerSwapState, action string, err error) {

	if ctx.Err() != nil {
		return
	}

	s.cfg.Logger.Printf("Loop In %s: %s: %v", loopIn.hash, action, err)
	loopIn.updates.finish(state)
}

// SubscribeLoopInUpdates replays all state history before forwarding live
// updates, so a client can reconnect without losing transitions.
func (s *Server) SubscribeLoopInUpdates(
	req *swapserverrpc.SubscribeUpdatesRequest,
	stream swapserverrpc.SwapServer_SubscribeLoopInUpdatesServer) error {

	if err := validateLoopInProtocol(req.GetProtocolVersion()); err != nil {
		return err
	}
	hash, err := parseHash(req.GetSwapHash())
	if err != nil {
		return err
	}

	s.mu.RLock()
	loopIn, ok := s.loopIns[hash]
	s.mu.RUnlock()
	if !ok {
		return status.Error(codes.NotFound, "Loop In swap not found")
	}

	subscription := loopIn.updates.subscribe()
	defer subscription.cancel()

	send := func(update serverUpdate) error {
		return stream.Send(&swapserverrpc.SubscribeLoopInUpdatesResponse{
			TimestampNs: update.timestamp.UnixNano(),
			State:       update.state,
		})
	}

	for _, update := range subscription.history {
		if err := send(update); err != nil {
			return err
		}
	}
	if subscription.done {
		return nil
	}

	for {
		select {
		case update, ok := <-subscription.updates:
			if !ok {
				return nil
			}
			if err := send(update); err != nil {
				return err
			}

		case <-s.ctx.Done():
			return s.ctx.Err()

		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
}

// PushKey acknowledges the protocol-11 internal-key reveal. The unilateral
// success path does not require the key, but validating it keeps the fake
// server faithful and allows repeated acknowledgements safely.
func (s *Server) PushKey(ctx context.Context,
	req *swapserverrpc.ServerPushKeyReq) (*swapserverrpc.ServerPushKeyRes,
	error) {

	if err := validateLoopInProtocol(req.GetProtocolVersion()); err != nil {
		return nil, err
	}
	hash, err := parseHash(req.GetSwapHash())
	if err != nil {
		return nil, err
	}
	if len(req.GetInternalPrivkey()) != 32 {
		return nil, status.Error(
			codes.InvalidArgument, "internal private key must be 32 bytes",
		)
	}

	s.mu.RLock()
	loopIn, ok := s.loopIns[hash]
	s.mu.RUnlock()
	if !ok {
		return nil, status.Error(codes.NotFound, "Loop In swap not found")
	}
	if _, err := loopIn.waitForInit(ctx); err != nil {
		return nil, status.Error(
			codes.FailedPrecondition, "Loop In swap initialization failed",
		)
	}

	_, publicKey := btcec.PrivKeyFromBytes(req.GetInternalPrivkey())
	if !bytes.Equal(
		publicKey.SerializeCompressed(), loopIn.senderInternalKey[:],
	) {

		return nil, status.Error(
			codes.InvalidArgument,
			"internal private key does not match the swap public key",
		)
	}

	loopIn.mu.Lock()
	loopIn.clientInternalKeyPushed = true
	loopIn.mu.Unlock()

	return &swapserverrpc.ServerPushKeyRes{}, nil
}
