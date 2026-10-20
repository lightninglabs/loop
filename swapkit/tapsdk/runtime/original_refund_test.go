package runtime

import (
	"encoding/json"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/lightninglabs/loop/swapkit/bridge"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/stretchr/testify/require"
)

func originalRefundFixture(t *testing.T) (*Runtime, *swapRecord,
	*proof.Proof, *bindingChain) {

	t.Helper()
	r, s, _, chain := bindingOutcome(t, true)
	// The server never obtained the client's completed fallback.
	s.Fallback = transaction{}
	require.NoError(t, r.save(s))
	d, err := r.localDeposit(s.Offer.Terms.DepositOutpoints[0])
	require.NoError(t, err)
	base, err := lastProof(d.Deposit.Proof)
	require.NoError(t, err)
	_, spend, err := originalRefundPolicy(d)
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
	op := bridge.Operation{Offer: s.Offer, Evidence: bindingRaw(t, p)}
	result, err := r.inspectRefund(t.Context(), op)
	require.NoError(t, err)
	require.True(t, result.Refunded)
	stored, err := r.swap(s.Offer)
	require.NoError(t, err)
	require.Nil(t, stored.Fallback.Package)
	require.Equal(t, op.Evidence, stored.RefundProof)

	// A stored original refund is revalidated just like an HTLC refund.
	chain.blocks[201].Header.Nonce++
	op.Evidence = nil
	result, err = r.inspectRefund(t.Context(), op)
	require.NoError(t, err)
	require.False(t, result.Refunded)
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
			err := r.verifyOriginalRefund(t.Context(), s,
				bindingRaw(t, p))
			require.Error(t, err)
		})
	}
}

func TestDepositContractRejectsIdentitySubstitution(t *testing.T) {
	r, s, _, _ := bindingOutcome(t, false)
	d, err := r.localDeposit(s.Offer.Terms.DepositOutpoints[0])
	require.NoError(t, err)
	for _, mutate := range []func(*depositContract){
		func(c *depositContract) { c.Output.IssuanceID[0] ^= 1 },
		func(c *depositContract) { c.Output.AssetType++ },
		func(c *depositContract) { c.Output.Amount++ },
	} {
		var c depositContract
		require.NoError(t, json.Unmarshal(d.Deposit.Contract, &c))
		mutate(&c)
		other := d.Deposit
		other.Contract, err = json.Marshal(c)
		require.NoError(t, err)
		_, err = r.verifyDeposit(t.Context(), other, false)
		require.ErrorIs(t, err, bridge.ErrInvalid)
	}
}

func TestServerCanReconcileSpentRefundButClientNeedsSpendability(t *testing.T) {
	for _, original := range []bool{false, true} {
		name := "htlc"
		if original {
			name = "original deposit"
		}
		t.Run(name, func(t *testing.T) {
			var r *Runtime
			var s *swapRecord
			var p *proof.Proof
			var chain *bindingChain
			if original {
				r, s, p, chain = originalRefundFixture(t)
			} else {
				r, s, p, chain = bindingOutcome(t, true)
			}
			raw := bindingRaw(t, p)
			chain.unspent = false
			err := r.verifyRefundOutcome(t.Context(), s, raw)
			require.NoError(t, err)
			if original {
				err = r.verifyOriginalRefund(
					t.Context(), s, raw,
				)
			} else {
				err = r.verifyOutcome(t.Context(), s, raw, true)
			}
			require.ErrorIs(t, err, bridge.ErrPending)

			chain.blocks[p.BlockHeight].Header.Nonce++
			err = r.verifyRefundOutcome(t.Context(), s, raw)
			require.ErrorIs(t, err, bridge.ErrPending)
		})
	}
}
