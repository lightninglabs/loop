package loopdb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/coreos/bbolt"
)

// migrateExactAmount migrates the loop out contracts to new format, having
// two new fields in the end: DestAmount and ChangeAddr.
func migrateExactAmount(tx *bbolt.Tx, chainParams *chaincfg.Params) error {
	// Prepare suffix.
	var b bytes.Buffer
	var amount btcutil.Amount
	if err := binary.Write(&b, byteOrder, amount); err != nil {
		return err
	}
	if err := wire.WriteVarString(&b, 0, ""); err != nil {
		return err
	}
	suffix := b.Bytes()

	// Make the list of buckets.
	rootBucket := tx.Bucket(loopOutBucketKey)
	if rootBucket == nil {
		return fmt.Errorf("bucket %v does not exist", loopOutBucketKey)
	}

	var swaps [][]byte
	// Do not modify inside the for each.
	err := rootBucket.ForEach(func(swapHash, v []byte) error {
		// Only go into things that we know are sub-bucket keys.
		if rootBucket.Bucket(swapHash) != nil {
			swaps = append(swaps, swapHash)
		}

		return nil
	})
	if err != nil {
		return err
	}

	// With the swaps listed, migrate them one by one.
	for _, swapHash := range swaps {
		swapBucket := rootBucket.Bucket(swapHash)
		if swapBucket == nil {
			return fmt.Errorf("swap bucket %x not found", swapHash)
		}

		contractBytes := swapBucket.Get(contractKey)
		if contractBytes == nil {
			return errors.New("contract not found")
		}

		contractBytes = append(contractBytes, suffix...)

		if err := swapBucket.Put(contractKey, contractBytes); err != nil {
			return fmt.Errorf("failed to save the updated contract for %x: %v", swapHash, err)
		}
	}

	return nil
}
