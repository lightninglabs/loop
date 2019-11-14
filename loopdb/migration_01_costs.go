package loopdb

import (
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/coreos/bbolt"
)

// noMigrationAvailable is the fall back migration in case there is no migration
// implemented.
func migrateCosts(tx *bbolt.Tx, _ *chaincfg.Params) error {
	if err := migrateCostsForBucket(tx, loopInBucketKey); err != nil {
		return err
	}
	if err := migrateCostsForBucket(tx, loopOutBucketKey); err != nil {
		return err
	}
	return nil
}

func migrateCostsForBucket(tx *bbolt.Tx, bucketKey []byte) error {
	// First, we'll grab our main loop in bucket key.
	rootBucket := tx.Bucket(bucketKey)
	if rootBucket == nil {
		return errors.New("bucket does not exist")
	}

	// We'll now traverse the root bucket for all active swaps. The
	// primary key is the swap hash itself.
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

		// Get the updates bucket.
		updatesBucket := swapBucket.Bucket(updatesBucketKey)
		if updatesBucket == nil {
			return errors.New("updates bucket not found")
		}

		// Get list of all update ids.
		var ids [][]byte
		err := updatesBucket.ForEach(func(k, v []byte) error {
			ids = append(ids, k)
			return nil
		})
		if err != nil {
			return err
		}

		// Append three zeroed cost factors to all updates.
		var emptyCosts [3 * 8]byte
		for _, id := range ids {
			v := updatesBucket.Get(id)
			if v == nil {
				return errors.New("empty value")
			}
			v = append(v, emptyCosts[:]...)
			err := updatesBucket.Put(id, v)
			if err != nil {
				return err
			}
		}

		return nil
	})
}
