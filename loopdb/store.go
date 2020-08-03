package loopdb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/lntypes"
)

var (
	// dbFileName is the default file name of the client-side loop sub-swap
	// database.
	dbFileName = "loop.db"

	// loopOutBucketKey is a bucket that contains all out swaps that are
	// currently pending or completed. This bucket is keyed by the swaphash,
	// and leads to a nested sub-bucket that houses information for that
	// swap.
	//
	// maps: swapHash -> swapBucket
	loopOutBucketKey = []byte("uncharge-swaps")

	// loopInBucketKey is a bucket that contains all in swaps that are
	// currently pending or completed. This bucket is keyed by the swaphash,
	// and leads to a nested sub-bucket that houses information for that
	// swap.
	//
	// maps: swapHash -> swapBucket
	loopInBucketKey = []byte("loop-in")

	// updatesBucketKey is a bucket that contains all updates pertaining to
	// a swap. This is a sub-bucket of the swap bucket for a particular
	// swap. This list only ever grows.
	//
	// path: loopInBucket/loopOutBucket -> swapBucket[hash] -> updatesBucket
	//
	// maps: updateNumber -> time || state
	updatesBucketKey = []byte("updates")

	// basicStateKey contains the serialized basic swap state.
	basicStateKey = []byte{0}

	// htlcTxHashKey contains the confirmed htlc tx id.
	htlcTxHashKey = []byte{1}

	// contractKey is the key that stores the serialized swap contract. It
	// is nested within the sub-bucket for each active swap.
	//
	// path: loopInBucket/loopOutBucket -> swapBucket[hash] -> contractKey
	//
	// value: time || rawSwapState
	contractKey = []byte("contract")

	// labelKey is the key that stores an optional label for the swap. If
	// a swap was created before we started adding labels, or was created
	// without a label, this key will not be present.
	//
	// path: loopInBucket/loopOutBucket -> swapBucket[hash] -> labelKey
	//
	// value: string label
	labelKey = []byte("label")

	// outgoingChanSetKey is the key that stores a list of channel ids that
	// restrict the loop out swap payment.
	//
	// path: loopOutBucket -> swapBucket[hash] -> outgoingChanSetKey
	//
	// value: concatenation of uint64 channel ids
	outgoingChanSetKey = []byte("outgoing-chan-set")

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
		// Check if the meta bucket exists. If it exists, we consider
		// the database as initialized and assume the meta bucket
		// contains the db version.
		metaBucket := tx.Bucket(metaBucketKey)
		if metaBucket == nil {
			log.Infof("Initializing new database with version %v",
				latestDBVersion)

			// Set db version to the current version.
			err := setDBVersion(tx, latestDBVersion)
			if err != nil {
				return err
			}
		}

		// Try creating these buckets, because loop in was added without
		// bumping the db version number.
		_, err = tx.CreateBucketIfNotExists(loopOutBucketKey)
		if err != nil {
			return err
		}

		_, err = tx.CreateBucketIfNotExists(loopInBucketKey)
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
	err = syncVersions(bdb, chainParams)
	if err != nil {
		return nil, err
	}

	return &boltSwapStore{
		db:          bdb,
		chainParams: chainParams,
	}, nil
}

// FetchLoopOutSwaps returns all loop out swaps currently in the store.
//
// NOTE: Part of the loopdb.SwapStore interface.
func (s *boltSwapStore) FetchLoopOutSwaps() ([]*LoopOut, error) {
	var swaps []*LoopOut

	err := s.db.View(func(tx *bbolt.Tx) error {
		// First, we'll grab our main loop in bucket key.
		rootBucket := tx.Bucket(loopOutBucketKey)
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

			// Get our label for this swap, if it is present.
			contract.Label = getLabel(swapBucket)

			// Read the list of concatenated outgoing channel ids
			// that form the outgoing set.
			setBytes := swapBucket.Get(outgoingChanSetKey)
			if outgoingChanSetKey != nil {
				r := bytes.NewReader(setBytes)
			readLoop:
				for {
					var chanID uint64
					err := binary.Read(r, byteOrder, &chanID)
					switch {
					case err == io.EOF:
						break readLoop
					case err != nil:
						return err
					}

					contract.OutgoingChanSet = append(
						contract.OutgoingChanSet,
						chanID,
					)
				}
			}

			updates, err := deserializeUpdates(swapBucket)
			if err != nil {
				return err
			}

			loop := LoopOut{
				Loop: Loop{
					Events: updates,
				},
				Contract: contract,
			}

			loop.Hash, err = lntypes.MakeHash(swapHash)
			if err != nil {
				return err
			}

			swaps = append(swaps, &loop)

			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	return swaps, nil
}

// deserializeUpdates deserializes the list of swap updates that are stored as a
// key of the given bucket.
func deserializeUpdates(swapBucket *bbolt.Bucket) ([]*LoopEvent, error) {
	// Once we have the raw swap, we'll also need to decode
	// each of the past updates to the swap itself.
	stateBucket := swapBucket.Bucket(updatesBucketKey)
	if stateBucket == nil {
		return nil, errors.New("updates bucket not found")
	}

	// Deserialize and collect each swap update into our slice of swap
	// events.
	var updates []*LoopEvent
	err := stateBucket.ForEach(func(k, v []byte) error {
		updateBucket := stateBucket.Bucket(k)
		if updateBucket == nil {
			return fmt.Errorf("expected state sub-bucket for %x", k)
		}

		basicState := updateBucket.Get(basicStateKey)
		if basicState == nil {
			return errors.New("no basic state for update")
		}

		event, err := deserializeLoopEvent(basicState)
		if err != nil {
			return err
		}

		// Deserialize htlc tx hash if this updates contains one.
		htlcTxHashBytes := updateBucket.Get(htlcTxHashKey)
		if htlcTxHashBytes != nil {
			htlcTxHash, err := chainhash.NewHash(htlcTxHashBytes)
			if err != nil {
				return err
			}
			event.HtlcTxHash = htlcTxHash
		}

		updates = append(updates, event)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return updates, nil
}

// FetchLoopInSwaps returns all loop in swaps currently in the store.
//
// NOTE: Part of the loopdb.SwapStore interface.
func (s *boltSwapStore) FetchLoopInSwaps() ([]*LoopIn, error) {
	var swaps []*LoopIn

	err := s.db.View(func(tx *bbolt.Tx) error {
		// First, we'll grab our main loop in bucket key.
		rootBucket := tx.Bucket(loopInBucketKey)
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

			contract, err := deserializeLoopInContract(
				contractBytes,
			)
			if err != nil {
				return err
			}

			// Get our label for this swap, if it is present.
			contract.Label = getLabel(swapBucket)

			updates, err := deserializeUpdates(swapBucket)
			if err != nil {
				return err
			}

			loop := LoopIn{
				Loop: Loop{
					Events: updates,
				},
				Contract: contract,
			}

			loop.Hash, err = lntypes.MakeHash(swapHash)
			if err != nil {
				return err
			}

			swaps = append(swaps, &loop)

			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	return swaps, nil
}

// createLoopBucket creates the bucket for a particular swap.
func createLoopBucket(tx *bbolt.Tx, swapTypeKey []byte, hash lntypes.Hash) (
	*bbolt.Bucket, error) {

	// First, we'll grab the root bucket that houses all of our
	// swaps of this type.
	swapTypeBucket, err := tx.CreateBucketIfNotExists(swapTypeKey)
	if err != nil {
		return nil, err
	}

	// If the swap already exists, then we'll exit as we don't want
	// to override a swap.
	if swapTypeBucket.Get(hash[:]) != nil {
		return nil, fmt.Errorf("swap %v already exists", hash)
	}

	// From the swap type bucket, we'll make a new sub swap bucket using the
	// swap hash to store the individual swap.
	return swapTypeBucket.CreateBucket(hash[:])
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
		// Create the swap bucket.
		swapBucket, err := createLoopBucket(tx, loopOutBucketKey, hash)
		if err != nil {
			return err
		}

		// With the swap bucket created, we'll store the swap itself.
		contractBytes, err := serializeLoopOutContract(swap)
		if err != nil {
			return err
		}

		err = swapBucket.Put(contractKey, contractBytes)
		if err != nil {
			return err
		}

		if err := putLabel(swapBucket, swap.Label); err != nil {
			return err
		}

		// Write the outgoing channel set.
		var b bytes.Buffer
		for _, chanID := range swap.OutgoingChanSet {
			err := binary.Write(&b, byteOrder, chanID)
			if err != nil {
				return err
			}
		}
		err = swapBucket.Put(outgoingChanSetKey, b.Bytes())
		if err != nil {
			return err
		}

		// Write label to disk if we have one.
		if err := putLabel(swapBucket, swap.Label); err != nil {
			return err
		}

		// Finally, we'll create an empty updates bucket for this swap
		// to track any future updates to the swap itself.
		_, err = swapBucket.CreateBucket(updatesBucketKey)
		return err
	})
}

// CreateLoopIn adds an initiated swap to the store.
//
// NOTE: Part of the loopdb.SwapStore interface.
func (s *boltSwapStore) CreateLoopIn(hash lntypes.Hash,
	swap *LoopInContract) error {

	// If the hash doesn't match the pre-image, then this is an invalid
	// swap so we'll bail out early.
	if hash != swap.Preimage.Hash() {
		return errors.New("hash and preimage do not match")
	}

	// Otherwise, we'll create a new swap within the database.
	return s.db.Update(func(tx *bbolt.Tx) error {
		// Create the swap bucket.
		swapBucket, err := createLoopBucket(tx, loopInBucketKey, hash)
		if err != nil {
			return err
		}

		// With the swap bucket created, we'll store the swap itself.
		contractBytes, err := serializeLoopInContract(swap)
		if err != nil {
			return err
		}

		err = swapBucket.Put(contractKey, contractBytes)
		if err != nil {
			return err
		}

		// Write label to disk if we have one.
		if err := putLabel(swapBucket, swap.Label); err != nil {
			return err
		}

		// Finally, we'll create an empty updates bucket for this swap
		// to track any future updates to the swap itself.
		_, err = swapBucket.CreateBucket(updatesBucketKey)
		return err
	})
}

// updateLoop saves a new swap state transition to the store. It takes in a
// bucket key so that this function can be used for both in and out swaps.
func (s *boltSwapStore) updateLoop(bucketKey []byte, hash lntypes.Hash,
	time time.Time, state SwapStateData) error {

	return s.db.Update(func(tx *bbolt.Tx) error {
		// Starting from the root bucket, we'll traverse the bucket
		// hierarchy all the way down to the swap bucket, and the
		// update sub-bucket within that.
		rootBucket := tx.Bucket(bucketKey)
		if rootBucket == nil {
			return errors.New("bucket does not exist")
		}
		swapBucket := rootBucket.Bucket(hash[:])
		if swapBucket == nil {
			return errors.New("swap not found")
		}
		updatesBucket := swapBucket.Bucket(updatesBucketKey)
		if updatesBucket == nil {
			return errors.New("udpate bucket not found")
		}

		// Each update for this swap will get a new monotonically
		// increasing ID number that we'll obtain now.
		id, err := updatesBucket.NextSequence()
		if err != nil {
			return err
		}

		nextUpdateBucket, err := updatesBucket.CreateBucket(itob(id))
		if err != nil {
			return fmt.Errorf("cannot create update bucket")
		}

		// With the ID obtained, we'll write out this new update value.
		updateValue, err := serializeLoopEvent(time, state)
		if err != nil {
			return err
		}

		err = nextUpdateBucket.Put(basicStateKey, updateValue)
		if err != nil {
			return err
		}

		// Write the htlc tx hash if available.
		if state.HtlcTxHash != nil {
			err := nextUpdateBucket.Put(
				htlcTxHashKey, state.HtlcTxHash[:],
			)
			if err != nil {
				return err
			}
		}

		return nil
	})
}

// UpdateLoopOut stores a swap update. This appends to the event log for
// a particular swap as it goes through the various stages in its lifetime.
//
// NOTE: Part of the loopdb.SwapStore interface.
func (s *boltSwapStore) UpdateLoopOut(hash lntypes.Hash, time time.Time,
	state SwapStateData) error {

	return s.updateLoop(loopOutBucketKey, hash, time, state)
}

// UpdateLoopIn stores a swap update. This appends to the event log for
// a particular swap as it goes through the various stages in its lifetime.
//
// NOTE: Part of the loopdb.SwapStore interface.
func (s *boltSwapStore) UpdateLoopIn(hash lntypes.Hash, time time.Time,
	state SwapStateData) error {

	return s.updateLoop(loopInBucketKey, hash, time, state)
}

// Close closes the underlying database.
//
// NOTE: Part of the loopdb.SwapStore interface.
func (s *boltSwapStore) Close() error {
	return s.db.Close()
}
