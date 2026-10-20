package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/loop/swapkit/bridge"
	kit "github.com/lightninglabs/loop/swapkit/tapsdk"
	sdk "github.com/lightninglabs/tap-sdk"
	bolt "go.etcd.io/bbolt"
)

// refundDeposit implements the original static deposit CSV path without a
// server offer. The client manager separately retains pending-swap exclusions.
func (r *Runtime) refundDeposit(ctx context.Context,
	request bridge.RefundDepositRequest) ([]byte, error) {

	d, err := r.localDeposit(request.Outpoint)
	if err != nil {
		return nil, err
	}
	p, err := r.verifyDeposit(ctx, d.Deposit, false)
	if err != nil {
		return nil, err
	}
	tip, err := r.cfg.Chain.Height(ctx)
	if err != nil {
		return nil, err
	}
	if uint64(tip) < uint64(p.BlockHeight)+uint64(d.Request.CSV) {
		return nil, bridge.ErrPending
	}
	// Expired signing material is retained, never erased to make the
	// anchor appear free. Every admitted fallback expires before this CSV.
	err = r.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte("runtime-v1")).Cursor()
		prefix := []byte("swap/")
		for k, v := c.Seek(prefix); k != nil &&
			bytes.HasPrefix(k, prefix); k, v = c.Next() {
			var swap swapRecord
			if err := json.Unmarshal(v, &swap); err != nil {
				return err
			}
			terms := swap.Offer.Terms
			for _, point := range terms.DepositOutpoints {
				if point != request.Outpoint {
					continue
				}
				deadline := uint64(terms.RefundHeight) +
					uint64(terms.RecoveryDelta)
				if time.Now().Before(terms.OfferExpiry) ||
					uint64(tip) < deadline {
					return bridge.ErrPending
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var c depositContract
	if err := json.Unmarshal(d.Deposit.Contract, &c); err != nil {
		return nil, err
	}
	policy, err := kit.NewDepositKit(kit.DepositKitParams{
		ClientKey: c.ClientKey, ServerKey: c.ServerKey,
		CSVExpiry: c.CSV,
	})
	if err != nil {
		return nil, err
	}
	spend, err := policy.TimeoutSpend(c.Output.TaprootAssetRoot)
	if err != nil {
		return nil, err
	}
	id := key("deposit/", d.Request.RequestID)
	if d.RefundPlan == nil {
		d.RefundSince = tip
		free, err := r.cfg.Chain.Unspent(ctx, p.OutPoint())
		if err != nil {
			return nil, err
		}
		if !free {
			return nil, bridge.ErrPending
		}
		value := c.Output.AnchorValueSat - int64(r.cfg.FeeSat)
		if value < 330 {
			return nil, bridge.ErrInvalid
		}
		recipient, err := walletOutput(c.ClientKey, d.Request.KeyFamily,
			d.Request.KeyIndex, c.Output.AssetRef,
			c.Output.Amount, value)
		if err != nil {
			return nil, err
		}
		_, err = r.cfg.SDK.DeclareScriptKey(ctx,
			&sdk.DeclareScriptKeyRequest{
				ScriptKey: recipient.Script.External.ScriptKey,
			})
		if err != nil {
			return nil, err
		}
		anchor, err := anchorTemplate(
			p, recipient.Anchor.InternalKey, value, spend,
		)
		if err != nil {
			return nil, err
		}
		input := assetInput(c.Output, d.Deposit.Proof)
		d.RefundPlan = customRequest(input, recipient, anchor,
			spend.SigningPlan(0))
		if err := r.put(id, d); err != nil {
			return nil, err
		}
	}
	if d.Refund.Package == nil {
		d.Refund.Package, err = r.commit(ctx, d.RefundPlan)
		if err != nil {
			return nil, err
		}
		if err := r.put(id, d); err != nil {
			return nil, err
		}
	}
	if len(d.Refund.Final) == 0 {
		requests, err := d.Refund.Package.SigningRequests()
		if err != nil || len(requests.ScriptPath) != 1 {
			return nil, bridge.ErrInvalid
		}
		request := requests.ScriptPath[0]
		sig, err := r.cfg.Signer.ScriptPath(ctx,
			locator(d.Request.KeyFamily, d.Request.KeyIndex),
			d.Refund.Package, request.ID)
		if err != nil {
			return nil, err
		}
		witness, err := policy.TimeoutWitness(
			c.Output.TaprootAssetRoot, sig,
		)
		if err != nil {
			return nil, err
		}
		final, err := d.Refund.Package.ApplyScriptPathWitness(
			request.ID, witness[:len(witness)-2])
		if err != nil {
			return nil, err
		}
		d.Refund.Final = final.AnchorPsbt
		if err := r.put(id, d); err != nil {
			return nil, err
		}
	}
	raw, err := r.publish(ctx, d.Refund, d.RefundSince)
	if err != nil {
		return nil, err
	}
	end, err := r.verifyProof(ctx, raw, d.Deposit.GroupKey,
		d.Deposit.Amount, true)
	if err != nil {
		return nil, err
	}
	anchor, err := finalTx(d.Refund)
	if err != nil {
		return nil, err
	}
	if end.AnchorTx.TxHash() != anchor.TxHash() ||
		len(anchor.TxIn) != 1 ||
		anchor.TxIn[0].PreviousOutPoint != p.OutPoint() ||
		anchor.TxIn[0].Sequence != c.CSV {
		return nil, bridge.ErrInvalid
	}
	w := anchor.TxIn[0].Witness
	if len(w) != 3 || !bytes.Equal(w[1], spend.LeafScript) ||
		!bytes.Equal(w[2], spend.ControlBlock) ||
		anchor.TxIn[0].Sequence&wire.SequenceLockTimeDisabled != 0 {
		return nil, bridge.ErrInvalid
	}
	_, err = r.cfg.Wallet.ImportProofFile(ctx,
		&sdk.ProofFile{RawProofFile: raw})
	if err != nil {
		return nil, err
	}
	d.RefundProof = raw
	return raw, r.put(id, d)
}
