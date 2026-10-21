package tapsdk

import (
	"context"
	"crypto/sha256"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
)

func walletOutput(public [33]byte, family int32, index uint32,
	ref sdk.AssetRef, amount uint64, value int64) (
	sdk.CustomAssetOutput, error) {

	pub, err := btcec.ParsePubKey(public[:])
	if err != nil {
		return sdk.CustomAssetOutput{}, err
	}
	tweaked := txscript.ComputeTaprootKeyNoScript(pub)
	even, err := schnorr.ParsePubKey(schnorr.SerializePubKey(tweaked))
	if err != nil {
		return sdk.CustomAssetOutput{}, err
	}
	scriptKey, err := sdk.ParsePubKey(even.SerializeCompressed())
	if err != nil {
		return sdk.CustomAssetOutput{}, err
	}
	loc := sdk.KeyLocator{Family: uint32(family), Index: index}
	return sdk.CustomAssetOutput{
		ID: "recipient", AssetRef: ref, Amount: amount,
		AnchorValueSat: uint64(value),
		Script: sdk.CustomAssetScriptPlan{
			Mode: sdk.CustomAssetScriptExternal,
			External: &sdk.CustomAssetExternalScriptPlan{
				ScriptKey: sdk.ScriptKey{PubKey: scriptKey,
					KeyDesc: sdk.KeyDescriptor{
						RawKeyBytes: public,
						KeyLocator:  loc,
					}},
			},
		},
		Anchor: sdk.CustomAnchorOutputPlan{
			InternalKey: sdk.InternalKey{
				PubKey: public, KeyLocator: loc,
			},
		},
	}, nil
}

// DiscoverOutcome reconstructs an offline peer's proof from its Bitcoin spend.
// It verifies the agreed leaf and observed witness before observer logging.
func DiscoverOutcome(ctx context.Context, wallet *sdk.Wallet,
	chain Chain, minConfs uint32, s *SwapContext,
	refund bool) ([]byte, error) {

	if s.Fallback == nil {
		return nil, swapkit.ErrPending
	}
	d := s.Deposit
	c := d.Contract
	out := s.Fallback.Outputs[0]
	point := SDKPoint(out.AnchorOutpoint)
	// The server may only have its public partial after a lost reply.
	// The committed unsigned anchor still fixes the output to watch.
	anchor, err := DecodePSBT(s.Fallback.AnchorPsbt)
	if err != nil {
		return nil, err
	}
	block, _, err := chain.Spend(ctx, point,
		anchor.UnsignedTx.TxOut[point.Index].PkScript, s.Deposit.Proof.BlockHeight)
	if err != nil {
		return nil, err
	}
	found := spendingTransaction(block, point)
	if len(found.TxIn) != 1 || len(found.TxOut) != 1 {
		return nil, swapkit.ErrInvalid
	}
	policy, err := HTLC(s.Offer, c)
	if err != nil {
		return nil, err
	}
	public := c.ServerKey
	spend, err := policy.SuccessSpend(out.TaprootAssetRoot)
	if refund {
		public = c.ClientKey
		spend, err = policy.TimeoutSpend(out.TaprootAssetRoot)
	}
	if err != nil {
		return nil, err
	}
	if !spendsLeaf(found, out, spend) {
		return nil, swapkit.ErrPending
	}
	fallbackProof, err := FallbackProof(ctx, wallet, chain, minConfs, s, true)
	if err != nil {
		return nil, err
	}
	raw, err := rebuildObservedSpend(
		ctx, wallet, chain, minConfs, out, fallbackProof, public,
		spend, found,
	)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// VerifyOutcome binds a proof to the agreed claim or timeout Bitcoin path.
func VerifyOutcome(ctx context.Context, wallet *sdk.Wallet,
	chain Chain, minConfs uint32, s *SwapContext,
	raw []byte, refund, requireUnspent bool) error {

	d := s.Deposit
	c := d.Contract
	if s.Fallback == nil {
		return swapkit.ErrPending
	}
	prior := s.Fallback.Outputs[0]
	if prior.AssetRef != sdk.AssetRefFromGroupKey(s.Offer.Terms.GroupKey) ||
		prior.Amount != s.Offer.Terms.DepositAmount {

		return swapkit.ErrInvalid
	}
	pub := c.ServerKey
	if refund {
		pub = c.ClientKey
	}
	policy, err := HTLC(s.Offer, c)
	if err != nil {
		return err
	}
	spend, err := policy.SuccessSpend(prior.TaprootAssetRoot)
	if refund {
		spend, err = policy.TimeoutSpend(prior.TaprootAssetRoot)
	}
	if err != nil {
		return err
	}
	err = VerifyWalletSpend(ctx, wallet, chain, minConfs, raw, prior, pub, spend,
		requireUnspent, 0)
	return err
}

// DiscoverPreimage reads only the fixed fallback's current on-chain spend.
func DiscoverPreimage(ctx context.Context, chain Chain,
	s *SwapContext) (swapkit.PreimageResult, error) {

	if s.Fallback == nil {
		return swapkit.PreimageResult{}, nil
	}
	point := SDKPoint(s.Fallback.Outputs[0].AnchorOutpoint)
	anchor, err := DecodePSBT(s.Fallback.AnchorPsbt)
	if err != nil {
		return swapkit.PreimageResult{}, err
	}
	block, _, err := chain.Spend(ctx, point,
		anchor.UnsignedTx.TxOut[point.Index].PkScript, s.Deposit.Proof.BlockHeight)
	if err == swapkit.ErrPending {
		return swapkit.PreimageResult{}, nil
	}
	if err != nil {
		return swapkit.PreimageResult{}, err
	}
	for _, input := range spendingTransaction(block, point).TxIn {
		if input.PreviousOutPoint != point {
			continue
		}
		for _, element := range input.Witness {
			if len(element) != 32 || sha256.Sum256(element) != s.Offer.Hash {
				continue
			}
			var preimage [32]byte
			copy(preimage[:], element)
			return swapkit.PreimageResult{Preimage: preimage, Found: true}, nil
		}
	}
	return swapkit.PreimageResult{}, nil
}

// FallbackProof locates the fixed fallback and logs its confirmed witness.
// SDK Build verifies the returned lineage when constructing its successor;
// validation-only callers verify the complete outcome proof instead.
func FallbackProof(ctx context.Context, wallet *sdk.Wallet,
	chain Chain, minConfs uint32,
	s *SwapContext, observed bool) ([]byte, error) {

	if s.Fallback == nil {
		return nil, swapkit.ErrPending
	}
	contract := s.Deposit.Contract
	if contract.Output.OPTrueSpend == nil {
		return nil, swapkit.ErrInvalid
	}
	if err := declareOPTrue(ctx, wallet, contract.Output.ScriptKey,
		contract.Output.OPTrueSpend.WitnessStack()); err != nil {
		return nil, err
	}
	pkg := s.Fallback
	packet, err := DecodePSBT(pkg.AnchorPsbt)
	if err != nil {
		return nil, err
	}
	if !packet.IsComplete() {
		observed = true
		if len(packet.UnsignedTx.TxIn) != 1 || len(packet.Inputs) != 1 ||
			packet.Inputs[0].WitnessUtxo == nil {

			return nil, swapkit.ErrInvalid
		}
		point := packet.UnsignedTx.TxIn[0].PreviousOutPoint
		block, _, err := chain.Spend(
			ctx, point, packet.Inputs[0].WitnessUtxo.PkScript,
			s.Deposit.Proof.BlockHeight,
		)
		if err != nil {
			return nil, err
		}
		tx := spendingTransaction(block, point)
		if tx == nil || tx.TxHash() != packet.UnsignedTx.TxHash() ||
			len(tx.TxIn[0].Witness) != 1 {

			return nil, swapkit.ErrPending
		}
		requests, err := pkg.SigningRequests()
		if err != nil || len(requests.MuSig2) != 1 {
			return nil, swapkit.ErrInvalid
		}
		signed, err := pkg.ApplyKeyPathSignature(
			requests.MuSig2[0].ID, tx.TxIn[0].Witness[0],
		)
		if err != nil {
			return nil, err
		}
		packet, err = DecodePSBT(signed.AnchorPsbt)
		if err != nil {
			return nil, err
		}
		if err := psbt.MaybeFinalizeAll(packet); err != nil {
			return nil, err
		}
		final, err := EncodePSBT(packet)
		if err != nil {
			return nil, err
		}
		s.Fallback, err = pkg.WithFinalAnchorPSBT(final)
		if err != nil {
			return nil, err
		}
	}
	raw, err := LogAndExport(ctx, wallet, chain, minConfs, s.Fallback, observed)
	if err != nil {
		return nil, err
	}
	// The next SDK Build verifies this input lineage once. A validation-only
	// caller verifies its complete outcome proof, including this lineage.
	return raw, nil
}
