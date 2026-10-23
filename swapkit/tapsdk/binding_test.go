package tapsdk

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
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
// tests exercise the Loop contract's additional policy and current-chain checks,
// which must reject even a cryptographically valid proof for another contract.
type bindingSDK struct {
	sdk.Client

	valid bool
}

func (s *bindingSDK) VerifyProof(_ context.Context,
	raw []byte) (*sdk.VerifyProofResponse, error) {

	return fixtureVerified(raw, s.valid)
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

func (c *bindingChain) BlockHash(ctx context.Context,
	height uint32) (chainhash.Hash, error) {

	block, err := c.Block(ctx, height)
	if err != nil {
		return chainhash.Hash{}, err
	}
	return block.BlockHash(), nil
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
		{name: testValid},
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
		{name: "invalid transaction merkle proof", mutate: func(_ *bindingSDK,
			_ *bindingChain, p *proof.Proof,
			_ *[33]byte, _ *uint64) {

			p.TxMerkleProof.Nodes = []chainhash.Hash{{1}}
			p.TxMerkleProof.Bits = []bool{true}
		}},
		{name: testSpentOutput, mutate: func(_ *bindingSDK,
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
			r := &proofNode{
				client:   client,
				chain:    chain,
				minConfs: 2,
			}
			_, err := verifyProof(
				t.Context(),
				r.wallet(),
				r.chain,
				r.minConfs,
				bindingRaw(t, p),
				group,
				amount,
				true,
			)
			if test.name == testValid || test.name == "opposite group parity" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func bindingOutcome(t *testing.T, refund bool) (*proofNode, *SwapContext,
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
	c := DepositContract{Version: 1, ClientKey: clientKey,
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
	s := &SwapContext{Offer: swapkit.Offer{Hash: sha256.Sum256(preimage[:]),
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
	policy, err := HTLC(s.Offer, c)
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
		receiver.PubKey(), nil, SDKPoint(prior.AnchorOutpoint),
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

	r := &proofNode{
		client:   &bindingSDK{valid: true},
		chain:    chain,
		minConfs: 1,
	}
	d := swapkit.Deposit{Outpoint: deposit.OutPoint().String(),
		Proof: bindingRaw(
			t,
			deposit,
		), Contract: contract, GroupKey: bindingPub(11), Amount: 1000}
	s.Deposit, err = LoadDeposit(d, keychain.KeyLocator{})
	require.NoError(t, err)
	require.NoError(
		t,
		VerifyDeposit(
			t.Context(),
			r.wallet(),
			chain,
			1,
			s.Deposit,
			false,
		),
	)

	return r, s, outcome, chain
}

func TestOutcomeRequiresExactSpendAndFreshRefund(t *testing.T) {
	for _, refund := range []bool{false, true} {
		name := "claim"
		if refund {
			name = "refund"
		}
		for _, mutation := range []string{testValid, testWrongSigner,
			"wrong control", "other prevout", "other issuance",
			"wrong recipient", "other group", "other amount", testReorg,
			testSpentRefund} {
			if mutation == testSpentRefund && !refund {
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
	case testWrongSigner:
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
		_, err := verifyProof(
			t.Context(),
			r.wallet(),
			r.chain,
			r.minConfs,
			bindingRaw(t, p),
			s.Offer.Terms.GroupKey,
			1000,
			refund,
		)
		require.NoError(t, err)
	case testReorg:
		chain.blocks[160].Header.Nonce++
	case testSpentRefund:
		chain.unspent = false
	}
	err := VerifyOutcome(
		t.Context(),
		r.wallet(),
		r.chain,
		r.minConfs,
		s,
		bindingRaw(t, p),
		refund,
		refund,
	)
	if mutation == testValid {
		require.NoError(t, err)
	} else {
		require.Error(t, err)
	}
}

func TestPersistedRefundProofRecheckedAfterReorg(t *testing.T) {
	r, s, p, chain := bindingOutcome(t, true)
	raw := bindingRaw(t, p)
	require.NoError(
		t,
		VerifyOutcome(
			t.Context(),
			r.wallet(),
			r.chain,
			r.minConfs,
			s,
			raw,
			true,
			true,
		),
	)
	stored := append([]byte(nil), raw...)

	// Persisting a proof does not freeze its current-chain validity.
	chain.blocks[160].Header.Nonce++
	err := VerifyOutcome(
		t.Context(),
		r.wallet(),
		r.chain,
		r.minConfs,
		s,
		stored,
		true,
		true,
	)
	require.ErrorIs(t, err, swapkit.ErrPending)
}

func TestReorgClassificationPrecedesSDKVerification(t *testing.T) {
	p := bindingProof(t, bindingGenesis(), bindingKey(2).PubKey(),
		bindingKey(3).PubKey(), nil, bindingGenesis().FirstPrevOut,
		100, false)
	chain := &bindingChain{tip: 101, unspent: true,
		blocks: map[uint32]*wire.MsgBlock{100: bindingBlock(p)}}
	r := &proofNode{
		client:   &bindingSDK{valid: false},
		chain:    chain,
		minConfs: 1,
	}
	chain.blocks[100].Header.Nonce++
	_, err := verifyProof(
		t.Context(),
		r.wallet(),
		r.chain,
		r.minConfs,
		bindingRaw(t, p),
		bindingPub(11),
		1000,
		true,
	)
	require.ErrorIs(t, err, swapkit.ErrPending)

	// Restoring the chain match never bypasses full SDK verification.
	chain.blocks[100].Header.Nonce--
	_, err = verifyProof(
		t.Context(),
		r.wallet(),
		r.chain,
		r.minConfs,
		bindingRaw(t, p),
		bindingPub(11),
		1000,
		true,
	)
	require.ErrorIs(t, err, swapkit.ErrInvalid)
}

// proofNode holds only test services. Tests call production primitives directly.
type proofNode struct {
	client   sdk.Client
	chain    Chain
	minConfs uint32
}

func (n *proofNode) wallet() *sdk.Wallet {
	return sdk.NewWallet(n.client, sdk.NetworkRegtest)
}

func (s *bindingSDK) GetInfo(
	context.Context,
) (*sdk.Info, error) {

	return fixtureNodeInfo(), nil
}

func fixtureNodeInfo() *sdk.Info { return &sdk.Info{Version: "0.8.6", Network: "regtest"} }
func fixtureVerified(raw []byte, valid bool) (*sdk.VerifyProofResponse, error) {
	p, err := LastProof(raw)
	if err != nil {
		return nil, err
	}
	key, err := sdk.ParsePubKey(
		p.Asset.ScriptKey.PubKey.SerializeCompressed(),
	)
	if err != nil {
		return nil, err
	}
	var group *sdk.PubKey
	if p.Asset.GroupKey != nil {
		k, err := sdk.ParsePubKey(
			p.Asset.GroupKey.GroupPubKey.SerializeCompressed(),
		)
		if err != nil {
			return nil, err
		}
		group = &k
	}
	return &sdk.VerifyProofResponse{
		Valid: valid,
		DecodedProof: &sdk.DecodedProof{
			AssetRef: sdk.AssetRefFromTypedAsset(
				sdk.AssetID(p.Asset.ID()),
				group,
				sdk.AssetType(p.Asset.Type),
			),
			IssuanceID: sdk.AssetID(p.Asset.ID()),
			Amount:     p.Asset.Amount, ScriptKey: key,
			Outpoint: sdk.Outpoint{Txid: sdk.Hash(p.OutPoint().Hash), Index: p.OutPoint().Index},
		},
	}, nil
}
