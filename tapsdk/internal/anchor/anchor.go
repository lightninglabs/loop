package anchor

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/taproot-assets/tappsbt"
)

const dummyAmtSats = 1_000

var dummyP2TRScript = append(
	[]byte{txscript.OP_1, txscript.OP_DATA_32},
	bytes.Repeat([]byte{0x00}, 32)...,
)

// PreparePsbt builds the BTC-level anchor PSBT required by tapd's
// CommitVirtualPsbts RPC from the signed active and passive virtual packets.
func PreparePsbt(activePsbts [][]byte, passivePsbts [][]byte) (
	[]byte, error) {

	packets := make(
		[]*tappsbt.VPacket, 0, len(activePsbts)+len(passivePsbts),
	)

	for idx, psbtBytes := range activePsbts {
		packet, err := decodePacket(psbtBytes)
		if err != nil {
			return nil, fmt.Errorf("active packet %d: %w", idx, err)
		}

		packets = append(packets, packet)
	}

	for idx, psbtBytes := range passivePsbts {
		packet, err := decodePacket(psbtBytes)
		if err != nil {
			return nil, fmt.Errorf("passive packet %d: %w", idx, err)
		}

		packets = append(packets, packet)
	}

	if len(packets) == 0 {
		return nil, fmt.Errorf("no virtual PSBTs specified")
	}

	anchorPacket, err := prepareTemplate(packets)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := anchorPacket.Serialize(&buf); err != nil {
		return nil, fmt.Errorf("serialize anchor PSBT: %w", err)
	}

	return buf.Bytes(), nil
}

func decodePacket(psbtBytes []byte) (*tappsbt.VPacket, error) {
	if len(psbtBytes) == 0 {
		return nil, fmt.Errorf("empty virtual PSBT")
	}

	packet, err := tappsbt.Decode(psbtBytes)
	if err != nil {
		return nil, err
	}

	return packet, nil
}

func prepareTemplate(vPackets []*tappsbt.VPacket) (*psbt.Packet, error) {
	btcPacket, err := createAnchorTx(vPackets)
	if err != nil {
		return nil, err
	}

	for _, vPkt := range vPackets {
		for _, vIn := range vPkt.Inputs {
			anchor := vIn.Anchor

			if hasInput(btcPacket.UnsignedTx, vIn.PrevID.OutPoint) {
				continue
			}

			btcPacket.Inputs = append(
				btcPacket.Inputs, psbt.PInput{
					WitnessUtxo: &wire.TxOut{
						Value:    int64(anchor.Value),
						PkScript: anchor.PkScript,
					},
					SighashType:     anchor.SigHashType,
					Bip32Derivation: anchor.Bip32Derivation,
					TaprootBip32Derivation: anchor.
						TrBip32Derivation,
					TaprootInternalKey: schnorr.SerializePubKey(
						anchor.InternalKey,
					),
					TaprootMerkleRoot: anchor.MerkleRoot,
				},
			)
			btcPacket.UnsignedTx.TxIn = append(
				btcPacket.UnsignedTx.TxIn, &wire.TxIn{
					PreviousOutPoint: vIn.PrevID.OutPoint,
				},
			)
		}
	}

	return btcPacket, nil
}

func createAnchorTx(vPackets []*tappsbt.VPacket) (*psbt.Packet, error) {
	var maxOutputIndex uint32
	for _, vPkt := range vPackets {
		if len(vPkt.Outputs) == 0 {
			return nil, fmt.Errorf("virtual packet has no outputs")
		}

		for _, vOut := range vPkt.Outputs {
			if vOut.AnchorOutputIndex > maxOutputIndex {
				maxOutputIndex = vOut.AnchorOutputIndex
			}
		}
	}

	txTemplate := wire.NewMsgTx(2)
	for i := uint32(0); i <= maxOutputIndex; i++ {
		txTemplate.AddTxOut(&wire.TxOut{
			Value:    dummyAmtSats,
			PkScript: bytes.Clone(dummyP2TRScript),
		})
	}

	packet, err := psbt.NewFromUnsignedTx(txTemplate)
	if err != nil {
		return nil, fmt.Errorf("make anchor PSBT: %w", err)
	}

	for _, vPkt := range vPackets {
		for _, vOut := range vPkt.Outputs {
			btcOut := &packet.Outputs[vOut.AnchorOutputIndex]
			btcOut.TaprootInternalKey = schnorr.SerializePubKey(
				vOut.AnchorOutputInternalKey,
			)

			for _, derivation := range vOut.AnchorOutputBip32Derivation {
				btcOut.Bip32Derivation = tappsbt.AddBip32Derivation(
					btcOut.Bip32Derivation, derivation,
				)
			}
			for _, derivation := range vOut.AnchorOutputTaprootBip32Derivation {
				btcOut.TaprootBip32Derivation =
					tappsbt.AddTaprootBip32Derivation(
						btcOut.TaprootBip32Derivation,
						derivation,
					)
			}
		}
	}

	return packet, nil
}

func hasInput(tx *wire.MsgTx, outpoint wire.OutPoint) bool {
	for _, txIn := range tx.TxIn {
		if txIn.PreviousOutPoint == outpoint {
			return true
		}
	}

	return false
}
