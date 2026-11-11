package tapsdk

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/stretchr/testify/require"
)

// publishRetryFixture returns a finalized package and a wallet stub whose
// proof archive holds only the package's input.
func publishRetryFixture(t *testing.T) (*sdk.CustomAnchorTransferPackage,
	*observedRecoverySDK) {

	t.Helper()
	pkg := signingPackage(t)
	pkg.Publish.SkipAnchorTxBroadcast = false
	pkg.Publish.ExternalBroadcast = false
	pkg, err := pkg.Seal()
	require.NoError(t, err)
	signed, err := signTestPackage(
		t.Context(), signingWallet(t, 2), signingWallet(t, 3), pkg,
	)
	require.NoError(t, err)
	final, err := pkg.WithFinalAnchorPSBT(finishSigningFixture(t, signed))
	require.NoError(t, err)

	inputRaw := final.Inputs[0].ProofSource.Blob
	input, err := LastProof(inputRaw)
	require.NoError(t, err)
	return final, &observedRecoverySDK{
		partialRecoverySDK: &partialRecoverySDK{
			bindingSDK: &bindingSDK{valid: true},
		},
		input: &sdk.ProofFile{RawProofFile: inputRaw},
		point: sdk.Outpoint{
			Txid:  sdk.Hash(input.OutPoint().Hash),
			Index: input.OutPoint().Index,
		},
		stopErr: errors.New("reached transfer log"),
	}
}

// TestLogAndExportDoesNotRelogAnchor checks that an anchor tapd already logged
// is never logged again while its exact output proof is not archived yet.
func TestLogAndExportDoesNotRelogAnchor(t *testing.T) {
	pkg, client := publishRetryFixture(t)
	wallet := sdk.NewWallet(client, sdk.NetworkRegtest)
	chain := &bindingChain{tip: 200}
	txid := SDKPoint(pkg.Outputs[0].AnchorOutpoint).Hash.String()

	// The first invocation finds no logged anchor and publishes.
	_, err := LogAndExport(t.Context(), wallet, chain, 1, pkg, false)
	require.ErrorIs(t, err, client.stopErr)
	require.NotNil(t, client.logged)
	require.Equal(t, []string{txid}, client.lookups)

	// A backend that ignores the anchor filter cannot suppress it.
	client.logged = nil
	client.transfers = []*sdk.AssetTransfer{
		nil, {AnchorTxid: chainhash.Hash{1}.String()},
	}
	_, err = LogAndExport(t.Context(), wallet, chain, 1, pkg, false)
	require.ErrorIs(t, err, client.stopErr)
	require.NotNil(t, client.logged)

	// Once tapd lists the anchor, a publisher or observer retry waits for
	// the archive instead of logging the transfer again.
	client.logged = nil
	client.transfers = append(client.transfers, &sdk.AssetTransfer{
		AnchorTxid: txid,
	})
	for _, observed := range []bool{false, true} {
		_, err = LogAndExport(
			t.Context(), wallet, chain, 1, pkg, observed,
		)
		require.ErrorIs(t, err, swapkit.ErrPending)
		require.Nil(t, client.logged)
	}

	// A failed lookup is reported and never read as an unlogged anchor.
	client.transfersErr = errors.New("transfer log unavailable")
	_, err = LogAndExport(t.Context(), wallet, chain, 1, pkg, false)
	require.ErrorIs(t, err, client.transfersErr)
	require.Nil(t, client.logged)
}

// archivedOutputSDK also archives the exact output proof of a package.
type archivedOutputSDK struct {
	*observedRecoverySDK

	output sdk.Outpoint
	file   *sdk.ProofFile
}

// ExportProof returns the archived output proof for its exact locator.
func (s *archivedOutputSDK) ExportProof(ctx context.Context, ref sdk.AssetRef,
	key sdk.PubKey, point *sdk.Outpoint) (*sdk.ProofFile, error) {

	if point != nil && *point == s.output {
		return s.file, nil
	}
	return s.observedRecoverySDK.ExportProof(ctx, ref, key, point)
}

// failingHeightChain reports a chain backend failure for the best height.
type failingHeightChain struct {
	*bindingChain

	err error
}

// Height returns the injected backend failure.
func (c *failingHeightChain) Height(context.Context) (uint32, error) {
	return 0, c.err
}

// archivedOutputProof derives the package's exact output proof at a height.
func archivedOutputProof(t *testing.T, pkg *sdk.CustomAnchorTransferPackage,
	height uint32) *proof.Proof {

	t.Helper()
	out := pkg.Outputs[0]
	p, err := LastProof(pkg.Inputs[0].ProofSource.Blob)
	require.NoError(t, err)
	packet, err := DecodePSBT(pkg.AnchorPsbt)
	require.NoError(t, err)
	tx, err := psbt.Extract(packet)
	require.NoError(t, err)
	scriptKey, err := btcec.ParsePubKey(out.ScriptKey[:])
	require.NoError(t, err)

	p.Asset.ScriptKey = asset.NewScriptKey(scriptKey)
	p.AnchorTx = *tx
	p.InclusionProof.OutputIndex = out.AnchorOutpoint.Index
	p.BlockHeight = height
	p.TxMerkleProof = proof.TxMerkleProof{}
	p.BlockHeader.MerkleRoot = tx.TxHash()
	require.Equal(t, SDKPoint(out.AnchorOutpoint), p.OutPoint())
	require.Equal(t, out.IssuanceID, sdk.AssetID(p.Asset.ID()))
	return p
}

// TestLogAndExportArchivedProofAwaitsConfirmation checks that an archived
// exact output proof reports its own confirmation result. Neither an
// outstanding confirmation nor a chain backend failure logs the transfer again.
func TestLogAndExportArchivedProofAwaitsConfirmation(t *testing.T) {
	pkg, recovery := publishRetryFixture(t)
	p := archivedOutputProof(t, pkg, 200)
	raw := bindingRaw(t, p)
	client := &archivedOutputSDK{
		observedRecoverySDK: recovery,
		output:              pkg.Outputs[0].AnchorOutpoint,
		file:                &sdk.ProofFile{RawProofFile: raw},
	}
	wallet := sdk.NewWallet(client, sdk.NetworkRegtest)
	chain := &bindingChain{tip: 200, blocks: map[uint32]*wire.MsgBlock{
		200: bindingBlock(p),
	}}

	// One of three required confirmations is outstanding.
	_, err := LogAndExport(t.Context(), wallet, chain, 3, pkg, false)
	require.ErrorIs(t, err, swapkit.ErrPending)

	// A chain backend failure is reported, not answered with a new log.
	backend := errors.New("chain backend unavailable")
	_, err = LogAndExport(t.Context(), wallet, &failingHeightChain{
		bindingChain: chain, err: backend,
	}, 1, pkg, false)
	require.ErrorIs(t, err, backend)

	// The same archived proof is returned once it is confirmed deeply.
	chain.tip = 202
	exported, err := LogAndExport(t.Context(), wallet, chain, 3, pkg, false)
	require.NoError(t, err)
	require.Equal(t, raw, exported)

	require.Nil(t, recovery.logged)
	require.Empty(t, recovery.lookups)
}
