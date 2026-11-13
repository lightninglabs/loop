package reservation

import (
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	assetpolicy "github.com/lightninglabs/loop/internal/assetpolicy"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightningnetwork/lnd/keychain"
)

// ReservationKeys is the LND wallet key interface used by both parties.
type ReservationKeys interface {
	DeriveNextKey(context.Context, int32) (*keychain.KeyDescriptor, error)
	DeriveKey(context.Context, *keychain.KeyLocator) (*keychain.KeyDescriptor,
		error)
}

// WalletVerifier checks reservations with the local tapd and LND nodes.
// It never asks the server whether the client can use its reservation.
type WalletVerifier struct {
	Keys      ReservationKeys
	Tap       ProofVerifier
	Chain     *ChainReader
	Params    *address.ChainParams
	KeyFamily int32
}

var _ ReservationVerifier = (*WalletVerifier)(nil)

// Validate checks dependencies before the reservation manager starts.
func (w *WalletVerifier) Validate() error {
	if w == nil || w.Keys == nil || w.Tap == nil || w.Params == nil ||
		w.Params.Params == nil || w.KeyFamily <= 0 {

		return errors.New("incomplete reservation proof verifier")
	}
	return w.Chain.Validate()
}

// DeriveKey obtains the client's local co-signing key from LND.
func (w *WalletVerifier) DeriveKey(ctx context.Context) (
	*keychain.KeyDescriptor, error) {

	if err := w.Validate(); err != nil {
		return nil, err
	}
	if err := assetpolicy.RequireOnchainSwapTapd(ctx, w.Tap); err != nil {
		return nil, err
	}
	return w.Keys.DeriveNextKey(ctx, w.KeyFamily)
}

// Verify checks the full proof, exact confirmed anchor, and any known spend.
// The FSM enforces confirmation depth and the original CSV lifetime.
func (w *WalletVerifier) Verify(ctx context.Context, r *Reservation,
	raw []byte) (FundingStatus, error) {

	var result FundingStatus
	if err := w.Validate(); err != nil {
		return result, err
	}
	if err := r.Validate(); err != nil {
		return result, err
	}
	if r.Quote == nil || r.FundingOutpoint == nil {
		return result, ErrInvalidReservation
	}
	key, err := w.Keys.DeriveKey(ctx, &r.ClientKey.KeyLocator)
	if err != nil {
		return result, err
	}
	if key == nil || key.PubKey == nil ||
		!key.PubKey.IsEqual(r.ClientKey.PubKey) {

		return result, ErrInvalidReservation
	}
	server, err := btcec.ParsePubKey(r.Quote.ServerKey)
	if err != nil {
		return result, ErrInvalidReservation
	}
	kit, err := NewKit(server, key.PubKey,
		asset.ID(r.AssetID), r.CSVDelay, w.Params)
	if err != nil {
		return result, err
	}
	// tapd answers an unverifiable proof and an invalid one alike. Check
	// our own chain view of the anchor block first, so that a node which
	// has not reached that block yet is retried and not taken for a bad
	// proof.
	if err := w.checkAnchorBlock(ctx, raw); err != nil {
		return result, err
	}
	terminal, err := VerifyReservationProof(ctx, w.Tap, kit, raw,
		*r.FundingOutpoint, r.Amount)
	if err != nil {
		return result, err
	}
	if r.GroupKey != ([33]byte{}) && !IsGroupMember(&terminal.Asset, r.GroupKey[:]) {
		return result, ErrInvalidReservation
	}
	index := r.FundingOutpoint.Index
	if uint64(index) >= uint64(len(terminal.AnchorTx.TxOut)) {
		return result, ErrInvalidReservation
	}
	result, err = w.Chain.Observe(ctx, *r.FundingOutpoint,
		terminal.AnchorTx.TxOut[index], terminal.BlockHeight)
	if err != nil {
		return result, err
	}
	if result.ConfirmationHeight != terminal.BlockHeight {
		return result, ErrInvalidReservation
	}
	return result, nil
}

// checkAnchorBlock compares the proof's anchor block with the local chain. A
// block the node cannot serve yet is a temporary error. A different block at
// that height means the proof does not belong to the active chain.
func (w *WalletVerifier) checkAnchorBlock(ctx context.Context,
	raw []byte) error {

	if len(raw) == 0 || len(raw) > maxReservationProofSize {
		return ErrInvalidReservation
	}
	file, err := proof.DecodeFile(raw)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidReservation, err)
	}
	terminal, err := file.LastProof()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidReservation, err)
	}
	hash, err := w.Chain.Chain.GetBlockHash(
		ctx, int64(terminal.BlockHeight),
	)
	if err != nil {
		return err
	}
	if hash != terminal.BlockHeader.BlockHash() {
		return ErrInvalidReservation
	}
	return nil
}

// Inspect watches the verified reservation for a spend or expiry progress.
func (w *WalletVerifier) Inspect(ctx context.Context,
	r *Reservation) (FundingStatus, error) {

	var status FundingStatus
	if err := w.Validate(); err != nil {
		return status, err
	}
	if r.FundingOutpoint == nil || r.ConfirmationHeight == 0 {
		return status, ErrInvalidReservation
	}
	file, err := proof.DecodeFile(r.ReservationProof)
	if err != nil {
		return status, err
	}
	terminal, err := file.LastProof()
	if err != nil {
		return status, err
	}
	point := *r.FundingOutpoint
	if terminal.AnchorTx.TxHash() != point.Hash ||
		uint64(point.Index) >= uint64(len(terminal.AnchorTx.TxOut)) {

		return status, ErrInvalidReservation
	}
	// A reorg can reconfirm the exact transaction at the same height.
	// Check its block as well as its outpoint on every ready watch.
	hash, err := w.Chain.Chain.GetBlockHash(ctx, int64(terminal.BlockHeight))
	if err != nil {
		return status, err
	}
	if hash != terminal.BlockHeader.BlockHash() {
		return status, ErrInvalidReservation
	}
	output := terminal.AnchorTx.TxOut[point.Index]
	status, err = w.Chain.Observe(ctx, point, output, r.ConfirmationHeight)
	if err != nil || status.Spent {
		return status, err
	}
	lifetime, err := r.Terms.Lifetime(status.ConfirmationHeight)
	if err != nil || status.Height >= lifetime.TimeoutHeight {
		return status, err
	}
	return w.Chain.WaitForChange(ctx, point, output.PkScript,
		status.ConfirmationHeight, status)
}
