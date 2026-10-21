package loopin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"time"

	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/swapkit"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// InitAssetHtlcAction obtains the server-owned hash and persists the normal
// swap before any client claim authority is produced.
func (f *FSM) InitAssetHtlcAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	cfg, a := f.cfg.Assets, f.loopIn.Asset
	if err := cfg.FetchL402(ctx); err != nil {
		return f.HandleError(err)
	}
	d := f.loopIn.Deposits[0].AssetSnapshot()
	point := f.loopIn.Deposits[0].OutPoint
	request := &swapserverrpc.CreateAssetLoopInRequest{
		ProtocolVersion: 1, RequestId: a.Request.RequestID[:],
		GroupKey: a.Request.GroupKey[:], PayoutAmount: a.Request.Amount,
		MaxAssetDebit: a.Request.MaxDebit, MaxAssetFee: a.Request.MaxFee,
		Contract: d.Contract, Deposits: []*swapserverrpc.AssetDepositInput{{
			Txid: point.Hash[:], OutputIndex: point.Index, Proof: d.Proof,
		}},
	}
	if proto.Size(request) > swapkit.PublicRPCMaxBytes {
		return f.HandleError(swapkit.ErrInvalid)
	}
	response, err := cfg.Server.CreateAssetLoopIn(ctx, request)
	if err != nil {
		return f.HandleError(err)
	}
	offer, err := validateAssetOffer(cfg.Owner, request, response)
	if err != nil {
		return f.HandleError(err)
	}
	if err := cfg.Driver.ValidateOffer(ctx, offer); err != nil {
		return f.HandleError(err)
	}
	a.Offer = offer
	f.loopIn.SwapHash = lntypes.Hash(offer.Hash)
	f.loopIn.HtlcCltvExpiry = int32(offer.Terms.RefundHeight)
	// Before signing, an interrupted initialization is simply abandoned on
	// recovery, exactly like a Bitcoin static Loop In.
	if err := f.cfg.Store.CreateLoopIn(ctx, f.loopIn); err != nil {
		return f.HandleError(err)
	}
	if err := f.cfg.DepositManager.TransitionDeposits(ctx, f.loopIn.Deposits,
		deposit.OnLoopInInitiated, deposit.LoopingIn); err != nil {
		return f.HandleError(err)
	}
	return OnHtlcInitiated
}

// SignAssetHtlcAction obtains the complete asset fallback and verifies the
// native incoming HTLC before entering the ordinary monitoring state.
func (f *FSM) SignAssetHtlcAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	cfg, a := f.cfg.Assets, f.loopIn.Asset
	height, err := cfg.Driver.Height(ctx)
	if err != nil {
		return f.HandleError(err)
	}
	a.Invoice, err = cfg.Receiver.Create(
		ctx, a.paymentTerms(), cfg.Peer[:], height,
	)
	if err != nil {
		return f.HandleError(err)
	}
	f.loopIn.SwapInvoice = a.Invoice.PaymentRequest
	a.RecoveryRequest, err = cfg.Driver.PrepareRecovery(ctx, a.Offer)
	if err != nil {
		return f.HandleError(err)
	}
	peerCtx, cancel := context.WithDeadline(ctx, a.Offer.Terms.OfferExpiry)
	defer cancel()
	prepare := &swapserverrpc.PrepareAssetLoopInRequest{
		PaymentHash:      a.Offer.Hash[:],
		AssetHoldInvoice: a.Invoice.PaymentRequest,
		RecoveryRequest:  a.RecoveryRequest,
	}
	for {
		_, err = cfg.Server.PrepareAssetLoopIn(peerCtx, prepare)
		if err == nil {
			break
		}
		if !retryAssetPeer(err, a.Offer.Terms.OfferExpiry) {
			return f.HandleError(err)
		}
		if !waitAssetRetry(ctx) {
			return fsm.NoOp
		}
	}
	round, err := cfg.Driver.BeginFallback(ctx, a.Offer, a.RecoveryRequest)
	if err != nil {
		return f.HandleError(err)
	}
	defer round.Close()
	var bundle *swapserverrpc.AssetLoopInFallbackResponse
	for {
		bundle, err = cfg.Server.AssetLoopInFallback(peerCtx,
			&swapserverrpc.AssetLoopInReference{
				PaymentHash:  a.Offer.Hash[:],
				SigningNonce: round.Nonce[:],
			})
		if err == nil {
			break
		}
		if !retryAssetPeer(err, a.Offer.Terms.OfferExpiry) {
			return f.HandleError(err)
		}
		if !waitAssetRetry(ctx) {
			return fsm.NoOp
		}
	}
	a.Fallback, err = round.Finalize(ctx, bundle.GetRecoveryBundle())
	if err != nil {
		return f.HandleError(err)
	}
	if len(a.Fallback.Bundle) == 0 || len(a.Fallback.ClaimSignatures) == 0 {
		return f.HandleError(swapkit.ErrInvalid)
	}
	// Preparing a payment and receiving its HTLC are asynchronous. Wait in
	// this existing action until the local node confirms the exact circuit.
	for {
		height, err = cfg.Driver.Height(ctx)
		if err == nil {
			a.Accepted, err = cfg.Receiver.Prepared(ctx, a.paymentTerms(), height)
		}
		if err == nil {
			break
		}
		if !time.Now().Before(a.Offer.Terms.OfferExpiry) {
			return f.HandleError(err)
		}
		if !waitAssetRetry(ctx) {
			return fsm.NoOp
		}
	}
	// This is the protocol's one safety boundary. Recovery must retain the
	// complete fallback and treat signatures as exposed even if the send's
	// response is lost. Save the existing lifecycle state before that send.
	f.loopIn.SetState(MonitorInvoiceAndHtlcTx)
	if err := f.cfg.Store.UpdateLoopIn(ctx, f.loopIn); err != nil {
		f.loopIn.SetState(SignHtlcTx)
		return f.HandleError(err)
	}
	return OnHtlcTxSigned
}

// MonitorAssetInvoiceAction settles only after server disclosure and verified
// native acceptance. Errors retain the swap's existing recovery state.
func (f *FSM) MonitorAssetInvoiceAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	cfg, a := f.cfg.Assets, f.loopIn.Asset
	for {
		settled, err := f.assetPaymentSettled(ctx)
		if err == nil && settled {
			return OnPaymentReceived
		}
		if ctx.Err() != nil {
			return fsm.NoOp
		}
		height, heightErr := cfg.Driver.Height(ctx)
		if heightErr == nil && (height >= a.Offer.Terms.RefundHeight ||
			!time.Now().Before(a.Offer.Terms.OfferExpiry)) {

			return OnSweepHtlcTimeout
		}
		// A restart may precede the first send. Recheck the same incoming asset
		// circuit and its expiry before retrying conditional claim signatures.
		if heightErr == nil && uint64(height)+uint64(a.Offer.Terms.ExecutionDelta) <
			uint64(a.Offer.Terms.RefundHeight) {

			evidence, err := cfg.Receiver.Prepared(ctx, a.paymentTerms(), height)
			if err == nil && a.Accepted != nil && *evidence == *a.Accepted {
				height, err = cfg.Driver.Height(ctx)
				if err != nil || !time.Now().Before(a.Offer.Terms.OfferExpiry) ||
					uint64(height)+uint64(a.Offer.Terms.ExecutionDelta) >=
						uint64(a.Offer.Terms.RefundHeight) {

					continue
				}
				peerCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
				_, err = cfg.Server.CommitAssetLoopIn(peerCtx,
					&swapserverrpc.CommitAssetLoopInRequest{
						PaymentHash:      a.Offer.Hash[:],
						ClientSignatures: a.Fallback.ClaimSignatures,
					})
				cancel()
				if err != nil {
					f.Debugf("asset claim submission pending: %v", err)
				}
			}
		}
		if !waitAssetRetry(ctx) {
			return fsm.NoOp
		}
	}
}

// assetPaymentSettled uses local invoice and accepted-circuit evidence. A
// server status or on-chain secret alone is never proof of payment.
func (f *FSM) assetPaymentSettled(ctx context.Context) (bool, error) {
	cfg, a := f.cfg.Assets, f.loopIn.Asset
	if a.Accepted == nil {
		return false, swapkit.ErrInvalid
	}
	settled, err := cfg.Receiver.Settled(ctx, a.paymentTerms(), *a.Accepted)
	if err != nil || settled {
		return settled, err
	}
	preimage, found, err := f.assetPreimage(ctx)
	if err != nil || !found {
		return false, nil
	}
	if err := cfg.Receiver.Settle(ctx, a.Offer.Hash, preimage); err != nil {
		return cfg.Receiver.Settled(ctx, a.paymentTerms(), *a.Accepted)
	}
	// LND persists settlement. Recover the preimage there if this SQL update
	// or its response is interrupted.
	f.loopIn.SwapPreimage = lntypes.Preimage(preimage)
	return cfg.Receiver.Settled(ctx, a.paymentTerms(), *a.Accepted)
}

func (f *FSM) assetPreimage(ctx context.Context) ([32]byte, bool, error) {
	cfg, a := f.cfg.Assets, f.loopIn.Asset
	if f.loopIn.SwapPreimage.Hash() == f.loopIn.SwapHash {
		return [32]byte(f.loopIn.SwapPreimage), true, nil
	}
	invoice, err := cfg.Receiver.Lookup(ctx, a.Offer.Hash)
	if err == nil && invoice != nil && invoice.State == lnrpc.Invoice_SETTLED {
		if len(invoice.RPreimage) != 32 ||
			!bytes.Equal(invoice.RHash, a.Offer.Hash[:]) {

			return [32]byte{}, false, swapkit.ErrInvalid
		}
		var preimage [32]byte
		copy(preimage[:], invoice.RPreimage)
		if sha256.Sum256(preimage[:]) != a.Offer.Hash {
			return preimage, false, swapkit.ErrInvalid
		}
		return preimage, true, nil
	}
	peerCtx, cancel := context.WithTimeout(ctx, time.Second)
	response, err := cfg.Server.GetAssetLoopIn(peerCtx,
		&swapserverrpc.AssetLoopInReference{PaymentHash: a.Offer.Hash[:]})
	cancel()
	if err == nil && len(response.GetExecutionPreimage()) == 32 {
		var preimage [32]byte
		copy(preimage[:], response.ExecutionPreimage)
		if sha256.Sum256(preimage[:]) == a.Offer.Hash {
			return preimage, true, nil
		}
	}
	chainCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	result, err := cfg.Driver.LookupPreimage(chainCtx, a.operation())
	return result.Preimage, result.Found, err
}

// SweepAssetHtlcTimeoutAction publishes the stored fallback and the
// proof-preserving refund. Local payment settlement can still win until the
// refund confirms.
func (f *FSM) SweepAssetHtlcTimeoutAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	cfg, a := f.cfg.Assets, f.loopIn.Asset
	for {
		if f.loopIn.Deposits[0].IsInState(deposit.HtlcTimeoutSwept) {
			return OnHtlcTimeoutSwept
		}
		settled, err := f.assetPaymentSettled(ctx)
		if settled && err == nil {
			return OnPaymentReceived
		}
		if ctx.Err() != nil {
			return fsm.NoOp
		}
		// Failure to check local settlement must not authorize a refund.
		if err != nil {
			if !waitAssetRetry(ctx) {
				return fsm.NoOp
			}
			continue
		}
		if !f.loopIn.Deposits[0].IsInState(deposit.SweepHtlcTimeout) {
			err = f.cfg.DepositManager.TransitionDeposits(ctx, f.loopIn.Deposits,
				deposit.OnSweepingHtlcTimeout, deposit.SweepHtlcTimeout)
		}
		if err == nil {
			var proof []byte
			proof, err = cfg.Driver.RecoverRefund(ctx, a.operation())
			if err == nil && len(proof) > 0 {
				err = cfg.Receiver.CancelBeforeExposure(ctx, a.Offer.Hash)
				if err == nil {
					err = f.cfg.DepositManager.TransitionDeposits(ctx, f.loopIn.Deposits,
						deposit.OnHtlcTimeoutSwept, deposit.HtlcTimeoutSwept)
				}
				if err == nil {
					peerCtx, cancel := context.WithTimeout(ctx, time.Second)
					_, _ = cfg.Server.ReportAssetLoopInRefund(peerCtx,
						&swapserverrpc.ReportAssetLoopInRefundRequest{
							PaymentHash: a.Offer.Hash[:], Proof: proof,
						})
					cancel()
					return OnHtlcTimeoutSwept
				}
			}
		}
		if err != nil {
			f.Debugf("asset refund pending: %v", err)
		}
		if !waitAssetRetry(ctx) {
			return fsm.NoOp
		}
	}
}

// UnlockAssetDepositsAction abandons an initialization before claim exposure.
func (f *FSM) UnlockAssetDepositsAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	cfg, a := f.cfg.Assets, f.loopIn.Asset
	if a.Offer.Hash != ([32]byte{}) {
		if err := cfg.Receiver.CancelBeforeExposure(ctx, a.Offer.Hash); err != nil {
			f.Errorf("unable to cancel asset invoice: %v", err)
			return fsm.NoOp
		}
		peerCtx, cancel := context.WithTimeout(ctx, time.Second)
		_, _ = cfg.Server.CancelAssetLoopIn(peerCtx,
			&swapserverrpc.AssetLoopInReference{PaymentHash: a.Offer.Hash[:]})
		cancel()
	}
	if err := f.unlockDeposits(ctx); err != nil {
		return f.HandleError(err)
	}
	return fsm.OnError
}

func waitAssetRetry(ctx context.Context) bool {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// retryAssetPeer permits transient retries only during the live handshake.
// Restarting before the complete fallback was saved still aborts the swap.
func retryAssetPeer(err error, deadline time.Time) bool {
	if !time.Now().Before(deadline) {
		return false
	}
	code := status.Code(err)
	return code == codes.Unavailable || code == codes.DeadlineExceeded
}
