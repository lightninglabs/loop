package htlc

import (
	"bytes"
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
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tappsbt"
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
	legacyOpTrueKey = "037bbbb85ff774141ee36c2cc9f38b40be4a02c2a156c8a" +
		"1def8781cf39177deca"
	legacyOpTrueTweak = "a85b2107f791b26a84e7586c28cec7cb61202ed3d019" +
		"44d832500f363782d675"
	legacyOpTrueControl = "c17c79b9b26e463895eef5679d8558942c86c4ad2233" +
		"adef01bc3e6d540b3653fe"
	legacyOpTrueScriptKey = "027bbbb85ff774141ee36c2cc9f38b40be4a02c2a" +
		"156c8a1def8781cf39177deca"
	legacySuccessControl = "c13b46d262d2f610e9038b44beabdfe97ab5a0feb89" +
		"870acc2264edfb7f63ec2eccbf40123d0ca1191c0dba575c7a92ea255" +
		"f9e9fc81484e3929afeaa1a6f459d6a0a1a2a3a4a5a6a7a8a9aaab" +
		"acadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebf"
	legacyTimeoutControl = "c13b46d262d2f610e9038b44beabdfe97ab5a0feb89" +
		"870acc2264edfb7f63ec2ec00114fbfd01b5de82dfa6f54f0f27af41" +
		"d5a66ed39ac1a87d614fa12808f9adfa0a1a2a3a4a5a6a7a8a9aaab" +
		"acadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebf"
	legacyVPacket = "70736274ff010089020000000100000000000000000000000000000000" +
		"0000000000000000000000000000000000000000000000000002000000" +
		"00000000002251207c79b9b26e463895eef5679d8558942c86c4ad2233" +
		"adef01bc3e6d540b3653fee8030000000000002251207bbbb85ff77414" +
		"1ee36c2cc9f38b40be4a02c2a156c8a1def8781cf39177deca00000000" +
		"0170010101710574617072740172010100017065000000000000000000" +
		"0000000000000000000000000000000000000000000000000000000001" +
		"02030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e" +
		"1f00000000000000000000000000000000000000000000000000000000" +
		"0000000000017108000000000000000001720001730800000000000000" +
		"0001750001780000017001010171010101720800000000000000000179" +
		"0101017c080000000000000000017d0800000000000000000001700100" +
		"017101010172080000000000000001017321023b46d262d2f610e9038b" +
		"44beabdfe97ab5a0feb89870acc2264edfb7f63ec2ec01784101cbf401" +
		"23d0ca1191c0dba575c7a92ea255f9e9fc81484e3929afeaa1a6f459d6" +
		"00114fbfd01b5de82dfa6f54f0f27af41d5a66ed39ac1a87d614fa1280" +
		"8f9adf01790101017c080000000000000000017d080000000000000000" +
		"00"
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

func vectorParams(t *testing.T) Params {
	t.Helper()

	_, sender := scalarKey(t, 1)
	_, receiver := scalarKey(t, 2)
	var assetID asset.ID
	var swapHash lntypes.Hash
	for idx := range assetID {
		assetID[idx] = byte(idx)
		swapHash[idx] = byte(idx)
	}

	return Params{
		SenderPubKey:   sender,
		ReceiverPubKey: receiver,
		AssetID:        assetID,
		Amount:         1000,
		SwapHash:       swapHash,
		CsvExpiry:      144,
		AddressParams:  cloneAddressParams(address.RegressionNetTap),
	}
}

func newVectorKit(t *testing.T, policy Policy) *SwapKit {
	t.Helper()

	kit, err := NewSwapKit(policy, vectorParams(t))
	require.NoError(t, err)

	return kit
}

// TestLegacyDepositVectors freezes the scripts and aggregate key used by the
// existing valid server deposit HTLC contract.
func TestLegacyDepositVectors(t *testing.T) {
	legacyKit := newVectorKit(t, LegacyDepositV0)

	successScript, err := legacyKit.GetSuccessScript()
	require.NoError(t, err)
	require.Equal(t, legacySuccessScript,
		hex.EncodeToString(successScript))

	timeoutScript, err := legacyKit.GetTimeoutScript()
	require.NoError(t, err)
	require.Equal(t, legacyTimeoutScript,
		hex.EncodeToString(timeoutScript))

	aggregateKey, err := legacyKit.GetAggregateKey()
	require.NoError(t, err)
	require.Equal(t, legacyAggregateKey,
		hex.EncodeToString(aggregateKey.SerializeCompressed()))

	sibling, err := legacyKit.GetSiblingPreimage()
	require.NoError(t, err)
	siblingBytes, _, err := commitment.MaybeEncodeTapscriptPreimage(
		&sibling,
	)
	require.NoError(t, err)
	require.Equal(t, legacySibling, hex.EncodeToString(siblingBytes))

	opTrueKey, _, _, opTrueControl, err := CreateOpTrueLeaf()
	require.NoError(t, err)
	require.Equal(t, legacyOpTrueKey, hex.EncodeToString(
		opTrueKey.PubKey.SerializeCompressed(),
	))
	require.Equal(t, legacyOpTrueTweak,
		hex.EncodeToString(opTrueKey.TweakedScriptKey.Tweak))
	opTrueControlBytes, err := opTrueControl.ToBytes()
	require.NoError(t, err)
	require.Equal(t, legacyOpTrueControl,
		hex.EncodeToString(opTrueControlBytes))
	opTrueScriptKey, err := GetOpTrueScriptKey()
	require.NoError(t, err)
	require.Equal(t, legacyOpTrueScriptKey,
		hex.EncodeToString(opTrueScriptKey))

	assetRoot := make([]byte, 32)
	for idx := range assetRoot {
		assetRoot[idx] = byte(0xa0 + idx)
	}
	successControl, err := legacyKit.GenSuccessBtcControlBlock(assetRoot)
	require.NoError(t, err)
	successControlBytes, err := successControl.ToBytes()
	require.NoError(t, err)
	require.Equal(t, legacySuccessControl,
		hex.EncodeToString(successControlBytes))
	timeoutControl, err := legacyKit.GenTimeoutBtcControlBlock(assetRoot)
	require.NoError(t, err)
	timeoutControlBytes, err := timeoutControl.ToBytes()
	require.NoError(t, err)
	require.Equal(t, legacyTimeoutControl,
		hex.EncodeToString(timeoutControlBytes))
	anchorPkScript, err := legacyKit.GetPkScriptFromRoot(assetRoot)
	require.NoError(t, err)
	require.Equal(t, legacyAnchorPkScript,
		hex.EncodeToString(anchorPkScript))

	packet, err := legacyKit.CreateHtlcVpkt()
	require.NoError(t, err)
	var packetBytes bytes.Buffer
	require.NoError(t, packet.Serialize(&packetBytes))
	require.Equal(t, legacyVPacket,
		hex.EncodeToString(packetBytes.Bytes()))
}

// TestTimeoutScriptNumberEncoding freezes BIP68 boundary encodings so a
// library update cannot silently change an already negotiated script.
func TestTimeoutScriptNumberEncoding(t *testing.T) {
	_, sender := scalarKey(t, 1)
	const prefix = "2079be667ef9dcbbac55a06295ce870b07029bfcdb2dce" +
		"28d959f2815b16f81798ad"
	tests := []struct {
		expiry int64
		hex    string
	}{
		{expiry: 1, hex: prefix + "51b2"},
		{expiry: 16, hex: prefix + "60b2"},
		{expiry: 128, hex: prefix + "028000b2"},
		{expiry: 256, hex: prefix + "020001b2"},
	}

	for _, testCase := range tests {
		script, err := GenTimeoutPathScript(sender, testCase.expiry)
		require.NoError(t, err)
		require.Equal(t, testCase.hex, hex.EncodeToString(script))
	}
}

// TestNewSwapKitValidation verifies malformed contracts fail before any
// scripts or addresses can be derived.
func TestNewSwapKitValidation(t *testing.T) {
	validParams := vectorParams(t)
	_, err := NewSwapKit(PolicyUnknown, validParams)
	require.Error(t, err)
	_, err = NewSwapKit(Policy(255), validParams)
	require.Error(t, err)

	tests := []struct {
		name   string
		mutate func(*Params)
	}{
		{
			name: "missing sender",
			mutate: func(params *Params) {
				params.SenderPubKey = nil
			},
		},
		{
			name: "missing receiver",
			mutate: func(params *Params) {
				params.ReceiverPubKey = nil
			},
		},
		{
			name: "identical keys",
			mutate: func(params *Params) {
				params.ReceiverPubKey = params.SenderPubKey
			},
		},
		{
			name: "zero amount",
			mutate: func(params *Params) {
				params.Amount = 0
			},
		},
		{
			name: "zero asset ID",
			mutate: func(params *Params) {
				params.AssetID = asset.ID{}
			},
		},
		{
			name: "zero swap hash",
			mutate: func(params *Params) {
				params.SwapHash = lntypes.Hash{}
			},
		},
		{
			name: "zero expiry",
			mutate: func(params *Params) {
				params.CsvExpiry = 0
			},
		},
		{
			name: "expiry equals success sequence",
			mutate: func(params *Params) {
				params.CsvExpiry = SuccessSequence
			},
		},
		{
			name: "time based expiry",
			mutate: func(params *Params) {
				params.CsvExpiry = wire.SequenceLockTimeIsSeconds
			},
		},
		{
			name: "missing address params",
			mutate: func(params *Params) {
				params.AddressParams = nil
			},
		},
		{
			name: "incomplete address params",
			mutate: func(params *Params) {
				params.AddressParams = &address.ChainParams{}
			},
		},
		{
			name: "mismatched address params",
			mutate: func(params *Params) {
				mismatch := address.RegressionNetTap
				mismatch.TapHRP = address.MainNetTap.TapHRP
				params.AddressParams = &mismatch
			},
		},
		{
			name: "unsupported address params",
			mutate: func(params *Params) {
				unsupported := address.RegressionNetTap
				unsupported.TapHRP = "tapunknown"
				params.AddressParams = &unsupported
			},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			params := validParams
			if testCase.mutate != nil {
				testCase.mutate(&params)
			}
			_, err := NewSwapKit(LegacyDepositV0, params)
			require.Error(t, err)
		})
	}

	_, err = GenSuccessPathScript(nil, lntypes.Hash{})
	require.Error(t, err)
	_, err = GenTimeoutPathScript(nil, 1)
	require.Error(t, err)
	_, err = GenTimeoutPathScript(validParams.SenderPubKey, 0)
	require.Error(t, err)
}

// TestSupportedNetworks ensures shared testnet HRPs do not collapse signet or
// testnet4 into testnet3, and both native and lnd-compatible simnet coin types
// remain supported.
func TestSupportedNetworks(t *testing.T) {
	rawSimNet := cloneAddressParams(address.SimNetTap)
	rawSimNet.HDCoinType = rawSimNetHDCoinType
	lndSimNet := cloneAddressParams(
		address.ParamsForChain(address.SimNetTap.Name),
	)

	networks := []*address.ChainParams{
		&address.MainNetTap,
		&address.TestNet3Tap,
		&address.TestNet4Tap,
		&address.RegressionNetTap,
		&address.SigNetTap,
		rawSimNet,
		lndSimNet,
	}
	for _, network := range networks {
		params := vectorParams(t)
		params.AddressParams = network
		kit, err := NewSwapKit(LegacyDepositV0, params)
		require.NoError(t, err)
		require.Equal(t, network.Net, kit.AddressParams().Net)
		require.Equal(t, network.TapHRP, kit.AddressParams().TapHRP)
		require.Equal(t, network.HDCoinType,
			kit.AddressParams().HDCoinType)
	}
}

// TestSwapKitIsImmutable verifies constructor inputs and returned address
// parameters cannot alter a previously constructed contract.
func TestSwapKitIsImmutable(t *testing.T) {
	params := vectorParams(t)
	kit, err := NewSwapKit(LegacyDepositV0, params)
	require.NoError(t, err)

	params.AssetID[0] = 99
	params.AddressParams.TapHRP = "mutated"
	require.Zero(t, kit.AssetID()[0])
	require.Equal(t, address.RegressionNetTap.TapHRP,
		kit.AddressParams().TapHRP)

	returnedParams := kit.AddressParams()
	returnedParams.TapHRP = "also-mutated"
	returnedParams.Params.Name = "mutated-chain"
	require.Equal(t, address.RegressionNetTap.TapHRP,
		kit.AddressParams().TapHRP)
	require.Equal(t, address.RegressionNetTap.Params.Name,
		kit.AddressParams().Params.Name)

	packet, err := kit.CreateHtlcVpkt()
	require.NoError(t, err)
	packet.ChainParams.TapHRP = "packet-mutated"
	nextPacket, err := kit.CreateHtlcVpkt()
	require.NoError(t, err)
	require.Equal(t, address.RegressionNetTap.TapHRP,
		nextPacket.ChainParams.TapHRP)
}

// TestCreateHtlcVpkt freezes the output roles and flags that existing Taproot
// Assets deposit transfers depend upon.
func TestCreateHtlcVpkt(t *testing.T) {
	kit := newVectorKit(t, LegacyDepositV0)
	packet, err := kit.CreateHtlcVpkt()
	require.NoError(t, err)

	require.Equal(t, tappsbt.V1, packet.Version)
	require.Len(t, packet.Inputs, 1)
	require.Equal(t, kit.AssetID(), packet.Inputs[0].PrevID.ID)
	require.Len(t, packet.Outputs, 2)

	splitRoot := packet.Outputs[0]
	require.Equal(t, asset.V1, splitRoot.AssetVersion)
	require.Zero(t, splitRoot.Amount)
	require.Equal(t, uint32(0), splitRoot.AnchorOutputIndex)
	require.True(t, splitRoot.Interactive)
	require.Equal(t, asset.NUMSScriptKey.PubKey.SerializeCompressed(),
		splitRoot.ScriptKey.PubKey.SerializeCompressed())

	htlcOutput := packet.Outputs[1]
	require.Equal(t, asset.V1, htlcOutput.AssetVersion)
	require.Equal(t, kit.Amount(), htlcOutput.Amount)
	require.Equal(t, uint32(1), htlcOutput.AnchorOutputIndex)
	require.True(t, htlcOutput.Interactive)
	require.NotNil(t, htlcOutput.AnchorOutputInternalKey)
	require.NotNil(t, htlcOutput.AnchorOutputTapscriptSibling)
}

// TestAssetHtlcScriptEngine proves both paths execute and reject the wrong
// preimage, signer, and relative sequence.
func TestAssetHtlcScriptEngine(t *testing.T) {
	senderPriv, senderPub := scalarKey(t, 1)
	receiverPriv, receiverPub := scalarKey(t, 2)
	var assetID asset.ID
	var preimage lntypes.Preimage
	for idx := range preimage {
		assetID[idx] = byte(idx)
		preimage[idx] = byte(31 - idx)
	}

	kit, err := NewSwapKit(LegacyDepositV0, Params{
		SenderPubKey: senderPub, ReceiverPubKey: receiverPub,
		AssetID: assetID, Amount: 1000, SwapHash: preimage.Hash(),
		CsvExpiry: 144, AddressParams: &address.RegressionNetTap,
	})
	require.NoError(t, err)

	tests := []struct {
		name       string
		success    bool
		sequence   uint32
		signingKey *btcec.PrivateKey
		preimage   lntypes.Preimage
		valid      bool
	}{
		{
			name: "success", success: true,
			sequence: SuccessSequence, signingKey: receiverPriv,
			preimage: preimage, valid: true,
		},
		{
			name: "wrong preimage", success: true,
			sequence: SuccessSequence, signingKey: receiverPriv,
			preimage: lntypes.Preimage{1},
		},
		{
			name: "wrong success key", success: true,
			sequence: SuccessSequence, signingKey: senderPriv,
			preimage: preimage,
		},
		{
			name: "timeout", sequence: kit.CsvExpiry(),
			signingKey: senderPriv, valid: true,
		},
		{
			name:     "timeout one block early",
			sequence: kit.CsvExpiry() - 1, signingKey: senderPriv,
		},
		{
			name: "wrong timeout key", sequence: kit.CsvExpiry(),
			signingKey: receiverPriv,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			assertScriptSpend(t, kit, testCase.success,
				testCase.sequence, testCase.signingKey,
				testCase.preimage, testCase.valid)
		})
	}
}

func assertScriptSpend(t *testing.T, kit *SwapKit, success bool,
	sequence uint32, signingKey *btcec.PrivateKey,
	preimage lntypes.Preimage, valid bool) {

	t.Helper()

	assetRoot := bytes.Repeat([]byte{0x42}, 32)
	var (
		controlBlock *txscript.ControlBlock
		leaf         txscript.TapLeaf
		script       []byte
		err          error
	)
	if success {
		controlBlock, err = kit.GenSuccessBtcControlBlock(assetRoot)
		require.NoError(t, err)
		leaf, err = kit.GetSuccessLeaf()
		require.NoError(t, err)
		script = leaf.Script
	} else {
		controlBlock, err = kit.GenTimeoutBtcControlBlock(assetRoot)
		require.NoError(t, err)
		leaf, err = kit.GetTimeoutLeaf()
		require.NoError(t, err)
		script = leaf.Script
	}

	rootHash := controlBlock.RootHash(script)
	internalKey, err := kit.GetAggregateKey()
	require.NoError(t, err)
	outputKey := txscript.ComputeTaprootOutputKey(internalKey, rootHash)
	pkScript, err := txscript.PayToTaprootScript(outputKey)
	require.NoError(t, err)

	const value = int64(50_000)
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash: chainhash.Hash{1}, Index: 2,
		},
		Sequence: sequence,
	})
	tx.AddTxOut(&wire.TxOut{Value: value - 1000,
		PkScript: []byte{txscript.OP_TRUE}})
	prevOutFetcher := txscript.NewCannedPrevOutputFetcher(pkScript, value)
	sigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)
	signature, err := txscript.RawTxInTapscriptSignature(
		tx, sigHashes, 0, value, pkScript, leaf,
		txscript.SigHashDefault, signingKey,
	)
	require.NoError(t, err)
	controlBytes, err := controlBlock.ToBytes()
	require.NoError(t, err)

	if success {
		tx.TxIn[0].Witness = wire.TxWitness{
			preimage[:], signature, script, controlBytes,
		}
	} else {
		tx.TxIn[0].Witness = wire.TxWitness{
			signature, script, controlBytes,
		}
	}

	engine, err := txscript.NewEngine(
		pkScript, tx, 0, txscript.StandardVerifyFlags, nil,
		sigHashes, value, prevOutFetcher,
	)
	require.NoError(t, err)
	if valid {
		require.NoError(t, engine.Execute())
	} else {
		require.Error(t, engine.Execute())
	}
}

type localSigner struct {
	lndclient.SignerClient

	privateKey    *btcec.PrivateKey
	expectedInput int
	calls         int
}

func (s *localSigner) SignOutputRaw(_ context.Context, tx *wire.MsgTx,
	descriptors []*lndclient.SignDescriptor,
	prevOutputs []*wire.TxOut) ([][]byte, error) {

	s.calls++
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
	kit          *SwapKit
	proof        *proof.Proof
	packet       *psbt.Packet
	prevOutputs  []*wire.TxOut
	senderKey    *btcec.PrivateKey
	receiverKey  *btcec.PrivateKey
	preimage     lntypes.Preimage
	assetInIndex int
}

func newWitnessFixture(t *testing.T, sequence uint32) *witnessFixture {
	return newWitnessFixtureWithVersions(
		t, sequence, asset.V1, commitment.TapCommitmentV2,
	)
}

func newWitnessFixtureWithVersions(t *testing.T, sequence uint32,
	assetVersion asset.Version,
	commitmentVersion commitment.TapCommitmentVersion) *witnessFixture {

	t.Helper()

	senderKey, senderPubKey := scalarKey(t, 1)
	receiverKey, receiverPubKey := scalarKey(t, 2)
	var preimage lntypes.Preimage
	for idx := range preimage {
		preimage[idx] = byte(31 - idx)
	}

	genesis := asset.Genesis{
		FirstPrevOut: wire.OutPoint{
			Hash:  chainhash.Hash{0x31},
			Index: 3,
		},
		Tag:         "asset htlc witness test",
		OutputIndex: 0,
		Type:        asset.Normal,
	}
	opTrueScriptKey, _, _, _, err := CreateOpTrueLeaf()
	require.NoError(t, err)
	opTrueScriptKey = asset.NewScriptKey(opTrueScriptKey.PubKey)
	htlcAsset, err := asset.New(
		genesis, 1000, 0, 0, opTrueScriptKey, nil,
		asset.WithAssetVersion(assetVersion),
	)
	require.NoError(t, err)

	kit, err := NewSwapKit(LegacyDepositV0, Params{
		SenderPubKey:   senderPubKey,
		ReceiverPubKey: receiverPubKey,
		AssetID:        genesis.ID(),
		Amount:         htlcAsset.Amount,
		SwapHash:       preimage.Hash(),
		CsvExpiry:      144,
		AddressParams:  &address.RegressionNetTap,
	})
	require.NoError(t, err)

	tapCommitment, err := commitment.FromAssets(
		&commitmentVersion, htlcAsset,
	)
	require.NoError(t, err)
	_, commitmentProof, err := tapCommitment.Proof(
		htlcAsset.TapCommitmentKey(),
		htlcAsset.AssetCommitmentKey(),
	)
	require.NoError(t, err)
	sibling, err := kit.GetSiblingPreimage()
	require.NoError(t, err)
	siblingHash, err := sibling.TapHash()
	require.NoError(t, err)
	internalKey, err := kit.GetAggregateKey()
	require.NoError(t, err)
	anchorPkScript, err := tapscript.PayToAddrScript(
		*internalKey, siblingHash, *tapCommitment,
	)
	require.NoError(t, err)

	const anchorValue = int64(50_000)
	anchorTx := wire.NewMsgTx(2)
	anchorTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: genesis.FirstPrevOut,
	})
	anchorTx.AddTxOut(&wire.TxOut{
		Value:    1000,
		PkScript: []byte{txscript.OP_TRUE},
	})
	anchorTx.AddTxOut(&wire.TxOut{
		Value:    anchorValue,
		PkScript: anchorPkScript,
	})

	assetProof := &proof.Proof{
		AnchorTx: *anchorTx,
		Asset:    *htlcAsset,
		InclusionProof: proof.TaprootProof{
			OutputIndex: 1,
			InternalKey: internalKey,
			CommitmentProof: &proof.CommitmentProof{
				Proof:              *commitmentProof,
				TapSiblingPreimage: &sibling,
			},
		},
	}
	_, err = assetProof.VerifyProofs()
	require.NoError(t, err)

	feeOutPoint := wire.OutPoint{
		Hash:  chainhash.Hash{0x42},
		Index: 5,
	}
	assetInIndex := 1
	sweepTx := wire.NewMsgTx(2)
	sweepTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: feeOutPoint,
		Sequence:         wire.MaxTxInSequenceNum,
	})
	sweepTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: assetProof.OutPoint(),
		Sequence:         sequence,
	})
	sweepTx.AddTxOut(&wire.TxOut{
		Value:    anchorValue - 2000,
		PkScript: []byte{txscript.OP_TRUE},
	})
	sweepPacket, err := psbt.NewFromUnsignedTx(sweepTx)
	require.NoError(t, err)
	prevOutputs := []*wire.TxOut{
		{
			Value:    3000,
			PkScript: []byte{txscript.OP_TRUE},
		},
		{
			Value:    anchorValue,
			PkScript: append([]byte(nil), anchorPkScript...),
		},
	}
	for idx := range prevOutputs {
		sweepPacket.Inputs[idx].WitnessUtxo = prevOutputs[idx]
	}

	return &witnessFixture{
		kit: kit, proof: assetProof, packet: sweepPacket,
		prevOutputs: prevOutputs, senderKey: senderKey,
		receiverKey: receiverKey, preimage: preimage,
		assetInIndex: assetInIndex,
	}
}

// TestGetPkScriptFromProof verifies that anchor reconstruction follows the
// commitment version proven on chain. The asset version alone cannot
// distinguish a legacy commitment from a version-two commitment.
func TestGetPkScriptFromProof(t *testing.T) {
	tests := []struct {
		name              string
		assetVersion      asset.Version
		commitmentVersion commitment.TapCommitmentVersion
	}{
		{
			name:              "legacy v0",
			assetVersion:      asset.V0,
			commitmentVersion: commitment.TapCommitmentV0,
		},
		{
			name:              "legacy v1",
			assetVersion:      asset.V1,
			commitmentVersion: commitment.TapCommitmentV1,
		},
		{
			name:              "version two",
			assetVersion:      asset.V1,
			commitmentVersion: commitment.TapCommitmentV2,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newWitnessFixtureWithVersions(
				t, SuccessSequence, testCase.assetVersion,
				testCase.commitmentVersion,
			)
			pkScript, err := fixture.kit.GetPkScriptFromProof(
				fixture.proof,
			)
			require.NoError(t, err)
			require.Equal(
				t, fixture.prevOutputs[fixture.assetInIndex].PkScript,
				pkScript,
			)
		})
	}
}

func (f *witnessFixture) executeWitness(t *testing.T,
	witness *SpendWitness) {

	t.Helper()

	require.Equal(t, uint32(f.assetInIndex), witness.InputIndex)
	tx := f.packet.UnsignedTx
	tx.TxIn[witness.InputIndex].Witness = witness.Witness

	prevOutFetcher := txscript.NewMultiPrevOutFetcher(nil)
	for idx, txIn := range tx.TxIn {
		prevOutFetcher.AddPrevOut(
			txIn.PreviousOutPoint, f.prevOutputs[idx],
		)
	}
	sigHashes := txscript.NewTxSigHashes(tx, prevOutFetcher)
	prevOutput := f.prevOutputs[witness.InputIndex]
	engine, err := txscript.NewEngine(
		prevOutput.PkScript, tx, int(witness.InputIndex),
		txscript.StandardVerifyFlags, nil, sigHashes,
		prevOutput.Value, prevOutFetcher,
	)
	require.NoError(t, err)
	require.NoError(t, engine.Execute())
}

// TestCreateProofBoundWitnesses proves both witness builders sign the input
// selected by the asset proof even when an unrelated input precedes it.
func TestCreateProofBoundWitnesses(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		fixture := newWitnessFixture(t, SuccessSequence)
		inputIndex, err := fixture.kit.AssetInputIndex(
			fixture.proof, fixture.packet,
		)
		require.NoError(t, err)
		require.Equal(t, uint32(fixture.assetInIndex), inputIndex)

		signer := &localSigner{
			privateKey:    fixture.receiverKey,
			expectedInput: fixture.assetInIndex,
		}
		witness, err := fixture.kit.CreatePreimageWitness(
			t.Context(), signer, fixture.proof, fixture.packet,
			keychain.KeyLocator{}, fixture.preimage,
		)
		require.NoError(t, err)
		require.Equal(t, 1, signer.calls)
		require.Len(t, witness.Witness, 4)
		fixture.executeWitness(t, witness)
	})

	t.Run("timeout", func(t *testing.T) {
		fixture := newWitnessFixture(t, 144)
		signer := &localSigner{
			privateKey:    fixture.senderKey,
			expectedInput: fixture.assetInIndex,
		}
		witness, err := fixture.kit.CreateTimeoutWitness(
			t.Context(), signer, fixture.proof, fixture.packet,
			keychain.KeyLocator{},
		)
		require.NoError(t, err)
		require.Equal(t, 1, signer.calls)
		require.Len(t, witness.Witness, 3)
		fixture.executeWitness(t, witness)
	})
}

// TestProofBoundWitnessValidation freezes the checks that keep proof, PSBT,
// contract, and signing key bound to the same funded asset output.
func TestProofBoundWitnessValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*witnessFixture)
		sign   func(*testing.T, *witnessFixture, *localSigner) error
	}{
		{
			name: "wrong success sequence",
			mutate: func(fixture *witnessFixture) {
				fixture.packet.UnsignedTx.TxIn[1].Sequence = 2
			},
		},
		{
			name: "wrong preimage",
			sign: func(t *testing.T, fixture *witnessFixture,
				signer *localSigner) error {

				_, err := fixture.kit.CreatePreimageWitness(
					t.Context(), signer, fixture.proof,
					fixture.packet, keychain.KeyLocator{},
					lntypes.Preimage{1},
				)
				return err
			},
		},
		{
			name: "wrong timeout sequence",
			mutate: func(fixture *witnessFixture) {
				fixture.packet.UnsignedTx.TxIn[1].Sequence = 143
			},
			sign: func(t *testing.T, fixture *witnessFixture,
				signer *localSigner) error {

				_, err := fixture.kit.CreateTimeoutWitness(
					t.Context(), signer, fixture.proof,
					fixture.packet, keychain.KeyLocator{},
				)
				return err
			},
		},
		{
			name: "wrong success signing key",
			mutate: func(fixture *witnessFixture) {
				fixture.receiverKey = fixture.senderKey
			},
		},
		{
			name: "wrong timeout signing key",
			sign: func(t *testing.T, fixture *witnessFixture,
				signer *localSigner) error {

				fixture.packet.UnsignedTx.TxIn[1].Sequence =
					fixture.kit.CsvExpiry()
				_, err := fixture.kit.CreateTimeoutWitness(
					t.Context(), signer, fixture.proof,
					fixture.packet, keychain.KeyLocator{},
				)
				return err
			},
		},
		{
			name: "witness value mismatch",
			mutate: func(fixture *witnessFixture) {
				fixture.packet.Inputs[1].WitnessUtxo.Value++
			},
		},
		{
			name: "witness script mismatch",
			mutate: func(fixture *witnessFixture) {
				fixture.packet.Inputs[1].WitnessUtxo.PkScript =
					[]byte{txscript.OP_TRUE}
			},
		},
		{
			name: "missing proof input",
			mutate: func(fixture *witnessFixture) {
				fixture.packet.UnsignedTx.TxIn =
					fixture.packet.UnsignedTx.TxIn[:1]
				fixture.packet.Inputs = fixture.packet.Inputs[:1]
			},
		},
		{
			name: "duplicate proof input",
			mutate: func(fixture *witnessFixture) {
				assetInput := *fixture.packet.UnsignedTx.TxIn[1]
				fixture.packet.UnsignedTx.TxIn = append(
					fixture.packet.UnsignedTx.TxIn, &assetInput,
				)
				fixture.packet.Inputs = append(
					fixture.packet.Inputs,
					psbt.PInput{WitnessUtxo: fixture.prevOutputs[1]},
				)
			},
		},
		{
			name: "missing fee input metadata",
			mutate: func(fixture *witnessFixture) {
				fixture.packet.Inputs[0].WitnessUtxo = nil
			},
		},
		{
			name: "version one sweep",
			mutate: func(fixture *witnessFixture) {
				fixture.packet.UnsignedTx.Version = 1
			},
		},
		{
			name: "asset version",
			mutate: func(fixture *witnessFixture) {
				fixture.proof.Asset.Version = asset.V0
			},
		},
		{
			name: "asset script key",
			mutate: func(fixture *witnessFixture) {
				_, publicKey := scalarKey(t, 3)
				fixture.proof.Asset.ScriptKey =
					asset.NewScriptKey(publicKey)
			},
		},
		{
			name: "asset script version",
			mutate: func(fixture *witnessFixture) {
				fixture.proof.Asset.ScriptVersion++
			},
		},
		{
			name: "asset lock time",
			mutate: func(fixture *witnessFixture) {
				fixture.proof.Asset.LockTime = 1
			},
		},
		{
			name: "asset relative lock time",
			mutate: func(fixture *witnessFixture) {
				fixture.proof.Asset.RelativeLockTime = 1
			},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newWitnessFixture(t, SuccessSequence)
			if testCase.mutate != nil {
				testCase.mutate(fixture)
			}
			signer := &localSigner{
				privateKey:    fixture.receiverKey,
				expectedInput: fixture.assetInIndex,
			}
			if testCase.sign != nil {
				require.Error(t, testCase.sign(t, fixture, signer))
				return
			}

			_, err := fixture.kit.CreatePreimageWitness(
				t.Context(), signer, fixture.proof,
				fixture.packet, keychain.KeyLocator{},
				fixture.preimage,
			)
			require.Error(t, err)
		})
	}
}

// TestValidateAssetPolicy isolates the asset-layer checks from proof
// verification so weakening one boundary cannot be masked by the other.
func TestValidateAssetPolicy(t *testing.T) {
	fixture := newWitnessFixture(t, SuccessSequence)
	require.NoError(t, fixture.kit.validateAsset(&fixture.proof.Asset))

	tests := []struct {
		name   string
		mutate func(*asset.Asset)
	}{
		{
			name: "asset ID",
			mutate: func(candidate *asset.Asset) {
				candidate.Genesis.Tag = "different asset"
			},
		},
		{
			name: "amount",
			mutate: func(candidate *asset.Asset) {
				candidate.Amount++
			},
		},
		{
			name: "asset version",
			mutate: func(candidate *asset.Asset) {
				candidate.Version = asset.Version(255)
			},
		},
		{
			name: "script version",
			mutate: func(candidate *asset.Asset) {
				candidate.ScriptVersion++
			},
		},
		{
			name: "script key",
			mutate: func(candidate *asset.Asset) {
				_, publicKey := scalarKey(t, 3)
				candidate.ScriptKey = asset.NewScriptKey(publicKey)
			},
		},
		{
			name: "absolute locktime",
			mutate: func(candidate *asset.Asset) {
				candidate.LockTime = 1
			},
		},
		{
			name: "relative locktime",
			mutate: func(candidate *asset.Asset) {
				candidate.RelativeLockTime = 1
			},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			candidate := fixture.proof.Asset.CopySpendTemplate()
			testCase.mutate(candidate)
			require.Error(t, fixture.kit.validateAsset(candidate))
		})
	}

	legacyV0Asset := fixture.proof.Asset.CopySpendTemplate()
	legacyV0Asset.Version = asset.V0
	require.NoError(t, fixture.kit.validateAsset(legacyV0Asset))
}

// TestControlBlockRootLength rejects roots that cannot represent a Taproot
// branch hash.
func TestControlBlockRootLength(t *testing.T) {
	kit := newVectorKit(t, LegacyDepositV0)
	_, err := kit.GenSuccessBtcControlBlock(make([]byte, 31))
	require.Error(t, err)
	_, err = kit.GenTimeoutBtcControlBlock(make([]byte, 33))
	require.Error(t, err)
}

// TestFindUniqueInput proves sweep input selection is based on the exact
// proof outpoint rather than a positional convention.
func TestFindUniqueInput(t *testing.T) {
	target := wire.OutPoint{Hash: chainhash.Hash{9}, Index: 7}
	other := wire.OutPoint{Hash: chainhash.Hash{8}, Index: 3}

	tests := []struct {
		name      string
		inputs    []*wire.TxIn
		expected  int
		expectErr bool
	}{
		{
			name: "first",
			inputs: []*wire.TxIn{
				{PreviousOutPoint: target},
				{PreviousOutPoint: other},
			},
			expected: 0,
		},
		{
			name: "middle of three",
			inputs: []*wire.TxIn{
				{PreviousOutPoint: other},
				{PreviousOutPoint: target},
				{
					PreviousOutPoint: wire.OutPoint{
						Hash: chainhash.Hash{7},
					},
				},
			},
			expected: 1,
		},
		{
			name: "missing",
			inputs: []*wire.TxIn{
				{PreviousOutPoint: other},
			},
			expectErr: true,
		},
		{
			name: "duplicate",
			inputs: []*wire.TxIn{
				{PreviousOutPoint: target},
				{PreviousOutPoint: target},
			},
			expectErr: true,
		},
		{
			name: "nil input",
			inputs: []*wire.TxIn{
				{PreviousOutPoint: target}, nil,
			},
			expectErr: true,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			tx := wire.NewMsgTx(2)
			tx.TxIn = testCase.inputs
			index, err := findUniqueInput(tx, target)
			if testCase.expectErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, testCase.expected, index)
		})
	}
}

// TestSiblingPreimageRoundTrip ensures the branch preimage retains its
// Taproot hash when encoded for an address RPC.
func TestSiblingPreimageRoundTrip(t *testing.T) {
	kit := newVectorKit(t, LegacyDepositV0)
	sibling, err := kit.GetSiblingPreimage()
	require.NoError(t, err)
	encoded, _, err := commitment.MaybeEncodeTapscriptPreimage(&sibling)
	require.NoError(t, err)
	require.NotEmpty(t, encoded)

	timeoutLeaf, err := kit.GetTimeOutLeaf()
	require.NoError(t, err)
	successLeaf, err := kit.GetSuccessLeaf()
	require.NoError(t, err)
	expectedPreimage := commitment.NewPreimageFromBranch(
		txscript.NewTapBranch(timeoutLeaf, successLeaf),
	)
	decoded, err := expectedPreimage.TapHash()
	require.NoError(t, err)
	actualHash, err := sibling.TapHash()
	require.NoError(t, err)
	require.Equal(t, decoded, actualHash)
}
