package loopdb

import (
	"bytes"
	"context"
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

	// protocolVersionKey is used to optionally store the protocol version
	// for the serialized swap contract. It is nested within the sub-bucket
	// for each active swap.
	//
	// path: loopInBucket/loopOutBucket -> swapBucket[hash] -> protocolVersionKey
	//
	// value: protocol version as specified in server.proto
	protocolVersionKey = []byte("protocol-version")

	// outgoingChanSetKey is the key that stores a list of channel ids that
	// restrict the loop out swap payment.
	//
	// path: loopOutBucket -> swapBucket[hash] -> outgoingChanSetKey
	//
	// value: concatenation of uint64 channel ids
	outgoingChanSetKey = []byte("outgoing-chan-set")

	// confirmationsKey is the key that stores the number of confirmations
	// that were requested for a loop out swap.
	//
	// path: loopOutBucket -> swapBucket[hash] -> confirmationsKey
	//
	// value: uint32 confirmation value
	confirmationsKey = []byte("confirmations")

	// liquidtyBucket is a root bucket used to save liquidity manager
	// related info.
	liquidityBucket = []byte("liquidity")

	// liquidtyParamsKey specifies the key used to store the liquidity
	// parameters.
	liquidtyParamsKey = []byte("params")

	// keyLocatorKey is the key that stores the receiver key's locator info
	// for loop outs or the sender key's locator info for loop ins. This is
	// required for MuSig2 swaps. Only serialized/deserialized for swaps
	// that have protocol version >= ProtocolVersionHtlcV3.
	//
	// path: loopInBucket/loopOutBucket -> swapBucket[hash] -> keyLocatorKey
	//
	// value: concatenation of uint32 values [family, index].
	keyLocatorKey = []byte("keylocator")

	// senderInternalPubKeyKey is the key that stores the sender's internal
	// public key which used when constructing the swap HTLC.
	//
	// path: loopInBucket/loopOutBucket -> swapBucket[hash]
	//                                      -> senderInternalPubKeyKey
	// value: serialized public key.
	senderInternalPubKeyKey = []byte("sender-internal-pubkey")

	// receiverInternalPubKeyKey is the key that stores the receiver's
	// internal public key which is used when constructing the swap HTLC.
	//
	// path: loopInBucket/loopOutBucket -> swapBucket[hash]
	//                                     -> receiverInternalPubKeyKey
	// value: serialized public key.
	receiverInternalPubKeyKey = []byte("receiver-internal-pubkey")

	byteOrder = binary.BigEndian

	// keyLength is the length of a serialized public key.
	keyLength = 33

	// errInvalidKey is returned when a serialized key is not the expected
	// length.
	errInvalidKey = fmt.Errorf("invalid serialized key")
)

const (
	// DefaultLoopOutHtlcConfirmations is the default number of
	// confirmations we set for a loop out htlc.
	DefaultLoopOutHtlcConfirmations uint32 = 1

	// DefaultLoopDBTimeout is the default maximum time we wait for the
	// Loop bbolt database to be opened. If the database is already opened
	// by another process, the unique lock cannot be obtained. With the
	// timeout we error out after the given time instead of just blocking
	// for forever.
	DefaultLoopDBTimeout = 5 * time.Second
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
	bdb, err := bbolt.Open(path, 0600, &bbolt.Options{
		Timeout: DefaultLoopDBTimeout,
	})
	if err == bbolt.ErrTimeout {
		return nil, fmt.Errorf("%w: couldn't obtain exclusive lock on "+
			"%s, timed out after %v", bbolt.ErrTimeout, path,
			DefaultLoopDBTimeout)
	}
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

		// Create liquidity manager's bucket.
		_, err = tx.CreateBucketIfNotExists(liquidityBucket)
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

// marshalHtlcKeys marshals the HTLC keys of the swap contract into the swap
// bucket.
func marshalHtlcKeys(swapBucket *bbolt.Bucket, contract *SwapContract) error {
	var err error

	// Store the key locator for swaps that use taproot HTLCs.
	if contract.ProtocolVersion >= ProtocolVersionHtlcV3 {
		keyLocator, err := MarshalKeyLocator(
			contract.HtlcKeys.ClientScriptKeyLocator,
		)
		if err != nil {
			return err
		}

		err = swapBucket.Put(keyLocatorKey, keyLocator)
		if err != nil {
			return err
		}
	}

	// Store the internal keys for MuSig2 swaps.
	if contract.ProtocolVersion >= ProtocolVersionMuSig2 {
		// Internal pubkeys are always filled.
		err = swapBucket.Put(
			senderInternalPubKeyKey,
			contract.HtlcKeys.SenderInternalPubKey[:],
		)
		if err != nil {
			return err
		}

		err = swapBucket.Put(
			receiverInternalPubKeyKey,
			contract.HtlcKeys.ReceiverInternalPubKey[:],
		)
		if err != nil {
			return err
		}
	}

	return nil
}

// unmarshalHtlcKeys deserializes the htlc keys from the swap bucket.
func unmarshalHtlcKeys(swapBucket *bbolt.Bucket, contract *SwapContract) error {
	var err error

	// HTLC V3 contracts have the client script key locator stored.
	if contract.ProtocolVersion >= ProtocolVersionHtlcV3 &&
		ProtocolVersionUnrecorded > contract.ProtocolVersion {

		contract.HtlcKeys.ClientScriptKeyLocator, err =
			UnmarshalKeyLocator(
				swapBucket.Get(keyLocatorKey),
			)
		if err != nil {
			return err
		}

		// Default the internal scriptkeys to the sender and receiver
		// keys.
		contract.HtlcKeys.SenderInternalPubKey =
			contract.HtlcKeys.SenderScriptKey
		contract.HtlcKeys.ReceiverInternalPubKey =
			contract.HtlcKeys.ReceiverScriptKey
	}

	// MuSig2 contracts have the internal keys stored too.
	if contract.ProtocolVersion >= ProtocolVersionMuSig2 &&
		ProtocolVersionUnrecorded > contract.ProtocolVersion {

		// The pubkeys used for the joint HTLC internal key are always
		// present.
		key := swapBucket.Get(senderInternalPubKeyKey)
		if len(key) != keyLength {
			return errInvalidKey
		}
		copy(contract.HtlcKeys.SenderInternalPubKey[:], key)

		key = swapBucket.Get(receiverInternalPubKeyKey)
		if len(key) != keyLength {
			return errInvalidKey
		}
		copy(contract.HtlcKeys.ReceiverInternalPubKey[:], key)
	}

	return nil
}

// FetchLoopOutSwaps returns all loop out swaps currently in the store.
//
// NOTE: Part of the loopdb.SwapStore interface.
func (s *boltSwapStore) FetchLoopOutSwaps(ctx context.Context) ([]*LoopOut,
	error) {

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

			loop, err := s.fetchLoopOutSwap(rootBucket, swapHash)
			if err != nil {
				return err
			}

			swaps = append(swaps, loop)

			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	return swaps, nil
}

// FetchLoopOutSwap returns the loop out swap with the given hash.
//
// NOTE: Part of the loopdb.SwapStore interface.
func (s *boltSwapStore) FetchLoopOutSwap(ctx context.Context,
	hash lntypes.Hash) (*LoopOut, error) {

	var swap *LoopOut

	err := s.db.View(func(tx *bbolt.Tx) error {
		// First, we'll grab our main loop out bucket key.
		rootBucket := tx.Bucket(loopOutBucketKey)
		if rootBucket == nil {
			return errors.New("bucket does not exist")
		}

		loop, err := s.fetchLoopOutSwap(rootBucket, hash[:])
		if err != nil {
			return err
		}

		swap = loop

		return nil
	})
	if err != nil {
		return nil, err
	}

	return swap, nil
}

// FetchLoopInSwaps returns all loop in swaps currently in the store.
//
// NOTE: Part of the loopdb.SwapStore interface.
func (s *boltSwapStore) FetchLoopInSwaps(ctx context.Context) ([]*LoopIn,
	error) {

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

			loop, err := s.fetchLoopInSwap(rootBucket, swapHash)
			if err != nil {
				return err
			}

			swaps = append(swaps, loop)

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
func (s *boltSwapStore) CreateLoopOut(ctx context.Context, hash lntypes.Hash,
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

		// Write our confirmation target under its own key.
		var buf bytes.Buffer
		err = binary.Write(&buf, byteOrder, swap.HtlcConfirmations)
		if err != nil {
			return err
		}

		err = swapBucket.Put(confirmationsKey, buf.Bytes())
		if err != nil {
			return err
		}

		// Store the current protocol version.
		err = swapBucket.Put(protocolVersionKey,
			MarshalProtocolVersion(swap.ProtocolVersion),
		)
		if err != nil {
			return err
		}

		// Store the htlc keys and server key locator.
		err = marshalHtlcKeys(swapBucket, &swap.SwapContract)
		if err != nil {
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
func (s *boltSwapStore) CreateLoopIn(ctx context.Context, hash lntypes.Hash,
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

		// Store the current protocol version.
		err = swapBucket.Put(protocolVersionKey,
			MarshalProtocolVersion(swap.ProtocolVersion),
		)
		if err != nil {
			return err
		}

		// Write label to disk if we have one.
		if err := putLabel(swapBucket, swap.Label); err != nil {
			return err
		}

		// Store the htlc keys and server key locator.
		err = marshalHtlcKeys(swapBucket, &swap.SwapContract)
		if err != nil {
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
func (s *boltSwapStore) UpdateLoopOut(ctx context.Context,
	hash lntypes.Hash, time time.Time, state SwapStateData) error {

	return s.updateLoop(loopOutBucketKey, hash, time, state)
}

// UpdateLoopIn stores a swap update. This appends to the event log for
// a particular swap as it goes through the various stages in its lifetime.
//
// NOTE: Part of the loopdb.SwapStore interface.
func (s *boltSwapStore) UpdateLoopIn(ctx context.Context, hash lntypes.Hash,
	time time.Time, state SwapStateData) error {

	return s.updateLoop(loopInBucketKey, hash, time, state)
}

// Close closes the underlying database.
//
// NOTE: Part of the loopdb.SwapStore interface.
func (s *boltSwapStore) Close() error {
	return s.db.Close()
}

// PutLiquidityParams writes the serialized `manager.Parameters` bytes into the
// bucket.
//
// NOTE: it's the caller's responsibility to encode the param. Atm, it's
// encoding using the proto package's `Marshal` method.
func (s *boltSwapStore) PutLiquidityParams(ctx context.Context,
	params []byte) error {

	return s.db.Update(func(tx *bbolt.Tx) error {
		// Read the root bucket.
		rootBucket := tx.Bucket(liquidityBucket)
		if rootBucket == nil {
			return errors.New("liquidity bucket does not exist")
		}
		return rootBucket.Put(liquidtyParamsKey, params)
	})
}

// FetchLiquidityParams reads the serialized `manager.Parameters` bytes from
// the bucket.
//
// NOTE: it's the caller's responsibility to decode the param. Atm, it's
// decoding using the proto package's `Unmarshal` method.
func (s *boltSwapStore) FetchLiquidityParams(ctx context.Context) ([]byte,
	error) {

	var params []byte

	err := s.db.View(func(tx *bbolt.Tx) error {
		// Read the root bucket.
		rootBucket := tx.Bucket(liquidityBucket)
		if rootBucket == nil {
			return errors.New("liquidity bucket does not exist")
		}

		params = rootBucket.Get(liquidtyParamsKey)
		return nil
	})

	return params, err
}

// fetchUpdates deserializes the list of swap updates that are stored as a
// key of the given bucket.
func fetchUpdates(swapBucket *bbolt.Bucket) ([]*LoopEvent, error) {
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

// fetchLoopOutSwap fetches and deserializes the raw swap bytes into a LoopOut
// struct.
func (s *boltSwapStore) fetchLoopOutSwap(rootBucket *bbolt.Bucket,
	swapHash []byte) (*LoopOut, error) {

	// From the root bucket, we'll grab the next swap
	// bucket for this swap from its swaphash.
	swapBucket := rootBucket.Bucket(swapHash)
	if swapBucket == nil {
		return nil, fmt.Errorf("swap bucket %x not found",
			swapHash)
	}

	hash, err := lntypes.MakeHash(swapHash)
	if err != nil {
		return nil, err
	}

	// With the main swap bucket obtained, we'll grab the
	// raw swap contract bytes and decode it.
	contractBytes := swapBucket.Get(contractKey)
	if contractBytes == nil {
		return nil, errors.New("contract not found")
	}

	contract, err := deserializeLoopOutContract(
		contractBytes, s.chainParams,
	)
	if err != nil {
		return nil, err
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
				return nil, err
			}

			contract.OutgoingChanSet = append(
				contract.OutgoingChanSet,
				chanID,
			)
		}
	}

	// Set our default number of confirmations for the swap.
	contract.HtlcConfirmations = DefaultLoopOutHtlcConfirmations

	// If we have the number of confirmations stored for
	// this swap, we overwrite our default with the stored
	// value.
	confBytes := swapBucket.Get(confirmationsKey)
	if confBytes != nil {
		r := bytes.NewReader(confBytes)
		err := binary.Read(
			r, byteOrder, &contract.HtlcConfirmations,
		)
		if err != nil {
			return nil, err
		}
	}

	updates, err := fetchUpdates(swapBucket)
	if err != nil {
		return nil, err
	}

	// Try to unmarshal the protocol version for the swap.
	// If the protocol version is not stored (which is
	// the case for old clients), we'll assume the
	// ProtocolVersionUnrecorded instead.
	contract.ProtocolVersion, err =
		UnmarshalProtocolVersion(
			swapBucket.Get(protocolVersionKey),
		)
	if err != nil {
		return nil, err
	}

	// Unmarshal HTLC keys if the contract is recent.
	err = unmarshalHtlcKeys(
		swapBucket, &contract.SwapContract,
	)
	if err != nil {
		return nil, err
	}

	loop := LoopOut{
		Loop: Loop{
			Events: updates,
		},
		Contract: contract,
	}

	loop.Hash, err = lntypes.MakeHash(hash[:])
	if err != nil {
		return nil, err
	}

	return &loop, nil
}

// fetchLoopInSwap fetches and deserializes the raw swap bytes into a LoopIn
// struct.
func (s *boltSwapStore) fetchLoopInSwap(rootBucket *bbolt.Bucket,
	swapHash []byte) (*LoopIn, error) {

	// From the root bucket, we'll grab the next swap
	// bucket for this swap from its swaphash.
	swapBucket := rootBucket.Bucket(swapHash)
	if swapBucket == nil {
		return nil, fmt.Errorf("swap bucket %x not found",
			swapHash)
	}

	hash, err := lntypes.MakeHash(swapHash)
	if err != nil {
		return nil, err
	}

	// With the main swap bucket obtained, we'll grab the
	// raw swap contract bytes and decode it.
	contractBytes := swapBucket.Get(contractKey)
	if contractBytes == nil {
		return nil, errors.New("contract not found")
	}

	contract, err := deserializeLoopInContract(
		contractBytes,
	)
	if err != nil {
		return nil, err
	}

	// Get our label for this swap, if it is present.
	contract.Label = getLabel(swapBucket)

	updates, err := fetchUpdates(swapBucket)
	if err != nil {
		return nil, err
	}

	// Try to unmarshal the protocol version for the swap.
	// If the protocol version is not stored (which is
	// the case for old clients), we'll assume the
	// ProtocolVersionUnrecorded instead.
	contract.ProtocolVersion, err =
		UnmarshalProtocolVersion(
			swapBucket.Get(protocolVersionKey),
		)
	if err != nil {
		return nil, err
	}

	// Unmarshal HTLC keys if the contract is recent.
	err = unmarshalHtlcKeys(
		swapBucket, &contract.SwapContract,
	)
	if err != nil {
		return nil, err
	}

	loop := LoopIn{
		Loop: Loop{
			Events: updates,
		},
		Contract: contract,
	}

	loop.Hash = hash

	return &loop, nil
}

// BatchCreateLoopOut creates a batch of swaps to the store.
func (b *boltSwapStore) BatchCreateLoopOut(ctx context.Context,
	swaps map[lntypes.Hash]*LoopOutContract) error {

	return errors.New("not implemented")
}

// BatchCreateLoopIn creates a batch of loop in swaps to the store.
func (b *boltSwapStore) BatchCreateLoopIn(ctx context.Context,
	swaps map[lntypes.Hash]*LoopInContract) error {

	return errors.New("not implemented")
}

// BatchInsertUpdate inserts batch of swap updates to the store.
func (b *boltSwapStore) BatchInsertUpdate(ctx context.Context,
	updateData map[lntypes.Hash][]BatchInsertUpdateData) error {

	return errors.New("not implemented")
}
