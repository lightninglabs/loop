package assetln

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"time"

	"github.com/lightninglabs/taproot-assets/taprpc/rfqrpc"
	"github.com/lightninglabs/taproot-assets/taprpc/tapchannelrpc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Receiver uses authenticated local RPC connections. It never receives the
// server preimage until the execution decision has been made durable.
type Receiver struct {
	Assets    tapchannelrpc.TaprootAssetChannelsClient
	Lightning lnrpc.LightningClient
	Invoices  invoicesrpc.InvoicesClient
	Now       func() time.Time
}

func (r *Receiver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// HoldInvoice is the invoice and its immutable receiver-side RFQ binding.
//
//nolint:tagliatelle // Preserve the existing recovery and wire format.
type HoldInvoice struct {
	PaymentRequest string                       `json:"PaymentRequest"`
	Quote          *rfqrpc.PeerAcceptedBuyQuote `json:"Quote"`
	FinalCLTV      uint64                       `json:"FinalCLTV"`
}

// Create creates an asset HOLD invoice using the server's payment hash. A
// caller must persist the returned result before communicating the invoice.
// An ambiguous RPC error is recovered through Lookup, never by a new hash.
func (r *Receiver) Create(ctx context.Context, terms Terms, peer []byte,
	height uint32) (*HoldInvoice, error) {

	now := r.now()
	if err := terms.Validate(now); err != nil {
		return nil, err
	}
	if height >= terms.RefundHeight {
		return nil, ErrEvidence
	}
	cltv := uint64(terms.RefundHeight-height) +
		uint64(terms.RecoveryBlocks) + uint64(terms.HoldExpiryDelta) + 1
	if cltv > math.MaxUint16 {
		return nil, fmt.Errorf(
			"asset invoice CLTV exceeds BOLT11 limit",
		)
	}
	amount := terms.Amount
	policy := rfqrpc.ExecutionPolicy_EXECUTION_POLICY_FOK
	response, err := r.Assets.AddInvoice(ctx,
		&tapchannelrpc.AddInvoiceRequest{
			GroupKey: terms.GroupKey[:], AssetAmount: amount,
			PeerPubkey: peer, AssetMinAmt: &amount,
			ExecutionPolicy: policy,
			HodlInvoice: &tapchannelrpc.HodlInvoice{
				PaymentHash: terms.Hash[:],
			},
			InvoiceRequest: &lnrpc.Invoice{
				Memo:       "asset loop in",
				Expiry:     terms.Deadline - now.Unix(),
				CltvExpiry: cltv,
			},
		})
	if err != nil {
		return nil, err
	}
	quote := response.GetAcceptedBuyQuote()
	result := response.GetInvoiceResult()
	if quote == nil || result == nil || quote.AssetSpec == nil ||
		!bytes.Equal(quote.AssetSpec.GroupPubKey, terms.GroupKey[1:]) ||
		len(
			quote.AssetSpec.Id,
		) != 0 || quote.AssetMaxAmount != amount ||
		(quote.AcceptedMaxAmount != 0 &&
			quote.AcceptedMaxAmount != amount) ||
		quote.Expiry < uint64(terms.Deadline) || len(quote.Id) != 32 ||
		(len(peer) != 0 && quote.Peer != hex.EncodeToString(peer)) {

		return nil, ErrEvidence
	}
	decoded, err := r.Lightning.DecodePayReq(ctx, &lnrpc.PayReqString{
		PayReq: result.PaymentRequest,
	})
	if err != nil {
		return nil, err
	}
	if err := validateInvoice(terms, decoded, now); err != nil {
		return nil, err
	}
	// Fail closed against tapd versions that drop CltvExpiry for HOLD
	// invoices. Invoice wall-clock expiry cannot substitute for this check.
	if decoded.CltvExpiry != int64(cltv) {
		return nil, ErrEvidence
	}
	return &HoldInvoice{
		PaymentRequest: result.PaymentRequest, Quote: quote,
		FinalCLTV: cltv,
	}, nil
}

func validateInvoice(terms Terms, decoded *lnrpc.PayReq, now time.Time) error {
	if decoded == nil ||
		decoded.PaymentHash != hex.EncodeToString(terms.Hash[:]) ||
		decoded.NumMsat <= 0 || decoded.Expiry <= 0 ||
		decoded.Timestamp > math.MaxInt64-decoded.Expiry ||
		decoded.Timestamp+decoded.Expiry < terms.Deadline ||
		decoded.Timestamp+decoded.Expiry <= now.Unix() {

		return ErrEvidence
	}
	return nil
}

// Lookup retrieves the persisted invoice after a lost creation or settlement
// response. A missing invoice is distinct from an unavailable local daemon.
func (r *Receiver) Lookup(ctx context.Context, hash [32]byte) (
	*lnrpc.Invoice, error) {

	return r.Lightning.LookupInvoice(
		ctx,
		&lnrpc.PaymentHash{RHash: hash[:]},
	)
}

// Recover retrieves an invoice after an ambiguous creation result without
// negotiating another RFQ. Accepted channel evidence, rather than a new price
// lookup, establishes its native quantity before claim signatures are exposed.
func (r *Receiver) Recover(ctx context.Context, terms Terms) (
	*HoldInvoice, error) {

	invoice, err := r.Lookup(ctx, terms.Hash)
	if err != nil {
		return nil, err
	}
	if invoice == nil || invoice.PaymentRequest == "" ||
		!bytes.Equal(invoice.RHash, terms.Hash[:]) ||
		invoice.State == lnrpc.Invoice_CANCELED {

		return nil, ErrEvidence
	}
	decoded, err := r.Lightning.DecodePayReq(ctx, &lnrpc.PayReqString{
		PayReq: invoice.PaymentRequest,
	})
	if err != nil {
		return nil, err
	}
	// A held invoice can outlive its admission deadline. Recovery must not
	// turn RFQ expiry into cancellation of an already exposed asset claim.
	if err := validateInvoice(terms, decoded, time.Unix(0, 0)); err != nil {
		return nil, err
	}
	if decoded.CltvExpiry <= int64(terms.RecoveryBlocks)+
		int64(terms.HoldExpiryDelta) {

		return nil, ErrEvidence
	}
	return &HoldInvoice{
		PaymentRequest: invoice.PaymentRequest,
		FinalCLTV:      uint64(decoded.CltvExpiry),
	}, nil
}

// Settled verifies that the previously accepted circuit actually settled.
// The binding survives a channel close because the acceptance evidence was
// persisted before exposure. A different circuit or a Bitcoin-only payment
// cannot authorize the client's cooperative deposit signature.
func (r *Receiver) Settled(ctx context.Context, terms Terms,
	evidence Evidence) (bool, error) {

	if evidence.Hash != terms.Hash || evidence.GroupKey != terms.GroupKey ||
		evidence.Amount != terms.Amount {

		return false, ErrEvidence
	}
	invoice, err := r.Lookup(ctx, terms.Hash)
	if err != nil {
		return false, err
	}
	if invoice == nil || !bytes.Equal(invoice.RHash, terms.Hash[:]) {
		return false, ErrEvidence
	}
	if invoice.State != lnrpc.Invoice_SETTLED {
		return false, nil
	}
	count := 0
	for _, htlc := range invoice.Htlcs {
		if htlc == nil ||
			htlc.State == lnrpc.InvoiceHTLCState_CANCELED {

			continue
		}
		if htlc.State != lnrpc.InvoiceHTLCState_SETTLED ||
			htlc.Amp != nil ||
			htlc.ChanId != evidence.ChannelID ||
			htlc.HtlcIndex != evidence.HTLCIndex {

			return false, ErrEvidence
		}
		count++
	}
	return count == 1, nil
}

// Prepared reads accepted HTLCs and channel metadata from the local node.
// Peer claims or the sender's IN_FLIGHT event cannot authorize signatures.
func (r *Receiver) Prepared(ctx context.Context, terms Terms,
	height uint32) (*Evidence, error) {

	invoice, err := r.Lookup(ctx, terms.Hash)
	if err != nil {
		return nil, err
	}
	channels, err := r.Lightning.ListChannels(
		ctx,
		&lnrpc.ListChannelsRequest{},
	)
	if err != nil {
		return nil, err
	}
	if channels == nil {
		return nil, ErrEvidence
	}
	return Accepted(terms, invoice, channels.Channels, height)
}

// Settle accepts only the committed hash's preimage. It is idempotent across a
// lost RPC response; a caller must keep monitoring until SETTLED is observed.
func (r *Receiver) Settle(ctx context.Context, hash, preimage [32]byte) error {
	if sha256.Sum256(preimage[:]) != hash {
		return ErrEvidence
	}
	_, err := r.Invoices.SettleInvoice(ctx, &invoicesrpc.SettleInvoiceMsg{
		Preimage: preimage[:],
	})
	if err == nil {
		return nil
	}
	invoice, lookupErr := r.Lookup(ctx, hash)
	if lookupErr == nil && invoice != nil &&
		bytes.Equal(invoice.RHash, hash[:]) &&
		invoice.State == lnrpc.Invoice_SETTLED {

		return nil
	}
	return err
}

// CancelBeforeExposure cancels an invoice when the owning swap FSM has
// established that no claim was exposed, or a confirmed spendable refund has
// resolved the on-chain exposure. A peer request cannot authorize cancellation.
func (r *Receiver) CancelBeforeExposure(ctx context.Context,
	hash [32]byte) error {

	_, err := r.Invoices.CancelInvoice(ctx, &invoicesrpc.CancelInvoiceMsg{
		PaymentHash: hash[:],
	})
	if status.Code(err) == codes.NotFound {
		return nil
	}
	return err
}
