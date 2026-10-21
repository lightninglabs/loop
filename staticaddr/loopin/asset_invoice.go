package loopin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"time"

	"github.com/lightninglabs/loop/assets"
	"github.com/lightninglabs/taproot-assets/taprpc/rfqrpc"
	"github.com/lightninglabs/taproot-assets/taprpc/tapchannelrpc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// assetHoldExpiryDelta covers LND's incoming HOLD cancellation margin.
const assetHoldExpiryDelta uint32 = 20

// createAssetInvoice creates a HOLD invoice for the server-owned hash.
// The caller persists it before communicating claim authority.
// An interrupted initialization aborts through the normal unlock action.
func (f *FSM) createAssetInvoice(ctx context.Context, height uint32) (
	string, error) {

	cfg, offer := f.cfg.Assets, f.loopIn.Asset.Offer
	now := time.Now()
	peer := cfg.Peer[:]
	if height >= offer.Terms.RefundHeight {
		return "", assets.ErrEvidence
	}
	cltv := uint64(offer.Terms.RefundHeight-height) +
		uint64(offer.Terms.RecoveryDelta) + uint64(assetHoldExpiryDelta) + 1
	if cltv > math.MaxUint16 {
		return "", fmt.Errorf(
			"asset invoice CLTV exceeds BOLT11 limit",
		)
	}
	amount := offer.Terms.PayoutAmount
	policy := rfqrpc.ExecutionPolicy_EXECUTION_POLICY_FOK
	response, err := cfg.Tapd.AddInvoice(ctx,
		&tapchannelrpc.AddInvoiceRequest{
			GroupKey: offer.Terms.GroupKey[:], AssetAmount: amount,
			PeerPubkey: peer, AssetMinAmt: &amount,
			ExecutionPolicy: policy,
			HodlInvoice: &tapchannelrpc.HodlInvoice{
				PaymentHash: offer.Hash[:],
			},
			InvoiceRequest: &lnrpc.Invoice{
				Memo:       "asset loop in",
				Expiry:     offer.Terms.OfferExpiry.Unix() - now.Unix(),
				CltvExpiry: cltv,
			},
		})
	if err != nil {
		return "", err
	}
	quote := response.GetAcceptedBuyQuote()
	result := response.GetInvoiceResult()
	if quote == nil || result == nil || quote.AssetSpec == nil ||
		!bytes.Equal(quote.AssetSpec.GroupPubKey, offer.Terms.GroupKey[1:]) ||
		len(quote.AssetSpec.Id) != 0 || quote.AssetMaxAmount != amount ||
		(quote.AcceptedMaxAmount != 0 &&
			quote.AcceptedMaxAmount != amount) ||
		quote.Expiry < uint64(offer.Terms.OfferExpiry.Unix()) ||
		len(quote.Id) != 32 ||
		(len(peer) != 0 && quote.Peer != hex.EncodeToString(peer)) {

		return "", assets.ErrEvidence
	}
	authCtx, _, lightning := f.cfg.LndClient.RawClientWithMacAuth(ctx)
	decoded, err := lightning.DecodePayReq(authCtx, &lnrpc.PayReqString{
		PayReq: result.PaymentRequest,
	})
	if err != nil {
		return "", err
	}
	if err := assets.ValidateInvoice(offer, decoded, now); err != nil {
		return "", err
	}
	// Fail closed against tapd versions that drop CltvExpiry for HOLD
	// invoices. Invoice wall-clock expiry cannot substitute for this check.
	if decoded.CltvExpiry != int64(cltv) {
		return "", assets.ErrEvidence
	}
	return result.PaymentRequest, nil
}

// acceptedAssetPayment reads the local circuit before exposing a signature.
func (f *FSM) acceptedAssetPayment(ctx context.Context, height uint32) (
	*assets.Evidence, error) {

	authCtx, _, lightning := f.cfg.LndClient.RawClientWithMacAuth(ctx)
	invoice, err := lightning.LookupInvoice(authCtx, &lnrpc.PaymentHash{
		RHash: f.loopIn.SwapHash[:],
	})
	if err != nil {
		return nil, err
	}
	channels, err := lightning.ListChannels(authCtx, &lnrpc.ListChannelsRequest{})
	if err != nil {
		return nil, err
	}
	if channels == nil {
		return nil, assets.ErrEvidence
	}
	return assets.Accepted(
		f.loopIn.Asset.Offer, assetHoldExpiryDelta, invoice,
		channels.Channels, height,
	)
}

// settledAssetPayment keeps the accepted circuit binding after channel close.
func (f *FSM) settledAssetPayment(ctx context.Context) (bool, error) {
	authCtx, _, lightning := f.cfg.LndClient.RawClientWithMacAuth(ctx)
	invoice, err := lightning.LookupInvoice(authCtx, &lnrpc.PaymentHash{
		RHash: f.loopIn.SwapHash[:],
	})
	if err != nil {
		return false, err
	}
	return assets.Settled(f.loopIn.Asset.Offer, *f.loopIn.Asset.Accepted, invoice)
}

// settleAssetInvoice retries a lost response against the same payment hash.
func (f *FSM) settleAssetInvoice(ctx context.Context, preimage [32]byte) error {
	hash := f.loopIn.SwapHash
	if sha256.Sum256(preimage[:]) != hash {
		return assets.ErrEvidence
	}
	authCtx, _, invoices := f.cfg.InvoicesClient.RawClientWithMacAuth(ctx)
	_, err := invoices.SettleInvoice(authCtx, &invoicesrpc.SettleInvoiceMsg{
		Preimage: preimage[:],
	})
	if err == nil {
		return nil
	}
	settled, lookupErr := f.settledAssetPayment(ctx)
	if lookupErr == nil && settled {
		return nil
	}
	return err
}

// cancelAssetInvoice is called only before exposure or after verified refund.
func (f *FSM) cancelAssetInvoice(ctx context.Context) error {
	authCtx, _, invoices := f.cfg.InvoicesClient.RawClientWithMacAuth(ctx)
	_, err := invoices.CancelInvoice(authCtx, &invoicesrpc.CancelInvoiceMsg{
		PaymentHash: f.loopIn.SwapHash[:],
	})
	if status.Code(err) == codes.NotFound {
		return nil
	}
	return err
}
