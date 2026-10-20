// Package assetclient binds the durable asset loop-in controller to local
// asset Lightning evidence and the versioned server protocol.
package assetclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/loop/assets/assetln"
	"github.com/lightninglabs/loop/staticaddr/loopin/assetexec"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ClientChainActions is the concrete SDK and shared inventory boundary.
// Lightning and server RPC methods are supplied by Adapter itself, so a
// production chain driver never embeds unimplemented payment callbacks.
type ClientChainActions interface {
	ValidateOffer(context.Context, assetexec.Offer) error
	VerifyFinalizeFallback(context.Context, assetexec.Offer,
		[]byte) (assetexec.Fallback, error)
	LookupPreimage(context.Context, assetexec.Offer) ([32]byte, bool, error)
	SignCooperative(
		context.Context,
		assetexec.Offer,
		[]byte,
	) ([]byte, error)
	InspectClaim(context.Context, assetexec.Offer,
		assetexec.Fallback) ([]byte, error)
	RecoverRefund(context.Context, assetexec.Offer,
		assetexec.Fallback) ([]byte, error)
}

// Actions preserves the adapter's existing embedded field name.
type Actions = ClientChainActions

// Adapter supplies the payment and peer methods of the client controller.
// The embedded SDK/lock adapter owns offer validation, fallback signatures,
// refunds, and the existing deposit manager's reservation lifecycle.
type Adapter struct {
	Actions

	Receiver        *assetln.Receiver
	Server          Server
	Store           assetexec.Store
	Height          func(context.Context) (uint32, error)
	HoldExpiryDelta uint32
}

// CurrentHeight supplies the authenticated local chain tip for recovery.
func (a *Adapter) CurrentHeight(ctx context.Context) (uint32, error) {
	return a.Height(ctx)
}

// InvoiceIntent fixes the receiver's asset-channel peer before invoice RPCs.
//
//nolint:tagliatelle // Preserve the existing recovery and wire format.
type InvoiceIntent struct {
	Peer []byte `json:"Peer"`
	// RecoveryRequest fixes fallback templates and public nonces before
	// any request is sent. Private nonces remain in the durable signer.
	RecoveryRequest []byte `json:"RecoveryRequest"`
}

// PaymentTerms translates the immutable client agreement to native units.
func PaymentTerms(offer assetexec.Offer, holdDelta uint32) assetln.Terms {
	return assetln.Terms{
		Hash: offer.Hash, GroupKey: offer.Terms.GroupKey,
		Amount:          offer.Terms.PayoutAmount,
		MaxDebit:        offer.Terms.MaxDebit,
		RefundHeight:    offer.Terms.RefundHeight,
		RecoveryBlocks:  offer.Terms.RecoveryDelta,
		HoldExpiryDelta: holdDelta,
		Deadline:        offer.Terms.OfferExpiry.Unix(),
	}
}

// CreateInvoice looks up a previous attempt before creating the held payout.
func (a *Adapter) CreateInvoice(ctx context.Context, offer assetexec.Offer,
	intent []byte) (assetexec.Invoice, error) {

	existing, err := a.LookupInvoice(ctx, offer)
	if !errorsNotFound(err) {
		return existing, err
	}
	var request InvoiceIntent
	if err := json.Unmarshal(intent, &request); err != nil {
		return assetexec.Invoice{}, err
	}
	height, err := a.Height(ctx)
	if err != nil {
		return assetexec.Invoice{}, err
	}
	invoice, err := a.Receiver.Create(ctx,
		PaymentTerms(offer, a.HoldExpiryDelta), request.Peer, height)
	if err != nil {
		return assetexec.Invoice{}, err
	}
	return marshalInvoice(invoice)
}

func errorsNotFound(err error) bool {
	return errors.Is(err, assetexec.ErrNotFound) ||
		status.Code(err) == codes.NotFound
}

func marshalInvoice(invoice *assetln.HoldInvoice) (assetexec.Invoice, error) {
	if invoice == nil {
		return assetexec.Invoice{}, assetexec.ErrInvalid
	}
	raw, err := json.Marshal(invoice)
	return assetexec.Invoice{
		PaymentRequest: invoice.PaymentRequest, PaymentPlan: raw,
	}, err
}

// LookupInvoice recovers the same hash without generating another quote.
func (a *Adapter) LookupInvoice(ctx context.Context, offer assetexec.Offer) (
	assetexec.Invoice, error) {

	invoice, err := a.Receiver.Recover(ctx,
		PaymentTerms(offer, a.HoldExpiryDelta))
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return assetexec.Invoice{}, assetexec.ErrNotFound
		}
		return assetexec.Invoice{}, err
	}
	return marshalInvoice(invoice)
}

// Prepared queries the recipient's local committed asset HTLC evidence.
func (a *Adapter) Prepared(ctx context.Context, offer assetexec.Offer) (
	[]byte, error) {

	height, err := a.Height(ctx)
	if err != nil {
		return nil, err
	}
	evidence, err := a.Receiver.Prepared(ctx,
		PaymentTerms(offer, a.HoldExpiryDelta), height)
	if err != nil {
		return nil, err
	}
	return json.Marshal(evidence)
}

// PushSignatures sends claim authority only after the controller journals it.
func (a *Adapter) PushSignatures(ctx context.Context, offer assetexec.Offer,
	signatures []byte) error {

	_, err := a.Server.CommitAssetLoopIn(ctx,
		&swapserverrpc.CommitAssetLoopInRequest{
			PaymentHash:      offer.Hash[:],
			ClientSignatures: signatures,
		})
	return err
}

// LookupPreimage first recovers a locally settled invoice, then polls an
// already committed server disclosure and the on-chain success witness.
func (a *Adapter) LookupPreimage(ctx context.Context, offer assetexec.Offer) (
	[32]byte, bool, error) {

	invoice, localErr := a.Receiver.Lookup(ctx, offer.Hash)
	if localErr == nil && invoice != nil &&
		invoice.State == lnrpc.Invoice_SETTLED {

		var preimage [32]byte
		if !bytes.Equal(invoice.RHash, offer.Hash[:]) ||
			len(invoice.RPreimage) != len(preimage) {

			return preimage, false, assetexec.ErrInvalid
		}
		copy(preimage[:], invoice.RPreimage)
		if sha256.Sum256(preimage[:]) != offer.Hash {
			return [32]byte{}, false, assetexec.ErrInvalid
		}
		return preimage, true, nil
	}
	// The peer cannot consume the discovery budget needed to inspect a
	// confirmed success witness. A silent or malformed reply falls through
	// to the independently authenticated chain observation.
	timeout := 250 * time.Millisecond
	if deadline, ok := ctx.Deadline(); ok {
		timeout = min(timeout, time.Until(deadline)/4)
	}
	peerCtx, cancel := context.WithTimeout(ctx, timeout)
	result, rpcErr := a.Server.GetAssetLoopIn(peerCtx,
		&swapserverrpc.AssetLoopInReference{PaymentHash: offer.Hash[:]})
	cancel()
	if rpcErr == nil && len(result.GetExecutionPreimage()) != 0 {
		var preimage [32]byte
		if len(result.ExecutionPreimage) == len(preimage) {
			copy(preimage[:], result.ExecutionPreimage)
			if sha256.Sum256(preimage[:]) == offer.Hash {
				return preimage, true, nil
			}
		}
		rpcErr = assetexec.ErrInvalid
	}
	// A server response is not the only disclosure channel. The SDK chain
	// driver watches the HTLC success witness, including after a restart.
	preimage, found, err := a.Actions.LookupPreimage(ctx, offer)
	if err != nil || found {
		return preimage, found, err
	}
	return preimage, false, rpcErr
}

// SettleInvoice supplies a verified execution preimage to local LND.
func (a *Adapter) SettleInvoice(ctx context.Context, offer assetexec.Offer,
	preimage [32]byte) error {

	return a.Receiver.Settle(ctx, offer.Hash, preimage)
}

// InvoiceSettled ties the settlement to the previously accepted asset circuit.
func (a *Adapter) InvoiceSettled(ctx context.Context, offer assetexec.Offer) (
	bool, error) {

	record, err := a.Store.Get(ctx, offer.Hash)
	if err != nil {
		return false, err
	}
	var evidence assetln.Evidence
	if err := json.Unmarshal(
		record.AcceptedEvidence, &evidence,
	); err != nil {
		return false, err
	}
	return a.Receiver.Settled(ctx,
		PaymentTerms(offer, a.HoldExpiryDelta), evidence)
}

// CancelInvoice is called only after the controller proves cancellation safe.
func (a *Adapter) CancelInvoice(ctx context.Context,
	offer assetexec.Offer) error {

	return a.Receiver.CancelBeforeExposure(ctx, offer.Hash)
}

// OfferFromRPC validates binary encodings before creating local recovery state.
// The SDK adapter still independently verifies the full contract and proofs.
func OfferFromRPC(owner [32]byte, response *swapserverrpc.AssetLoopInStatus) (
	assetexec.Offer, error) {

	if response == nil || response.Offer == nil {
		return assetexec.Offer{}, assetexec.ErrInvalid
	}
	rpc := response.Offer
	if len(rpc.PaymentHash) != 32 || len(rpc.GroupKey) != 33 ||
		len(rpc.RequestId) != 32 || len(rpc.RequestDigest) != 32 {

		return assetexec.Offer{}, assetexec.ErrInvalid
	}
	var offer assetexec.Offer
	copy(offer.Hash[:], rpc.PaymentHash)
	copy(offer.Terms.GroupKey[:], rpc.GroupKey)
	copy(offer.Terms.RequestID[:], rpc.RequestId)
	copy(offer.Terms.RequestDigest[:], rpc.RequestDigest)
	terms := &offer.Terms
	terms.Owner, terms.Version = owner, rpc.ProtocolVersion
	terms.DepositAmount = rpc.DepositAmount
	terms.PayoutAmount = rpc.PayoutAmount
	terms.AssetFee, terms.ChangeAmount = rpc.AssetFee, rpc.ChangeAmount
	terms.MaxDebit, terms.RefundHeight = rpc.MaxAssetDebit, rpc.RefundHeight
	terms.MinPayoutExpiry = rpc.MinPayoutExpiry
	terms.RecoveryDelta = rpc.RecoveryDelta
	terms.ExecutionDelta = rpc.ExecutionDelta
	terms.OfferExpiry, terms.Contract = time.Unix(rpc.OfferExpiry, 0),
		bytes.Clone(rpc.Contract)
	key, err := btcec.ParsePubKey(terms.GroupKey[:])
	if err != nil || !bytes.Equal(key.SerializeCompressed(),
		terms.GroupKey[:]) || terms.RequestID == ([32]byte{}) ||
		terms.RequestDigest == ([32]byte{}) {

		return assetexec.Offer{}, assetexec.ErrInvalid
	}
	terms.DepositOutpoints, err = depositOutpoints(rpc.Deposits)
	if err != nil {
		return assetexec.Offer{}, err
	}
	return offer, nil
}

func depositOutpoints(deposits []*swapserverrpc.AssetDepositInput) (
	[]string, error) {

	points := make([]string, 0, len(deposits))
	seen := make(map[string]bool, len(deposits))
	for _, deposit := range deposits {
		if deposit == nil || len(deposit.Txid) != 32 {
			return nil, assetexec.ErrInvalid
		}
		txid := bytes.Clone(deposit.Txid)
		for i := 0; i < len(txid)/2; i++ {
			last := len(txid) - 1 - i
			txid[i], txid[last] = txid[last], txid[i]
		}
		point := fmt.Sprintf("%x:%d", txid, deposit.OutputIndex)
		if seen[point] {
			return nil, assetexec.ErrInvalid
		}
		seen[point] = true
		points = append(points, point)
	}
	return points, nil
}

var _ assetexec.Actions = (*Adapter)(nil)
