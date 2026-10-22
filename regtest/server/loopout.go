package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/input"
	invpkg "github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// loopOutInvoiceCltvDelta is the safety delta between the on-chain HTLC
	// expiry and the final-hop CLTV used by the two hold invoices.
	loopOutInvoiceCltvDelta = int32(50)

	// loopOutFundingConfTarget is deliberately conservative enough to be
	// accepted by lnd's fee estimator while still confirming quickly on
	// regtest once a block is mined.
	loopOutFundingConfTarget = int32(6)

	loopOutInvoiceExpiry = int64((365 * 24 * time.Hour) / time.Second)
)

type loopOutSwap struct {
	mu sync.Mutex

	hash                lntypes.Hash
	amount              btcutil.Amount
	expiry              int32
	initiationHeight    int32
	publicationDeadline time.Time

	senderKey     [33]byte
	receiverKey   [33]byte
	senderLocator keychain.KeyLocator

	prepayPreimage lntypes.Preimage
	prepayHash     lntypes.Hash

	swapInvoice   string
	prepayInvoice string
	paymentAddr   [32]byte

	htlc *swap.Htlc

	state    swapserverrpc.ServerSwapState
	updates  *updateHub
	ctx      context.Context
	cancel   context.CancelFunc
	terminal bool
	canceled bool

	cancelRequested bool
	cancelState     swapserverrpc.ServerSwapState
	mainCancelAck   bool
	prepayCancelAck bool

	fundingStarted  bool
	fundingTx       *wire.MsgTx
	fundingOutpoint *wire.OutPoint
	confirmed       bool
	prepaySettled   bool
	mainSettled     bool
}

func validateLoopOutProtocol(version swapserverrpc.ProtocolVersion) error {
	if version != swapserverrpc.ProtocolVersion_MUSIG2 {
		return status.Errorf(
			codes.InvalidArgument, "protocol version %d unsupported; want %d",
			version, swapserverrpc.ProtocolVersion_MUSIG2,
		)
	}

	return nil
}

func (s *Server) LoopOutTerms(_ context.Context,
	req *swapserverrpc.ServerLoopOutTermsRequest) (
	*swapserverrpc.ServerLoopOutTerms, error) {

	if err := validateLoopOutProtocol(req.ProtocolVersion); err != nil {
		return nil, err
	}

	return &swapserverrpc.ServerLoopOutTerms{
		MinSwapAmount: uint64(s.cfg.MinSwapAmount),
		MaxSwapAmount: uint64(s.cfg.MaxSwapAmount),
		MinCltvDelta:  s.cfg.LoopOutMinCltvDelta,
		MaxCltvDelta:  s.cfg.LoopOutMaxCltvDelta,
	}, nil
}

func (s *Server) LoopOutQuote(ctx context.Context,
	req *swapserverrpc.ServerLoopOutQuoteRequest) (
	*swapserverrpc.ServerLoopOutQuote, error) {

	if err := validateLoopOutProtocol(req.ProtocolVersion); err != nil {
		return nil, err
	}

	amount, err := s.loopOutAmount(req.Amt, true)
	if err != nil {
		return nil, err
	}

	height, err := s.currentHeight(ctx)
	if err != nil {
		return nil, status.Errorf(
			codes.Unavailable, "get current height: %v", err,
		)
	}
	if err := s.validateLoopOutExpiry(height, req.Expiry); err != nil {
		return nil, err
	}

	dest := s.cfg.Lnd.NodePubkey
	if dest == ([33]byte{}) {
		info, err := s.cfg.Lnd.Client.GetInfo(ctx)
		if err != nil {
			return nil, status.Errorf(
				codes.Unavailable, "get server identity: %v", err,
			)
		}
		dest = info.IdentityPubkey
	}

	return &swapserverrpc.ServerLoopOutQuote{
		SwapPaymentDest: hex.EncodeToString(dest[:]),
		SwapFee:         int64(s.swapFee(amount)),
		PrepayAmt:       uint64(s.cfg.PrepaySat),
		MinSwapAmount:   uint64(s.cfg.MinSwapAmount),
		MaxSwapAmount:   uint64(s.cfg.MaxSwapAmount),
		CltvDelta:       req.Expiry - height,
	}, nil
}

func (s *Server) loopOutAmount(rpcAmount uint64,
	allowZero bool) (btcutil.Amount, error) {

	if rpcAmount == 0 && allowZero {
		return s.cfg.MaxSwapAmount, nil
	}
	if rpcAmount > math.MaxInt64 {
		return 0, status.Error(codes.InvalidArgument, "amount overflows int64")
	}

	amount := btcutil.Amount(rpcAmount)
	if err := s.validateAmount(amount); err != nil {
		return 0, err
	}
	if s.cfg.PrepaySat < 0 || s.cfg.PrepaySat > amount+s.swapFee(amount) {
		return 0, status.Error(
			codes.InvalidArgument, "invalid configured prepay amount",
		)
	}

	return amount, nil
}

func (s *Server) validateLoopOutExpiry(height, expiry int32) error {
	delta := int64(expiry) - int64(height)
	if delta < int64(s.cfg.LoopOutMinCltvDelta) ||
		delta > int64(s.cfg.LoopOutMaxCltvDelta) {

		return status.Errorf(
			codes.OutOfRange, "CLTV delta %d outside range [%d,%d]",
			delta, s.cfg.LoopOutMinCltvDelta,
			s.cfg.LoopOutMaxCltvDelta,
		)
	}

	return nil
}

func (s *Server) NewLoopOutSwap(ctx context.Context,
	req *swapserverrpc.ServerLoopOutRequest) (
	*swapserverrpc.ServerLoopOutResponse, error) {

	requestReceived := time.Now()
	hash, err := parseHash(req.SwapHash)
	if err != nil {
		return nil, err
	}

	// Serialize creation for this deliberately small server. Apart from
	// making duplicate requests idempotent, this prevents two concurrent
	// requests for the same hash from creating distinct invoice/key pairs.
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.loopOuts[hash]; ok {
		if err := existing.matchesRequest(req); err != nil {
			return nil, err
		}

		return existing.response(), nil
	}

	// The client's payment recovery request intentionally contains only the
	// hash. It can only succeed if the swap already exists.
	if req.UserAgent == "resume_swap" && req.Amt == 0 &&
		len(req.ReceiverKey) == 0 {

		return nil, status.Error(codes.NotFound, "swap not found")
	}

	if err := validateLoopOutProtocol(req.ProtocolVersion); err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(req.UserAgent)) > math.MaxUint8 {
		return nil, status.Error(
			codes.InvalidArgument, "user agent exceeds 255 bytes",
		)
	}

	amount, err := s.loopOutAmount(req.Amt, false)
	if err != nil {
		return nil, err
	}
	receiverPub, err := parseKey("receiver key", req.ReceiverKey)
	if err != nil {
		return nil, err
	}
	receiverKey := keyBytes(receiverPub)

	height, err := s.currentHeight(ctx)
	if err != nil {
		return nil, status.Errorf(
			codes.Unavailable, "get current height: %v", err,
		)
	}
	if err := s.validateLoopOutExpiry(height, req.Expiry); err != nil {
		return nil, err
	}

	serverKey, err := s.deriveKey(ctx, swap.KeyFamily)
	if err != nil {
		return nil, status.Errorf(
			codes.Unavailable, "derive sender key: %v", err,
		)
	}
	senderKey := keyBytes(serverKey.pubKey)

	htlc, err := swap.NewHtlcV3(
		input.MuSig2Version100RC2, req.Expiry, senderKey, receiverKey,
		senderKey, receiverKey, hash, s.cfg.Lnd.ChainParams,
	)
	if err != nil {
		return nil, status.Errorf(
			codes.InvalidArgument, "construct HTLC: %v", err,
		)
	}

	var prepayPreimage lntypes.Preimage
	if _, err := rand.Read(prepayPreimage[:]); err != nil {
		return nil, status.Errorf(
			codes.Internal, "generate prepay preimage: %v", err,
		)
	}
	prepayHash := prepayPreimage.Hash()

	fee := s.swapFee(amount)
	mainAmount := amount + fee - s.cfg.PrepaySat
	invoiceCltv := int64(req.Expiry) - int64(height) +
		int64(loopOutInvoiceCltvDelta)
	if invoiceCltv <= 0 {
		return nil, status.Error(
			codes.InvalidArgument, "invalid invoice CLTV delta",
		)
	}

	mainInvoice, err := s.cfg.Lnd.Invoices.AddHoldInvoice(
		ctx, &invoicesrpc.AddInvoiceData{
			Hash:       &hash,
			Value:      lnwire.NewMSatFromSatoshis(mainAmount),
			CltvExpiry: uint64(invoiceCltv),
			Memo:       fmt.Sprintf("loop out - script: %x", htlc.PkScript),
			Expiry:     loopOutInvoiceExpiry,
			Private:    true,
		},
	)
	if err != nil {
		return nil, status.Errorf(
			codes.Unavailable, "create swap hold invoice: %v", err,
		)
	}

	prepayInvoice, err := s.cfg.Lnd.Invoices.AddHoldInvoice(
		ctx, &invoicesrpc.AddInvoiceData{
			Hash:       &prepayHash,
			Value:      lnwire.NewMSatFromSatoshis(s.cfg.PrepaySat),
			CltvExpiry: uint64(invoiceCltv),
			Memo:       "loop out prepay",
			Expiry:     loopOutInvoiceExpiry,
			Private:    true,
		},
	)
	if err != nil {
		_ = s.cfg.Lnd.Invoices.CancelInvoice(ctx, hash)
		return nil, status.Errorf(
			codes.Unavailable, "create prepay hold invoice: %v", err,
		)
	}

	paymentAddr, err := loopOutPaymentAddress(
		mainInvoice, s.cfg.Lnd.ChainParams,
	)
	if err != nil {
		_ = s.cfg.Lnd.Invoices.CancelInvoice(ctx, hash)
		_ = s.cfg.Lnd.Invoices.CancelInvoice(ctx, prepayHash)
		return nil, status.Errorf(
			codes.Internal, "decode swap payment address: %v", err,
		)
	}

	publicationDeadline := loopOutPublicationDeadline(
		req.SwapPublicationDeadline, requestReceived,
	)
	swapCtx, cancel := context.WithCancel(s.ctx)
	loopOut := &loopOutSwap{
		hash:                hash,
		amount:              amount,
		expiry:              req.Expiry,
		initiationHeight:    height,
		publicationDeadline: publicationDeadline,
		senderKey:           senderKey,
		receiverKey:         receiverKey,
		senderLocator:       serverKey.locator,
		prepayPreimage:      prepayPreimage,
		prepayHash:          prepayHash,
		swapInvoice:         mainInvoice,
		prepayInvoice:       prepayInvoice,
		paymentAddr:         paymentAddr,
		htlc:                htlc,
		state:               swapserverrpc.ServerSwapState_SERVER_INITIATED,
		updates:             newUpdateHub(),
		ctx:                 swapCtx,
		cancel:              cancel,
	}
	loopOut.updates.publish(
		swapserverrpc.ServerSwapState_SERVER_INITIATED,
	)
	s.loopOuts[hash] = loopOut

	response := loopOut.response()
	s.goSwap(func(context.Context) {
		s.runLoopOut(loopOut)
	})

	return response, nil
}

// loopOutPublicationDeadline converts the client's absolute publication
// deadline into an enforced deadline. A deadline at or before request receipt
// means "fast": both the CLI (now) and autolooper (zero time) use such values
// to request immediate publication once both hold invoices are accepted.
func loopOutPublicationDeadline(unixSeconds int64,
	requestReceived time.Time) time.Time {

	deadline := time.Unix(unixSeconds, 0)
	if !deadline.After(requestReceived) {
		return time.Time{}
	}

	return deadline
}

func loopOutPaymentAddress(invoice string,
	chainParams *chaincfg.Params) ([32]byte, error) {

	decoded, err := zpay32.Decode(invoice, chainParams)
	if err != nil {
		return [32]byte{}, err
	}
	paymentAddr, err := decoded.PaymentAddr.UnwrapOrErr(
		errors.New("invoice has no payment address"),
	)
	if err != nil {
		return [32]byte{}, err
	}

	return paymentAddr, nil
}

func (o *loopOutSwap) matchesRequest(
	req *swapserverrpc.ServerLoopOutRequest) error {

	// A payment recovery request deliberately only carries the hash.
	if req.UserAgent == "resume_swap" && req.Amt == 0 &&
		len(req.ReceiverKey) == 0 {

		return nil
	}

	if req.ProtocolVersion != swapserverrpc.ProtocolVersion_MUSIG2 ||
		req.Amt != uint64(o.amount) || req.Expiry != o.expiry ||
		!bytes.Equal(req.ReceiverKey, o.receiverKey[:]) {

		return status.Error(
			codes.AlreadyExists, "swap hash already has a different contract",
		)
	}

	return nil
}

func (o *loopOutSwap) response() *swapserverrpc.ServerLoopOutResponse {
	return &swapserverrpc.ServerLoopOutResponse{
		SwapInvoice:   o.swapInvoice,
		PrepayInvoice: o.prepayInvoice,
		SenderKey:     bytes.Clone(o.senderKey[:]),
		Expiry:        o.expiry,
	}
}

func (s *Server) runLoopOut(loopOut *loopOutSwap) {
	ctx := loopOut.ctx
	publicationCtx, cancelPublication := context.WithCancel(ctx)
	if !loopOut.publicationDeadline.IsZero() {
		publicationCtx, cancelPublication = context.WithDeadline(
			ctx, loopOut.publicationDeadline,
		)
	}
	defer cancelPublication()

	mainUpdates, mainErrors, err := s.cfg.Lnd.Invoices.SubscribeSingleInvoice(
		publicationCtx, loopOut.hash,
	)
	if err != nil {
		s.failLoopOutBeforeFunding(
			loopOut,
			swapserverrpc.ServerSwapState_SERVER_FAILED_INITIALIZATION,
			fmt.Errorf("subscribe swap invoice: %w", err),
		)
		return
	}
	prepayUpdates, prepayErrors, err :=
		s.cfg.Lnd.Invoices.SubscribeSingleInvoice(
			publicationCtx, loopOut.prepayHash,
		)
	if err != nil {
		s.failLoopOutBeforeFunding(
			loopOut,
			swapserverrpc.ServerSwapState_SERVER_FAILED_INITIALIZATION,
			fmt.Errorf("subscribe prepay invoice: %w", err),
		)
		return
	}

	if err := waitForLoopOutInvoices(
		publicationCtx, mainUpdates, mainErrors, prepayUpdates,
		prepayErrors,
	); err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			s.failLoopOutBeforeFunding(
				loopOut,
				swapserverrpc.ServerSwapState_SERVER_FAILED_HTLC_PUBLICATION,
				fmt.Errorf("publication deadline while waiting for invoices: %w",
					err),
			)

		case !errors.Is(err, context.Canceled):
			s.failLoopOutBeforeFunding(
				loopOut,
				swapserverrpc.ServerSwapState_SERVER_FAILED_OFF_CHAIN_TIMEOUT,
				err,
			)
		}
		return
	}
	if err := publicationCtx.Err(); err != nil {
		s.failLoopOutBeforeFunding(
			loopOut,
			swapserverrpc.ServerSwapState_SERVER_FAILED_HTLC_PUBLICATION,
			fmt.Errorf("publication deadline before funding: %w", err),
		)
		return
	}

	loopOut.mu.Lock()
	if loopOut.terminal || loopOut.canceled || loopOut.cancelRequested {
		loopOut.mu.Unlock()
		return
	}
	loopOut.fundingStarted = true
	loopOut.mu.Unlock()

	feeRate, err := s.cfg.Lnd.WalletKit.EstimateFeeRate(
		publicationCtx, loopOutFundingConfTarget,
	)
	if err != nil {
		s.failLoopOutBeforeBroadcast(loopOut, fmt.Errorf(
			"estimate HTLC fee: %w", err,
		))
		return
	}
	if err := publicationCtx.Err(); err != nil {
		s.failLoopOutBeforeBroadcast(loopOut, fmt.Errorf(
			"publication deadline before broadcast: %w", err,
		))
		return
	}

	fundingTx, err := s.cfg.Lnd.WalletKit.SendOutputs(
		publicationCtx, []*wire.TxOut{{
			Value:    int64(loopOut.amount),
			PkScript: bytes.Clone(loopOut.htlc.PkScript),
		}}, feeRate, "loop-out-regtest-htlc",
	)
	cancelPublication()

	var confirmationErr error
	if err != nil || fundingTx == nil {
		if err == nil {
			err = errors.New("wallet returned no funding transaction")
		}
		s.cfg.Logger.Printf(
			"Loop Out %x broadcast result is ambiguous: %v; "+
				"reconciling by HTLC script", loopOut.hash[:6], err,
		)
		confirmationErr = s.waitForAmbiguousLoopOutConfirmation(loopOut)
	} else {
		outpoint, value, outputErr := swap.GetScriptOutput(
			fundingTx, loopOut.htlc.PkScript,
		)
		if outputErr != nil || value != loopOut.amount {
			s.cfg.Logger.Printf(
				"Loop Out %x wallet returned an ambiguous HTLC "+
					"transaction: outpoint=%v, value=%v, err=%v; "+
					"reconciling by script", loopOut.hash[:6],
				outpoint, value, outputErr,
			)
			confirmationErr =
				s.waitForAmbiguousLoopOutConfirmation(loopOut)
		} else {
			s.recordPublishedLoopOut(loopOut, fundingTx, *outpoint)
			confirmationErr = s.waitForLoopOutConfirmation(loopOut)
		}
	}

	if confirmationErr != nil {
		if !errors.Is(confirmationErr, context.Canceled) {
			s.failLoopOutAfterPublication(loopOut, confirmationErr)
		}
		return
	}

	if err := s.settleLoopOutPrepay(loopOut); err != nil {
		s.failLoopOutAfterPublication(loopOut, err)
		return
	}

	// LoopOutPushPreimage is a best-effort optimization. Also watch the exact
	// HTLC outpoint so a valid unilateral success spend can recover the
	// preimage and settle the main hold invoice if that RPC is lost.
	if err := s.waitForLoopOutSuccessSpend(loopOut); err != nil &&
		!errors.Is(err, context.Canceled) {

		s.failLoopOutAfterPublication(loopOut, err)
	}
}

func waitForLoopOutInvoices(ctx context.Context,
	mainUpdates <-chan lndclient.InvoiceUpdate, mainErrors <-chan error,
	prepayUpdates <-chan lndclient.InvoiceUpdate,
	prepayErrors <-chan error) error {

	var mainAccepted, prepayAccepted bool
	for !mainAccepted || !prepayAccepted {
		select {
		case update, ok := <-mainUpdates:
			if !ok {
				mainUpdates = nil
				if !mainAccepted {
					return errors.New("swap invoice subscription closed")
				}
				continue
			}
			switch update.State {
			case invpkg.ContractAccepted:
				mainAccepted = true
			case invpkg.ContractCanceled:
				return errors.New("swap invoice canceled")
			case invpkg.ContractSettled:
				return errors.New("swap invoice settled before HTLC publication")
			}

		case update, ok := <-prepayUpdates:
			if !ok {
				prepayUpdates = nil
				if !prepayAccepted {
					return errors.New("prepay invoice subscription closed")
				}
				continue
			}
			switch update.State {
			case invpkg.ContractAccepted:
				prepayAccepted = true
			case invpkg.ContractCanceled:
				return errors.New("prepay invoice canceled")
			case invpkg.ContractSettled:
				return errors.New("prepay invoice settled before HTLC publication")
			}

		case err, ok := <-mainErrors:
			if !ok {
				mainErrors = nil
				continue
			}
			if err != nil {
				return fmt.Errorf("swap invoice subscription: %w", err)
			}

		case err, ok := <-prepayErrors:
			if !ok {
				prepayErrors = nil
				continue
			}
			if err != nil {
				return fmt.Errorf("prepay invoice subscription: %w", err)
			}

		case <-ctx.Done():
			return ctx.Err()
		}

		if mainUpdates == nil && mainErrors == nil && !mainAccepted {
			return errors.New("swap invoice subscription ended")
		}
		if prepayUpdates == nil && prepayErrors == nil && !prepayAccepted {
			return errors.New("prepay invoice subscription ended")
		}
	}

	return nil
}

func (s *Server) recordPublishedLoopOut(loopOut *loopOutSwap,
	fundingTx *wire.MsgTx, outpoint wire.OutPoint) bool {

	loopOut.mu.Lock()
	if loopOut.terminal {
		loopOut.mu.Unlock()
		return false
	}
	loopOut.fundingTx = fundingTx.Copy()
	copyOutpoint := outpoint
	loopOut.fundingOutpoint = &copyOutpoint
	loopOut.state = swapserverrpc.ServerSwapState_SERVER_HTLC_PUBLISHED
	loopOut.mu.Unlock()
	loopOut.updates.publish(
		swapserverrpc.ServerSwapState_SERVER_HTLC_PUBLISHED,
	)

	return true
}

func (s *Server) waitForLoopOutConfirmation(loopOut *loopOutSwap) error {
	loopOut.mu.Lock()
	if loopOut.fundingTx == nil || loopOut.fundingOutpoint == nil {
		loopOut.mu.Unlock()
		return errors.New("published HTLC transaction is missing")
	}
	fundingTx := loopOut.fundingTx.Copy()
	outpoint := *loopOut.fundingOutpoint
	loopOut.mu.Unlock()

	txid := fundingTx.TxHash()
	return s.waitForLoopOutConfirmationMatch(
		loopOut, &txid, &outpoint, false,
	)
}

// waitForAmbiguousLoopOutConfirmation reconciles a SendOutputs call whose
// result cannot prove whether lnd broadcast the transaction. The invoice
// holds remain intact while a script-only notification discovers the exact
// HTLC. This avoids canceling accepted payments after a possibly successful
// publication.
func (s *Server) waitForAmbiguousLoopOutConfirmation(
	loopOut *loopOutSwap) error {

	return s.waitForLoopOutConfirmationMatch(loopOut, nil, nil, true)
}

func (s *Server) waitForLoopOutConfirmationMatch(loopOut *loopOutSwap,
	expectedTxID *chainhash.Hash, expectedOutpoint *wire.OutPoint,
	recordPublication bool) error {

	confirmations, confirmationErrors, err :=
		s.cfg.Lnd.ChainNotifier.RegisterConfirmationsNtfn(
			loopOut.ctx, expectedTxID, loopOut.htlc.PkScript, 1,
			loopOut.initiationHeight,
		)
	if err != nil {
		return fmt.Errorf("register HTLC confirmation: %w", err)
	}

	select {
	case confirmation, ok := <-confirmations:
		if !ok || confirmation == nil || confirmation.Tx == nil {
			return errors.New("HTLC confirmation stream closed")
		}
		if expectedTxID != nil &&
			confirmation.Tx.TxHash() != *expectedTxID {

			return errors.New("confirmed HTLC transaction hash mismatch")
		}
		confirmedOutpoint, value, err := swap.GetScriptOutput(
			confirmation.Tx, loopOut.htlc.PkScript,
		)
		if err != nil || value != loopOut.amount ||
			(expectedOutpoint != nil &&
				*confirmedOutpoint != *expectedOutpoint) {

			return fmt.Errorf(
				"invalid confirmed HTLC: outpoint=%v, value=%v, err=%v",
				confirmedOutpoint, value, err,
			)
		}
		if recordPublication && !s.recordPublishedLoopOut(
			loopOut, confirmation.Tx, *confirmedOutpoint,
		) {

			return context.Canceled
		}

		loopOut.mu.Lock()
		if loopOut.terminal {
			loopOut.mu.Unlock()
			return context.Canceled
		}
		loopOut.confirmed = true
		loopOut.state = swapserverrpc.ServerSwapState_SERVER_HTLC_CONFIRMED
		loopOut.mu.Unlock()
		loopOut.updates.publish(
			swapserverrpc.ServerSwapState_SERVER_HTLC_CONFIRMED,
		)

		return nil

	case err, ok := <-confirmationErrors:
		if !ok || err == nil {
			return errors.New("HTLC confirmation error stream closed")
		}
		return fmt.Errorf("HTLC confirmation: %w", err)

	case <-loopOut.ctx.Done():
		return loopOut.ctx.Err()
	}
}

func (s *Server) settleLoopOutPrepay(loopOut *loopOutSwap) error {
	loopOut.mu.Lock()
	defer loopOut.mu.Unlock()

	if loopOut.terminal || loopOut.canceled {
		return context.Canceled
	}
	if !loopOut.confirmed {
		return errors.New("cannot settle prepay before HTLC confirmation")
	}
	if loopOut.prepaySettled {
		return nil
	}

	err := s.cfg.Lnd.Invoices.SettleInvoice(
		loopOut.ctx, loopOut.prepayPreimage,
	)
	if err != nil && !invoiceAlreadySettled(err) {
		return fmt.Errorf("settle prepay invoice: %w", err)
	}
	loopOut.prepaySettled = true

	return nil
}

func (s *Server) waitForLoopOutSuccessSpend(loopOut *loopOutSwap) error {
	loopOut.mu.Lock()
	if loopOut.fundingOutpoint == nil {
		loopOut.mu.Unlock()
		return errors.New("confirmed HTLC outpoint is missing")
	}
	outpoint := *loopOut.fundingOutpoint
	loopOut.mu.Unlock()

	spends, spendErrors, err := s.cfg.Lnd.ChainNotifier.RegisterSpendNtfn(
		loopOut.ctx, &outpoint, loopOut.htlc.PkScript,
		loopOut.initiationHeight,
	)
	if err != nil {
		return fmt.Errorf("register HTLC spend: %w", err)
	}

	select {
	case spend, ok := <-spends:
		if !ok || spend == nil {
			return errors.New("HTLC spend stream closed")
		}
		preimage, err := s.validateLoopOutSuccessSpend(loopOut, spend)
		if err != nil {
			return fmt.Errorf("invalid HTLC success spend: %w", err)
		}

		// A settlement RPC can fail after lnd has committed the invoice
		// state. Retry the idempotent completion while the swap is active so
		// the one-shot spend notification is not lost to a transient error.
		for {
			err := s.completeLoopOut(
				loopOut.ctx, loopOut, preimage,
			)
			if err == nil {
				return nil
			}
			if status.Code(err) != codes.Unavailable {
				return err
			}

			timer := time.NewTimer(100 * time.Millisecond)
			select {
			case <-timer.C:
			case <-loopOut.ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return loopOut.ctx.Err()
			}
		}

	case err, ok := <-spendErrors:
		if !ok || err == nil {
			return errors.New("HTLC spend error stream closed")
		}
		return fmt.Errorf("HTLC spend: %w", err)

	case <-loopOut.ctx.Done():
		return loopOut.ctx.Err()
	}
}

// validateLoopOutSuccessSpend returns a preimage only after validating the
// exact notified outpoint, the tapscript success path and the complete Bitcoin
// script execution. This deliberately rejects key spends and timeout spends,
// neither of which reveal a preimage.
func (s *Server) validateLoopOutSuccessSpend(loopOut *loopOutSwap,
	spend *chainntnfs.SpendDetail) (lntypes.Preimage, error) {

	loopOut.mu.Lock()
	if loopOut.fundingOutpoint == nil {
		loopOut.mu.Unlock()
		return lntypes.Preimage{}, errors.New("funding outpoint missing")
	}
	fundingOutpoint := *loopOut.fundingOutpoint
	htlcValue := loopOut.amount
	htlcPkScript := bytes.Clone(loopOut.htlc.PkScript)
	successScript := bytes.Clone(loopOut.htlc.SuccessScript())
	swapHash := loopOut.hash
	loopOut.mu.Unlock()

	if spend.SpentOutPoint == nil ||
		*spend.SpentOutPoint != fundingOutpoint {

		return lntypes.Preimage{}, errors.New("spent outpoint mismatch")
	}
	if spend.SpendingTx == nil {
		return lntypes.Preimage{}, errors.New("spending transaction missing")
	}
	spendingTx := spend.SpendingTx
	if spend.SpenderTxHash != nil &&
		spendingTx.TxHash() != *spend.SpenderTxHash {

		return lntypes.Preimage{}, errors.New("spender transaction hash mismatch")
	}
	inputIndex := int(spend.SpenderInputIndex)
	if inputIndex < 0 || inputIndex >= len(spendingTx.TxIn) {
		return lntypes.Preimage{}, errors.New("spender input index out of range")
	}
	txIn := spendingTx.TxIn[inputIndex]
	if txIn.PreviousOutPoint != fundingOutpoint {
		return lntypes.Preimage{}, errors.New("spender input outpoint mismatch")
	}
	witness := txIn.Witness
	if len(witness) != 4 {
		return lntypes.Preimage{}, fmt.Errorf(
			"success witness has %d elements", len(witness),
		)
	}
	if !bytes.Equal(witness[2], successScript) {
		return lntypes.Preimage{}, errors.New("success tapscript mismatch")
	}

	preimage, err := lntypes.MakePreimage(witness[0])
	if err != nil || preimage.Hash() != swapHash {
		return lntypes.Preimage{}, errors.New("success preimage mismatch")
	}

	witnessVersion, witnessProgram, err :=
		txscript.ExtractWitnessProgramInfo(htlcPkScript)
	if err != nil || witnessVersion != 1 || len(witnessProgram) != 32 {
		return lntypes.Preimage{}, errors.New("invalid HTLC taproot output")
	}
	controlBlock, err := txscript.ParseControlBlock(witness[3])
	if err != nil {
		return lntypes.Preimage{}, fmt.Errorf("parse control block: %w", err)
	}
	if err := txscript.VerifyTaprootLeafCommitment(
		controlBlock, witnessProgram, witness[2],
	); err != nil {
		return lntypes.Preimage{}, fmt.Errorf(
			"verify success tapleaf commitment: %w", err,
		)
	}

	prevOuts := make(map[wire.OutPoint]*wire.TxOut, len(spendingTx.TxIn))
	for index, input := range spendingTx.TxIn {
		outpoint := input.PreviousOutPoint
		if _, duplicate := prevOuts[outpoint]; duplicate {
			return lntypes.Preimage{}, fmt.Errorf(
				"duplicate transaction input %d", index,
			)
		}
		if outpoint == fundingOutpoint {
			prevOuts[outpoint] = &wire.TxOut{
				Value:    int64(htlcValue),
				PkScript: bytes.Clone(htlcPkScript),
			}
			continue
		}

		prevTx, err := s.cfg.Bitcoin.GetRawTransaction(&outpoint.Hash)
		if err != nil {
			return lntypes.Preimage{}, fmt.Errorf(
				"fetch prevout %d: %w", index, err,
			)
		}
		if prevTx == nil || int(outpoint.Index) >= len(prevTx.MsgTx().TxOut) {
			return lntypes.Preimage{}, fmt.Errorf(
				"prevout %d is missing", index,
			)
		}
		prevOutput := prevTx.MsgTx().TxOut[outpoint.Index]
		prevOuts[outpoint] = &wire.TxOut{
			Value:    prevOutput.Value,
			PkScript: bytes.Clone(prevOutput.PkScript),
		}
	}

	prevOutFetcher := txscript.NewMultiPrevOutFetcher(prevOuts)
	sigHashes := txscript.NewTxSigHashes(spendingTx, prevOutFetcher)
	engine, err := txscript.NewEngine(
		htlcPkScript, spendingTx, inputIndex,
		txscript.StandardVerifyFlags, nil, sigHashes, int64(htlcValue),
		prevOutFetcher,
	)
	if err != nil {
		return lntypes.Preimage{}, fmt.Errorf("create script engine: %w", err)
	}
	if err := engine.Execute(); err != nil {
		return lntypes.Preimage{}, fmt.Errorf(
			"execute success witness: %w", err,
		)
	}

	return preimage, nil
}

func invoiceAlreadySettled(err error) bool {
	return err != nil && strings.Contains(
		strings.ToLower(err.Error()), "already settled",
	)
}

func (s *Server) failLoopOutBeforeFunding(loopOut *loopOutSwap,
	state swapserverrpc.ServerSwapState, err error) {

	loopOut.mu.Lock()
	cancelRequested := loopOut.cancelRequested
	loopOut.mu.Unlock()
	if cancelRequested {
		return
	}

	_ = s.cfg.Lnd.Invoices.CancelInvoice(s.ctx, loopOut.hash)
	_ = s.cfg.Lnd.Invoices.CancelInvoice(s.ctx, loopOut.prepayHash)
	s.finishLoopOut(loopOut, state, err)
}

// failLoopOutBeforeBroadcast is only used before SendOutputs is invoked, when
// it is still certain that this server did not publish an HTLC transaction.
func (s *Server) failLoopOutBeforeBroadcast(loopOut *loopOutSwap,
	err error) {

	_ = s.cfg.Lnd.Invoices.CancelInvoice(s.ctx, loopOut.hash)
	_ = s.cfg.Lnd.Invoices.CancelInvoice(s.ctx, loopOut.prepayHash)
	s.finishLoopOut(
		loopOut,
		swapserverrpc.ServerSwapState_SERVER_FAILED_HTLC_PUBLICATION, err,
	)
}

func (s *Server) failLoopOutAfterPublication(loopOut *loopOutSwap,
	err error) {

	// Once broadcasting was attempted, retain both accepted hold invoices:
	// the client may already possess the preimage and an HTLC may exist even
	// if lnd returned an error to SendOutputs.
	s.finishLoopOut(
		loopOut, swapserverrpc.ServerSwapState_SERVER_UNEXPECTED_FAILURE,
		err,
	)
}

func (s *Server) finishLoopOut(loopOut *loopOutSwap,
	state swapserverrpc.ServerSwapState, err error) {

	loopOut.mu.Lock()
	if loopOut.terminal || loopOut.cancelRequested {
		loopOut.mu.Unlock()
		return
	}
	loopOut.state = state
	loopOut.terminal = true
	loopOut.mu.Unlock()

	if err != nil {
		s.cfg.Logger.Printf("Loop Out %x finished in %v: %v",
			loopOut.hash[:6], state, err)
	}
	loopOut.updates.finish(state)
	loopOut.cancel()
}

func (s *Server) LoopOutPushPreimage(ctx context.Context,
	req *swapserverrpc.ServerLoopOutPushPreimageRequest) (
	*swapserverrpc.ServerLoopOutPushPreimageResponse, error) {

	if err := validateLoopOutProtocol(req.ProtocolVersion); err != nil {
		return nil, err
	}
	preimage, err := lntypes.MakePreimage(req.Preimage)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid preimage")
	}
	hash := preimage.Hash()
	loopOut := s.lookupLoopOut(hash)
	if loopOut == nil {
		return nil, status.Error(codes.NotFound, "swap not found")
	}
	if err := s.completeLoopOut(ctx, loopOut, preimage); err != nil {
		return nil, err
	}

	return &swapserverrpc.ServerLoopOutPushPreimageResponse{}, nil
}

// completeLoopOut atomically settles both invoices and terminalizes the swap.
// It is shared by the preimage RPC and spend-based recovery, so either path can
// win without double settlement or duplicate terminal updates.
func (s *Server) completeLoopOut(ctx context.Context, loopOut *loopOutSwap,
	preimage lntypes.Preimage) error {

	loopOut.mu.Lock()
	switch {
	case preimage.Hash() != loopOut.hash:
		loopOut.mu.Unlock()
		return status.Error(codes.InvalidArgument, "preimage hash mismatch")

	case loopOut.mainSettled && loopOut.state ==
		swapserverrpc.ServerSwapState_SERVER_SUCCESS:

		loopOut.mu.Unlock()
		return nil

	case loopOut.terminal:
		loopOut.mu.Unlock()
		return status.Error(codes.FailedPrecondition, "swap is terminal")

	case !loopOut.confirmed:
		loopOut.mu.Unlock()
		return status.Error(
			codes.FailedPrecondition, "HTLC is not confirmed",
		)
	}

	// Confirmation and the corresponding update are visible just before the
	// swap worker settles the prepay invoice. Settle it here as well so that
	// an immediate preimage push cannot terminalize the swap while leaving
	// the accepted prepay invoice held. The operation is idempotent with the
	// worker and with retries after an uncertain RPC result.
	if !loopOut.prepaySettled {
		err := s.cfg.Lnd.Invoices.SettleInvoice(
			ctx, loopOut.prepayPreimage,
		)
		if err != nil && !invoiceAlreadySettled(err) {
			loopOut.mu.Unlock()
			return status.Errorf(
				codes.Unavailable, "settle prepay invoice: %v", err,
			)
		}
		loopOut.prepaySettled = true
	}

	err := s.cfg.Lnd.Invoices.SettleInvoice(ctx, preimage)
	if err != nil && !invoiceAlreadySettled(err) {
		loopOut.mu.Unlock()
		return status.Errorf(
			codes.Unavailable, "settle swap invoice: %v", err,
		)
	}
	loopOut.mainSettled = true
	loopOut.state = swapserverrpc.ServerSwapState_SERVER_SUCCESS
	loopOut.terminal = true
	loopOut.mu.Unlock()

	loopOut.updates.finish(swapserverrpc.ServerSwapState_SERVER_SUCCESS)
	loopOut.cancel()

	return nil
}

func (s *Server) lookupLoopOut(hash lntypes.Hash) *loopOutSwap {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.loopOuts[hash]
}

func (s *Server) SubscribeLoopOutUpdates(
	req *swapserverrpc.SubscribeUpdatesRequest,
	stream swapserverrpc.SwapServer_SubscribeLoopOutUpdatesServer) error {

	if err := validateLoopOutProtocol(req.ProtocolVersion); err != nil {
		return err
	}
	hash, err := parseHash(req.SwapHash)
	if err != nil {
		return err
	}
	loopOut := s.lookupLoopOut(hash)
	if loopOut == nil {
		return status.Error(codes.NotFound, "swap not found")
	}

	subscription := loopOut.updates.subscribe()
	defer subscription.cancel()

	send := func(update serverUpdate) error {
		return stream.Send(&swapserverrpc.SubscribeLoopOutUpdatesResponse{
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

func (s *Server) CancelLoopOutSwap(ctx context.Context,
	req *swapserverrpc.CancelLoopOutSwapRequest) (
	*swapserverrpc.CancelLoopOutSwapResponse, error) {

	if err := validateLoopOutProtocol(req.ProtocolVersion); err != nil {
		return nil, err
	}
	hash, err := parseHash(req.SwapHash)
	if err != nil {
		return nil, err
	}
	if len(req.PaymentAddress) != 32 {
		return nil, status.Error(codes.PermissionDenied, "invalid swap owner")
	}
	loopOut := s.lookupLoopOut(hash)
	if loopOut == nil {
		return nil, status.Error(codes.NotFound, "swap not found")
	}

	cancelInfo := req.GetRouteCancel()
	if cancelInfo == nil {
		return nil, status.Error(
			codes.InvalidArgument, "route cancellation details required",
		)
	}
	var terminalState swapserverrpc.ServerSwapState
	switch cancelInfo.RouteType {
	case swapserverrpc.RoutePaymentType_PREPAY_ROUTE:
		terminalState =
			swapserverrpc.ServerSwapState_SERVER_CLIENT_PREPAY_CANCEL
	case swapserverrpc.RoutePaymentType_INVOICE_ROUTE:
		terminalState =
			swapserverrpc.ServerSwapState_SERVER_CLIENT_INVOICE_CANCEL
	default:
		return nil, status.Error(
			codes.InvalidArgument, "invalid cancellation route type",
		)
	}

	loopOut.mu.Lock()
	if subtle.ConstantTimeCompare(
		req.PaymentAddress, loopOut.paymentAddr[:],
	) != 1 {

		loopOut.mu.Unlock()
		return nil, status.Error(codes.PermissionDenied, "invalid swap owner")
	}
	if loopOut.canceled {
		loopOut.mu.Unlock()
		return &swapserverrpc.CancelLoopOutSwapResponse{}, nil
	}
	if loopOut.terminal || loopOut.fundingStarted {
		loopOut.mu.Unlock()
		return nil, status.Error(
			codes.FailedPrecondition, "HTLC publication already started",
		)
	}
	if loopOut.cancelRequested && loopOut.cancelState != terminalState {
		loopOut.mu.Unlock()
		return nil, status.Error(
			codes.FailedPrecondition,
			"swap cancellation already requested for another route",
		)
	}
	loopOut.cancelRequested = true
	loopOut.cancelState = terminalState

	// Keep the lock across the idempotent invoice RPCs so concurrent retries
	// cannot issue duplicate requests or overwrite acknowledgement state.
	// A partial result remains retryable: only invoices without a successful
	// acknowledgement are called again.
	var cancelErr error
	if !loopOut.mainCancelAck {
		err := s.cfg.Lnd.Invoices.CancelInvoice(ctx, loopOut.hash)
		if err == nil {
			loopOut.mainCancelAck = true
		} else {
			cancelErr = errors.Join(
				cancelErr, fmt.Errorf("cancel swap invoice: %w", err),
			)
		}
	}
	if !loopOut.prepayCancelAck {
		err := s.cfg.Lnd.Invoices.CancelInvoice(ctx, loopOut.prepayHash)
		if err == nil {
			loopOut.prepayCancelAck = true
		} else {
			cancelErr = errors.Join(
				cancelErr, fmt.Errorf("cancel prepay invoice: %w", err),
			)
		}
	}
	if cancelErr != nil {
		loopOut.mu.Unlock()
		return nil, status.Error(codes.Unavailable, cancelErr.Error())
	}

	loopOut.canceled = true
	loopOut.state = terminalState
	loopOut.terminal = true
	loopOut.mu.Unlock()

	loopOut.updates.finish(terminalState)
	loopOut.cancel()

	return &swapserverrpc.CancelLoopOutSwapResponse{}, nil
}

func (s *Server) MuSig2SignSweep(ctx context.Context,
	req *swapserverrpc.MuSig2SignSweepReq) (
	*swapserverrpc.MuSig2SignSweepRes, error) {

	if err := validateLoopOutProtocol(req.ProtocolVersion); err != nil {
		return nil, err
	}
	hash, err := parseHash(req.SwapHash)
	if err != nil {
		return nil, errMuSig2Sweep()
	}
	if len(req.PaymentAddress) != 32 || len(req.Nonce) != musig2.PubNonceSize {
		return nil, errMuSig2Sweep()
	}

	loopOut := s.lookupLoopOut(hash)
	if loopOut == nil {
		return nil, errMuSig2Sweep()
	}

	loopOut.mu.Lock()
	if subtle.ConstantTimeCompare(
		req.PaymentAddress, loopOut.paymentAddr[:],
	) != 1 || !loopOut.confirmed || !loopOut.mainSettled ||
		loopOut.state != swapserverrpc.ServerSwapState_SERVER_SUCCESS ||
		loopOut.fundingOutpoint == nil {

		loopOut.mu.Unlock()
		return nil, errMuSig2Sweep()
	}

	fundingOutpoint := *loopOut.fundingOutpoint
	senderKey := loopOut.senderKey
	receiverKey := loopOut.receiverKey
	senderLocator := loopOut.senderLocator
	htlc := loopOut.htlc
	amount := loopOut.amount
	loopOut.mu.Unlock()

	packet, inputIndex, prevOutputFetcher, err := validateLoopOutSweepPSBT(
		req, fundingOutpoint, htlc.PkScript, amount,
	)
	if err != nil {
		s.cfg.Logger.Printf("reject MuSig2 sweep for %x: %v", hash[:6], err)
		return nil, errMuSig2Sweep()
	}

	htlcV3, ok := htlc.HtlcScript.(*swap.HtlcScriptV3)
	if !ok {
		return nil, errMuSig2Sweep()
	}
	sigHashes := txscript.NewTxSigHashes(
		packet.UnsignedTx, prevOutputFetcher,
	)
	sigHash, err := txscript.CalcTaprootSignatureHash(
		sigHashes, txscript.SigHashDefault, packet.UnsignedTx, inputIndex,
		prevOutputFetcher,
	)
	if err != nil {
		return nil, errMuSig2Sweep()
	}

	var clientNonce [musig2.PubNonceSize]byte
	copy(clientNonce[:], req.Nonce)
	session, err := s.cfg.Lnd.Signer.MuSig2CreateSession(
		ctx, input.MuSig2Version100RC2, &senderLocator,
		[][]byte{senderKey[:], receiverKey[:]},
		lndclient.MuSig2TaprootTweakOpt(htlcV3.RootHash[:], false),
		lndclient.MuSig2NonceOpt(
			[][musig2.PubNonceSize]byte{clientNonce},
		),
	)
	if err != nil {
		return nil, errMuSig2Sweep()
	}
	if !session.HaveAllNonces {
		_ = s.cfg.Lnd.Signer.MuSig2Cleanup(ctx, session.SessionID)
		return nil, errMuSig2Sweep()
	}

	var digest [32]byte
	copy(digest[:], sigHash)
	partialSignature, err := s.cfg.Lnd.Signer.MuSig2Sign(
		ctx, session.SessionID, digest, true,
	)
	if err != nil || len(partialSignature) != input.MuSig2PartialSigSize {
		if err == nil {
			err = fmt.Errorf(
				"partial signature has length %d", len(partialSignature),
			)
		}
		s.cfg.Logger.Printf("MuSig2 sign failed for %x: %v", hash[:6], err)
		return nil, errMuSig2Sweep()
	}

	return &swapserverrpc.MuSig2SignSweepRes{
		Nonce:            bytes.Clone(session.PublicNonce[:]),
		PartialSignature: bytes.Clone(partialSignature),
	}, nil
}

func errMuSig2Sweep() error {
	return status.Error(codes.PermissionDenied, "MuSig2 sweep rejected")
}

func validateLoopOutSweepPSBT(req *swapserverrpc.MuSig2SignSweepReq,
	fundingOutpoint wire.OutPoint, htlcPkScript []byte,
	amount btcutil.Amount) (*psbt.Packet, int, txscript.PrevOutputFetcher,
	error) {

	packet, err := psbt.NewFromRawBytes(
		bytes.NewReader(req.SweepTxPsbt), false,
	)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("decode PSBT: %w", err)
	}
	tx := packet.UnsignedTx
	if len(tx.TxIn) == 0 || len(tx.TxOut) == 0 ||
		len(tx.TxIn) != len(packet.Inputs) {

		return nil, 0, nil, errors.New("invalid PSBT shape")
	}

	selectedInput := -1
	for index := range tx.TxIn {
		witnessUtxo := packet.Inputs[index].WitnessUtxo
		if witnessUtxo == nil || witnessUtxo.Value <= 0 {
			return nil, 0, nil, fmt.Errorf(
				"input %d missing valid witness UTXO", index,
			)
		}
		if tx.TxIn[index].PreviousOutPoint == fundingOutpoint &&
			witnessUtxo.Value == int64(amount) &&
			bytes.Equal(witnessUtxo.PkScript, htlcPkScript) {

			if selectedInput != -1 {
				return nil, 0, nil, errors.New("duplicate HTLC input")
			}
			selectedInput = index
		}
	}
	if selectedInput == -1 {
		return nil, 0, nil, errors.New("recorded HTLC input not found")
	}

	if len(req.PrevoutInfo) == 0 {
		if len(tx.TxIn) != 1 {
			return nil, 0, nil, errors.New(
				"multi-input sweep requires every prevout",
			)
		}
		prevOut := packet.Inputs[selectedInput].WitnessUtxo

		return packet, selectedInput,
			txscript.NewCannedPrevOutputFetcher(
				prevOut.PkScript, prevOut.Value,
			), nil
	}

	if len(req.PrevoutInfo) != len(tx.TxIn) {
		return nil, 0, nil, errors.New("prevout count mismatch")
	}
	prevOuts := make(map[wire.OutPoint]*wire.TxOut, len(req.PrevoutInfo))
	for _, rpcPrevOut := range req.PrevoutInfo {
		txid, err := chainhash.NewHash(rpcPrevOut.TxidBytes)
		if err != nil || rpcPrevOut.Value > math.MaxInt64 {
			return nil, 0, nil, errors.New("invalid prevout")
		}
		outpoint := wire.OutPoint{
			Hash:  *txid,
			Index: rpcPrevOut.OutputIndex,
		}
		if _, duplicate := prevOuts[outpoint]; duplicate {
			return nil, 0, nil, errors.New("duplicate prevout")
		}
		prevOuts[outpoint] = &wire.TxOut{
			Value:    int64(rpcPrevOut.Value),
			PkScript: bytes.Clone(rpcPrevOut.PkScript),
		}
	}
	if len(prevOuts) != len(tx.TxIn) {
		return nil, 0, nil, errors.New("prevout map mismatch")
	}
	for index, txIn := range tx.TxIn {
		prevOut, ok := prevOuts[txIn.PreviousOutPoint]
		if !ok {
			return nil, 0, nil, errors.New("missing transaction prevout")
		}
		witnessUtxo := packet.Inputs[index].WitnessUtxo
		if witnessUtxo.Value != prevOut.Value ||
			!bytes.Equal(witnessUtxo.PkScript, prevOut.PkScript) {

			return nil, 0, nil, errors.New("PSBT prevout mismatch")
		}
	}

	return packet, selectedInput,
		txscript.NewMultiPrevOutFetcher(prevOuts), nil
}
