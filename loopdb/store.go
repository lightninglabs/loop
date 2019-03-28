package loopdb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/lntypes"
)

var (
	// dbFileName is the default file name of the client-side loop sub-swap
	// database.
	dbFileName = "loop.db"

	// unchargeSwapsBucketKey is a bucket that contains all swaps that are
	// currently pending or completed. This bucket is keyed by the
	// swaphash, and leads to a nested sub-bucket that houses information
	// for that swap.
	//
	// maps: swapHash -> swapBucket
	unchargeSwapsBucketKey = []byte("uncharge-swaps")

	// unchargeUpdatesBucketKey is a bucket that contains all updates
	// pertaining to a swap. This is a sub-bucket of the swap bucket for a
	// particular swap. This list only ever grows.
	//
	// path: unchargeUpdatesBucket -> swapBucket[hash] -> updateBucket
	//
	// maps: updateNumber -> time || state
	updatesBucketKey = []byte("updates")

	// contractKey is the key that stores the serialized swap contract. It
	// is nested within the sub-bucket for each active swap.
	//
	// path: unchargeUpdatesBucket -> swapBucket[hash]
	//
	// value: time || rawSwapState
	contractKey = []byte("contract")

	byteOrder = binary.BigEndian

	keyLength = 33
)

// fileExists returns true if the file exists, and false otherwise.
func fileExists(path string) bool {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}

	return true
}

// boltSwapStore stores swap data in boltdb.
type boltSwapStore struct {
	db          *bbolt.DB
	chainParams *chaincfg.Params
}

// A compile-time flag to ensure that boltSwapStore implements the SwapStore
// interface.
var _ = (*boltSwapStore)(nil)

// NewBoltSwapStore creates a new client swap store.
func NewBoltSwapStore(dbPath string, chainParams *chaincfg.Params) (
	*boltSwapStore, error) {

	// If the target path for the swap store doesn't exist, then we'll
	// create it now before we proceed.
	if !fileExists(dbPath) {
		if err := os.MkdirAll(dbPath, 0700); err != nil {
			return nil, err
		}
	}

	// Now that we know that path exists, we'll open up bolt, which
	// implements our default swap store.
	path := filepath.Join(dbPath, dbFileName)
	bdb, err := bbolt.Open(path, 0600, nil)
	if err != nil {
		return nil, err
	}

	// We'll create all the buckets we need if this is the first time we're
	// starting up. If they already exist, then these calls will be noops.
	err = bdb.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(unchargeSwapsBucketKey)
		if err != nil {
			return err
		}
		_, err = tx.CreateBucketIfNotExists(updatesBucketKey)
		if err != nil {
			return err
		}
		_, err = tx.CreateBucketIfNotExists(metaBucket)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Finally, before we start, we'll sync the DB versions to pick up any
	// possible DB migrations.
	err = syncVersions(bdb)
	if err != nil {
		return nil, err
	}

	return &boltSwapStore{
		db:          bdb,
		chainParams: chainParams,
	}, nil
}

// FetchLoopOutSwaps returns all swaps currently in the store.
//
// NOTE: Part of the loopdb.SwapStore interface.
func (s *boltSwapStore) FetchLoopOutSwaps() ([]*LoopOut, error) {
	var swaps []*LoopOut

	err := s.db.View(func(tx *bbolt.Tx) error {
		// First, we'll grab our main loop out swap bucket key.
		rootBucket := tx.Bucket(unchargeSwapsBucketKey)
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

			// With the main swap bucket obtained, we'll grab the
			// raw swap contract bytes and decode it.
			contractBytes := swapBucket.Get(contractKey)
			if contractBytes == nil {
				return errors.New("contract not found")
			}
			contract, err := deserializeLoopOutContract(
				contractBytes, s.chainParams,
			)
			if err != nil {
				return err
			}

			// Once we have the raw swap, we'll also need to decode
			// each of the past updates to the swap itself.
			stateBucket := swapBucket.Bucket(updatesBucketKey)
			if stateBucket == nil {
				return errors.New("updates bucket not found")
			}

			// De serialize and collect each swap update into our
			// slice of swap events.
			var updates []*LoopOutEvent
			err = stateBucket.ForEach(func(k, v []byte) error {
				event, err := deserializeLoopOutEvent(v)
				if err != nil {
					return err
				}

				updates = append(updates, event)
				return nil
			})
			if err != nil {
				return err
			}

			var hash lntypes.Hash
			copy(hash[:], swapHash)

			swap := LoopOut{
				Contract: contract,
				Hash:     hash,
				Events:   updates,
			}

			swaps = append(swaps, &swap)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	return swaps, nil
}

// CreateLoopOut adds an initiated swap to the store.
//
// NOTE: Part of the loopdb.SwapStore interface.
func (s *boltSwapStore) CreateLoopOut(hash lntypes.Hash,
	swap *LoopOutContract) error {

	// If the hash doesn't match the pre-image, then this is an invalid
	// swap so we'll bail out early.
	if hash != swap.Preimage.Hash() {
		return errors.New("hash and preimage do not match")
	}

	// Otherwise, we'll create a new swap within the database.
	return s.db.Update(func(tx *bbolt.Tx) error {
		// First, we'll grab the root bucket that houses all of our
		// main swaps.
		rootBucket, err := tx.CreateBucketIfNotExists(
			unchargeSwapsBucketKey,
		)
		if err != nil {
			return err
		}

		// If the swap already exists, then we'll exit as we don't want
		// to override a swap.
		if rootBucket.Get(hash[:]) != nil {
			return fmt.Errorf("swap %v already exists",
				swap.Preimage)
		}

		// From the root bucket, we'll make a new sub swap bucket using
		// the swap hash.
		swapBucket, err := rootBucket.CreateBucket(hash[:])
		if err != nil {
			return err
		}

		// With out swap bucket created, we'll serialize and store the
		// swap itself.
		contract, err := serializeLoopOutContract(swap)
		if err != nil {
			return err
		}
		if err := swapBucket.Put(contractKey, contract); err != nil {
			return err
		}

		// Finally, we'll create an empty updates bucket for this swap
		// to track any future updates to the swap itself.
		_, err = swapBucket.CreateBucket(updatesBucketKey)
		return err
	})
}

// UpdateLoopOut stores a swap updateLoopOut. This appends to the event log for
// a particular swap as it goes through the various stages in its lifetime.
//
// NOTE: Part of the loopdb.SwapStore interface.
func (s *boltSwapStore) UpdateLoopOut(hash lntypes.Hash, time time.Time,
	state SwapState) error {

	return s.db.Update(func(tx *bbolt.Tx) error {
		// Starting from the root bucket, we'll traverse the bucket
		// hierarchy all the way down to the swap bucket, and the
		// update sub-bucket within that.
		rootBucket := tx.Bucket(unchargeSwapsBucketKey)
		if rootBucket == nil {
			return errors.New("bucket does not exist")
		}
		swapBucket := rootBucket.Bucket(hash[:])
		if swapBucket == nil {
			return errors.New("swap not found")
		}
		updateBucket := swapBucket.Bucket(updatesBucketKey)
		if updateBucket == nil {
			return errors.New("udpate bucket not found")
		}

		// Each update for this swap will get a new monotonically
		// increasing ID number that we'll obtain now.
		id, err := updateBucket.NextSequence()
		if err != nil {
			return err
		}

		// With the ID obtained, we'll write out this new update value.
		updateValue, err := serializeLoopOutEvent(time, state)
		if err != nil {
			return err
		}
		return updateBucket.Put(itob(id), updateValue)
	})
}

// Close closes the underlying database.
//
// NOTE: Part of the loopdb.SwapStore interface.
func (s *boltSwapStore) Close() error {
	return s.db.Close()
}
