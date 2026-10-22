package deposit

import (
	"testing"

	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/loop/assets/sweep"
	"github.com/stretchr/testify/require"
)

// addBitcoinChange appends an ordinary Bitcoin-only output to a sweep PSBT.
func addBitcoinChange(packet *psbt.Packet) {
	packet.UnsignedTx.AddTxOut(&wire.TxOut{
		Value: 330,
		PkScript: append([]byte{txscript.OP_0, txscript.OP_DATA_20},
			make([]byte, 20)...),
	})
	packet.Outputs = append(packet.Outputs, psbt.POutput{})
}

// TestTimeoutBitcoinOutputApproval rejects changes to the approved Bitcoin
// outputs before the deposit refund can request a signature.
func TestTimeoutBitcoinOutputApproval(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*witnessFixture)
	}{
		{"root value", func(f *witnessFixture) {
			f.packet.UnsignedTx.TxOut[0].Value++
		}},
		{"recipient value", func(f *witnessFixture) {
			f.packet.UnsignedTx.TxOut[1].Value++
		}},
		{"recipient script", func(f *witnessFixture) {
			f.packet.UnsignedTx.TxOut[1].PkScript[2] ^= 1
		}},
		{"added Bitcoin change", func(f *witnessFixture) {
			addBitcoinChange(f.packet)
		}},
		{"removed output", func(f *witnessFixture) {
			f.packet.UnsignedTx.TxOut = f.packet.UnsignedTx.TxOut[:1]
			f.packet.Outputs = f.packet.Outputs[:1]
		}},
		{"reordered outputs", func(f *witnessFixture) {
			outputs := f.packet.UnsignedTx.TxOut
			outputs[0], outputs[1] = outputs[1], outputs[0]
		}},
		{"missing approval", func(f *witnessFixture) {
			f.transfer = &sweep.Transfer{Packets: f.transfer.Packets}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newWitnessFixture(t)
			test.mutate(f)
			signer := &localSigner{
				privateKey: f.funderKey, expectedInput: f.assetInIndex,
			}
			_, err := f.kit.CreateTimeoutWitness(
				t.Context(), signer, f.proof, f.packet, f.transfer,
			)
			require.ErrorContains(t, err, "approval")
			require.Zero(t, signer.calls)
		})
	}
}

// TestTimeoutApprovedBitcoinChange accepts explicitly approved fee change and
// proves the approval does not alias its original slice, outputs or scripts.
func TestTimeoutApprovedBitcoinChange(t *testing.T) {
	f := newWitnessFixture(t)
	addBitcoinChange(f.packet)
	approved := f.packet.UnsignedTx.Copy().TxOut
	f.transfer = sweep.NewTransfer(f.transfer.Packets, approved)
	approved[0].Value++
	approved[1].PkScript[2] ^= 1
	approved[2] = nil
	signer := &localSigner{
		privateKey: f.funderKey, expectedInput: f.assetInIndex,
	}
	witness, err := f.kit.CreateTimeoutWitness(
		t.Context(), signer, f.proof, f.packet, f.transfer,
	)
	require.NoError(t, err)
	require.NotNil(t, witness)
	require.Equal(t, 1, signer.calls)
}
