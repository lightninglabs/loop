package codec

import (
	"bytes"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

const numsKeyHex = "027c79b9b26e463895eef5679d855894" +
	"2c86c4ad2233adef01bc3e6d540b3653fe"

var numsPubKey = func() *btcec.PublicKey {
	numsBytes, _ := hex.DecodeString(numsKeyHex)
	pubKey, _ := btcec.ParsePubKey(numsBytes)
	return pubKey
}()

// Outpoint identifies the anchor outpoint for a previous asset input.
type Outpoint struct {
	Txid  [32]byte
	Index uint32
}

// PrevID contains the protocol fields needed for STXO burn key derivation.
type PrevID struct {
	Outpoint   Outpoint
	IssuanceID [32]byte
	ScriptKey  [33]byte
}

// DeriveBurnKey derives a provably unspendable but unique key by tweaking the
// public NUMS key with a tap tweak:
//
//	burnTweak = h_tapTweak(NUMSKey || outPoint || assetID || scriptKeyXOnly)
//	burnKey = NUMSKey + burnTweak*G
func DeriveBurnKey(prevID PrevID) ([33]byte, error) {
	var (
		buf bytes.Buffer
		op  wire.OutPoint
		h   chainhash.Hash
	)

	copy(h[:], prevID.Outpoint.Txid[:])
	op.Hash = h
	op.Index = prevID.Outpoint.Index

	_ = wire.WriteOutPoint(&buf, 0, 0, &op)
	_, _ = buf.Write(prevID.IssuanceID[:])
	_, _ = buf.Write(prevID.ScriptKey[1:]) // x-only 32 bytes

	burnKey := txscript.ComputeTaprootOutputKey(numsPubKey, buf.Bytes())

	// Since we'll never query lnd for a burn key, it doesn't matter if we lose
	// parity information here. And this will only ever be serialized on chain
	// in a 32-byte representation as well, as this is always a script key.
	burnKey, _ = schnorr.ParsePubKey(schnorr.SerializePubKey(burnKey))

	var burnKeyBytes [33]byte
	copy(burnKeyBytes[:], burnKey.SerializeCompressed())

	return burnKeyBytes, nil
}

// DeriveSTXOAltLeaves returns the raw alt-leaf TLV blobs for the STXO set
// implied by the given prev IDs. Each leaf is encoded with script version 0 and
// a burn-key derived from the prev ID.
func DeriveSTXOAltLeaves(prevIDs []PrevID) ([][]byte, error) {
	if len(prevIDs) == 0 {
		return nil, nil
	}

	leaves := make([][]byte, 0, len(prevIDs))
	seen := make(map[[33]byte]struct{}, len(prevIDs))
	for idx, prevID := range prevIDs {
		burnKey, err := DeriveBurnKey(prevID)
		if err != nil {
			return nil, fmt.Errorf("derive burn key for prevID %d: %w",
				idx, err)
		}

		if _, ok := seen[burnKey]; ok {
			return nil, fmt.Errorf("duplicate derived burn key for prevID %d",
				idx)
		}
		seen[burnKey] = struct{}{}

		leaf, err := EncodeAltLeaf(0, burnKey)
		if err != nil {
			return nil, fmt.Errorf("encode STXO leaf %d: %w", idx,
				err)
		}

		leaves = append(leaves, leaf)
	}

	return leaves, nil
}
