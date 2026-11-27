package tapsdk

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

// fallbackFixture is an honest Loop In deposit, offer and native fallback
// package built with the kit's own FallbackRequest and the real SDK builder.
type fallbackFixture struct {
	offer   swapkit.Offer
	deposit *DepositContext
	swap    *SwapContext
	pkg     *sdk.CustomAnchorTransferPackage
	prevout *wire.TxOut
}

// newFallbackFixture builds the deposit with client scalar two and server
// scalar three, matching the keys of the package's LND session fixtures.
func newFallbackFixture(t *testing.T) *fallbackFixture {
	t.Helper()
	clientKey, serverKey := bindingPub(2), bindingPub(3)
	dk, err := swapkit.NewDepositKit(swapkit.DepositKitParams{
		ClientKey: clientKey, ServerKey: serverKey, CSVExpiry: 100,
	})
	require.NoError(t, err)
	key, _, err := LegacyOPTrueDescriptor()
	require.NoError(t, err)
	scriptKey, err := btcec.ParsePubKey(key.PubKey[:])
	require.NoError(t, err)
	plan := dk.AnchorPlan()
	internal, err := btcec.ParsePubKey(plan.InternalKey[:])
	require.NoError(t, err)
	sibling, err := commitment.NewPreimageFromLeaf(
		txscript.NewBaseTapLeaf(plan.Leaves[0]),
	)
	require.NoError(t, err)
	deposit := bindingProof(t, bindingGenesis(), scriptKey, internal,
		sibling, bindingGenesis().FirstPrevOut, 100, false)
	assets, merkle, err := isolatedRoots(deposit)
	require.NoError(t, err)

	c := DepositContract{Version: 1, ClientKey: clientKey,
		ServerKey: serverKey, CSV: 100,
		Output: sdk.CustomAnchorAssetOutputSummary{
			LogicalOutputID: "deposit",
			ScriptKey:       key.PubKey,
			ScriptMode:      sdk.CustomAssetScriptExternal,
			AssetRef:        sdk.AssetRefFromGroupKey(bindingPub(11)),
			IssuanceID:      sdk.AssetID(deposit.Asset.ID()),
			AssetType: sdk.AssetType(
				deposit.Asset.Genesis.Type,
			),
			Amount: 1000, AnchorValueSat: 10_000,
			TaprootAssetRoot: assets, TaprootMerkleRoot: merkle,
			AnchorOutpoint: sdk.Outpoint{Txid: sdk.Hash(
				deposit.AnchorTx.TxHash())},
		}}
	// Preserve the existing SDK/contract JSON field names.
	//nolint:musttag
	contract, err := json.Marshal(c)
	require.NoError(t, err)
	preimage := [32]byte{99}
	offer := swapkit.Offer{Hash: sha256.Sum256(preimage[:]),
		Terms: swapkit.Terms{Version: 1, GroupKey: bindingPub(11),
			DepositOutpoints: []string{deposit.OutPoint().String()},
			DepositAmount:    1000, PayoutAmount: 900, AssetFee: 100,
			RefundHeight: 150, RecoveryDelta: 10, Contract: contract}}
	d, err := LoadDeposit(swapkit.Deposit{
		Outpoint: deposit.OutPoint().String(),
		Proof:    bindingRaw(t, deposit), Contract: contract,
		GroupKey: bindingPub(11), Amount: 1000,
	}, keychain.KeyLocator{})
	require.NoError(t, err)
	chain := &bindingChain{tip: 161, unspent: true,
		blocks: map[uint32]*wire.MsgBlock{100: bindingBlock(deposit)}}
	require.NoError(t, InspectDeposit(t.Context(), chain, 1, d, true))

	// The honest author builds and commits the fallback natively.
	s, err := NewSwap(offer, d, nil)
	require.NoError(t, err)
	request, err := FallbackRequest(s, 1_000)
	require.NoError(t, err)
	wallet := sdk.NewWallet(&pathClient{t: t}, sdk.NetworkRegtest)
	built, err := wallet.NewCustomAnchorTxBuilder().Build(
		t.Context(), request,
	)
	require.NoError(t, err)
	pkg, err := built.Commit(t.Context(), sdk.CustomAnchorCommitOptions{})
	require.NoError(t, err)

	// The honest package is accepted, so the fixture is representative.
	raw, err := pkg.MarshalBinary()
	require.NoError(t, err)
	_, err = NewSwap(offer, d, raw)
	require.NoError(t, err)

	point := deposit.OutPoint()
	return &fallbackFixture{offer: offer, deposit: d, swap: s, pkg: pkg,
		prevout: deposit.AnchorTx.TxOut[point.Index]}
}

// executeAgainst runs the finalized anchor against one previous output.
func executeAgainst(t *testing.T, pkg *sdk.CustomAnchorTransferPackage,
	prevout *wire.TxOut) error {

	t.Helper()
	packet, err := DecodePSBT(pkg.AnchorPsbt)
	require.NoError(t, err)
	tx, err := psbt.Extract(packet)
	require.NoError(t, err)
	fetch := txscript.NewCannedPrevOutputFetcher(
		prevout.PkScript, prevout.Value,
	)
	engine, err := txscript.NewEngine(prevout.PkScript, tx, 0,
		txscript.StandardVerifyFlags, nil,
		txscript.NewTxSigHashes(tx, fetch), prevout.Value, fetch)
	if err != nil {
		return err
	}
	return engine.Execute()
}

// finishFallback signs with both real LND session managers and finalizes
// through the SDK, exactly as an honest validator would after approval.
func finishFallback(t *testing.T,
	pkg *sdk.CustomAnchorTransferPackage) *sdk.CustomAnchorTransferPackage {

	t.Helper()
	signed, err := signTestPackage(t.Context(), signingWallet(t, 2),
		signingWallet(t, 3), pkg)
	require.NoError(t, err)
	packet, err := DecodePSBT(signed.AnchorPsbt)
	require.NoError(t, err)
	require.NoError(t, psbt.MaybeFinalizeAll(packet))
	raw, err := EncodePSBT(packet)
	require.NoError(t, err)
	final, err := pkg.WithFinalAnchorPSBT(raw)
	require.NoError(t, err)
	return final
}

// TestFallbackHonestBaselineExecutes shows that the honest fallback is signed
// into a transaction that is valid for the authenticated deposit output.
func TestFallbackHonestBaselineExecutes(t *testing.T) {
	f := newFallbackFixture(t)
	raw, err := f.pkg.MarshalBinary()
	require.NoError(t, err)
	s, err := NewSwap(f.offer, f.deposit, raw)
	require.NoError(t, err)
	final := finishFallback(t, s.Fallback)
	require.NoError(t, executeAgainst(t, final, f.prevout))
}

// TestFallbackIsZeroFeeP2AParent checks the shape of the honest fallback: a
// version 3 transaction that moves the whole deposit anchor value into the
// HTLC, pays no fee and carries one zero-value pay-to-anchor output.
func TestFallbackIsZeroFeeP2AParent(t *testing.T) {
	f := newFallbackFixture(t)
	packet, err := DecodePSBT(f.pkg.CommittedAnchorPsbt)
	require.NoError(t, err)
	tx := packet.UnsignedTx
	require.EqualValues(t, 3, tx.Version)
	require.Len(t, tx.TxIn, 1)
	require.Len(t, tx.TxOut, 2)
	require.Equal(t, f.prevout.Value, tx.TxOut[0].Value)
	require.Zero(t, tx.TxOut[1].Value)
	require.Equal(t, []byte{0x51, 0x02, 0x4e, 0x73}, tx.TxOut[1].PkScript)

	require.Equal(t, sdk.CustomAnchorFundingExternalP2AFeeBump,
		f.pkg.Funding.Mode)
	require.NotNil(t, f.pkg.Funding.P2AOutputIndex)
	require.EqualValues(t, 1, *f.pkg.Funding.P2AOutputIndex)
	require.Zero(t, f.pkg.Funding.ActualFeeSat)

	// The publisher broadcasts the package itself; tapd only logs it.
	require.True(t, f.pkg.Publish.SkipAnchorTxBroadcast)
	require.True(t, f.pkg.Publish.ExternalBroadcast)
}

// TestFallbackRequestReservesSweepFee checks that the HTLC output must keep
// the requested claim or refund fee above the dust limit.
func TestFallbackRequestReservesSweepFee(t *testing.T) {
	f := newFallbackFixture(t)
	value := uint64(f.prevout.Value)

	_, err := FallbackRequest(f.swap, value-330)
	require.NoError(t, err)
	_, err = FallbackRequest(f.swap, value-329)
	require.ErrorIs(t, err, swapkit.ErrInvalid)
}

// TestFallbackRejectsFeePayingPackage checks that a fallback which fixes its
// fee at signing time is rejected. Such a transaction has no pay-to-anchor
// output, so its publisher could not raise the fee later.
func TestFallbackRejectsFeePayingPackage(t *testing.T) {
	f := newFallbackFixture(t)
	request, err := FallbackRequest(f.swap, 1_000)
	require.NoError(t, err)

	packet, err := DecodePSBT(request.AnchorPSBT)
	require.NoError(t, err)
	packet.UnsignedTx.Version = 2
	packet.UnsignedTx.TxOut = packet.UnsignedTx.TxOut[:1]
	packet.UnsignedTx.TxOut[0].Value -= 1_000
	packet.Outputs = packet.Outputs[:1]
	request.AnchorPSBT, err = EncodePSBT(packet)
	require.NoError(t, err)
	request.Outputs[0].AnchorValueSat -= 1_000
	request.Funding = sdk.CustomAnchorFundingPlan{
		Mode:              sdk.CustomAnchorFundingCallerFundedExact,
		CallerFundedExact: &sdk.CustomAnchorCallerFundedExact{},
	}

	wallet := sdk.NewWallet(&pathClient{t: t}, sdk.NetworkRegtest)
	built, err := wallet.NewCustomAnchorTxBuilder().Build(
		t.Context(), request,
	)
	require.NoError(t, err)
	pkg, err := built.Commit(t.Context(), sdk.CustomAnchorCommitOptions{})
	require.NoError(t, err)
	raw, err := pkg.MarshalBinary()
	require.NoError(t, err)

	_, err = NewSwap(f.offer, f.deposit, raw)
	require.ErrorIs(t, err, swapkit.ErrInvalid)
}

// TestFallbackForgedPrevoutCannotBeSealed checks that a peer cannot hand over
// a fallback whose signed previous output differs from the deposit. A changed
// value breaks the zero-fee rule of a pay-to-anchor parent and a changed script
// no longer matches the asset input, so the SDK refuses to seal either one. The
// validator's own prevout comparison remains as a second, independent check.
func TestFallbackForgedPrevoutCannotBeSealed(t *testing.T) {
	f := newFallbackFixture(t)
	script := bytes.Clone(f.prevout.PkScript)
	script[len(script)-1] ^= 1

	forgeries := map[string]*wire.TxOut{
		"value":  wire.NewTxOut(f.prevout.Value+5_000, f.prevout.PkScript),
		"script": wire.NewTxOut(f.prevout.Value, script),
	}
	for name, prevout := range forgeries {
		t.Run(name, func(t *testing.T) {
			forged := f.pkg.Clone()
			packet, err := DecodePSBT(forged.CommittedAnchorPsbt)
			require.NoError(t, err)
			packet.Inputs[0].WitnessUtxo = prevout
			anchor, err := EncodePSBT(packet)
			require.NoError(t, err)
			forged.CommittedAnchorPsbt = anchor
			forged.AnchorPsbt = bytes.Clone(anchor)
			_, err = forged.Seal()
			require.Error(t, err)

			// An unsealed forgery cannot be encoded for a peer either.
			_, err = forged.MarshalBinary()
			require.Error(t, err)
		})
	}
}

// TestFallbackRejectsInternalBroadcast checks that the validator rejects a
// package whose sealed publication metadata would have tapd broadcast the
// fallback on its own. A zero-fee parent only relays together with its child,
// so the honest package always leaves the broadcast to its publisher.
func TestFallbackRejectsInternalBroadcast(t *testing.T) {
	f := newFallbackFixture(t)

	forged := f.pkg.Clone()
	forged.Publish.SkipAnchorTxBroadcast = false
	forged.Publish.ExternalBroadcast = false
	forged, err := forged.Seal()
	require.NoError(t, err)
	raw, err := forged.MarshalBinary()
	require.NoError(t, err)

	_, err = NewSwap(f.offer, f.deposit, raw)
	require.ErrorIs(t, err, swapkit.ErrInvalid)
}
