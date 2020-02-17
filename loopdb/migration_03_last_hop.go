package loopdb

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/coreos/bbolt"
)

// migrateLastHop migrates the database to v03, replacing the never used loop in
// channel by a last hop pubkey.
func migrateLastHop(tx *bbolt.Tx, chainParams *chaincfg.Params) error {
	rootBucket := tx.Bucket(loopInBucketKey)
	if rootBucket == nil {
		return errors.New("bucket does not exist")
	}

	return rootBucket.ForEach(func(swapHash, v []byte) error {
		// Only go into things that we know are sub-bucket
		// keys.
		if v != nil {
			return nil
		}

		// From the root bucket, we'll grab the next swap
		// bucket for this swap from its swaphash.
		swapBucket := rootBucket.Bucket(swapHash)
		if swapBucket == nil {
			return fmt.Errorf("swap bucket %x not found",
				swapHash)
		}

		// With the main swap bucket obtained, we'll grab the
		// raw swap contract bytes.
		contractBytes := swapBucket.Get(contractKey)
		if contractBytes == nil {
			return errors.New("contract not found")
		}

		const (
			// deprecatedLoopInChannelStart is the starting offset
			// of the old loop in channel field: 8 (initiation time)
			// + 32 (preimage) + 8 (amount requested) + 33 + 33
			// (sender and receiver keys) + 4 (cltv expiry) + 8 + 8
			// (max miner and swap fee) + 4 (initiation height) + 4
			// (conf target) = 142.
			deprecatedLoopInChannelStart = 142

			// expectedTotalLength is the expect total length of the
			// serialized contract. It adds 8 (old loop in channel)
			// + 1 (external htlc) = 9 bytes to
			// deprecatedLoopInChannelStart.
			expectedTotalLength = deprecatedLoopInChannelStart + 9
		)

		// Sanity check to see if the constants above match the contract
		// bytes.
		if len(contractBytes) != expectedTotalLength {
			return errors.New("invalid serialized contract length")
		}

		// Copy the unchanged fields into the buffer.
		b := &bytes.Buffer{}
		_, err := b.Write(contractBytes[:deprecatedLoopInChannelStart])
		if err != nil {
			return err
		}

		// We now set the new last hop field to all zeroes to indicate
		// that there is no restriction.
		var noLastHop [33]byte
		if _, err := b.Write(noLastHop[:]); err != nil {
			return err
		}

		// Append the remaining field ExternalHtlc.
		_, err = b.Write(contractBytes[deprecatedLoopInChannelStart+8:])
		if err != nil {
			return err
		}

		return swapBucket.Put(contractKey, b.Bytes())
	})
}
