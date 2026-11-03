package tapsdk

import (
	"bytes"
	"context"
	"errors"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/taproot-assets/proof"
)

// AnchorTemplate constructs the Bitcoin transaction for one asset input.
func AnchorTemplate(p *proof.Proof, receiver sdk.InternalKey, value int64,
	spend *swapkit.SwapAnchorSpendInfo) ([]byte, error) {

	if value < 330 || p == nil ||
		p.OutPoint().Index >= uint32(len(p.AnchorTx.TxOut)) ||
		p.AnchorTx.TxOut[p.OutPoint().Index] == nil {

		return nil, swapkit.ErrInvalid
	}
	internal, err := btcec.ParsePubKey(receiver.PubKey[:])
	if err != nil {
		return nil, err
	}
	placeholder, err := txscript.PayToTaprootScript(
		txscript.ComputeTaprootKeyNoScript(internal))
	if err != nil {
		return nil, err
	}
	point := p.OutPoint()
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&point, nil, nil))
	tx.AddTxOut(wire.NewTxOut(value, placeholder))
	tx.TxIn[0].Sequence = 0xfffffffc
	prev := p.AnchorTx.TxOut[point.Index]
	if spend != nil {
		if !bytes.Equal(prev.PkScript, spend.PkScript) {
			return nil, swapkit.ErrInvalid
		}
		tx.LockTime = spend.LockTime
		tx.TxIn[0].Sequence = spend.Sequence
	}
	packet, err := psbt.NewFromUnsignedTx(tx)
	if err != nil {
		return nil, err
	}
	packet.Inputs[0].WitnessUtxo = prev
	if spend != nil {
		packet.Inputs[0].TaprootInternalKey = spend.InternalKey[1:]
		packet.Inputs[0].TaprootMerkleRoot = spend.TaprootMerkleRoot[:]
		leaf := &psbt.TaprootTapLeafScript{
			ControlBlock: spend.ControlBlock,
			Script:       spend.LeafScript,
			LeafVersion:  txscript.BaseLeafVersion,
		}
		leaves := []*psbt.TaprootTapLeafScript{leaf}
		packet.Inputs[0].TaprootLeafScript = leaves
	}
	return EncodePSBT(packet)
}

// CustomRequest fixes one input/output with no passive assets or asset loss.
func CustomRequest(input sdk.CustomAssetInput,
	output sdk.CustomAssetOutput, anchor []byte,
	signing sdk.CustomAnchorInputSigningPlan) *sdk.CustomAnchorRequest {

	exact := &sdk.CustomAnchorCallerFundedExact{}
	mode := sdk.CustomAnchorFundingCallerFundedExact
	return &sdk.CustomAnchorRequest{
		Inputs:  []sdk.CustomAssetInput{input},
		Outputs: []sdk.CustomAssetOutput{output}, AnchorPSBT: anchor,
		Funding: sdk.CustomAnchorFundingPlan{
			Mode:              mode,
			CallerFundedExact: exact,
		},
		PassiveAssets: sdk.CustomAnchorPassiveAssets{
			Policy: sdk.CustomAnchorPassiveReject,
		},
		LossPolicy: sdk.CustomAnchorLossPolicy{
			Mode: sdk.CustomAnchorLossReject,
		},
		SigningPlans: []sdk.CustomAnchorInputSigningPlan{signing},
	}
}

// FundingProof selects the exact confirmed wallet output for a deposit.
// It inspects Loop policy; the subsequent SDK Build verifies lineage once.
// ErrInvalid means the wallet holds no matching unspent output. A matching
// output that only waits on the chain returns ErrPending for outstanding
// confirmations, or the chain backend's own error, so the caller can retry.
func FundingProof(ctx context.Context, wallet *sdk.Wallet,
	chain Chain, minConfs uint32,
	request swapkit.CreateDepositRequest) ([]byte, error) {

	if err := CheckNodeVersion(ctx, wallet); err != nil {
		return nil, err
	}
	ref := sdk.AssetRefFromGroupKey(request.GroupKey)
	assets, err := wallet.Client().ListAssetRecords(ctx,
		&sdk.ListAssetsRequest{AssetRef: &ref})
	if err != nil {
		return nil, err
	}
	// waiting is the first chain-state failure of an exact candidate. It is
	// reported only when no candidate is ready.
	var waiting error
	for _, row := range assets {
		if row == nil || row.Amount != request.Amount ||
			row.AnchorOutpoint == nil {

			continue
		}
		file, err := wallet.ExportProofFile(ctx,
			sdk.AssetRefFromAssetID(row.Genesis.IssuanceID),
			row.ScriptKey.PubKey, row.AnchorOutpoint)
		if err != nil || file == nil {
			continue
		}
		if err := checkProvenance(ctx, wallet, file.RawProofFile); err != nil {
			continue
		}
		p, err := inspectAsset(file.RawProofFile, request.GroupKey,
			request.Amount)
		if err != nil {
			continue
		}
		scriptKey := p.Asset.ScriptKey.PubKey.SerializeCompressed()
		if p.OutPoint() != SDKPoint(*row.AnchorOutpoint) ||
			sdk.AssetID(p.Asset.ID()) != row.Genesis.IssuanceID ||
			!bytes.Equal(scriptKey, row.ScriptKey.PubKey[:]) {

			continue
		}
		// This is the exact wallet output. An outstanding confirmation or
		// a chain backend failure does not make the request invalid,
		// while a spent output can never fund the deposit.
		err = checkConfirmation(ctx, chain, minConfs, p, false)
		if err == nil {
			var unspent bool
			unspent, err = chain.Unspent(ctx, p.OutPoint())
			if err == nil && !unspent {
				continue
			}
		}
		if err != nil {
			if waiting == nil && !errors.Is(err, swapkit.ErrInvalid) {
				waiting = err
			}
			continue
		}
		return file.RawProofFile, nil
	}
	if waiting != nil {
		return nil, waiting
	}
	return nil, swapkit.ErrInvalid
}
