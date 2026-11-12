package tapsdk

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/loop/swapkit"
	"github.com/stretchr/testify/require"
)

// Shared subtest names for the claim and refund directions.
const (
	hardeningClaimCase  = "claim"
	hardeningRefundCase = "refund"
)

// TestVerifyOutcomeRecognizesAnnexSpend checks that a consensus-valid claim or
// refund through the agreed leaf is still recognized when its witness also
// carries a BIP-341 annex. spendsLeaf strips the annex before reading the leaf
// script and control block from the last two witness elements.
func TestVerifyOutcomeRecognizesAnnexSpend(t *testing.T) {
	for _, refund := range []bool{false, true} {
		name := hardeningClaimCase
		if refund {
			name = hardeningRefundCase
		}
		t.Run(name, func(t *testing.T) {
			checkAnnexSpend(t, refund)
		})
	}
}

func checkAnnexSpend(t *testing.T, refund bool) {
	t.Helper()
	r, s, p, chain := bindingOutcome(t, refund)
	prior := s.Fallback.Outputs[0]
	policy, err := HTLC(s.Offer, s.Deposit.Contract)
	require.NoError(t, err)
	path, err := policy.SuccessSpend(prior.TaprootAssetRoot)
	signer := bindingKey(3)
	if refund {
		path, err = policy.TimeoutSpend(prior.TaprootAssetRoot)
		signer = bindingKey(2)
	}
	require.NoError(t, err)

	// Sign the same transaction again, committing to an annex.
	tx := &p.AnchorTx
	annex := []byte{txscript.TaprootAnnexTag, 0x01}
	fetch := txscript.NewCannedPrevOutputFetcher(
		path.PkScript, prior.AnchorValueSat,
	)
	hashes := txscript.NewTxSigHashes(tx, fetch)
	digest, err := txscript.CalcTapscriptSignaturehash(
		hashes, txscript.SigHashDefault, tx, 0, fetch,
		txscript.NewBaseTapLeaf(path.LeafScript),
		txscript.WithAnnex(annex),
	)
	require.NoError(t, err)
	sig, err := schnorr.Sign(signer, digest)
	require.NoError(t, err)
	old := tx.TxIn[0].Witness
	witness := [][]byte{sig.Serialize(), path.LeafScript,
		path.ControlBlock, annex}
	if !refund {
		// The success witness starts with the preimage.
		witness = append([][]byte{old[0]}, witness...)
	}
	tx.TxIn[0].Witness = witness
	require.Equal(t, p.BlockHeader.MerkleRoot, tx.TxHash())

	// The spend satisfies the agreed leaf under consensus rules.
	engine, err := txscript.NewEngine(path.PkScript, tx, 0,
		txscript.StandardVerifyFlags, nil,
		txscript.NewTxSigHashes(tx, fetch),
		prior.AnchorValueSat, fetch)
	require.NoError(t, err)
	require.NoError(t, engine.Execute())

	err = VerifyOutcome(t.Context(), r.wallet(), chain, 1, s,
		bindingRaw(t, p), refund, refund)
	require.NoError(t, err, "refund=%v", refund)
	require.NotErrorIs(t, err, swapkit.ErrInvalid)
}
