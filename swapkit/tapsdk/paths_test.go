package tapsdk

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/tapsend"
	"github.com/stretchr/testify/require"
)

type pathChain struct {
	Chain

	blocks map[uint32]*wire.MsgBlock
	utxos  map[wire.OutPoint]*wire.TxOut
}

func (c *pathChain) Prevout(_ context.Context, point wire.OutPoint) (*wire.TxOut, error) {
	return c.utxos[point], nil
}

func (c *pathChain) Height(context.Context) (uint32, error) { return 200, nil }
func (c *pathChain) Block(_ context.Context, h uint32) (*wire.MsgBlock, error) {
	return c.blocks[h], nil
}
func (c *pathChain) BlockHash(_ context.Context,
	height uint32) (chainhash.Hash, error) {

	block := c.blocks[height]
	if block == nil {
		return chainhash.Hash{}, swapkit.ErrPending
	}
	return block.BlockHash(), nil
}
func (c *pathChain) Unspent(_ context.Context, p wire.OutPoint) (bool, error) {
	return c.utxos[p] != nil, nil
}

type pathClient struct {
	sdk.Client

	t                            *testing.T
	verifies, commits, publishes int
	afterVerify                  func()
	afterInfo                    func()
}

func (c *pathClient) GetInfo(context.Context) (*sdk.Info, error) {
	if c.afterInfo != nil {
		c.afterInfo()
	}
	return fixtureNodeInfo(), nil
}
func (c *pathClient) VerifyProof(_ context.Context, raw []byte) (*sdk.VerifyProofResponse, error) {
	c.verifies++
	if c.afterVerify != nil {
		c.afterVerify()
	}
	return fixtureVerified(raw, true)
}
func (c *pathClient) CustomAnchorCapabilities(context.Context) (*sdk.CustomAnchorCapabilities, error) {
	v := sdk.DefaultTapdCustomAnchorCapabilities()
	return &v, nil
}
func (c *pathClient) PublishAndLogTransferWithRequest(context.Context,
	*sdk.PublishAndLogTransferRequest) (*sdk.AssetPacket, error) {

	c.publishes++
	return nil, fmt.Errorf("publication is outside this test")
}

// The actual SDK builds and validates transitions and verifies this deterministic
// test backend's commit response. Production uses tapd's implementation.
func (c *pathClient) CommitVirtualPsbtsWithRequest(_ context.Context,
	r *sdk.CommitVirtualPsbtsRequest) (*sdk.CommitVirtualPsbtsResponse, error) {

	c.commits++
	t := c.t
	anchor, err := DecodePSBT(r.AnchorPsbt)
	require.NoError(t, err)
	decode := func(raws [][]byte) []*tappsbt.VPacket {
		v := make([]*tappsbt.VPacket, len(raws))
		for i, raw := range raws {
			v[i], err = tappsbt.Decode(raw)
			require.NoError(t, err)
		}
		return v
	}
	active, passive := decode(r.VirtualPsbts), decode(r.PassiveAssetPsbts)
	all := append(append([]*tappsbt.VPacket(nil), active...), passive...)
	commits, err := tapsend.CreateOutputCommitments(all, tapsend.WithSpenderLeaves())
	require.NoError(t, err)
	for _, p := range all {
		require.NoError(t, tapsend.UpdateTaprootOutputKeys(anchor, p, commits))
	}
	for _, p := range all {
		for i := range p.Outputs {
			p.Outputs[i].ProofSuffix, err = tapsend.CreateProofSuffix(
				anchor.UnsignedTx, anchor.Outputs, p, commits, i, all,
				proof.WithVersion(proof.TransitionV1),
			)
			require.NoError(t, err)
		}
	}
	encode := func(packets []*tappsbt.VPacket) [][]byte {
		raw := make([][]byte, len(packets))
		for i, p := range packets {
			raw[i], err = tappsbt.Encode(p)
			require.NoError(t, err)
		}
		return raw
	}
	raw, err := EncodePSBT(anchor)
	require.NoError(t, err)
	return &sdk.CommitVirtualPsbtsResponse{
		AnchorPsbt: raw, VirtualPsbts: encode(active),
		PassiveAssetPsbts: encode(passive), ChangeOutputIndex: -1,
	}, nil
}

type pathFixture struct {
	wallet  *sdk.Wallet
	client  *pathClient
	chain   *pathChain
	request *sdk.CustomAnchorRequest
	binding InputBinding
	kit     *swapkit.SwapKit
}

func newPathFixture(t *testing.T) *pathFixture {
	t.Helper()
	f := &pathFixture{client: &pathClient{t: t}, chain: &pathChain{
		blocks: make(map[uint32]*wire.MsgBlock),
		utxos:  make(map[wire.OutPoint]*wire.TxOut),
	}}
	f.wallet = sdk.NewWallet(f.client, sdk.NetworkRegtest)
	deposit, err := swapkit.NewDepositKit(swapkit.DepositKitParams{
		ClientKey: bindingPub(1), ServerKey: bindingPub(2), CSVExpiry: 144,
	})
	require.NoError(t, err)
	params := swapKitTestParams(t)
	params.Policy, params.Expiry = swapkit.SwapHTLCCLTV, 1_000_000
	f.kit, err = swapkit.NewSwapKit(params)
	require.NoError(t, err)
	key, optrue, err := LegacyOPTrueDescriptor()
	require.NoError(t, err)
	pub, err := btcec.ParsePubKey(key.PubKey[:])
	require.NoError(t, err)
	genesis := asset.Genesis{
		FirstPrevOut: wire.OutPoint{Hash: chainhash.Hash{1}, Index: 1},
		Tag:          "native-path", OutputIndex: 0, Type: asset.Normal,
	}
	a, err := asset.New(genesis, 1_000, 0, 0, asset.NewScriptKey(pub), nil, asset.WithAssetVersion(asset.V1))
	require.NoError(t, err)
	version := commitment.TapCommitmentV2
	tc, err := commitment.FromAssets(&version, a)
	require.NoError(t, err)
	root := tc.TapscriptRoot(nil)
	spend, err := deposit.TimeoutSpend(swapkit.Hash(root))
	require.NoError(t, err)
	mint := wire.NewMsgTx(2)
	mint.AddTxIn(wire.NewTxIn(&genesis.FirstPrevOut, nil, nil))
	mint.AddTxOut(wire.NewTxOut(1_000, spend.PkScript))
	block := &wire.MsgBlock{Header: wire.BlockHeader{MerkleRoot: mint.TxHash()}, Transactions: []*wire.MsgTx{mint}}
	plan := deposit.AnchorPlan()
	internal, err := btcec.ParsePubKey(plan.InternalKey[:])
	require.NoError(t, err)
	sibling, err := commitment.NewPreimageFromLeaf(txscript.NewBaseTapLeaf(plan.Leaves[0]))
	require.NoError(t, err)
	generated, err := proof.NewMintingBlobs(&proof.MintParams{
		Block: block, BlockHeight: 101, Tx: mint, TxIndex: 0,
		OutputIndex: 0, InternalKey: internal, TaprootAssetRoot: tc,
		TapscriptSibling: sibling, GenesisPoint: genesis.FirstPrevOut,
	}, proof.MockVerifierCtx, proof.WithSiblingPreimage(sibling),
		proof.WithGenOption(proof.WithVersion(proof.TransitionV1)))
	require.NoError(t, err)
	p := generated[asset.ToSerialized(a.ScriptKey.PubKey)]
	raw, err := proof.EncodeAsProofFile(p)
	require.NoError(t, err)
	f.chain.blocks[101] = block
	f.chain.utxos[p.OutPoint()] = mint.TxOut[0]
	f.binding = InputBinding{
		ID: "asset", IssuanceID: sdk.AssetID(a.ID()), Amount: a.Amount,
		ScriptKey: key.PubKey, Anchor: &plan, Spend: spend,
		AnchorOutpoint: sdk.Outpoint{Txid: sdk.Hash(p.OutPoint().Hash), Index: 0},
	}
	output, err := LegacyOPTrueOutput(sdk.CustomAssetOutput{
		ID: "next", AssetRef: sdk.AssetRefFromAssetID(sdk.AssetID(a.ID())),
		Amount: a.Amount, AnchorOutputIndex: 1, AnchorValueSat: 1_000,
	}, f.kit.AnchorPlan())
	require.NoError(t, err)
	input := sdk.CustomAssetInput{
		ID: "asset", AssetRef: output.AssetRef, Amount: a.Amount, ProofFile: raw,
		Witness: sdk.CustomAssetWitnessPlan{
			Mode: sdk.CustomAssetWitnessCallerProvided, Stack: optrue.WitnessStack(),
		},
	}
	f.request = f.requestFor(t, p, spend, input, output, wire.OutPoint{Hash: chainhash.Hash{99}}, 50_000)
	return f
}

func (f *pathFixture) requestFor(t *testing.T, p *proof.Proof,
	spend *swapkit.SwapAnchorSpendInfo, input sdk.CustomAssetInput,
	output sdk.CustomAssetOutput, feePoint wire.OutPoint,
	feeValue int64) *sdk.CustomAnchorRequest {

	t.Helper()
	feeKey := bindingKey(5)
	feeScript, err := txscript.PayToTaprootScript(txscript.ComputeTaprootKeyNoScript(feeKey.PubKey()))
	require.NoError(t, err)
	feeOut := wire.NewTxOut(feeValue, feeScript)
	if f.chain.utxos[feePoint] == nil {
		f.chain.utxos[feePoint] = feeOut
	}
	tx := wire.NewMsgTx(2)
	tx.LockTime = spend.LockTime
	tx.AddTxIn(&wire.TxIn{PreviousOutPoint: feePoint, Sequence: wire.MaxTxInSequenceNum - 1})
	tx.AddTxIn(&wire.TxIn{PreviousOutPoint: p.OutPoint(), Sequence: spend.Sequence})
	tx.AddTxOut(wire.NewTxOut(feeValue-2_000, feeScript))
	tx.AddTxOut(wire.NewTxOut(1_000, feeScript))
	packet, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)
	packet.Inputs[0].WitnessUtxo = feeOut
	pub := feeKey.PubKey().SerializeCompressed()
	packet.Inputs[0].TaprootInternalKey = bytes.Clone(pub[1:])
	packet.Outputs[0].TaprootInternalKey = bytes.Clone(pub[1:])
	packet.Inputs[1].WitnessUtxo = p.AnchorTx.TxOut[p.OutPoint().Index]
	packet.Inputs[1].TaprootInternalKey = bytes.Clone(spend.InternalKey[1:])
	packet.Inputs[1].TaprootMerkleRoot = bytes.Clone(spend.TaprootMerkleRoot[:])
	packet.Inputs[1].TaprootLeafScript = []*psbt.TaprootTapLeafScript{{
		ControlBlock: spend.ControlBlock, Script: spend.LeafScript,
		LeafVersion: txscript.BaseLeafVersion,
	}}
	raw, err := EncodePSBT(packet)
	require.NoError(t, err)
	return &sdk.CustomAnchorRequest{
		Inputs: []sdk.CustomAssetInput{input}, Outputs: []sdk.CustomAssetOutput{output},
		AnchorPSBT: raw, Funding: sdk.CustomAnchorFundingPlan{
			Mode:              sdk.CustomAnchorFundingCallerFundedExact,
			CallerFundedExact: &sdk.CustomAnchorCallerFundedExact{},
		},
		SigningPlans: []sdk.CustomAnchorInputSigningPlan{{InputIndex: 0,
			KeyPath: &sdk.CustomAnchorKeyPathSigningPlan{Signer: sdk.XOnlyPubKey(pub[1:])}},
			ScriptSigningPlan(spend, 1)},
	}
}

func pathFinal(t *testing.T, pkg *sdk.CustomAnchorTransferPackage) *sdk.CustomAnchorTransferPackage {
	t.Helper()
	packet, err := DecodePSBT(pkg.AnchorPsbt)
	require.NoError(t, err)
	requests, err := pkg.SigningRequests()
	require.NoError(t, err)
	for _, req := range requests.KeyPath {
		key := txscript.TweakTaprootPrivKey(*bindingKey(5), req.TaprootMerkleRoot)
		sig, err := schnorr.Sign(key, req.Sighash[:])
		require.NoError(t, err)
		packet.Inputs[req.InputIndex].TaprootKeySpendSig = sig.Serialize()
	}
	for _, req := range requests.ScriptPath {
		sig, err := schnorr.Sign(bindingKey(1), req.Sighash[:])
		require.NoError(t, err)
		var b bytes.Buffer
		require.NoError(t, psbt.WriteTxWitness(&b, [][]byte{sig.Serialize(), req.LeafScript, req.ControlBlock}))
		packet.Inputs[req.InputIndex].FinalScriptWitness = b.Bytes()
	}
	require.NoError(t, psbt.MaybeFinalizeAll(packet))
	raw, err := EncodePSBT(packet)
	require.NoError(t, err)
	final, err := pkg.WithFinalAnchorPSBT(raw)
	require.NoError(t, err)
	return final
}

func (f *pathFixture) parent(t *testing.T) (*sdk.CustomAnchorTransferPackage, *sdk.CustomAnchorTransferPackage) {
	t.Helper()
	plan, err := f.wallet.NewCustomAnchorTxBuilder().Build(t.Context(), f.request)
	require.NoError(t, err)
	unsigned, err := plan.Commit(t.Context(), sdk.CustomAnchorCommitOptions{})
	require.NoError(t, err)
	return unsigned, pathFinal(t, unsigned)
}

func (f *pathFixture) child(t *testing.T,
	parent *sdk.CustomAnchorTransferPackage) (*sdk.CustomAnchorRequest, InputBinding) {

	t.Helper()
	p, err := proof.Decode(parent.ProofUpdates[0].ProofBlob)
	require.NoError(t, err)
	root, _, err := isolatedRoots(p)
	require.NoError(t, err)
	spend, err := f.kit.TimeoutSpend(swapkit.Hash(root))
	require.NoError(t, err)
	binding := f.binding
	plan := f.kit.AnchorPlan()
	binding.Anchor, binding.Spend = &plan, spend
	binding.AnchorOutpoint = sdk.Outpoint{Txid: sdk.Hash(p.OutPoint().Hash), Index: p.OutPoint().Index}
	input := f.request.Inputs[0]
	input.ProofPath = &sdk.AssetProofPath{
		ConfirmedBaseProof: input.ProofFile,
		Steps:              []sdk.AssetProofPathStep{{TransitionProof: parent.ProofUpdates[0].ProofBlob}},
	}
	input.ProofFile = nil
	request := f.requestFor(t, p, spend, input, f.request.Outputs[0],
		wire.OutPoint{Hash: p.OutPoint().Hash, Index: 0}, 48_000)
	delete(f.chain.utxos, requestPoint(t, request, 0))
	return request, binding
}

func requestPoint(t *testing.T, request *sdk.CustomAnchorRequest, index int) wire.OutPoint {
	t.Helper()
	p, err := DecodePSBT(request.AnchorPSBT)
	require.NoError(t, err)
	return p.UnsignedTx.TxIn[index].PreviousOutPoint
}

func TestApprovedParentsNativeBuild(t *testing.T) {
	f := newPathFixture(t)
	unsigned, parent := f.parent(t)
	request, binding := f.child(t, parent)
	for _, approval := range []ParentApproval{
		{Package: unsigned, Digest: unsigned.CommittedPackageDigest},
		{Package: parent, Digest: sdk.Hash{1}},
	} {
		_, err := NewApprovedParents(t.Context(), f.wallet, f.chain, 2,
			request, []InputBinding{binding}, []ParentApproval{approval})
		require.Error(t, err)
	}
	parents, err := NewApprovedParents(t.Context(), f.wallet, f.chain, 2,
		request, []InputBinding{binding}, []ParentApproval{{Package: parent,
			Digest: parent.CommittedPackageDigest}})
	require.NoError(t, err)
	f.client.verifies = 0
	builder := f.wallet.NewCustomAnchorTxBuilder()
	builder.SetConfirmedProofVerifier(parents)
	plan, err := builder.Build(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, 1, f.client.verifies)
	require.NoError(t, parents.CheckPlan(t.Context(), plan))
	child, err := plan.Commit(t.Context(), sdk.CustomAnchorCommitOptions{})
	require.NoError(t, err)
	require.NoError(t, child.Validate())
	require.Equal(t, sdk.CustomAnchorProofSourceCompactPath, child.Inputs[0].ProofSource.Kind)
	delete(f.chain.utxos, requestPoint(t, f.request, 0))
	require.Error(t, parents.CheckCurrent(t.Context()))
}

func TestApprovedParentsRejectUnapprovedTransition(t *testing.T) {
	f := newPathFixture(t)
	_, parent := f.parent(t)
	request, binding := f.child(t, parent)
	_, err := NewApprovedParents(t.Context(), f.wallet, f.chain, 2,
		request, []InputBinding{binding}, nil)
	require.Error(t, err)
}

func TestApprovedParentsRequireIsolatedBases(t *testing.T) {
	f := newPathFixture(t)
	p := bindingProof(t, bindingGenesis(), bindingKey(2).PubKey(),
		bindingKey(3).PubKey(), nil, bindingGenesis().FirstPrevOut, 100, true)
	request := f.request.Clone()
	request.Inputs[0].ProofFile = nil
	request.Inputs[0].ProofPath = &sdk.AssetProofPath{ConfirmedBaseProof: bindingRaw(t, p)}
	_, err := NewApprovedParents(t.Context(), f.wallet, f.chain, 2,
		request, []InputBinding{f.binding}, nil)
	require.ErrorContains(t, err, "compact parent paths require isolated asset anchors")
	require.Zero(t, f.client.verifies)
}

func TestApprovedParentsRecheckFundingAfterVerification(t *testing.T) {
	f := newPathFixture(t)
	_, parent := f.parent(t)
	request, binding := f.child(t, parent)
	parents, err := NewApprovedParents(t.Context(), f.wallet, f.chain, 2,
		request, []InputBinding{binding}, []ParentApproval{{Package: parent,
			Digest: parent.CommittedPackageDigest}})
	require.NoError(t, err)
	f.client.verifies = 0
	f.client.afterVerify = func() { clear(f.chain.utxos) }
	builder := f.wallet.NewCustomAnchorTxBuilder()
	builder.SetConfirmedProofVerifier(parents)
	_, err = builder.Build(t.Context(), request)
	require.Error(t, err)
	require.Equal(t, 1, f.client.verifies)
}

func TestApprovedParentsSnapshotBeforeRPC(t *testing.T) {
	f := newPathFixture(t)
	_, parent := f.parent(t)
	request, binding := f.child(t, parent)
	f.client.afterInfo = func() {
		binding.Anchor.Leaves[0][0] ^= 1
		request.Inputs[0].ProofPath.ConfirmedBaseProof[0] ^= 1
	}
	_, err := NewApprovedParents(t.Context(), f.wallet, f.chain, 2,
		request, []InputBinding{binding}, []ParentApproval{{Package: parent,
			Digest: parent.CommittedPackageDigest}})
	require.NoError(t, err)
}

func TestApprovedParentsBindNativePlan(t *testing.T) {
	f := newPathFixture(t)
	_, parent := f.parent(t)
	request, binding := f.child(t, parent)
	parents, err := NewApprovedParents(t.Context(), f.wallet, f.chain, 2,
		request, []InputBinding{binding}, []ParentApproval{{Package: parent,
			Digest: parent.CommittedPackageDigest}})
	require.NoError(t, err)
	changed := request.Clone()
	changed.Outputs[0].ProofDelivery.RecipientID = "substituted delivery"
	builder := f.wallet.NewCustomAnchorTxBuilder()
	builder.SetConfirmedProofVerifier(parents)
	plan, err := builder.Build(t.Context(), changed)
	require.NoError(t, err)
	require.ErrorContains(t, parents.CheckPlan(t.Context(), plan), "differs from inspected")
}

func TestApprovedParentsAuthenticateRootFunding(t *testing.T) {
	for _, script := range []bool{false, true} {
		f := newPathFixture(t)
		_, parent := f.parent(t)
		request, binding := f.child(t, parent)
		point := requestPoint(t, f.request, 0)
		if script {
			f.chain.utxos[point].PkScript = []byte{txscript.OP_TRUE}
		} else {
			f.chain.utxos[point].Value++
		}
		_, err := NewApprovedParents(t.Context(), f.wallet, f.chain, 2,
			request, []InputBinding{binding}, []ParentApproval{{Package: parent,
				Digest: parent.CommittedPackageDigest}})
		require.ErrorContains(t, err, "authenticated Bitcoin prevout")
	}
}
