package tapsdk

import (
	"encoding/json"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript"
	"github.com/lightninglabs/loop/swapkit"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/stretchr/testify/require"
)

func originalRefundFixture(t *testing.T) (*proofNode, *SwapContext,
	*proof.Proof, *bindingChain) {

	t.Helper()
	r, s, _, chain := bindingOutcome(t, true)
	// The server never obtained the client's completed fallback.
	s.Fallback = nil
	d := s.Deposit
	base, err := LastProof(d.Deposit.Proof)
	require.NoError(t, err)
	_, spend, err := OriginalRefundPolicy(d)
	require.NoError(t, err)
	key := bindingKey(2)
	output := txscript.ComputeTaprootKeyNoScript(key.PubKey())
	output, err = schnorr.ParsePubKey(schnorr.SerializePubKey(output))
	require.NoError(t, err)
	p := bindingProof(t, bindingGenesis(), output, key.PubKey(),
		nil, base.OutPoint(), 201, false)
	p.AnchorTx.TxIn[0].Sequence = spend.Sequence
	p.AnchorTx.LockTime = spend.LockTime
	p.AnchorTx.TxOut[0].Value = 9000
	fetch := txscript.NewCannedPrevOutputFetcher(spend.PkScript, 10_000)
	sig, err := txscript.RawTxInTapscriptSignature(&p.AnchorTx,
		txscript.NewTxSigHashes(&p.AnchorTx, fetch), 0, 10_000,
		spend.PkScript, txscript.NewBaseTapLeaf(spend.LeafScript),
		txscript.SigHashDefault, key)
	require.NoError(t, err)
	p.AnchorTx.TxIn[0].Witness = [][]byte{
		sig, spend.LeafScript, spend.ControlBlock,
	}
	p.BlockHeader.MerkleRoot = p.AnchorTx.TxHash()
	chain.tip, chain.blocks[201] = 202, bindingBlock(p)
	return r, s, p, chain
}

func TestOriginalCSVRefundWithoutCompletedFallback(t *testing.T) {
	r, s, p, chain := originalRefundFixture(t)
	raw := bindingRaw(t, p)
	require.NoError(t, VerifyRefundOutcome(t.Context(), r.wallet(), chain, 1, s, raw))
	require.Nil(t, s.Fallback)
	chain.blocks[201].Header.Nonce++
	require.ErrorIs(t, VerifyRefundOutcome(t.Context(), r.wallet(), chain, 1, s, raw), swapkit.ErrPending)
}

func TestOriginalRefundRejectsWrongSpend(t *testing.T) {
	for _, name := range []string{"wrong signer", "wrong prevout",
		"wrong leaf", "spent output", "too early"} {
		t.Run(name, func(t *testing.T) {
			r, s, p, chain := originalRefundFixture(t)
			input := p.AnchorTx.TxIn[0]
			switch name {
			case "wrong signer":
				input.Witness[0][0] ^= 1
			case "wrong prevout":
				input.PreviousOutPoint.Index++
			case "wrong leaf":
				input.Witness[1][0] ^= 1
			case "spent output":
				chain.unspent = false
			case "too early":
				p.BlockHeight = 199
				chain.blocks[199] = bindingBlock(p)
			}
			err := verifyOriginalRefund(t.Context(), r.wallet(), r.chain, 1, s, bindingRaw(t, p), true)
			require.Error(t, err)
		})
	}
}

func TestDepositContractRejectsIdentitySubstitution(t *testing.T) {
	_, s, _, _ := bindingOutcome(t, false)
	d := s.Deposit
	for _, mutate := range []func(*DepositContract){
		func(c *DepositContract) { c.Output.IssuanceID[0] ^= 1 },
		func(c *DepositContract) { c.Output.AssetType++ },
		func(c *DepositContract) { c.Output.Amount++ },
	} {
		var c DepositContract
		require.NoError(t, json.Unmarshal(d.Deposit.Contract, &c))
		mutate(&c)
		err := checkDepositContract(d.Deposit, c, d.Proof)
		require.ErrorIs(t, err, swapkit.ErrInvalid)
	}
}

func TestServerCanReconcileSpentRefundButClientNeedsSpendability(t *testing.T) {
	for _, original := range []bool{false, true} {
		name := "HTLC"
		if original {
			name = "original deposit"
		}
		t.Run(name, func(t *testing.T) {
			var r *proofNode
			var s *SwapContext
			var p *proof.Proof
			var chain *bindingChain
			if original {
				r, s, p, chain = originalRefundFixture(t)
			} else {
				r, s, p, chain = bindingOutcome(t, true)
			}
			raw := bindingRaw(t, p)
			chain.unspent = false
			err := VerifyRefundOutcome(t.Context(), r.wallet(), r.chain, r.minConfs, s, raw)
			require.NoError(t, err)
			if original {
				err = verifyOriginalRefund(t.Context(), r.wallet(), r.chain, 1, s, raw, true)
			} else {
				err = VerifyOutcome(t.Context(), r.wallet(), r.chain, r.minConfs, s, raw, true, true)
			}
			require.ErrorIs(t, err, swapkit.ErrPending)

			chain.blocks[p.BlockHeight].Header.Nonce++
			err = VerifyRefundOutcome(t.Context(), r.wallet(), r.chain, r.minConfs, s, raw)
			require.ErrorIs(t, err, swapkit.ErrPending)
		})
	}
}
