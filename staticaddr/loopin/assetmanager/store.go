package assetmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/lightninglabs/loop/staticaddr/loopin/assetexec"
	"github.com/lightninglabs/loop/swapkit/bridge"
	bolt "go.etcd.io/bbolt"
)

var (
	jobsBucket      = []byte("requests-v1")
	inventoryBucket = []byte("inventory-v1")
	releasesBucket  = []byte("released-swaps-v1")
)

// SwapRequest fixes user-approved native quantities before any server call.
//
//nolint:tagliatelle // Preserve the existing recovery and wire format.
type SwapRequest struct {
	RequestID [32]byte `json:"RequestID"`
	GroupKey  [33]byte `json:"GroupKey"`
	Outpoint  string   `json:"Outpoint"`
	Amount    uint64   `json:"Amount"` // Exact native Lightning payout.
	MaxDebit  uint64   `json:"MaxDebit"`
	MaxFee    uint64   `json:"MaxFee"`
}

// validSwapAmount spends the complete deposit without a change output. Its
// excess over the exact payout must fit the caller's native service fee cap.
func validSwapAmount(depositAmount uint64, request SwapRequest) bool {
	return request.Amount > 0 && request.Amount <= depositAmount &&
		request.MaxDebit >= request.Amount &&
		depositAmount-request.Amount <= request.MaxFee
}

// Job persists creation identity before a possibly ambiguous remote effect.
//
//nolint:tagliatelle // Preserve the existing recovery and wire format.
type Job struct {
	ID                   [32]byte                     `json:"ID"`
	Deposit              *bridge.CreateDepositRequest `json:"Deposit"`
	Swap                 *SwapRequest                 `json:"Swap"`
	Refund               *bridge.RefundDepositRequest `json:"Refund"`
	RefundProof          []byte                       `json:"RefundProof"`
	Offer                *assetexec.Offer             `json:"Offer"`
	Intent               []byte                       `json:"Intent"`
	ServerRequest        []byte                       `json:"ServerRequest"`
	Complete             bool                         `json:"Complete"`
	Canceled             bool                         `json:"Canceled"`
	RefundReported       bool                         `json:"RefundReported"`
	CancellationReported bool                         `json:"CancellationReported"`
}

// InventoryEntry binds one isolated asset anchor to a single active request.
// Consumed anchors remain excluded from every Bitcoin-only spending path.
//
//nolint:tagliatelle // Preserve the existing recovery and wire format.
type InventoryEntry struct {
	Deposit   bridge.Deposit `json:"Deposit"`
	RequestID [32]byte       `json:"RequestID"`
	Hash      [32]byte       `json:"Hash"`
	Consumed  bool           `json:"Consumed"`
}

// Store journals requests and inventory reservations in atomic transactions.
type Store struct{ db *bolt.DB }

// OpenStore opens the private asset inventory and pending-request journal.
func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{
			jobsBucket, inventoryBucket, releasesBucket,
		} {
			if _, err := tx.CreateBucketIfNotExists(
				name,
			); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close closes the store after its worker has stopped.
func (s *Store) Close() error { return s.db.Close() }

// PutJob creates an immutable request and reserves its input atomically.
func (s *Store) PutJob(ctx context.Context, j *Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if j == nil || j.ID == ([32]byte{}) {
		return assetexec.ErrInvalid
	}
	requests := 0
	for _, present := range []bool{
		j.Deposit != nil, j.Swap != nil, j.Refund != nil,
	} {
		if present {
			requests++
		}
	}
	if requests != 1 {
		return assetexec.ErrInvalid
	}
	if (j.Deposit != nil && j.Deposit.RequestID != j.ID) ||
		(j.Swap != nil && j.Swap.RequestID != j.ID) {

		return assetexec.ErrInvalid
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(jobsBucket)
		if old := b.Get(j.ID[:]); old != nil {
			var prior Job
			if err := json.Unmarshal(old, &prior); err != nil {
				return err
			}
			// Compare only the immutable request; recovery fields
			// may advance.
			a, _ := json.Marshal(prior.Deposit)
			c, _ := json.Marshal(j.Deposit)
			d, _ := json.Marshal(prior.Swap)
			e, _ := json.Marshal(j.Swap)
			f, _ := json.Marshal(prior.Refund)
			g, _ := json.Marshal(j.Refund)
			if !bytes.Equal(a, c) || !bytes.Equal(d, e) ||
				!bytes.Equal(f, g) {

				return assetexec.ErrConflict
			}
			return nil
		}
		if j.Swap != nil || j.Refund != nil {
			var point string
			if j.Swap != nil {
				point = j.Swap.Outpoint
			} else {
				point = j.Refund.Outpoint
			}
			inv := tx.Bucket(inventoryBucket)
			raw := inv.Get([]byte(point))
			if raw == nil {
				return assetexec.ErrNotFound
			}
			var entry InventoryEntry
			if err := json.Unmarshal(raw, &entry); err != nil {
				return err
			}
			if entry.Consumed || entry.RequestID != ([32]byte{}) {
				return assetexec.ErrConflict
			}
			if j.Swap != nil &&
				(entry.Deposit.GroupKey != j.Swap.GroupKey ||
					!validSwapAmount(
						entry.Deposit.Amount, *j.Swap,
					)) {

				return assetexec.ErrConflict
			}
			entry.RequestID = j.ID
			entry.Hash = [32]byte{}
			encoded, err := json.Marshal(entry)
			if err != nil {
				return err
			}
			if err = inv.Put([]byte(point), encoded); err != nil {
				return err
			}
		}
		raw, err := json.Marshal(j)
		if err != nil {
			return err
		}
		return b.Put(j.ID[:], raw)
	})
}

func (s *Store) updateJob(ctx context.Context, j *Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(jobsBucket)
		if b.Get(j.ID[:]) == nil {
			return assetexec.ErrNotFound
		}
		raw, err := json.Marshal(j)
		if err != nil {
			return err
		}
		return b.Put(j.ID[:], raw)
	})
}

// Jobs returns independent snapshots for status and startup recovery.
func (s *Store) Jobs(ctx context.Context) ([]Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var jobs []Job
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(jobsBucket).ForEach(func(_, raw []byte) error {
			var j Job
			if err := json.Unmarshal(raw, &j); err != nil {
				return err
			}
			jobs = append(jobs, j)
			return nil
		})
	})
	return jobs, err
}

func (s *Store) getJob(ctx context.Context, id [32]byte) (*Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var j Job
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(jobsBucket).Get(id[:])
		if raw == nil {
			return assetexec.ErrNotFound
		}
		return json.Unmarshal(raw, &j)
	})
	return &j, err
}

// Inventory returns every registered asset anchor, including consumed entries.
func (s *Store) Inventory(ctx context.Context) ([]InventoryEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var entries []InventoryEntry
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(inventoryBucket).
			ForEach(func(_, raw []byte) error {
				var e InventoryEntry
				if err := json.Unmarshal(raw, &e); err != nil {
					return err
				}
				entries = append(entries, e)
				return nil
			})
	})
	return entries, err
}

func (s *Store) addDeposit(d bridge.Deposit) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(inventoryBucket)
		var entry InventoryEntry
		if raw := b.Get([]byte(d.Outpoint)); raw != nil {
			if err := json.Unmarshal(raw, &entry); err != nil {
				return err
			}
			if entry.Deposit.GroupKey != d.GroupKey ||
				entry.Deposit.Amount != d.Amount ||
				!bytes.Equal(
					entry.Deposit.Contract,
					d.Contract,
				) {

				return assetexec.ErrConflict
			}
		}
		entry.Deposit = d
		raw, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		return b.Put([]byte(d.Outpoint), raw)
	})
}

func (s *Store) bind(ctx context.Context, o assetexec.Offer, release bool,
	consumed bool) error {

	if err := ctx.Err(); err != nil {
		return err
	}
	if len(o.Terms.DepositOutpoints) != 1 {
		return assetexec.ErrInvalid
	}
	point := o.Terms.DepositOutpoints[0]
	return s.db.Update(func(tx *bolt.Tx) error {
		// A crash may occur after this inventory release but before the
		// controller marks its own release complete. Preserve a receipt
		// so a new request cannot make that old cleanup unrecoverable.
		receipts := tx.Bucket(releasesBucket)
		receipt := append(
			bytes.Clone(o.Terms.RequestID[:]),
			[]byte(point)...)
		if prior := receipts.Get(o.Hash[:]); release && prior != nil {
			if !bytes.Equal(prior, receipt) {
				return assetexec.ErrConflict
			}
			return nil
		}
		b := tx.Bucket(inventoryBucket)
		raw := b.Get([]byte(point))
		if raw == nil {
			return assetexec.ErrNotFound
		}
		var e InventoryEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			return err
		}
		if release && e.RequestID == ([32]byte{}) && e.Hash == o.Hash {
			return receipts.Put(o.Hash[:], receipt)
		}
		if e.RequestID != o.Terms.RequestID ||
			(e.Hash != ([32]byte{}) && e.Hash != o.Hash) {

			return assetexec.ErrConflict
		}
		if release {
			if err := receipts.Put(o.Hash[:], receipt); err != nil {
				return err
			}
			e.RequestID = [32]byte{}
			e.Consumed = consumed
		} else if e.Consumed {
			return assetexec.ErrConflict
		}
		e.Hash = o.Hash
		encoded, err := json.Marshal(e)
		if err != nil {
			return err
		}
		return b.Put([]byte(point), encoded)
	})
}

func pending(err error) bool {
	return errors.Is(err, assetexec.ErrPending) ||
		errors.Is(err, bridge.ErrPending)
}

// finishRefund atomically marks the original anchor spent with its proof.
func (s *Store) finishRefund(ctx context.Context, job *Job,
	proof []byte) error {

	if err := ctx.Err(); err != nil {
		return err
	}
	if job.Refund == nil || len(proof) == 0 {
		return assetexec.ErrInvalid
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		inventory := tx.Bucket(inventoryBucket)
		point := []byte(job.Refund.Outpoint)
		var entry InventoryEntry
		if err := json.Unmarshal(
			inventory.Get(point), &entry,
		); err != nil {
			return err
		}
		if entry.RequestID != job.ID || entry.Hash != ([32]byte{}) {
			return assetexec.ErrConflict
		}
		entry.Consumed = true
		entry.RequestID = [32]byte{}
		raw, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		if err := inventory.Put(point, raw); err != nil {
			return err
		}
		job.RefundProof = bytes.Clone(proof)
		job.Complete = true
		raw, err = json.Marshal(job)
		if err != nil {
			return err
		}
		return tx.Bucket(jobsBucket).Put(job.ID[:], raw)
	})
}

// cancelUnstarted atomically releases a request that never reached a client
// controller. The caller must first prove its journal record does not exist.
func (s *Store) cancelUnstarted(ctx context.Context, job *Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if job.Swap == nil {
		return assetexec.ErrInvalid
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		inventory := tx.Bucket(inventoryBucket)
		point := []byte(job.Swap.Outpoint)
		var entry InventoryEntry
		if err := json.Unmarshal(
			inventory.Get(point), &entry,
		); err != nil {
			return err
		}
		if entry.RequestID != job.ID || entry.Hash != ([32]byte{}) ||
			entry.Consumed {

			return assetexec.ErrConflict
		}
		entry.RequestID = [32]byte{}
		raw, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		if err := inventory.Put(point, raw); err != nil {
			return err
		}
		job.Canceled = true
		raw, err = json.Marshal(job)
		if err != nil {
			return err
		}
		return tx.Bucket(jobsBucket).Put(job.ID[:], raw)
	})
}
