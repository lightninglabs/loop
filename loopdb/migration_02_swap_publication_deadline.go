package loopdb

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/coreos/bbolt"
)

// migrateSwapPublicationDeadline migrates the database to v02, by adding the
// SwapPublicationDeadline field to loop out contracts.
func migrateSwapPublicationDeadline(tx *bbolt.Tx, chainParams *chaincfg.Params) error {
	rootBucket := tx.Bucket(loopOutBucketKey)
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

		// Write the current contract serialization into a buffer.
		b := &bytes.Buffer{}
		if _, err := b.Write(contractBytes); err != nil {
			return err
		}

		// We migrate to the new format by copying the first 8 bytes
		// (the creation time) to the end (the swap deadline)
		var swapDeadline [8]byte
		copy(swapDeadline[:], contractBytes[:8])
		if _, err := b.Write(swapDeadline[:]); err != nil {
			return err
		}

		return swapBucket.Put(contractKey, b.Bytes())
	})
}
