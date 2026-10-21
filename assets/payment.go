package assets

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/loop/swapkit"
	"github.com/lightninglabs/taproot-assets/rfqmsg"
	"github.com/lightningnetwork/lnd/lnrpc"
)

// ErrEvidence means the local node has not proved a safe asset payout. It must
// never cause a caller to release already exposed claim signatures or locks.
var ErrEvidence = errors.New("asset payment evidence is not sufficient")

// Evidence identifies one accepted, locally committed asset HTLC. The first
// protocol version deliberately excludes multipart and AMP payments.
//
//nolint:tagliatelle // Preserve the existing recovery and wire format.
type Evidence struct {
	Hash         [32]byte `json:"Hash"`
	GroupKey     [33]byte `json:"GroupKey"`
	Amount       uint64   `json:"Amount"`
	ChannelID    uint64   `json:"ChannelID"`
	HTLCIndex    uint64   `json:"HTLCIndex"`
	ExpiryHeight uint32   `json:"ExpiryHeight"`
	RFQID        [32]byte `json:"RFQID"`
}

// NativeAmount binds tapd's decoded HTLC to an authenticated local channel.
// Group placeholders are x-only keys; concrete tranches must occur in that
// channel's funding assets. Caller data is never sufficient for this check.
func NativeAmount(group [33]byte, raw, channelRaw []byte) (
	uint64, [32]byte, error) {

	var id [32]byte
	var channel rfqmsg.JsonAssetChannel
	if err := json.Unmarshal(channelRaw, &channel); err != nil {
		return 0, id, ErrEvidence
	}
	channelGroup, err := hex.DecodeString(channel.GroupKey)
	if err != nil || len(channelGroup) != 33 ||
		hex.EncodeToString(channelGroup) != channel.GroupKey {

		return 0, id, ErrEvidence
	}
	key, err := btcec.ParsePubKey(channelGroup)
	if err != nil ||
		!bytes.Equal(key.SerializeCompressed(), channelGroup) ||
		!bytes.Equal(channelGroup, group[:]) {

		return 0, id, ErrEvidence
	}
	var data rfqmsg.JsonHtlc
	if err := json.Unmarshal(raw, &data); err != nil ||
		len(data.Balances) == 0 {

		return 0, id, ErrEvidence
	}
	rfq, err := hex.DecodeString(data.RfqID)
	if err != nil || len(rfq) != len(id) ||
		hex.EncodeToString(rfq) != data.RfqID {

		return 0, id, ErrEvidence
	}
	copy(id[:], rfq)
	allowed := map[string]bool{hex.EncodeToString(group[1:]): true}
	for _, a := range channel.FundingAssets {
		allowed[a.AssetGenesis.AssetID] = true
	}
	var amount uint64
	seen := make(map[string]bool)
	for _, balance := range data.Balances {
		if balance == nil {
			return 0, id, ErrEvidence
		}
		assetID, err := hex.DecodeString(balance.AssetID)
		if err != nil || len(assetID) != 32 ||
			hex.EncodeToString(assetID) != balance.AssetID ||
			!allowed[balance.AssetID] || seen[balance.AssetID] ||
			balance.Amount == 0 ||
			balance.Amount > math.MaxUint64-amount {

			return 0, id, ErrEvidence
		}
		seen[balance.AssetID] = true
		amount += balance.Amount
	}
	return amount, id, nil
}

// Accepted verifies local invoice and channel evidence before client claim
// signatures may be exposed. It is also safe to repeat after a restart.
func Accepted(offer swapkit.Offer, holdDelta uint32, invoice *lnrpc.Invoice,
	channels []*lnrpc.Channel, height uint32) (*Evidence, error) {

	if invoice == nil || invoice.State != lnrpc.Invoice_ACCEPTED ||
		!bytes.Equal(invoice.RHash, offer.Hash[:]) ||
		height >= offer.Terms.RefundHeight {

		return nil, ErrEvidence
	}
	var accepted *lnrpc.InvoiceHTLC
	for _, htlc := range invoice.Htlcs {
		if htlc == nil ||
			htlc.State == lnrpc.InvoiceHTLCState_CANCELED {

			continue
		}
		if accepted != nil || htlc.Amp != nil ||
			htlc.State != lnrpc.InvoiceHTLCState_ACCEPTED {

			return nil, ErrEvidence
		}
		accepted = htlc
	}
	minimum := uint64(offer.Terms.RefundHeight) +
		uint64(offer.Terms.RecoveryDelta) +
		uint64(holdDelta)
	if accepted == nil || accepted.ExpiryHeight <= 0 ||
		uint64(accepted.ExpiryHeight) <= minimum {

		return nil, ErrEvidence
	}
	for _, channel := range channels {
		if channel == nil || channel.ChanId != accepted.ChanId {
			continue
		}
		amount, rfq, err := NativeAmount(
			offer.Terms.GroupKey, accepted.CustomChannelData,
			channel.CustomChannelData,
		)
		if err != nil || amount != offer.Terms.PayoutAmount {
			return nil, ErrEvidence
		}
		return &Evidence{
			Hash:         offer.Hash,
			GroupKey:     offer.Terms.GroupKey,
			Amount:       amount,
			ChannelID:    accepted.ChanId,
			HTLCIndex:    accepted.HtlcIndex,
			ExpiryHeight: uint32(accepted.ExpiryHeight), RFQID: rfq,
		}, nil
	}
	return nil, ErrEvidence
}

// ValidateInvoice checks the carrier hash, amount and lifetime.
func ValidateInvoice(offer swapkit.Offer, decoded *lnrpc.PayReq,
	now time.Time) error {

	if decoded == nil ||
		decoded.PaymentHash != hex.EncodeToString(offer.Hash[:]) ||
		decoded.NumMsat <= 0 || decoded.Expiry <= 0 ||
		decoded.Timestamp > math.MaxInt64-decoded.Expiry ||
		decoded.Timestamp+decoded.Expiry < offer.Terms.OfferExpiry.Unix() ||
		decoded.Timestamp+decoded.Expiry <= now.Unix() {

		return ErrEvidence
	}
	return nil
}

// Settled verifies that the previously accepted circuit actually settled.
// The binding survives a channel close because the acceptance evidence was
// persisted before exposure. A different circuit or a Bitcoin-only payment
// cannot authorize the client's cooperative deposit signature.
func Settled(offer swapkit.Offer, evidence Evidence,
	invoice *lnrpc.Invoice) (bool, error) {

	if evidence.Hash != offer.Hash || evidence.GroupKey != offer.Terms.GroupKey ||
		evidence.Amount != offer.Terms.PayoutAmount {

		return false, ErrEvidence
	}
	if invoice == nil || !bytes.Equal(invoice.RHash, offer.Hash[:]) {
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
