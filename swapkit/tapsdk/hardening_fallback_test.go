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
	request, err := FallbackRequest(s, 1_000, 1_000)
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
	return &fallbackFixture{offer: offer, deposit: d, pkg: pkg,
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

// TestFallbackRejectsForgedPrevout checks that the fallback validator binds
// the committed package's signed previous output to the authenticated deposit.
// A peer that changes only the PSBT's claimed input value keeps the unsigned
// transaction, every summary and the honest txid, so only this prevout
// comparison distinguishes it. The forged package must be rejected before it
// is ever co-signed.
func TestFallbackRejectsForgedPrevout(t *testing.T) {
	f := newFallbackFixture(t)

	forged := f.pkg.Clone()
	packet, err := DecodePSBT(forged.CommittedAnchorPsbt)
	require.NoError(t, err)
	packet.Inputs[0].WitnessUtxo = wire.NewTxOut(
		f.prevout.Value+5_000, bytes.Clone(f.prevout.PkScript),
	)
	anchor, err := EncodePSBT(packet)
	require.NoError(t, err)
	forged.CommittedAnchorPsbt = anchor
	forged.AnchorPsbt = bytes.Clone(anchor)
	forged.Funding.ActualFeeSat += 5_000
	forged, err = forged.Seal()
	require.NoError(t, err)
	raw, err := forged.MarshalBinary()
	require.NoError(t, err)

	// The unsigned transaction and txid are exactly the honest ones, so the
	// only tell is the committed previous output the signature covers.
	honest, err := DecodePSBT(f.pkg.CommittedAnchorPsbt)
	require.NoError(t, err)
	require.Equal(t, honest.UnsignedTx.TxHash(), packet.UnsignedTx.TxHash())

	_, err = NewSwap(f.offer, f.deposit, raw)
	require.ErrorIs(t, err, swapkit.ErrInvalid)
}

// TestFallbackRejectsSkipBroadcast checks that the validator rejects a package
// whose sealed publication metadata would log the fallback without broadcasting
// it. The honest package never sets this flag.
func TestFallbackRejectsSkipBroadcast(t *testing.T) {
	f := newFallbackFixture(t)
	require.False(t, f.pkg.Publish.SkipAnchorTxBroadcast)

	forged := f.pkg.Clone()
	forged.Publish.SkipAnchorTxBroadcast = true
	forged, err := forged.Seal()
	require.NoError(t, err)
	raw, err := forged.MarshalBinary()
	require.NoError(t, err)

	_, err = NewSwap(f.offer, f.deposit, raw)
	require.ErrorIs(t, err, swapkit.ErrInvalid)
}
