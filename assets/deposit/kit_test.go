package deposit

import (
	"context"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/assets/htlc"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tapscript"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

const (
	legacySuccessScript = "20c6047f9441ed7d6d3045406e95c07cd85c778e" +
		"4b8cef3ca7abac09b95c709ee5ad82012088a914e6babb9619d7a812" +
		"72711fc546a16b211dd939578851b2"
	legacyTimeoutScript = "2079be667ef9dcbbac55a06295ce870b07029bfcdb2d" +
		"ce28d959f2815b16f81798ad029000b2"
	legacyAggregateKey = "023b46d262d2f610e9038b44beabdfe97ab5a0feb" +
		"89870acc2264edfb7f63ec2ec"
	legacySibling = "01cbf40123d0ca1191c0dba575c7a92ea255f9e9fc81484e" +
		"3929afeaa1a6f459d600114fbfd01b5de82dfa6f54f0f27af41d5a66" +
		"ed39ac1a87d614fa12808f9adf"
	legacyAnchorPkScript = "512021b15bddf2d991b8791ec7c2105fa69be61008d78" +
		"b67d261451290d7a44dce5f"
)

func scalarKey(t *testing.T, scalar byte) (*btcec.PrivateKey,
	*btcec.PublicKey) {

	t.Helper()
	keyBytes := make([]byte, 32)
	keyBytes[31] = scalar
	privateKey, publicKey := btcec.PrivKeyFromBytes(keyBytes)

	return privateKey, publicKey
}

// TestNewHtlcSwapKitLegacyContract proves the moved deposit adapter retains
// the exact contract used by the existing valid asset deposit flow.
func TestNewHtlcSwapKitLegacyContract(t *testing.T) {
	_, funderKey := scalarKey(t, 1)
	_, coSignerKey := scalarKey(t, 2)
	var assetID asset.ID
	var swapHash lntypes.Hash
	for idx := range assetID {
		assetID[idx] = byte(idx)
		swapHash[idx] = byte(idx)
	}

	depositKit, err := NewKit(
		funderKey, coSignerKey, keychain.KeyLocator{}, assetID, 4032,
		&address.RegressionNetTap,
	)
	require.NoError(t, err)

	swapKit, err := depositKit.newHtlcSwapKit(1000, swapHash, 144)
	require.NoError(t, err)
	require.Equal(t, htlc.LegacyDepositV0, swapKit.Policy())

	successScript, err := swapKit.GetSuccessScript()
	require.NoError(t, err)
	require.Equal(t, legacySuccessScript, hex.EncodeToString(successScript))
	timeoutScript, err := swapKit.GetTimeoutScript()
	require.NoError(t, err)
	require.Equal(t, legacyTimeoutScript, hex.EncodeToString(timeoutScript))
	aggregateKey, err := swapKit.GetAggregateKey()
	require.NoError(t, err)
	require.Equal(t, legacyAggregateKey,
		hex.EncodeToString(aggregateKey.SerializeCompressed()))
	sibling, err := swapKit.GetSiblingPreimage()
	require.NoError(t, err)
	siblingBytes, _, err := commitment.MaybeEncodeTapscriptPreimage(&sibling)
	require.NoError(t, err)
	require.Equal(t, legacySibling, hex.EncodeToString(siblingBytes))

	assetRoot := make([]byte, chainhash.HashSize)
	for idx := range assetRoot {
		assetRoot[idx] = byte(0xa0 + idx)
	}
	pkScript, err := swapKit.GetPkScriptFromRoot(assetRoot)
	require.NoError(t, err)
	require.Equal(t, legacyAnchorPkScript, hex.EncodeToString(pkScript))

	packet, err := swapKit.CreateHtlcVpkt()
	require.NoError(t, err)
	require.Equal(t, &address.RegressionNetTap, packet.ChainParams)
	require.Len(t, packet.Outputs, 2)
	require.Equal(t, uint32(0), packet.Outputs[0].AnchorOutputIndex)
	require.Equal(t, uint32(1), packet.Outputs[1].AnchorOutputIndex)
	require.Equal(t, uint64(1000), packet.Outputs[1].Amount)
	require.True(t, packet.Outputs[0].Interactive)
	require.True(t, packet.Outputs[1].Interactive)
}

// TestNewKitValidation verifies invalid or mutable constructor inputs cannot
// create or alter a deposit contract.
func TestNewKitValidation(t *testing.T) {
	_, funderKey := scalarKey(t, 1)
	_, coSignerKey := scalarKey(t, 2)
	assetID := asset.ID{1}
	params := cloneAddressParams(address.RegressionNetTap)

	tests := []struct {
		name        string
		funder      *btcec.PublicKey
		coSigner    *btcec.PublicKey
		assetID     asset.ID
		expiry      uint32
		chainParams *address.ChainParams
	}{
		{
			name: "nil funder", coSigner: coSignerKey,
			assetID: assetID, expiry: 1, chainParams: params,
		},
		{
			name: "nil co-signer", funder: funderKey,
			assetID: assetID, expiry: 1, chainParams: params,
		},
		{
			name: "same keys", funder: funderKey, coSigner: funderKey,
			assetID: assetID, expiry: 1, chainParams: params,
		},
		{
			name: "zero asset ID", funder: funderKey,
			coSigner: coSignerKey, expiry: 1, chainParams: params,
		},
		{
			name: "zero expiry", funder: funderKey,
			coSigner: coSignerKey, assetID: assetID,
			chainParams: params,
		},
		{
			name: "non-block expiry", funder: funderKey,
			coSigner: coSignerKey, assetID: assetID,
			expiry:      wire.SequenceLockTimeMask + 1,
			chainParams: params,
		},
		{
			name: "nil chain parameters", funder: funderKey,
			coSigner: coSignerKey, assetID: assetID, expiry: 1,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := NewKit(
				testCase.funder, testCase.coSigner,
				keychain.KeyLocator{}, testCase.assetID,
				testCase.expiry, testCase.chainParams,
			)
			require.Error(t, err)
		})
	}

	kit, err := NewKit(
		funderKey, coSignerKey, keychain.KeyLocator{}, assetID, 144,
		params,
	)
	require.NoError(t, err)
	params.TapHRP = "mutated"
	params.Params.Name = "mutated"
	swapKit, err := kit.newHtlcSwapKit(1, lntypes.Hash{1}, 2)
	require.NoError(t, err)
	require.Equal(t, address.RegressionNetTap.TapHRP,
		swapKit.AddressParams().TapHRP)
	require.Equal(t, address.RegressionNetTap.Name,
		swapKit.AddressParams().Name)

	var nilKit *Kit
	_, err = nilKit.NewAddr(t.Context(), nil, 1)
	require.Error(t, err)
}

type localSigner struct {
	lndclient.SignerClient

	privateKey    *btcec.PrivateKey
	expectedInput int
	calls         int
	invalidSet    bool
}

func (s *localSigner) SignOutputRaw(_ context.Context, tx *wire.MsgTx,
	descriptors []*lndclient.SignDescriptor,
	prevOutputs []*wire.TxOut) ([][]byte, error) {

	s.calls++
	if s.invalidSet {
		return nil, nil
	}
	if len(descriptors) != 1 {
		return nil, fmt.Errorf("expected one sign descriptor")
	}
	if len(prevOutputs) != len(tx.TxIn) {
		return nil, fmt.Errorf("expected all previous outputs")
	}
	descriptor := descriptors[0]
	if descriptor.InputIndex != s.expectedInput {
		return nil, fmt.Errorf("unexpected input index: %d",
			descriptor.InputIndex)
	}
	if descriptor.SignMethod != input.TaprootScriptSpendSignMethod {
		return nil, fmt.Errorf("unexpected signing method")
	}

	prevOutFetcher := txscript.NewMultiPrevOutFetcher(nil)
	for idx, txIn := range tx.TxIn {
		prevOutFetcher.AddPrevOut(
			txIn.PreviousOutPoint, prevOutputs[idx],
		)
	}
	sigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)
	tapLeaf := txscript.NewBaseTapLeaf(descriptor.WitnessScript)
	signature, err := txscript.RawTxInTapscriptSignature(
		tx, sigHashes, descriptor.InputIndex,
		descriptor.Output.Value, descriptor.Output.PkScript, tapLeaf,
		txscript.SigHashDefault, s.privateKey,
	)
	if err != nil {
		return nil, err
	}

	return [][]byte{signature}, nil
}

type witnessFixture struct {
	kit          *Kit
	proof        *proof.Proof
	packet       *psbt.Packet
	prevOutputs  []*wire.TxOut
	funderKey    *btcec.PrivateKey
	assetInIndex int
}

func newWitnessFixture(t *testing.T) *witnessFixture {
	t.Helper()

	funderKey, funderPubKey := scalarKey(t, 1)
	_, coSignerPubKey := scalarKey(t, 2)
	genesis := asset.Genesis{
		FirstPrevOut: wire.OutPoint{
			Hash: chainhash.Hash{0x31}, Index: 3,
		},
		Tag: "asset deposit witness test", OutputIndex: 0,
		Type: asset.Normal,
	}
	kit, err := NewKit(
		funderPubKey, coSignerPubKey, keychain.KeyLocator{Family: 7,
			Index: 9}, genesis.ID(), 144, &address.RegressionNetTap,
	)
	require.NoError(t, err)
	opTrueScriptKey, _, _, _, err := htlc.CreateOpTrueLeaf()
	require.NoError(t, err)
	depositAsset, err := asset.New(
		genesis, 1000, 0, 0,
		asset.NewScriptKey(opTrueScriptKey.PubKey), nil,
		asset.WithAssetVersion(asset.V1),
	)
	require.NoError(t, err)
	commitmentVersion := commitment.TapCommitmentV2
	tapCommitment, err := commitment.FromAssets(
		&commitmentVersion, depositAsset,
	)
	require.NoError(t, err)
	_, commitmentProof, err := tapCommitment.Proof(
		depositAsset.TapCommitmentKey(),
		depositAsset.AssetCommitmentKey(),
	)
	require.NoError(t, err)
	sibling, err := kit.timeoutPathSibling()
	require.NoError(t, err)
	siblingHash, err := sibling.TapHash()
	require.NoError(t, err)
	anchorPkScript, err := tapscript.PayToAddrScript(
		*kit.muSig2Key.PreTweakedKey, siblingHash, *tapCommitment,
	)
	require.NoError(t, err)

	const anchorValue = int64(50_000)
	anchorTx := wire.NewMsgTx(2)
	anchorTx.AddTxIn(&wire.TxIn{PreviousOutPoint: genesis.FirstPrevOut})
	anchorTx.AddTxOut(&wire.TxOut{
		Value: 1000, PkScript: []byte{txscript.OP_TRUE},
	})
	anchorTx.AddTxOut(&wire.TxOut{
		Value: anchorValue, PkScript: anchorPkScript,
	})
	assetProof := &proof.Proof{
		AnchorTx: *anchorTx,
		Asset:    *depositAsset,
		InclusionProof: proof.TaprootProof{
			OutputIndex: 1,
			InternalKey: kit.muSig2Key.PreTweakedKey,
			CommitmentProof: &proof.CommitmentProof{
				Proof:              *commitmentProof,
				TapSiblingPreimage: sibling,
			},
		},
	}
	_, err = assetProof.VerifyProofs()
	require.NoError(t, err)

	feeOutpoint := wire.OutPoint{Hash: chainhash.Hash{0x42}, Index: 5}
	assetInIndex := 1
	sweepTx := wire.NewMsgTx(2)
	sweepTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: feeOutpoint,
		Sequence:         wire.MaxTxInSequenceNum,
	})
	sweepTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: assetProof.OutPoint(),
		Sequence:         wire.MaxTxInSequenceNum,
	})
	sweepTx.AddTxOut(&wire.TxOut{
		Value: anchorValue - 2000, PkScript: []byte{txscript.OP_TRUE},
	})
	packet, err := psbt.NewFromUnsignedTx(sweepTx)
	require.NoError(t, err)
	prevOutputs := []*wire.TxOut{
		{Value: 3000, PkScript: []byte{txscript.OP_TRUE}},
		{Value: anchorValue, PkScript: anchorPkScript},
	}
	for idx := range prevOutputs {
		packet.Inputs[idx].WitnessUtxo = prevOutputs[idx]
	}

	return &witnessFixture{
		kit: kit, proof: assetProof, packet: packet,
		prevOutputs: prevOutputs, funderKey: funderKey,
		assetInIndex: assetInIndex,
	}
}

// TestVerifyProofReturnsAnchorRoot verifies the proof result can be used as
// the Taproot tweak for the deposit's cooperative MuSig2 key-path spend.
func TestVerifyProofReturnsAnchorRoot(t *testing.T) {
	fixture := newWitnessFixture(t)

	root, err := fixture.kit.VerifyProof(fixture.proof)
	require.NoError(t, err)
	require.Len(t, root, chainhash.HashSize)

	outputKey := txscript.ComputeTaprootOutputKey(
		fixture.kit.muSig2Key.PreTweakedKey, root,
	)
	pkScript, err := txscript.PayToTaprootScript(outputKey)
	require.NoError(t, err)
	require.Equal(
		t, fixture.prevOutputs[fixture.assetInIndex].PkScript, pkScript,
	)
}

// TestCreateTimeoutWitness verifies the refund signature and witness bind to
// the proof-selected input rather than an assumed position.
func TestCreateTimeoutWitness(t *testing.T) {
	fixture := newWitnessFixture(t)
	signer := &localSigner{
		privateKey: fixture.funderKey, expectedInput: fixture.assetInIndex,
	}
	spend, err := fixture.kit.CreateTimeoutWitness(
		t.Context(), signer, fixture.proof, fixture.packet,
	)
	require.NoError(t, err)
	require.Equal(t, uint32(fixture.assetInIndex), spend.InputIndex)
	require.Equal(t, fixture.kit.csvExpiry,
		fixture.packet.UnsignedTx.TxIn[fixture.assetInIndex].Sequence)
	require.Len(t, spend.Witness, 3)
	require.Equal(t, 1, signer.calls)

	tx := fixture.packet.UnsignedTx
	tx.TxIn[fixture.assetInIndex].Witness = spend.Witness
	prevOutFetcher := txscript.NewMultiPrevOutFetcher(nil)
	for idx, txIn := range tx.TxIn {
		prevOutFetcher.AddPrevOut(
			txIn.PreviousOutPoint, fixture.prevOutputs[idx],
		)
	}
	sigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)
	engine, err := txscript.NewEngine(
		fixture.prevOutputs[fixture.assetInIndex].PkScript, tx,
		fixture.assetInIndex, txscript.StandardVerifyFlags, nil,
		sigHashes, fixture.prevOutputs[fixture.assetInIndex].Value,
		prevOutFetcher,
	)
	require.NoError(t, err)
	require.NoError(t, engine.Execute())
}

// TestCreateTimeoutWitnessRejectsInvalidInputs covers the payment boundary
// checks that the legacy server helper previously omitted.
func TestCreateTimeoutWitnessRejectsInvalidInputs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*witnessFixture, *localSigner)
	}{
		{
			name: "missing proof input",
			mutate: func(f *witnessFixture, _ *localSigner) {
				f.packet.UnsignedTx.TxIn[1].PreviousOutPoint.Index++
			},
		},
		{
			name: "duplicate proof input",
			mutate: func(f *witnessFixture, _ *localSigner) {
				inputCopy := *f.packet.UnsignedTx.TxIn[1]
				f.packet.UnsignedTx.TxIn = append(
					f.packet.UnsignedTx.TxIn, &inputCopy,
				)
				f.packet.Inputs = append(
					f.packet.Inputs,
					psbt.PInput{WitnessUtxo: f.prevOutputs[1]},
				)
			},
		},
		{
			name: "missing fee prevout",
			mutate: func(f *witnessFixture, _ *localSigner) {
				f.packet.Inputs[0].WitnessUtxo = nil
			},
		},
		{
			name: "mismatched anchor value",
			mutate: func(f *witnessFixture, _ *localSigner) {
				f.packet.Inputs[1].WitnessUtxo.Value++
			},
		},
		{
			name: "invalid signature set",
			mutate: func(_ *witnessFixture, signer *localSigner) {
				signer.invalidSet = true
			},
		},
		{
			name: "wrong signing key",
			mutate: func(_ *witnessFixture, signer *localSigner) {
				signer.privateKey, _ = scalarKey(t, 3)
			},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newWitnessFixture(t)
			signer := &localSigner{
				privateKey:    fixture.funderKey,
				expectedInput: fixture.assetInIndex,
			}
			testCase.mutate(fixture, signer)
			_, err := fixture.kit.CreateTimeoutWitness(
				t.Context(), signer, fixture.proof,
				fixture.packet,
			)
			require.Error(t, err)
			require.Equal(
				t, wire.MaxTxInSequenceNum,
				fixture.packet.UnsignedTx.
					TxIn[fixture.assetInIndex].Sequence,
			)
		})
	}

	fixture := newWitnessFixture(t)
	_, err := fixture.kit.CreateTimeoutWitness(
		t.Context(), nil, fixture.proof, fixture.packet,
	)
	require.Error(t, err)
	_, err = fixture.kit.CreateTimeoutWitness(
		t.Context(), &localSigner{}, nil, fixture.packet,
	)
	require.Error(t, err)
}
