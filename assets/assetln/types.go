// Package assetln prepares same-group asset payments without disclosing the
// payer's preimage. Callers persist payment intent before dispatch and retain
// their recovery state until both the payment and the on-chain claim resolve.
package assetln

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lnrpc"
)

// ErrEvidence means the local node has not proved a safe asset payout. It must
// never cause a caller to release already exposed claim signatures or locks.
var ErrEvidence = errors.New("asset payment evidence is not sufficient")

// Terms binds a held payout to its on-chain claim and native-unit limits.
type Terms struct {
	Hash            [32]byte
	GroupKey        [33]byte
	Amount          uint64
	MaxDebit        uint64
	RefundHeight    uint32
	RecoveryBlocks  uint32
	HoldExpiryDelta uint32
	Deadline        int64
}

// Validate checks immutable terms before any quote or payment is created.
func (t Terms) Validate(now time.Time) error {
	key, err := btcec.ParsePubKey(t.GroupKey[:])
	if err != nil ||
		!bytes.Equal(key.SerializeCompressed(), t.GroupKey[:]) {
		return fmt.Errorf("invalid canonical group key")
	}
	if t.Hash == ([32]byte{}) || t.Amount == 0 || t.MaxDebit < t.Amount ||
		t.RefundHeight == 0 || t.RecoveryBlocks == 0 ||
		t.HoldExpiryDelta == 0 || t.Deadline <= now.Unix() {

		return fmt.Errorf("invalid asset payment terms")
	}
	if uint64(t.RefundHeight)+uint64(t.RecoveryBlocks)+
		uint64(t.HoldExpiryDelta) >= math.MaxInt32 {

		return fmt.Errorf("asset payment expiry overflow")
	}
	return nil
}

// Evidence identifies one accepted, locally committed asset HTLC. The first
// protocol version deliberately excludes multipart and AMP payments.
type Evidence struct {
	Hash         [32]byte
	GroupKey     [33]byte
	Amount       uint64
	ChannelID    uint64
	HTLCIndex    uint64
	ExpiryHeight uint32
	RFQID        [32]byte
}

// channelData and htlcData mirror tapd's RPC JSON contract. Keeping these
// transport types local avoids coupling the payment API to Bitcoin libraries.
type channelData struct {
	GroupKey      string `json:"group_key"`
	FundingAssets []struct {
		Genesis struct {
			AssetID string `json:"asset_id"`
		} `json:"asset_genesis"`
	} `json:"funding_assets"`
}

type htlcData struct {
	Balances []struct {
		AssetID string `json:"asset_id"`
		Amount  uint64 `json:"amount"`
	} `json:"balances"`
	RFQID string `json:"rfq_id"`
}

// nativeAmount binds tapd's decoded HTLC to an authenticated local channel.
// Group placeholders are x-only keys; concrete tranches must occur in that
// channel's funding assets. Caller data is never sufficient for this check.
func nativeAmount(group [33]byte, raw, channelRaw []byte) (
	uint64, [32]byte, error) {

	var id [32]byte
	var channel channelData
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
	var data htlcData
	if err := json.Unmarshal(raw, &data); err != nil ||
		len(data.Balances) == 0 {

		return 0, id, ErrEvidence
	}
	rfq, err := hex.DecodeString(data.RFQID)
	if err != nil || len(rfq) != len(id) ||
		hex.EncodeToString(rfq) != data.RFQID {

		return 0, id, ErrEvidence
	}
	copy(id[:], rfq)
	allowed := map[string]bool{hex.EncodeToString(group[1:]): true}
	for _, a := range channel.FundingAssets {
		allowed[a.Genesis.AssetID] = true
	}
	var amount uint64
	seen := make(map[string]bool)
	for _, balance := range data.Balances {
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
func Accepted(terms Terms, invoice *lnrpc.Invoice,
	channels []*lnrpc.Channel, height uint32) (*Evidence, error) {

	if invoice == nil || invoice.State != lnrpc.Invoice_ACCEPTED ||
		!bytes.Equal(invoice.RHash, terms.Hash[:]) ||
		height >= terms.RefundHeight {

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
	minimum := uint64(terms.RefundHeight) + uint64(terms.RecoveryBlocks) +
		uint64(terms.HoldExpiryDelta)
	if accepted == nil || accepted.ExpiryHeight <= 0 ||
		uint64(accepted.ExpiryHeight) <= minimum {

		return nil, ErrEvidence
	}
	for _, channel := range channels {
		if channel == nil || channel.ChanId != accepted.ChanId {
			continue
		}
		amount, rfq, err := nativeAmount(
			terms.GroupKey, accepted.CustomChannelData,
			channel.CustomChannelData,
		)
		if err != nil || amount != terms.Amount {
			return nil, ErrEvidence
		}
		return &Evidence{
			Hash:         terms.Hash,
			GroupKey:     terms.GroupKey,
			Amount:       amount,
			ChannelID:    accepted.ChanId,
			HTLCIndex:    accepted.HtlcIndex,
			ExpiryHeight: uint32(accepted.ExpiryHeight), RFQID: rfq,
		}, nil
	}
	return nil, ErrEvidence
}
