package tapsdk

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"reflect"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// depositContract is public immutable policy, including exact OP_TRUE data.
// It contains no wallet locator, private key or signing nonce.
//
//nolint:tagliatelle // Preserve existing serialized contract field names.
type depositContract struct {
	Version uint32 `json:"Version"`
	// #nosec G117 -- Both keys are public Bitcoin keys.
	ClientKey [33]byte                           `json:"ClientKey"`
	ServerKey [33]byte                           `json:"ServerKey"`
	CSV       uint32                             `json:"CSV"`
	Output    sdk.CustomAnchorAssetOutputSummary `json:"Output"`
}

// depositRecord is derived from the normal inventory and address key locator.
type depositRecord struct {
	Request  swapkit.CreateDepositRequest
	Deposit  swapkit.Deposit
	Contract depositContract
	Proof    *proof.Proof
}

func locator(family int32, index uint32) keychain.KeyLocator {
	return keychain.KeyLocator{
		Family: keychain.KeyFamily(family), Index: index,
	}
}

func (r *Driver) checkKey(ctx context.Context, family int32,
	index uint32, expected [33]byte) error {

	if index > math.MaxInt32 || family < 0 {
		return swapkit.ErrInvalid
	}
	loc := locator(family, index)
	key, err := r.cfg.WalletKit.DeriveKey(ctx, &loc)
	if err != nil {
		return err
	}
	if key == nil || key.PubKey == nil || key.KeyLocator != loc ||
		!bytes.Equal(key.PubKey.SerializeCompressed(), expected[:]) {

		return swapkit.ErrInvalid
	}
	return nil
}

// BuildDeposit funds and signs an asset deposit without publishing it. The
// deposit FSM saves the returned package before calling PublishDeposit.
func (r *Driver) BuildDeposit(ctx context.Context,
	request swapkit.CreateDepositRequest) (swapkit.Deposit, []byte, error) {

	if request.RequestID == ([32]byte{}) || request.Amount == 0 ||
		request.CSV == 0 || r.cfg.MaxFeeSat > 1_000_000 {

		return swapkit.Deposit{}, nil, swapkit.ErrInvalid
	}
	err := r.checkKey(ctx, request.KeyFamily, request.KeyIndex,
		request.ClientKey)
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
	inputProof, err := r.fundingProof(ctx, request)
	if err != nil {
		return swapkit.Deposit{}, nil, err
	}
	p, err := lastProof(inputProof)
	if err != nil {
		return swapkit.Deposit{}, nil, err
	}
	keys, err := r.cfg.Wallet.DeriveKeys(ctx)
	if err != nil {
		return swapkit.Deposit{}, nil, err
	}
	value := 330 + 4*r.cfg.MaxFeeSat
	output := OPTrueOutput(sdk.CustomAssetOutput{
		ID: "deposit", AssetRef: ref, Amount: request.Amount,
		AnchorValueSat: value,
	}, keys.ScriptKey.KeyDesc, dk.AnchorPlan())
	anchor, err := anchorTemplate(p, output.Anchor.InternalKey,
		int64(value), nil)
	if err != nil {
		return swapkit.Deposit{}, nil, err
	}
	key, err := sdk.ParseXOnlyPubKey(schnorr.SerializePubKey(
		p.InclusionProof.InternalKey))
	if err != nil {
		return swapkit.Deposit{}, nil, err
	}
	plan := customRequest(sdk.CustomAssetInput{
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
	rate, err := sdk.NewFeeRateSatPerVByte(max(1, r.cfg.FeeSat/200))
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
			MaxFeeSat:             r.cfg.MaxFeeSat,
			CustomLockID:          request.RequestID[:],
			LockExpirationSeconds: 60,
		},
	}
	pkg, err := r.commit(ctx, plan)
	if err != nil {
		return swapkit.Deposit{}, nil, err
	}
	if len(pkg.Outputs) != 1 {
		return swapkit.Deposit{}, nil, swapkit.ErrInvalid
	}
	packet, err := decodePSBT(pkg.AnchorPsbt)
	if err != nil {
		return swapkit.Deposit{}, nil, err
	}
	packet, err = r.cfg.WalletKit.SignPsbt(ctx, packet)
	if err != nil {
		return swapkit.Deposit{}, nil, err
	}
	packet, _, err = r.cfg.WalletKit.FinalizePsbt(
		ctx, packet, lnwallet.DefaultAccountName,
	)
	if err != nil {
		return swapkit.Deposit{}, nil, err
	}
	final, err := encodePSBT(packet)
	if err != nil {
		return swapkit.Deposit{}, nil, err
	}
	pkg, err = pkg.WithFinalAnchorPSBT(final)
	if err != nil {
		return swapkit.Deposit{}, nil, err
	}
	out := pkg.Outputs[0]
	contract, err := json.Marshal(depositContract{
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
		Outpoint: sdkPoint(out.AnchorOutpoint).String(),
		GroupKey: request.GroupKey, Amount: request.Amount,
		Contract: contract, ValueSat: uint64(out.AnchorValueSat),
	}, raw, nil
}

// PublishDeposit publishes the saved transaction and returns confirmed asset
// metadata. Repeating after a restart never funds another spend.
func (r *Driver) PublishDeposit(ctx context.Context, deposit swapkit.Deposit,
	signedPackage []byte) (swapkit.Deposit, error) {

	pkg, err := decodeFinalPackage(signedPackage)
	var contract depositContract
	if err != nil ||
		swapkit.Decode(deposit.Contract, &contract) != nil ||
		contract.Version != 1 || len(pkg.Outputs) != 1 ||
		!reflect.DeepEqual(contract.Output, pkg.Outputs[0]) ||
		deposit.Outpoint != sdkPoint(contract.Output.AnchorOutpoint).String() ||
		deposit.Amount != contract.Output.Amount ||
		deposit.ValueSat != uint64(contract.Output.AnchorValueSat) {

		return swapkit.Deposit{}, swapkit.ErrInvalid
	}
	group, grouped := contract.Output.AssetRef.GroupKey()
	if !grouped || group != deposit.GroupKey {
		return swapkit.Deposit{}, swapkit.ErrInvalid
	}
	raw, err := r.publish(ctx, pkg)
	if err != nil {
		return swapkit.Deposit{}, err
	}
	p, err := lastProof(raw)
	if err != nil {
		return swapkit.Deposit{}, err
	}
	if uint64(p.BlockHeight)+uint64(contract.CSV) > math.MaxUint32 {
		return swapkit.Deposit{}, swapkit.ErrInvalid
	}
	deposit.Proof = raw
	deposit.ConfirmationHeight = p.BlockHeight
	deposit.RefundHeight = p.BlockHeight + contract.CSV
	_, err = r.verifyDepositContract(ctx, deposit, contract, false)
	if err != nil {
		return swapkit.Deposit{}, err
	}
	return deposit, nil
}

func (r *Driver) admit(ctx context.Context,
	a swapkit.Admission) (swapkit.AdmissionResult, error) {

	if len(a.Deposits) != 1 || a.Registration.Owner == ([32]byte{}) ||
		a.RequestID == ([32]byte{}) ||
		a.MinConfirmations < r.cfg.MinConfs {

		return swapkit.AdmissionResult{}, swapkit.ErrInvalid
	}
	if err := r.checkKey(ctx, a.Registration.KeyFamily,
		a.Registration.KeyIndex, a.Registration.ServerKey); err != nil {
		return swapkit.AdmissionResult{}, err
	}
	d := a.Deposits[0]
	if !bytes.Equal(d.Contract, a.Contract) && len(d.Contract) != 0 {
		return swapkit.AdmissionResult{}, swapkit.ErrInvalid
	}
	var c depositContract
	if swapkit.Decode(a.Contract, &c) != nil || c.Version != 1 ||
		c.ClientKey != a.Registration.ClientKey ||
		c.ServerKey != a.Registration.ServerKey ||
		c.CSV != a.Registration.CSV {

		return swapkit.AdmissionResult{}, swapkit.ErrInvalid
	}
	d.Contract = a.Contract
	d.Amount, d.GroupKey = c.Output.Amount, a.GroupKey
	p, err := r.verifyDepositContract(ctx, d, c, true)
	if err != nil {
		return swapkit.AdmissionResult{}, err
	}
	tip, err := r.cfg.Chain.Height(ctx)
	if err != nil {
		return swapkit.AdmissionResult{}, err
	}
	if tip < p.BlockHeight || tip-p.BlockHeight+1 < a.MinConfirmations ||
		uint64(p.BlockHeight)+uint64(c.CSV) <= uint64(a.RefundHeight) {

		return swapkit.AdmissionResult{}, swapkit.ErrInvalid
	}
	return swapkit.AdmissionResult{DepositAmount: p.Asset.Amount}, nil
}

func (r *Driver) verifyDepositContract(ctx context.Context, d swapkit.Deposit,
	c depositContract, unspent bool) (*proof.Proof, error) {

	if c.Version != 1 || c.Output.OPTrueSpend == nil ||
		c.Output.Amount != d.Amount {

		return nil, swapkit.ErrInvalid
	}
	p, err := r.verifyProof(ctx, d.Proof, d.GroupKey, d.Amount, unspent)
	if err != nil {
		return nil, err
	}
	group, grouped := c.Output.AssetRef.GroupKey()
	if !grouped || group != d.GroupKey ||
		c.Output.IssuanceID != sdk.AssetID(p.Asset.ID()) ||
		c.Output.AssetType != sdk.AssetType(p.Asset.Genesis.Type) {

		return nil, swapkit.ErrInvalid
	}
	assets, merkle, err := isolatedRoots(p)
	if err != nil {
		return nil, err
	}
	previous := p.AnchorTx.TxOut[p.OutPoint().Index]
	if p.OutPoint().String() != d.Outpoint ||
		sdkPoint(c.Output.AnchorOutpoint) != p.OutPoint() ||
		c.Output.TaprootAssetRoot != assets ||
		c.Output.TaprootMerkleRoot != merkle ||
		c.Output.AnchorValueSat != previous.Value ||
		!bytes.Equal(p.Asset.ScriptKey.PubKey.SerializeCompressed(),
			c.Output.ScriptKey[:]) {

		return nil, swapkit.ErrInvalid
	}
	dk, err := swapkit.NewDepositKit(swapkit.DepositKitParams{
		ClientKey: c.ClientKey, ServerKey: c.ServerKey,
		CSVExpiry: c.CSV,
	})
	if err != nil {
		return nil, err
	}
	spend, err := dk.TimeoutSpend(assets)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(spend.PkScript, previous.PkScript) {
		return nil, swapkit.ErrInvalid
	}
	// Validate the exact asset witness, not just a caller's OP_TRUE label.
	err = c.Output.OPTrueSpend.Validate(c.Output.ScriptKey)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func anchorTemplate(p *proof.Proof, receiver sdk.InternalKey, value int64,
	spend *swapkit.SwapAnchorSpendInfo) ([]byte, error) {

	if value < 330 {
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
	return encodePSBT(packet)
}

func customRequest(input sdk.CustomAssetInput,
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

func (r *Driver) commit(ctx context.Context,
	request *sdk.CustomAnchorRequest) (
	*sdk.CustomAnchorTransferPackage, error) {

	for _, input := range request.Inputs {
		if input.Witness.Mode == sdk.CustomAssetWitnessCallerProvided {
			p, err := lastProof(input.ProofFile)
			if err != nil {
				return nil, err
			}
			pub, err := sdk.ParsePubKey(
				p.Asset.ScriptKey.PubKey.SerializeCompressed(),
			)
			if err != nil {
				return nil, err
			}
			err = r.declareOPTrue(ctx, pub, input.Witness.Stack)
			if err != nil {
				return nil, err
			}
		}
		err := r.importProof(ctx, input.ProofFile)
		if err != nil {
			return nil, err
		}
	}
	plan, err := r.cfg.Wallet.NewCustomAnchorTxBuilder().Build(ctx, request)
	if err != nil {
		return nil, err
	}
	verification := plan.Verification()
	if !verification.Valid() {
		return nil, swapkit.ErrInvalid
	}
	pkg, err := plan.Commit(ctx, sdk.CustomAnchorCommitOptions{
		Publish: sdk.CustomAnchorPublishMetadata{
			Label: "Loop asset transfer",
		},
	})
	if err != nil {
		return nil, err
	}
	if err := pkg.Validate(); err != nil {
		return nil, err
	}
	return pkg, nil
}

// fundingProof uses the exact output locator. A registered wallet script key
// can hold several UTXOs of the same issuance after repeated swaps.
func (r *Driver) fundingProof(ctx context.Context,
	request swapkit.CreateDepositRequest) ([]byte, error) {

	ref := sdk.AssetRefFromGroupKey(request.GroupKey)
	assets, err := r.cfg.SDK.ListAssetRecords(ctx,
		&sdk.ListAssetsRequest{AssetRef: &ref})
	if err != nil {
		return nil, err
	}
	for _, row := range assets {
		if row == nil || row.Amount != request.Amount ||
			row.AnchorOutpoint == nil {

			continue
		}
		file, err := r.cfg.Wallet.ExportProofFile(ctx,
			sdk.AssetRefFromAssetID(row.Genesis.IssuanceID),
			row.ScriptKey.PubKey, row.AnchorOutpoint)
		if err != nil || file == nil {
			continue
		}
		p, err := r.verifyProof(ctx, file.RawProofFile,
			request.GroupKey, request.Amount, true)
		if err != nil {
			continue
		}
		scriptKey := p.Asset.ScriptKey.PubKey.SerializeCompressed()
		if p.OutPoint() != sdkPoint(*row.AnchorOutpoint) ||
			sdk.AssetID(p.Asset.ID()) != row.Genesis.IssuanceID ||
			!bytes.Equal(scriptKey, row.ScriptKey.PubKey[:]) {

			continue
		}
		return file.RawProofFile, nil
	}
	return nil, swapkit.ErrInvalid
}
