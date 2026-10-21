package tapsdk

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tapsend"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// bindingSDK models successful authenticated tapd proof verification. These
// tests exercise the driver's additional policy and current-chain checks,
// which must reject even a cryptographically valid proof for another contract.
type bindingSDK struct {
	sdk.Client

	valid bool
}

func (s *bindingSDK) VerifyProof(context.Context,
	[]byte) (*sdk.VerifyProofResponse, error) {

	return &sdk.VerifyProofResponse{Valid: s.valid}, nil
}

type bindingChain struct {
	Chain

	tip     uint32
	blocks  map[uint32]*wire.MsgBlock
	unspent bool
}

func (c *bindingChain) Height(context.Context) (uint32, error) {
	return c.tip, nil
}

func (c *bindingChain) Block(_ context.Context,
	height uint32) (*wire.MsgBlock, error) {

	block := c.blocks[height]
	if block == nil {
		return nil, swapkit.ErrPending
	}
	return block, nil
}

func (c *bindingChain) Unspent(context.Context,
	wire.OutPoint) (bool, error) {

	return c.unspent, nil
}

func bindingKey(scalar byte) *btcec.PrivateKey {
	key, _ := btcec.PrivKeyFromBytes([]byte{scalar})
	return key
}

func bindingPub(scalar byte) [33]byte {
	var key [33]byte
	copy(key[:], bindingKey(scalar).PubKey().SerializeCompressed())
	return key
}

// bindingProof constructs a real isolated Taproot commitment; an optional
// second asset deliberately makes reconstruction from the disclosed one fail.
func bindingProof(t *testing.T, genesis asset.Genesis, script,
	internal *btcec.PublicKey, sibling *commitment.TapscriptPreimage,
	previous wire.OutPoint, height uint32, hidden bool) *proof.Proof {

	t.Helper()
	group := &asset.GroupKey{GroupPubKey: *bindingKey(11).PubKey()}
	a, err := asset.New(genesis, 1000, 0, 0,
		asset.NewScriptKey(script), group,
		asset.WithAssetVersion(asset.V1))
	require.NoError(t, err)
	assets := []*asset.Asset{a}
	if hidden {
		otherGenesis := genesis
		otherGenesis.Tag += "-hidden"
		other, err := asset.New(otherGenesis, 1, 0, 0,
			asset.NewScriptKey(bindingKey(9).PubKey()), nil,
			asset.WithAssetVersion(asset.V1))
		require.NoError(t, err)
		assets = append(assets, other)
	}
	version := commitment.TapCommitmentV2
	tree, err := commitment.FromAssets(&version, assets...)
	require.NoError(t, err)
	_, inclusion, err := tree.Proof(a.TapCommitmentKey(),
		a.AssetCommitmentKey())
	require.NoError(t, err)
	pkScript, _, _, err := tapsend.AnchorOutputScript(
		internal, sibling, tree,
	)
	require.NoError(t, err)
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&previous, nil, nil))
	tx.AddTxOut(wire.NewTxOut(10_000, pkScript))
	return &proof.Proof{
		PrevOut: previous, AnchorTx: *tx, Asset: *a,
		BlockHeight: height,
		BlockHeader: wire.BlockHeader{MerkleRoot: tx.TxHash()},
		InclusionProof: proof.TaprootProof{
			InternalKey: internal, OutputIndex: 0,
			CommitmentProof: &proof.CommitmentProof{
				Proof: *inclusion, TapSiblingPreimage: sibling,
			},
		},
		GenesisReveal: &genesis,
	}
}

func bindingGenesis() asset.Genesis {
	return asset.Genesis{
		FirstPrevOut: wire.OutPoint{Hash: chainhash.Hash{7}},
		Tag:          "binding-test", Type: asset.Normal,
	}
}

func bindingRaw(t *testing.T, p *proof.Proof) []byte {
	t.Helper()
	raw, err := proof.EncodeAsProofFile(p)
	require.NoError(t, err)
	return raw
}

func bindingBlock(p *proof.Proof) *wire.MsgBlock {
	return &wire.MsgBlock{
		Header: p.BlockHeader, Transactions: []*wire.MsgTx{&p.AnchorTx},
	}
}

func TestProofRequiresExactPolicyAndCurrentChain(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*bindingSDK, *bindingChain, *proof.Proof,
			*[33]byte, *uint64)
		hidden bool
	}{
		{name: "valid"},
		{name: "hidden co-anchored asset", hidden: true},
		{name: "invalid SDK proof", mutate: func(s *bindingSDK,
			_ *bindingChain, _ *proof.Proof,
			_ *[33]byte, _ *uint64) {

			s.valid = false
		}},
		{name: "opposite group parity", mutate: func(_ *bindingSDK,
			_ *bindingChain, _ *proof.Proof,
			g *[33]byte, _ *uint64) {

			g[0] ^= 1
		}},
		{name: "different amount", mutate: func(_ *bindingSDK,
			_ *bindingChain, _ *proof.Proof,
			_ *[33]byte, a *uint64) {

			*a++
		}},
		{name: "insufficient depth", mutate: func(_ *bindingSDK,
			c *bindingChain, _ *proof.Proof,
			_ *[33]byte, _ *uint64) {

			c.tip = 100
		}},
		{name: "tip below proof", mutate: func(_ *bindingSDK,
			c *bindingChain, _ *proof.Proof,
			_ *[33]byte, _ *uint64) {

			c.tip = 99
		}},
		{name: "reorged proof", mutate: func(_ *bindingSDK,
			c *bindingChain, _ *proof.Proof,
			_ *[33]byte, _ *uint64) {

			c.blocks[100].Header.Nonce++
		}},
		{name: "missing transaction", mutate: func(_ *bindingSDK,
			c *bindingChain, _ *proof.Proof,
			_ *[33]byte, _ *uint64) {

			c.blocks[100].Transactions = nil
		}},
		{name: "spent output", mutate: func(_ *bindingSDK,
			c *bindingChain, _ *proof.Proof,
			_ *[33]byte, _ *uint64) {

			c.unspent = false
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := bindingProof(t, bindingGenesis(),
				bindingKey(2).PubKey(), bindingKey(3).PubKey(),
				nil, bindingGenesis().FirstPrevOut,
				100, test.hidden)
			client := &bindingSDK{valid: true}
			chain := &bindingChain{tip: 101, unspent: true,
				blocks: map[uint32]*wire.MsgBlock{
					100: bindingBlock(p),
				}}
			group, amount := bindingPub(11), uint64(1000)
			if test.mutate != nil {
				test.mutate(client, chain, p, &group, &amount)
			}
			r := &Driver{cfg: DriverConfig{SDK: client, Chain: chain,
				MinConfs: 2}}
			_, err := r.verifyProof(t.Context(), bindingRaw(t, p),
				group, amount, true)
			if test.name == "valid" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func bindingOutcome(t *testing.T, refund bool) (*Driver, *swapContext,
	*proof.Proof, *bindingChain) {

	t.Helper()
	clientKey, serverKey := bindingPub(2), bindingPub(3)
	dk, err := swapkit.NewDepositKit(swapkit.DepositKitParams{
		ClientKey: clientKey, ServerKey: serverKey, CSVExpiry: 100,
	})
	require.NoError(t, err)
	leaf := txscript.NewBaseTapLeaf([]byte{txscript.OP_TRUE})
	assetTree := txscript.AssembleTaprootScriptTree(leaf)
	assetRoot := assetTree.RootNode.TapHash()
	assetKey := bindingKey(5).PubKey()
	assetOutputKey := txscript.ComputeTaprootOutputKey(
		assetKey, assetRoot[:],
	)
	assetOutputKey, err = schnorr.ParsePubKey(
		schnorr.SerializePubKey(assetOutputKey))
	require.NoError(t, err)
	control := assetTree.LeafMerkleProofs[0].ToControlBlock(assetKey)
	controlRaw, err := control.ToBytes()
	require.NoError(t, err)
	spend := &sdk.CustomAssetOPTrueSpendInfo{
		LeafScript: leaf.Script, LeafVersion: uint8(leaf.LeafVersion),
		InternalKey: bindingPub(5), ControlBlock: controlRaw,
	}
	timeout, err := dk.TimeoutSpend(sdk.Hash{1})
	require.NoError(t, err)
	sibling, err := commitment.NewPreimageFromLeaf(
		txscript.NewBaseTapLeaf(timeout.LeafScript))
	require.NoError(t, err)
	anchorPlan := dk.AnchorPlan()
	internal, err := btcec.ParsePubKey(anchorPlan.InternalKey[:])
	require.NoError(t, err)
	deposit := bindingProof(t, bindingGenesis(), assetOutputKey, internal,
		sibling, bindingGenesis().FirstPrevOut, 100, false)
	assets, merkle, err := isolatedRoots(deposit)
	require.NoError(t, err)
	var script sdk.PubKey
	copy(script[:], assetOutputKey.SerializeCompressed())
	ref := sdk.AssetRefFromGroupKey(bindingPub(11))
	c := depositContract{Version: 1, ClientKey: clientKey,
		ServerKey: serverKey, CSV: 100,
		Output: sdk.CustomAnchorAssetOutputSummary{
			ScriptKey: script, OPTrueSpend: spend, AssetRef: ref,
			IssuanceID: sdk.AssetID(deposit.Asset.ID()),
			AssetType:  sdk.AssetType(deposit.Asset.Genesis.Type),
			Amount:     1000, AnchorValueSat: 10_000,
			TaprootAssetRoot: assets, TaprootMerkleRoot: merkle,
			AnchorOutpoint: sdk.Outpoint{Txid: sdk.Hash(
				deposit.AnchorTx.TxHash())},
		}}
	contract, err := json.Marshal(c)
	require.NoError(t, err)
	preimage := [32]byte{99}
	s := &swapContext{Offer: swapkit.Offer{Hash: sha256.Sum256(preimage[:]),
		Terms: swapkit.Terms{Version: 1, GroupKey: bindingPub(11),
			DepositOutpoints: []string{deposit.OutPoint().String()},
			DepositAmount:    1000, PayoutAmount: 900,
			AssetFee:     100,
			RefundHeight: 150, RecoveryDelta: 10,
			Contract: contract}}}
	prior := sdk.CustomAnchorAssetOutputSummary{
		AssetRef: ref, Amount: 1000,
		IssuanceID:     sdk.AssetID(deposit.Asset.ID()),
		AnchorValueSat: 9000, TaprootAssetRoot: sdk.Hash{42},
		AnchorOutpoint: sdk.Outpoint{Txid: sdk.Hash{88}},
	}
	s.Fallback = &sdk.CustomAnchorTransferPackage{
		Outputs: []sdk.CustomAnchorAssetOutputSummary{prior},
	}
	policy, err := htlc(s.Offer, c)
	require.NoError(t, err)
	path, err := policy.SuccessSpend(prior.TaprootAssetRoot)
	require.NoError(t, err)
	receiver := bindingKey(3)
	if refund {
		receiver = bindingKey(2)
		path, err = policy.TimeoutSpend(prior.TaprootAssetRoot)
		require.NoError(t, err)
	}
	outputKey := txscript.ComputeTaprootKeyNoScript(receiver.PubKey())
	outputKey, err = schnorr.ParsePubKey(schnorr.SerializePubKey(outputKey))
	require.NoError(t, err)
	outcome := bindingProof(t, bindingGenesis(), outputKey,
		receiver.PubKey(), nil, sdkPoint(prior.AnchorOutpoint),
		160, false)
	outcome.AnchorTx.TxIn[0].Sequence = path.Sequence
	outcome.AnchorTx.LockTime = path.LockTime
	outcome.AnchorTx.TxOut[0].Value = 8000
	fetch := txscript.NewCannedPrevOutputFetcher(path.PkScript, 9000)
	sig, err := txscript.RawTxInTapscriptSignature(&outcome.AnchorTx,
		txscript.NewTxSigHashes(&outcome.AnchorTx, fetch), 0, 9000,
		path.PkScript, txscript.NewBaseTapLeaf(path.LeafScript),
		txscript.SigHashDefault, receiver)
	require.NoError(t, err)
	witness, err := policy.SuccessWitness(
		prior.TaprootAssetRoot, sig, preimage,
	)
	if refund {
		witness, err = policy.TimeoutWitness(
			prior.TaprootAssetRoot, sig,
		)
	}
	require.NoError(t, err)
	outcome.AnchorTx.TxIn[0].Witness = witness
	outcome.BlockHeader.MerkleRoot = outcome.AnchorTx.TxHash()
	chain := &bindingChain{tip: 161, unspent: true,
		blocks: map[uint32]*wire.MsgBlock{
			100: bindingBlock(deposit), 160: bindingBlock(outcome),
		}}
	cfg := identityConfig(t, 2, sdk.NetworkRegtest)
	cfg.SDK, cfg.Chain = &bindingSDK{valid: true}, chain
	d := swapkit.Deposit{Outpoint: deposit.OutPoint().String(),
		Proof: bindingRaw(t, deposit), Contract: contract,
		GroupKey: bindingPub(11), Amount: 1000}
	cfg.Deposit = func(context.Context, string) (swapkit.Deposit,
		keychain.KeyLocator, error) {

		return d, keychain.KeyLocator{}, nil
	}
	r, err := NewDriver(cfg)
	require.NoError(t, err)
	s.Deposit, err = r.offerDeposit(t.Context(), s.Offer, false)
	require.NoError(t, err)
	return r, s, outcome, chain
}

func TestOutcomeRequiresExactSpendAndFreshRefund(t *testing.T) {
	for _, refund := range []bool{false, true} {
		name := "claim"
		if refund {
			name = "refund"
		}
		for _, mutation := range []string{"valid", "wrong signer",
			"wrong control", "other prevout", "other issuance",
			"wrong recipient", "other group", "other amount", "reorg",
			"spent refund"} {
			if mutation == "spent refund" && !refund {
				continue
			}
			t.Run(name+"/"+mutation, func(t *testing.T) {
				testOutcomeBinding(t, refund, mutation)
			})
		}
	}
}

func testOutcomeBinding(t *testing.T, refund bool, mutation string) {
	t.Helper()
	r, s, p, chain := bindingOutcome(t, refund)
	switch mutation {
	case "wrong signer":
		p.AnchorTx.TxIn[0].Witness[0][0] ^= 1
	case "wrong control":
		w := p.AnchorTx.TxIn[0].Witness
		w[len(w)-1][0] ^= 1
	case "other prevout":
		p.AnchorTx.TxIn[0].PreviousOutPoint.Index++
	case "other issuance":
		s.Fallback.Outputs[0].IssuanceID[0] ^= 1
	case "other group":
		s.Fallback.Outputs[0].AssetRef =
			sdk.AssetRefFromGroupKey(bindingPub(12))
	case "other amount":
		s.Fallback.Outputs[0].Amount++
	case "wrong recipient":
		old := p
		p = bindingProof(t, bindingGenesis(), bindingKey(9).PubKey(),
			bindingKey(9).PubKey(), nil, old.PrevOut, 160, false)
		p.AnchorTx.TxIn[0].Sequence = old.AnchorTx.TxIn[0].Sequence
		p.AnchorTx.TxIn[0].Witness = old.AnchorTx.TxIn[0].Witness
		p.AnchorTx.LockTime = old.AnchorTx.LockTime
		p.BlockHeader.MerkleRoot = p.AnchorTx.TxHash()
		chain.blocks[160] = bindingBlock(p)
		// The wrong recipient still has a coherent asset commitment and
		// confirmed transaction. The contract gate must reject it.
		_, err := r.verifyProof(t.Context(), bindingRaw(t, p),
			s.Offer.Terms.GroupKey, 1000, refund)
		require.NoError(t, err)
	case "reorg":
		chain.blocks[160].Header.Nonce++
	case "spent refund":
		chain.unspent = false
	}
	err := r.verifyOutcome(t.Context(), s, bindingRaw(t, p), refund)
	if mutation == "valid" {
		require.NoError(t, err)
	} else {
		require.Error(t, err)
	}
}

func TestPersistedRefundProofRecheckedAfterReorg(t *testing.T) {
	r, s, p, chain := bindingOutcome(t, true)
	raw := bindingRaw(t, p)
	require.NoError(t, r.verifyOutcome(t.Context(), s, raw, true))
	stored := append([]byte(nil), raw...)

	// Persisting a proof does not freeze its current-chain validity.
	chain.blocks[160].Header.Nonce++
	r = &Driver{cfg: r.cfg}
	err := r.verifyOutcome(t.Context(), s, stored, true)
	require.ErrorIs(t, err, swapkit.ErrPending)
}

func TestReorgClassificationPrecedesSDKVerification(t *testing.T) {
	p := bindingProof(t, bindingGenesis(), bindingKey(2).PubKey(),
		bindingKey(3).PubKey(), nil, bindingGenesis().FirstPrevOut,
		100, false)
	chain := &bindingChain{tip: 101, unspent: true,
		blocks: map[uint32]*wire.MsgBlock{100: bindingBlock(p)}}
	r := &Driver{cfg: DriverConfig{SDK: &bindingSDK{valid: false},
		Chain: chain, MinConfs: 1}}
	chain.blocks[100].Header.Nonce++
	_, err := r.verifyProof(t.Context(), bindingRaw(t, p),
		bindingPub(11), 1000, true)
	require.ErrorIs(t, err, swapkit.ErrPending)

	// Restoring the chain match never bypasses full SDK verification.
	chain.blocks[100].Header.Nonce--
	_, err = r.verifyProof(t.Context(), bindingRaw(t, p),
		bindingPub(11), 1000, true)
	require.ErrorIs(t, err, swapkit.ErrInvalid)
}
