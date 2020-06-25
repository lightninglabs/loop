package loopdb

import (
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/coreos/bbolt"
)

// migrateUpdates migrates the swap updates to add an additional level of
// nesting, allowing for optional keys to be added.
func migrateUpdates(tx *bbolt.Tx, chainParams *chaincfg.Params) error {
	for _, key := range [][]byte{loopInBucketKey, loopOutBucketKey} {
		rootBucket := tx.Bucket(key)
		if rootBucket == nil {
			return fmt.Errorf("bucket %v does not exist", key)
		}

		err := migrateSwapTypeUpdates(rootBucket)
		if err != nil {
			return err
		}
	}

	return nil
}

// migrateSwapTypeUpdates migrates updates for swaps in the specified bucket.
func migrateSwapTypeUpdates(rootBucket *bbolt.Bucket) error {
	var swaps [][]byte

	// Do not modify inside the for each.
	err := rootBucket.ForEach(func(swapHash, v []byte) error {
		// Only go into things that we know are sub-bucket
		// keys.
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
			return fmt.Errorf("swap bucket %x not found",
				swapHash)
		}

		err := migrateSwapUpdates(swapBucket)
		if err != nil {
			return err
		}
	}

	return nil
}

// migrateSwapUpdates migrates updates for the swap stored in the specified
// bucket.
func migrateSwapUpdates(swapBucket *bbolt.Bucket) error {
	// With the main swap bucket obtained, we'll grab the
	// raw swap contract bytes.
	updatesBucket := swapBucket.Bucket(updatesBucketKey)
	if updatesBucket == nil {
		return errors.New("updates bucket not found")
	}

	type state struct {
		id, state []byte
	}

	var existingStates []state

	// Do not modify inside the for each.
	err := updatesBucket.ForEach(func(k, v []byte) error {
		existingStates = append(existingStates, state{id: k, state: v})
		return nil
	})
	if err != nil {
		return err
	}

	for _, existingState := range existingStates {
		// Delete the existing state key.
		err := updatesBucket.Delete(existingState.id)
		if err != nil {
			return err
		}

		// Re-create as a bucket.
		updateBucket, err := updatesBucket.CreateBucket(
			existingState.id,
		)
		if err != nil {
			return err
		}

		// Write back the basic state as a sub-key.
		err = updateBucket.Put(basicStateKey, existingState.state)
		if err != nil {
			return err
		}
	}

	return nil
}
