package deposit

import (
	"context"
	"encoding/json"
	"math"
	"reflect"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/lightninglabs/loop/swapkit"
	assetkit "github.com/lightninglabs/loop/swapkit/tapsdk"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// buildAssetDeposit constructs and signs with the SDK and existing LND wallet.
// CreateAssetDeposit saves this package before starting the publication action.
func (m *Manager) buildAssetDeposit(ctx context.Context,
	request swapkit.CreateDepositRequest) (swapkit.Deposit, []byte, error) {

	a := m.cfg.Asset
	if request.RequestID == ([32]byte{}) || request.Amount == 0 ||
		request.CSV == 0 || a.MaxFeeSat > 1_000_000 {

		return swapkit.Deposit{}, nil, swapkit.ErrInvalid
	}
	err := assetkit.CheckKey(ctx, m.cfg.WalletKit, keychain.KeyLocator{
		Family: keychain.KeyFamily(request.KeyFamily), Index: request.KeyIndex,
	}, request.ClientKey)
	if err != nil {
		return swapkit.Deposit{}, nil, err
	}
	dk, err := swapkit.NewDepositKit(swapkit.DepositKitParams{
		ClientKey: request.ClientKey,
		ServerKey: request.ServerKey,
		CSVExpiry: request.CSV,
	})
	if err != nil {
		return swapkit.Deposit{}, nil, err
	}
	ref := sdk.AssetRefFromGroupKey(request.GroupKey)
	inputProof, err := assetkit.FundingProof(ctx, a.Wallet, a.Chain,
		a.MinConfs, request)
	if err != nil {
		return swapkit.Deposit{}, nil, err
	}
	p, err := assetkit.LastProof(inputProof)
	if err != nil {
		return swapkit.Deposit{}, nil, err
	}
	keys, err := a.Wallet.DeriveKeys(ctx)
	if err != nil {
		return swapkit.Deposit{}, nil, err
	}
	value := 330 + 4*a.MaxFeeSat
	output := assetkit.OPTrueOutput(sdk.CustomAssetOutput{
		ID: "deposit", AssetRef: ref, Amount: request.Amount,
		AnchorValueSat: value,
	}, keys.ScriptKey.KeyDesc, dk.AnchorPlan())
	anchor, err := assetkit.AnchorTemplate(p, output.Anchor.InternalKey,
		int64(value), nil)
	if err != nil {
		return swapkit.Deposit{}, nil, err
	}
	key, err := sdk.ParseXOnlyPubKey(schnorr.SerializePubKey(
		p.InclusionProof.InternalKey))
	if err != nil {
		return swapkit.Deposit{}, nil, err
	}
	plan := assetkit.CustomRequest(sdk.CustomAssetInput{
		ID: "wallet", AssetRef: ref, Amount: request.Amount,
		ProofFile: inputProof,
		Witness: sdk.CustomAssetWitnessPlan{
			Mode: sdk.CustomAssetWitnessBackendSigner,
		},
	}, output, anchor, sdk.CustomAnchorInputSigningPlan{
		InputIndex: 0,
		KeyPath: &sdk.CustomAnchorKeyPathSigningPlan{
			Signer: key,
		},
	})
	rate, err := sdk.NewFeeRateSatPerVByte(max(1, a.FeeSat/200))
	if err != nil {
		return swapkit.Deposit{}, nil, err
	}
	plan.Funding = sdk.CustomAnchorFundingPlan{
		Mode: sdk.CustomAnchorFundingWalletFunded,
		WalletFunded: &sdk.CustomAnchorWalletFunding{
			ChangeOutput: sdk.AnchorChangeOutput{
				Mode: sdk.AnchorChangeOutputAdd,
			},
			Fee: sdk.AnchorFee{
				Mode:    sdk.AnchorFeeSatPerVByte,
				FeeRate: rate},
			MaxFeeSat:             a.MaxFeeSat,
			CustomLockID:          request.RequestID[:],
			LockExpirationSeconds: 60,
		},
	}
	if err := assetkit.PrepareInputs(ctx, a.Wallet, plan); err != nil {
		return swapkit.Deposit{}, nil, err
	}
	prepared, err := a.Wallet.NewCustomAnchorTxBuilder().Build(ctx, plan)
	if err != nil {
		return swapkit.Deposit{}, nil, err
	}
	if err := assetkit.CheckInputsCurrent(ctx, a.Chain, a.MinConfs,
		plan, true); err != nil {
		return swapkit.Deposit{}, nil, err
	}
	pkg, err := prepared.Commit(ctx, sdk.CustomAnchorCommitOptions{
		Publish: sdk.CustomAnchorPublishMetadata{Label: "Loop asset deposit"},
	})
	if err != nil {
		return swapkit.Deposit{}, nil, err
	}
	if len(pkg.Outputs) != 1 {
		return swapkit.Deposit{}, nil, swapkit.ErrInvalid
	}
	packet, err := assetkit.DecodePSBT(pkg.AnchorPsbt)
	if err != nil {
		return swapkit.Deposit{}, nil, err
	}
	packet, err = m.cfg.WalletKit.SignPsbt(ctx, packet)
	if err != nil {
		return swapkit.Deposit{}, nil, err
	}
	packet, _, err = m.cfg.WalletKit.FinalizePsbt(
		ctx, packet, lnwallet.DefaultAccountName,
	)
	if err != nil {
		return swapkit.Deposit{}, nil, err
	}
	final, err := assetkit.EncodePSBT(packet)
	if err != nil {
		return swapkit.Deposit{}, nil, err
	}
	pkg, err = pkg.WithFinalAnchorPSBT(final)
	if err != nil {
		return swapkit.Deposit{}, nil, err
	}
	out := pkg.Outputs[0]
	contract, err := json.Marshal(assetkit.DepositContract{
		Version: 1, ClientKey: request.ClientKey,
		ServerKey: request.ServerKey, CSV: request.CSV, Output: out,
	})
	if err != nil {
		return swapkit.Deposit{}, nil, err
	}
	raw, err := pkg.MarshalBinary()
	if err != nil {
		return swapkit.Deposit{}, nil, err
	}
	return swapkit.Deposit{
		Outpoint: assetkit.SDKPoint(out.AnchorOutpoint).String(),
		GroupKey: request.GroupKey, Amount: request.Amount,
		Contract: contract, ValueSat: uint64(out.AnchorValueSat),
	}, raw, nil
}

// publishAssetDeposit publishes the saved transaction and returns its confirmed
// proof. Repeating after a restart never funds another spend.
func (f *FSM) publishAssetDeposit(ctx context.Context, deposit swapkit.Deposit,
	signedPackage []byte) (swapkit.Deposit, error) {

	pkg, err := assetkit.DecodeFinalPackage(signedPackage)
	var contract assetkit.DepositContract
	if err != nil ||
		swapkit.Decode(deposit.Contract, &contract) != nil ||
		contract.Version != 1 || len(pkg.Outputs) != 1 ||
		!reflect.DeepEqual(contract.Output, pkg.Outputs[0]) ||
		deposit.Amount != contract.Output.Amount ||
		deposit.ValueSat != uint64(contract.Output.AnchorValueSat) {

		return swapkit.Deposit{}, swapkit.ErrInvalid
	}
	point := assetkit.SDKPoint(contract.Output.AnchorOutpoint)
	group, grouped := contract.Output.AssetRef.GroupKey()
	if !grouped || group != deposit.GroupKey ||
		deposit.Outpoint != point.String() {

		return swapkit.Deposit{}, swapkit.ErrInvalid
	}
	a := f.cfg.Asset
	raw, err := assetkit.LogAndExport(ctx, a.Wallet, a.Chain,
		a.MinConfs, pkg, false)
	if err != nil {
		return swapkit.Deposit{}, err
	}
	p, err := assetkit.LastProof(raw)
	if err != nil {
		return swapkit.Deposit{}, err
	}
	if uint64(p.BlockHeight)+uint64(contract.CSV) > math.MaxUint32 {
		return swapkit.Deposit{}, swapkit.ErrInvalid
	}
	deposit.Proof = raw
	deposit.ConfirmationHeight = p.BlockHeight
	deposit.RefundHeight = p.BlockHeight + contract.CSV
	d, err := assetkit.LoadDeposit(deposit, keychain.KeyLocator{})
	if err != nil {
		return swapkit.Deposit{}, err
	}
	err = assetkit.VerifyDeposit(ctx, a.Wallet, a.Chain, a.MinConfs, d, false)
	if err != nil {
		return swapkit.Deposit{}, err
	}
	return deposit, nil
}

// buildAssetRefund prepares the original CSV spend without publishing it.
// The expiry action saves the completed SDK package before publication.
func (f *FSM) buildAssetRefund(ctx context.Context,
	deposit swapkit.Deposit) ([]byte, error) {

	params, err := f.cfg.AddressManager.GetStaticAddressParameters(ctx)
	if err != nil {
		return nil, err
	}
	d, err := assetkit.LoadDeposit(deposit, params.KeyLocator)
	if err != nil {
		return nil, err
	}
	a := f.cfg.Asset
	if err := assetkit.InspectDeposit(ctx, a.Chain, a.MinConfs,
		d, true); err != nil {
		return nil, err
	}
	tip, err := a.Chain.Height(ctx)
	if err != nil {
		return nil, err
	}
	if uint64(tip) < uint64(d.Proof.BlockHeight)+uint64(d.Contract.CSV) {
		return nil, swapkit.ErrPending
	}
	c, spend, err := assetkit.OriginalRefundPolicy(d)
	if err != nil {
		return nil, err
	}
	request, err := assetkit.RecoveryRequest(ctx, a.Wallet, c.Output,
		deposit.Proof, c.ClientKey, d.Key, spend, a.FeeSat)
	if err != nil {
		return nil, err
	}
	if err := assetkit.PrepareInputs(ctx, a.Wallet, request); err != nil {
		return nil, err
	}
	plan, err := a.Wallet.NewCustomAnchorTxBuilder().Build(ctx, request)
	if err != nil {
		return nil, err
	}
	if err := assetkit.CheckInputsCurrent(ctx, a.Chain, a.MinConfs,
		request, true); err != nil {
		return nil, err
	}
	pkg, err := plan.Commit(ctx, sdk.CustomAnchorCommitOptions{
		Publish: sdk.CustomAnchorPublishMetadata{
			Label: "Loop asset deposit refund",
		},
	})
	if err != nil {
		return nil, err
	}
	pkg, err = assetkit.FinalizeScriptSpend(ctx, f.cfg.Signer,
		d.Key, pkg, nil)
	if err != nil {
		return nil, err
	}
	return pkg.MarshalBinary()
}

// publishAssetRefund resumes the saved CSV transaction. The original deposit
// may already be spent when its publication response was lost.
func (f *FSM) publishAssetRefund(ctx context.Context,
	deposit swapkit.Deposit, signedPackage []byte) ([]byte, error) {

	d, err := assetkit.LoadDeposit(deposit, keychain.KeyLocator{})
	if err != nil {
		return nil, err
	}
	a := f.cfg.Asset
	if err := assetkit.VerifyDeposit(ctx, a.Wallet, a.Chain,
		a.MinConfs, d, false); err != nil {
		return nil, err
	}
	c, spend, err := assetkit.OriginalRefundPolicy(d)
	if err != nil {
		return nil, err
	}
	pkg, err := assetkit.DecodeFinalPackage(signedPackage)
	if err != nil {
		return nil, err
	}
	raw, err := assetkit.LogAndExport(ctx, a.Wallet, a.Chain,
		a.MinConfs, pkg, false)
	if err != nil {
		return nil, err
	}
	err = assetkit.VerifyWalletSpend(ctx, a.Wallet, a.Chain, a.MinConfs,
		raw, c.Output, c.ClientKey, spend, true,
		uint64(d.Proof.BlockHeight)+uint64(c.CSV))
	return raw, err
}
