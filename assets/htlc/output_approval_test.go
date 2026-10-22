package htlc

import (
	"testing"

	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/loop/assets/sweep"
	"github.com/lightningnetwork/lnd/keychain"
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

// TestWitnessBitcoinOutputApproval rejects changes to the approved Bitcoin
// outputs before either HTLC signing path can request a signature.
func TestWitnessBitcoinOutputApproval(t *testing.T) {
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
	for _, timeout := range []bool{false, true} {
		path := preimagePath
		sequence := SuccessSequence
		if timeout {
			path = timeoutPath
			sequence = 144
		}
		for _, test := range tests {
			t.Run(path+"/"+test.name, func(t *testing.T) {
				f := newWitnessFixture(t, sequence)
				test.mutate(f)
				signer := &localSigner{
					privateKey:    f.receiverKey,
					expectedInput: f.assetInIndex,
				}
				var err error
				if timeout {
					signer.privateKey = f.senderKey
					_, err = f.kit.CreateTimeoutWitness(
						t.Context(), signer, f.proof, f.packet,
						keychain.KeyLocator{}, f.transfer,
					)
				} else {
					_, err = f.kit.CreatePreimageWitness(
						t.Context(), signer, f.proof, f.packet,
						keychain.KeyLocator{}, f.preimage,
						f.transfer,
					)
				}
				require.ErrorContains(t, err, "approval")
				require.Zero(t, signer.calls)
			})
		}
	}
}

// TestWitnessApprovedBitcoinChange accepts explicitly approved fee change and
// proves the approval does not alias its original slice, outputs or scripts.
func TestWitnessApprovedBitcoinChange(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		path := preimagePath
		sequence := SuccessSequence
		if timeout {
			path = timeoutPath
			sequence = 144
		}
		t.Run(path, func(t *testing.T) {
			f := newWitnessFixture(t, sequence)
			addBitcoinChange(f.packet)
			approved := f.packet.UnsignedTx.Copy().TxOut
			f.transfer = sweep.NewTransfer(f.transfer.Packets, approved)
			approved[0].Value++
			approved[1].PkScript[2] ^= 1
			approved[2] = nil
			signer := &localSigner{
				privateKey: f.receiverKey, expectedInput: f.assetInIndex,
			}
			var (
				witness *SpendWitness
				err     error
			)
			if timeout {
				signer.privateKey = f.senderKey
				witness, err = f.kit.CreateTimeoutWitness(
					t.Context(), signer, f.proof, f.packet,
					keychain.KeyLocator{}, f.transfer,
				)
			} else {
				witness, err = f.kit.CreatePreimageWitness(
					t.Context(), signer, f.proof, f.packet,
					keychain.KeyLocator{}, f.preimage, f.transfer,
				)
			}
			require.NoError(t, err)
			require.Equal(t, 1, signer.calls)
			f.executeWitness(t, witness)
		})
	}
}
