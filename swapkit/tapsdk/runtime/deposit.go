package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"math"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/loop/swapkit/bridge"
	kit "github.com/lightninglabs/loop/swapkit/tapsdk"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	bolt "go.etcd.io/bbolt"
)

// depositContract is public immutable policy, including exact OP_TRUE data.
// It contains no wallet locator, private key or signing nonce.
type depositContract struct {
	Version              uint32
	ClientKey, ServerKey [33]byte
	CSV                  uint32
	Output               sdk.CustomAnchorAssetOutputSummary
}

type depositRecord struct {
	Request     bridge.CreateDepositRequest
	Plan        *sdk.CustomAnchorRequest
	Transaction transaction
	Deposit     bridge.Deposit
	Since       uint32
	RefundPlan  *sdk.CustomAnchorRequest
	Refund      transaction
	RefundProof []byte
	RefundSince uint32
}

func locator(family int32, index uint32) keychain.KeyLocator {
	return keychain.KeyLocator{
		Family: keychain.KeyFamily(family), Index: index,
	}
}

func (r *Runtime) checkKey(ctx context.Context, family int32,
	index uint32, expected [33]byte) error {

	if index > math.MaxInt32 || family < 0 {
		return bridge.ErrInvalid
	}
	key, err := r.cfg.WalletKit.DeriveKey(ctx, &signrpc.KeyLocator{
		KeyFamily: family, KeyIndex: int32(index),
	})
	if err != nil {
		return err
	}
	if key == nil || key.KeyLoc == nil ||
		key.KeyLoc.KeyFamily != family ||
		key.KeyLoc.KeyIndex != int32(index) ||
		!bytes.Equal(key.RawKeyBytes, expected[:]) {
		return bridge.ErrInvalid
	}
	return nil
}

func (r *Runtime) createDeposit(ctx context.Context,
	request bridge.CreateDepositRequest) (bridge.Deposit, error) {

	var record depositRecord
	id := key("deposit/", request.RequestID)
	err := r.get(id, &record)
	if err == nil && !equal(record.Request, request) {
		return bridge.Deposit{}, bridge.ErrInvalid
	}
	if err != nil && !missing(err) {
		return bridge.Deposit{}, err
	}
	if err == nil && len(record.Deposit.Proof) != 0 {
		// Inventory keeps spent deposits for Bitcoin exclusions. Check
		// their persisted confirmation directly instead of rescanning
		// every block since funding on each inventory refresh.
		_, err = r.verifyDeposit(ctx, record.Deposit, false)
		if err == nil {
			return record.Deposit, nil
		}
		if err != bridge.ErrPending {
			return bridge.Deposit{}, err
		}
		// A reorg may relocate the same transaction. Reconstruct its
		// proof from the current chain without rebuilding the spend.
	}
	if missing(err) {
		if request.RequestID == ([32]byte{}) || request.Amount == 0 ||
			request.CSV == 0 || r.cfg.MaxFeeSat > 1_000_000 {
			return bridge.Deposit{}, bridge.ErrInvalid
		}
		err := r.checkKey(ctx, request.KeyFamily, request.KeyIndex,
			request.ClientKey)
		if err != nil {
			return bridge.Deposit{}, err
		}
		dk, err := kit.NewDepositKit(kit.DepositKitParams{
			ClientKey: request.ClientKey,
			ServerKey: request.ServerKey,
			CSVExpiry: request.CSV,
		})
		if err != nil {
			return bridge.Deposit{}, err
		}
		ref := sdk.AssetRefFromGroupKey(request.GroupKey)
		inputProof, err := r.fundingProof(ctx, request)
		if err != nil {
			return bridge.Deposit{}, err
		}
		p, err := lastProof(inputProof)
		if err != nil {
			return bridge.Deposit{}, err
		}
		keys, err := r.cfg.Wallet.DeriveKeys(ctx)
		if err != nil {
			return bridge.Deposit{}, err
		}
		value := 330 + 4*r.cfg.MaxFeeSat
		output, err := dk.Output(kit.SwapOutputParams{
			ID: "deposit", AssetRef: ref, Amount: request.Amount,
			AnchorValueSat:   value,
			AssetInternalKey: keys.ScriptKey.KeyDesc,
		})
		if err != nil {
			return bridge.Deposit{}, err
		}
		anchor, err := anchorTemplate(p, output.Anchor.InternalKey,
			int64(value), nil)
		if err != nil {
			return bridge.Deposit{}, err
		}
		key, err := sdk.ParseXOnlyPubKey(schnorr.SerializePubKey(
			p.InclusionProof.InternalKey))
		if err != nil {
			return bridge.Deposit{}, err
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
			return bridge.Deposit{}, err
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
		since, err := r.cfg.Chain.Height(ctx)
		if err != nil {
			return bridge.Deposit{}, err
		}
		record = depositRecord{Request: request, Plan: plan,
			Since: since}
		if err := r.put(id, record); err != nil {
			return bridge.Deposit{}, err
		}
	}
	if record.Transaction.Package == nil {
		pkg, err := r.commit(ctx, record.Plan)
		if err != nil {
			return bridge.Deposit{}, err
		}
		record.Transaction.Package = pkg
		if err := r.put(id, record); err != nil {
			return bridge.Deposit{}, err
		}
	}
	if len(record.Transaction.Final) == 0 {
		final, err := r.cfg.Signer.AnchorSigner()(ctx,
			record.Transaction.Package.AnchorPsbt)
		if err != nil {
			return bridge.Deposit{}, err
		}
		err = record.Transaction.Package.VerifyFinalAnchorPSBT(final)
		if err != nil {
			return bridge.Deposit{}, err
		}
		record.Transaction.Final = final
		if err := r.put(id, record); err != nil {
			return bridge.Deposit{}, err
		}
	}
	raw, err := r.publish(ctx, record.Transaction, record.Since)
	if err != nil {
		return bridge.Deposit{}, err
	}
	output := record.Transaction.Package.Outputs[0]
	contract, err := json.Marshal(depositContract{
		Version: 1, ClientKey: request.ClientKey,
		ServerKey: request.ServerKey, CSV: request.CSV, Output: output,
	})
	if err != nil {
		return bridge.Deposit{}, err
	}
	p, err := lastProof(raw)
	if err != nil {
		return bridge.Deposit{}, err
	}
	if uint64(p.BlockHeight)+uint64(request.CSV) > math.MaxUint32 {
		return bridge.Deposit{}, bridge.ErrInvalid
	}
	record.Deposit = bridge.Deposit{
		Outpoint: p.OutPoint().String(), GroupKey: request.GroupKey,
		Amount: request.Amount, Proof: raw, Contract: contract,
		ConfirmationHeight: p.BlockHeight,
		RefundHeight:       p.BlockHeight + request.CSV,
		ValueSat:           uint64(output.AnchorValueSat),
	}
	if err := r.put(id, record); err != nil {
		return bridge.Deposit{}, err
	}
	return record.Deposit, nil
}

func (r *Runtime) inventory(ctx context.Context,
	request bridge.InventoryRequest) ([]bridge.Deposit, error) {

	var records []depositRecord
	err := r.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte("runtime-v1")).Cursor()
		prefix := []byte("deposit/")
		for k, v := c.Seek(prefix); k != nil &&
			bytes.HasPrefix(k, prefix); k, v = c.Next() {
			var rec depositRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return err
			}
			records = append(records, rec)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	result := make([]bridge.Deposit, 0, len(records))
	for _, record := range records {
		if request.GroupKey != ([33]byte{}) &&
			request.GroupKey != record.Request.GroupKey {
			continue
		}
		deposit, err := r.createDeposit(ctx, record.Request)
		if err == bridge.ErrPending {
			continue
		}
		if err != nil {
			return nil, err
		}
		// Retain spent inventory for shared-manager exclusions.
		// Selection rechecks spendability before reserving.
		result = append(result, deposit)
	}
	return result, nil
}

func (r *Runtime) admit(ctx context.Context,
	a bridge.Admission) (bridge.AdmissionResult, error) {

	if len(a.Deposits) != 1 || a.Registration.Owner == ([32]byte{}) ||
		a.RequestID == ([32]byte{}) ||
		a.MinConfirmations < r.cfg.MinConfs {
		return bridge.AdmissionResult{}, bridge.ErrInvalid
	}
	if err := r.checkKey(ctx, a.Registration.KeyFamily,
		a.Registration.KeyIndex, a.Registration.ServerKey); err != nil {
		return bridge.AdmissionResult{}, err
	}
	d := a.Deposits[0]
	if !bytes.Equal(d.Contract, a.Contract) && len(d.Contract) != 0 {
		return bridge.AdmissionResult{}, bridge.ErrInvalid
	}
	var c depositContract
	if bridge.Decode(a.Contract, &c) != nil || c.Version != 1 ||
		c.ClientKey != a.Registration.ClientKey ||
		c.ServerKey != a.Registration.ServerKey ||
		c.CSV != a.Registration.CSV {
		return bridge.AdmissionResult{}, bridge.ErrInvalid
	}
	d.Contract = a.Contract
	d.Amount, d.GroupKey = c.Output.Amount, a.GroupKey
	p, err := r.verifyDeposit(ctx, d, true)
	if err != nil {
		return bridge.AdmissionResult{}, err
	}
	tip, err := r.cfg.Chain.Height(ctx)
	if err != nil {
		return bridge.AdmissionResult{}, err
	}
	if tip < p.BlockHeight || tip-p.BlockHeight+1 < a.MinConfirmations ||
		uint64(p.BlockHeight)+uint64(c.CSV) <= uint64(a.RefundHeight) {
		return bridge.AdmissionResult{}, bridge.ErrInvalid
	}
	return bridge.AdmissionResult{DepositAmount: p.Asset.Amount}, nil
}

func (r *Runtime) verifyDeposit(ctx context.Context, d bridge.Deposit,
	unspent bool) (*proof.Proof, error) {

	var c depositContract
	if bridge.Decode(d.Contract, &c) != nil || c.Version != 1 ||
		c.Output.OPTrueSpend == nil || c.Output.Amount != d.Amount {
		return nil, bridge.ErrInvalid
	}
	p, err := r.verifyProof(ctx, d.Proof, d.GroupKey, d.Amount, unspent)
	if err != nil {
		return nil, err
	}
	group, grouped := c.Output.AssetRef.GroupKey()
	if !grouped || group != d.GroupKey ||
		c.Output.IssuanceID != sdk.AssetID(p.Asset.ID()) ||
		c.Output.AssetType != sdk.AssetType(p.Asset.Genesis.Type) {

		return nil, bridge.ErrInvalid
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
		return nil, bridge.ErrInvalid
	}
	dk, err := kit.NewDepositKit(kit.DepositKitParams{
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
		return nil, bridge.ErrInvalid
	}
	// Validate the exact asset witness, not just a caller's OP_TRUE label.
	err = c.Output.OPTrueSpend.Validate(c.Output.ScriptKey)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func anchorTemplate(p *proof.Proof, receiver sdk.InternalKey, value int64,
	spend *kit.SwapAnchorSpendInfo) ([]byte, error) {

	if value < 330 {
		return nil, bridge.ErrInvalid
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
			return nil, bridge.ErrInvalid
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

func (r *Runtime) commit(ctx context.Context,
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
		_, err := r.cfg.Wallet.ImportProofFile(ctx,
			&sdk.ProofFile{RawProofFile: input.ProofFile})
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
		return nil, bridge.ErrInvalid
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
func (r *Runtime) fundingProof(ctx context.Context,
	request bridge.CreateDepositRequest) ([]byte, error) {

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
	return nil, bridge.ErrInvalid
}
