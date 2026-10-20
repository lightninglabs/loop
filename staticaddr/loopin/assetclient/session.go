package assetclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"sync/atomic"
	"time"

	"github.com/lightninglabs/loop/staticaddr/loopin/assetexec"
	"github.com/lightninglabs/loop/swapkit/bridge"
	"github.com/lightninglabs/loop/swapserverrpc"
	"google.golang.org/protobuf/proto"
)

// Session connects the existing deposit/signing driver, durable controller,
// local asset node and server RPC. The server's execution worker is independent
// of this client session: Prepare only grants conditional claim authority.
type Session struct {
	Controller *assetexec.Controller
	Adapter    *Adapter
	running    atomic.Bool
}

// RequestOffer retrieves an idempotent server offer. The caller persists the
// request ID and selected deposits before this call so a lost response can be
// recovered. No asset claim signature is produced during this operation.
func (a *Adapter) RequestOffer(
	ctx context.Context,
	owner [32]byte,
	request *swapserverrpc.CreateAssetLoopInRequest,
) (assetexec.Offer, error) {

	if request == nil || proto.Size(request) > bridge.PublicRPCMaxBytes ||
		request.ProtocolVersion != 1 ||
		len(request.RequestId) != 32 ||
		bytes.Equal(request.RequestId, make([]byte, 32)) {

		return assetexec.Offer{}, assetexec.ErrInvalid
	}
	selected, err := depositOutpoints(request.Deposits)
	if err != nil || len(selected) == 0 {
		return assetexec.Offer{}, assetexec.ErrInvalid
	}
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(request)
	if err != nil {
		return assetexec.Offer{}, err
	}
	digest := sha256.Sum256(raw)
	response, err := a.Server.CreateAssetLoopIn(ctx, request)
	if err != nil {
		return assetexec.Offer{}, err
	}
	offer, err := OfferFromRPC(owner, response)
	if err != nil {
		return assetexec.Offer{}, err
	}
	terms := offer.Terms
	if terms.Version != request.ProtocolVersion ||
		!bytes.Equal(terms.RequestID[:], request.RequestId) ||
		terms.RequestDigest != digest ||
		!bytes.Equal(terms.GroupKey[:], request.GroupKey) ||
		terms.PayoutAmount != request.PayoutAmount ||
		terms.MaxDebit > request.MaxAssetDebit ||
		terms.AssetFee > request.MaxAssetFee ||
		!bytes.Equal(terms.Contract, request.Contract) ||
		!sameDeposits(selected, terms.DepositOutpoints) {

		return assetexec.Offer{}, assetexec.ErrInvalid
	}
	return offer, nil
}

// Prepare drives the network-time exchange from a previously persisted Start.
// Retries use the same invoice, fallback templates, and signatures. Neither a
// payment timeout nor an RPC error clears the controller's exposure record.
func (s *Session) Prepare(ctx context.Context, hash [32]byte) error {
	record, err := s.Adapter.Store.Get(ctx, hash)
	if err != nil {
		return err
	}
	var intent InvoiceIntent
	if err := json.Unmarshal(record.InvoiceIntent, &intent); err != nil {
		return err
	}
	if len(intent.RecoveryRequest) == 0 {
		return assetexec.ErrInvalid
	}
	invoice, err := s.Controller.PrepareInvoice(ctx, hash)
	if err != nil {
		return err
	}
	_, err = s.Adapter.Server.PrepareAssetLoopIn(ctx,
		&swapserverrpc.PrepareAssetLoopInRequest{
			PaymentHash:      hash[:],
			AssetHoldInvoice: invoice.PaymentRequest,
			RecoveryRequest:  intent.RecoveryRequest,
		})
	if err != nil {
		return err
	}
	fallback, err := s.Adapter.Server.AssetLoopInFallback(ctx,
		&swapserverrpc.AssetLoopInReference{PaymentHash: hash[:]})
	if err != nil {
		return err
	}
	if err := s.Controller.ReceiveFallback(ctx, hash,
		fallback.GetRecoveryBundle()); err != nil {
		return err
	}
	return s.Controller.ExposeClaim(ctx, hash)
}

// Recover resumes settlement or independent on-chain recovery after restart.
func (s *Session) Recover(ctx context.Context, hash [32]byte) error {
	record, err := s.Adapter.Store.Get(ctx, hash)
	if err != nil {
		return err
	}
	if !record.CancellationRequested && !record.SignaturesExposed &&
		(record.State == assetexec.InitHtlcTx ||
			record.State == assetexec.SignHtlcTx) {

		if !time.Now().Before(record.Offer.Terms.OfferExpiry) {
			return s.Controller.Cancel(ctx, hash)
		}
		return s.Prepare(ctx, hash)
	}
	return s.Controller.Recover(ctx, hash)
}

func sameDeposits(selected, offered []string) bool {
	if len(selected) != len(offered) {
		return false
	}
	set := make(map[string]bool, len(selected))
	for _, point := range selected {
		if set[point] {
			return false
		}
		set[point] = true
	}
	for _, point := range offered {
		if !set[point] {
			return false
		}
		delete(set, point)
	}
	return len(set) == 0
}
