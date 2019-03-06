package client

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/coreos/bbolt"
	"github.com/lightninglabs/loop/utils"
	"github.com/lightningnetwork/lnd/lntypes"
)

var (
	dbFileName = "swapclient.db"

	// unchargeSwapsBucketKey is a bucket that contains all swaps that are
	// currently pending or completed.
	//
	// maps: swap_hash -> UnchargeContract
	unchargeSwapsBucketKey = []byte("uncharge-swaps")

	// unchargeUpdatesBucketKey is a bucket that contains all updates
	// pertaining to a swap. This list only ever grows.
	//
	// maps: update_nr -> time | state
	updatesBucketKey = []byte("updates")

	// contractKey is the key that stores the serialized swap contract.
	contractKey = []byte("contract")

	byteOrder = binary.BigEndian

	keyLength = 33
)

// boltSwapClientStore stores swap data in boltdb.
type boltSwapClientStore struct {
	db *bbolt.DB
}

// newBoltSwapClientStore creates a new client swap store.
func newBoltSwapClientStore(dbPath string) (*boltSwapClientStore, error) {
	if !utils.FileExists(dbPath) {
		if err := os.MkdirAll(dbPath, 0700); err != nil {
			return nil, err
		}
	}
	path := filepath.Join(dbPath, dbFileName)
	bdb, err := bbolt.Open(path, 0600, nil)
	if err != nil {
		return nil, err
	}

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

	err = syncVersions(bdb)
	if err != nil {
		return nil, err
	}

	return &boltSwapClientStore{
		db: bdb,
	}, nil
}

// getUnchargeSwaps returns all swaps currently in the store.
func (s *boltSwapClientStore) getUnchargeSwaps() ([]*PersistentUncharge, error) {
	var swaps []*PersistentUncharge
	err := s.db.View(func(tx *bbolt.Tx) error {

		bucket := tx.Bucket(unchargeSwapsBucketKey)
		if bucket == nil {
			return errors.New("bucket does not exist")
		}

		err := bucket.ForEach(func(k, _ []byte) error {
			swapBucket := bucket.Bucket(k)
			if swapBucket == nil {
				return fmt.Errorf("swap bucket %x not found",
					k)
			}

			contractBytes := swapBucket.Get(contractKey)
			if contractBytes == nil {
				return errors.New("contract not found")
			}

			contract, err := deserializeUnchargeContract(
				contractBytes,
			)
			if err != nil {
				return err
			}

			stateBucket := swapBucket.Bucket(updatesBucketKey)
			if stateBucket == nil {
				return errors.New("updates bucket not found")
			}
			var updates []*PersistentUnchargeEvent
			err = stateBucket.ForEach(func(k, v []byte) error {
				event, err := deserializeUnchargeUpdate(v)
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
			copy(hash[:], k)

			swap := PersistentUncharge{
				Contract: contract,
				Hash:     hash,
				Events:   updates,
			}

			swaps = append(swaps, &swap)
			return nil
		})
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return swaps, nil
}

// createUncharge adds an initiated swap to the store.
func (s *boltSwapClientStore) createUncharge(hash lntypes.Hash,
	swap *UnchargeContract) error {

	if hash != swap.Preimage.Hash() {
		return errors.New("hash and preimage do not match")
	}

	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists(unchargeSwapsBucketKey)
		if err != nil {
			return err
		}

		if bucket.Get(hash[:]) != nil {
			return fmt.Errorf("swap %v already exists", swap.Preimage)
		}

		// Create bucket for swap.
		swapBucket, err := bucket.CreateBucket(hash[:])
		if err != nil {
			return err
		}

		contract, err := serializeUnchargeContract(swap)
		if err != nil {
			return err
		}

		// Store contact.
		if err := swapBucket.Put(contractKey, contract); err != nil {
			return err
		}

		// Create empty updates bucket.
		_, err = swapBucket.CreateBucket(updatesBucketKey)
		return err
	})
}

// updateUncharge stores a swap updateUncharge.
func (s *boltSwapClientStore) updateUncharge(hash lntypes.Hash, time time.Time,
	state SwapState) error {

	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(unchargeSwapsBucketKey)
		if bucket == nil {
			return errors.New("bucket does not exist")
		}

		swapBucket := bucket.Bucket(hash[:])
		if swapBucket == nil {
			return errors.New("swap not found")
		}

		updateBucket := swapBucket.Bucket(updatesBucketKey)
		if updateBucket == nil {
			return errors.New("udpate bucket not found")
		}

		id, err := updateBucket.NextSequence()
		if err != nil {
			return err
		}

		updateValue, err := serializeUnchargeUpdate(time, state)
		if err != nil {
			return err
		}

		return updateBucket.Put(itob(id), updateValue)
	})
}

// Close closes the underlying bolt db.
func (s *boltSwapClientStore) close() error {
	return s.db.Close()
}

func deserializeUnchargeContract(value []byte) (*UnchargeContract, error) {
	r := bytes.NewReader(value)

	contract, err := deserializeContract(r)
	if err != nil {
		return nil, err
	}

	swap := UnchargeContract{
		SwapContract: *contract,
	}

	addr, err := wire.ReadVarString(r, 0)
	if err != nil {
		return nil, err
	}
	swap.DestAddr, err = btcutil.DecodeAddress(addr, nil)
	if err != nil {
		return nil, err
	}

	swap.SwapInvoice, err = wire.ReadVarString(r, 0)
	if err != nil {
		return nil, err
	}

	if err := binary.Read(r, byteOrder, &swap.SweepConfTarget); err != nil {
		return nil, err
	}

	if err := binary.Read(r, byteOrder, &swap.MaxSwapRoutingFee); err != nil {
		return nil, err
	}

	var unchargeChannel uint64
	if err := binary.Read(r, byteOrder, &unchargeChannel); err != nil {
		return nil, err
	}
	if unchargeChannel != 0 {
		swap.UnchargeChannel = &unchargeChannel
	}

	return &swap, nil
}

func serializeUnchargeContract(swap *UnchargeContract) (
	[]byte, error) {

	var b bytes.Buffer

	serializeContract(&swap.SwapContract, &b)

	addr := swap.DestAddr.String()
	if err := wire.WriteVarString(&b, 0, addr); err != nil {
		return nil, err
	}

	if err := wire.WriteVarString(&b, 0, swap.SwapInvoice); err != nil {
		return nil, err
	}

	if err := binary.Write(&b, byteOrder, swap.SweepConfTarget); err != nil {
		return nil, err
	}

	if err := binary.Write(&b, byteOrder, swap.MaxSwapRoutingFee); err != nil {
		return nil, err
	}

	var unchargeChannel uint64
	if swap.UnchargeChannel != nil {
		unchargeChannel = *swap.UnchargeChannel
	}
	if err := binary.Write(&b, byteOrder, unchargeChannel); err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}

func deserializeContract(r io.Reader) (*SwapContract, error) {
	swap := SwapContract{}
	var err error
	var unixNano int64
	if err := binary.Read(r, byteOrder, &unixNano); err != nil {
		return nil, err
	}
	swap.InitiationTime = time.Unix(0, unixNano)

	if err := binary.Read(r, byteOrder, &swap.Preimage); err != nil {
		return nil, err
	}

	binary.Read(r, byteOrder, &swap.AmountRequested)

	swap.PrepayInvoice, err = wire.ReadVarString(r, 0)
	if err != nil {
		return nil, err
	}

	n, err := r.Read(swap.SenderKey[:])
	if err != nil {
		return nil, err
	}
	if n != keyLength {
		return nil, fmt.Errorf("sender key has invalid length")
	}

	n, err = r.Read(swap.ReceiverKey[:])
	if err != nil {
		return nil, err
	}
	if n != keyLength {
		return nil, fmt.Errorf("receiver key has invalid length")
	}

	if err := binary.Read(r, byteOrder, &swap.CltvExpiry); err != nil {
		return nil, err
	}
	if err := binary.Read(r, byteOrder, &swap.MaxMinerFee); err != nil {
		return nil, err
	}

	if err := binary.Read(r, byteOrder, &swap.MaxSwapFee); err != nil {
		return nil, err
	}

	if err := binary.Read(r, byteOrder, &swap.MaxPrepayRoutingFee); err != nil {
		return nil, err
	}
	if err := binary.Read(r, byteOrder, &swap.InitiationHeight); err != nil {
		return nil, err
	}

	return &swap, nil
}

func serializeContract(swap *SwapContract, b *bytes.Buffer) error {
	if err := binary.Write(b, byteOrder, swap.InitiationTime.UnixNano()); err != nil {
		return err
	}

	if err := binary.Write(b, byteOrder, swap.Preimage); err != nil {
		return err
	}

	if err := binary.Write(b, byteOrder, swap.AmountRequested); err != nil {
		return err
	}

	if err := wire.WriteVarString(b, 0, swap.PrepayInvoice); err != nil {
		return err
	}

	n, err := b.Write(swap.SenderKey[:])
	if err != nil {
		return err
	}
	if n != keyLength {
		return fmt.Errorf("sender key has invalid length")
	}

	n, err = b.Write(swap.ReceiverKey[:])
	if err != nil {
		return err
	}
	if n != keyLength {
		return fmt.Errorf("receiver key has invalid length")
	}

	if err := binary.Write(b, byteOrder, swap.CltvExpiry); err != nil {
		return err
	}

	if err := binary.Write(b, byteOrder, swap.MaxMinerFee); err != nil {
		return err
	}

	if err := binary.Write(b, byteOrder, swap.MaxSwapFee); err != nil {
		return err
	}

	if err := binary.Write(b, byteOrder, swap.MaxPrepayRoutingFee); err != nil {
		return err
	}

	if err := binary.Write(b, byteOrder, swap.InitiationHeight); err != nil {
		return err
	}

	return nil
}

func serializeUnchargeUpdate(time time.Time, state SwapState) (
	[]byte, error) {

	var b bytes.Buffer

	if err := binary.Write(&b, byteOrder, time.UnixNano()); err != nil {
		return nil, err
	}

	if err := binary.Write(&b, byteOrder, state); err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}

func deserializeUnchargeUpdate(value []byte) (*PersistentUnchargeEvent, error) {
	update := &PersistentUnchargeEvent{}

	r := bytes.NewReader(value)

	var unixNano int64
	if err := binary.Read(r, byteOrder, &unixNano); err != nil {
		return nil, err
	}
	update.Time = time.Unix(0, unixNano)

	if err := binary.Read(r, byteOrder, &update.State); err != nil {
		return nil, err
	}

	return update, nil
}

// itob returns an 8-byte big endian representation of v.
func itob(v uint64) []byte {
	b := make([]byte, 8)
	byteOrder.PutUint64(b, v)
	return b
}
