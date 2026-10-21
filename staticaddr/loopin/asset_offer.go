package loopin

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/loop/swapkit"
	"github.com/lightninglabs/loop/swapserverrpc"
	"google.golang.org/protobuf/proto"
)

// assetOfferFromRPC validates binary encodings before creating recovery state.
// The SDK adapter still independently verifies the full contract and proofs.
func assetOfferFromRPC(owner [32]byte,
	response *swapserverrpc.AssetLoopInStatus) (swapkit.Offer, error) {

	if response == nil || response.Offer == nil {
		return swapkit.Offer{}, swapkit.ErrInvalid
	}
	rpc := response.Offer
	if len(rpc.PaymentHash) != 32 || len(rpc.GroupKey) != 33 ||
		len(rpc.RequestId) != 32 || len(rpc.RequestDigest) != 32 {

		return swapkit.Offer{}, swapkit.ErrInvalid
	}
	var offer swapkit.Offer
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
		terms.RequestDigest == ([32]byte{}) ||
		offer.Hash == ([32]byte{}) || owner == ([32]byte{}) {

		return swapkit.Offer{}, swapkit.ErrInvalid
	}
	terms.DepositOutpoints, err = assetDepositOutpoints(rpc.Deposits)
	if err != nil {
		return swapkit.Offer{}, err
	}
	return offer, nil
}

func assetDepositOutpoints(deposits []*swapserverrpc.AssetDepositInput) (
	[]string, error) {

	points := make([]string, 0, len(deposits))
	seen := make(map[string]bool, len(deposits))
	for _, deposit := range deposits {
		if deposit == nil || len(deposit.Txid) != 32 {
			return nil, swapkit.ErrInvalid
		}
		txid := bytes.Clone(deposit.Txid)
		for i := 0; i < len(txid)/2; i++ {
			last := len(txid) - 1 - i
			txid[i], txid[last] = txid[last], txid[i]
		}
		point := fmt.Sprintf("%x:%d", txid, deposit.OutputIndex)
		if seen[point] {
			return nil, swapkit.ErrInvalid
		}
		seen[point] = true
		points = append(points, point)
	}
	return points, nil
}

// validateAssetOffer binds the peer's terms to the exact request and deposit.
func validateAssetOffer(owner [32]byte,
	req *swapserverrpc.CreateAssetLoopInRequest,
	response *swapserverrpc.AssetLoopInStatus) (swapkit.Offer, error) {

	offer, err := assetOfferFromRPC(owner, response)
	if err != nil {
		return offer, err
	}
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(req)
	if err != nil {
		return offer, err
	}
	points, err := assetDepositOutpoints(req.Deposits)
	if err != nil {
		return offer, err
	}
	terms := offer.Terms
	if terms.Version != req.ProtocolVersion ||
		!bytes.Equal(terms.RequestID[:], req.RequestId) ||
		terms.RequestDigest != sha256.Sum256(raw) ||
		!bytes.Equal(terms.GroupKey[:], req.GroupKey) ||
		terms.PayoutAmount != req.PayoutAmount ||
		terms.MaxDebit != req.MaxAssetDebit || terms.AssetFee > req.MaxAssetFee ||
		!bytes.Equal(terms.Contract, req.Contract) || len(points) != 1 ||
		len(terms.DepositOutpoints) != 1 || points[0] != terms.DepositOutpoints[0] ||
		terms.DepositAmount < terms.PayoutAmount ||
		terms.DepositAmount-terms.PayoutAmount != terms.AssetFee ||
		terms.ChangeAmount != 0 || terms.RecoveryDelta == 0 ||
		terms.ExecutionDelta == 0 || terms.RefundHeight == 0 ||
		uint64(terms.MinPayoutExpiry) <= uint64(terms.RefundHeight)+
			uint64(terms.RecoveryDelta)+uint64(terms.ExecutionDelta) ||
		!time.Now().Before(terms.OfferExpiry) {

		return offer, swapkit.ErrInvalid
	}
	return offer, nil
}
