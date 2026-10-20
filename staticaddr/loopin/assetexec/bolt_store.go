package assetexec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"math"
	"time"

	"github.com/lightninglabs/loop/staticaddr/loopin/flow"
	"go.etcd.io/bbolt"
)

var (
	recordBucket  = []byte("asset-loopin-client-v1")
	depositBucket = []byte("asset-loopin-client-deposits-v1")
)

// BoltStore is a synchronously flushed journal with an exclusive process lock.
// It contains signed recovery transactions and payment preimages; protect it
// like the client wallet. Its deposit index supplements, but never replaces,
// the existing deposit manager's Bitcoin-swap and withdrawal reservations.
type BoltStore struct {
	db *bbolt.DB
}

// OpenBoltStore opens or initializes a private crash-durable journal.
func OpenBoltStore(path string) (*BoltStore, error) {
	db, err := bbolt.Open(path, 0600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(
			recordBucket,
		); err != nil {
			return err
		}
		_, err := tx.CreateBucketIfNotExists(depositBucket)
		return err
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &BoltStore{db: db}, nil
}

// Close releases the journal's exclusive process lock.
func (s *BoltStore) Close() error {
	return s.db.Close()
}

// Create atomically journals the offer and reserves its local deposit index.
func (s *BoltStore) Create(ctx context.Context, r *Record) error {
	if r.Revision != 0 || r.State != InitHtlcTx || r.Reserved ||
		r.DepositsReleased || r.InvoiceCreating ||
		r.SignaturesExposed ||
		r.HasPreimage || r.CancellationRequested ||
		r.Invoice.PaymentRequest != "" || len(r.ServerFallback) != 0 {

		return ErrInvalid
	}
	if err := validateRecord(r); err != nil {
		return err
	}
	payload, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		records := tx.Bucket(recordBucket)
		deposits := tx.Bucket(depositBucket)
		if records.Get(r.Offer.Hash[:]) != nil {
			return ErrConflict
		}
		for _, outpoint := range r.Offer.Terms.DepositOutpoints {
			if deposits.Get([]byte(outpoint)) != nil {
				return ErrConflict
			}
			if err := deposits.Put(
				[]byte(outpoint), r.Offer.Hash[:],
			); err != nil {

				return err
			}
		}
		return records.Put(r.Offer.Hash[:], payload)
	})
}

// Get returns a private record decoded into independently allocated memory.
func (s *BoltStore) Get(ctx context.Context, hash [32]byte) (*Record, error) {
	var record *Record
	err := s.db.View(func(tx *bbolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var err error
		record, err = decodeRecord(tx.Bucket(recordBucket).Get(hash[:]))
		if err == nil && record.Offer.Hash != hash {
			return ErrInvalid
		}
		return err
	})
	return record, err
}

// Update atomically advances a revision and releases only this swap's local
// index after the shared deposit manager has completed terminal cleanup.
func (s *BoltStore) Update(ctx context.Context, r *Record,
	previous int64) error {

	if previous < 0 || previous == math.MaxInt64 ||
		r.Revision != previous+1 {

		return ErrInvalid
	}
	if err := validateRecord(r); err != nil {
		return err
	}
	payload, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		records := tx.Bucket(recordBucket)
		old, err := decodeRecord(records.Get(r.Offer.Hash[:]))
		if err != nil {
			return err
		}
		if old.Revision != previous {
			return ErrConflict
		}
		if err := validateUpdate(old, r); err != nil {
			return err
		}
		if r.DepositsReleased {
			deposits := tx.Bucket(depositBucket)
			for _, point := range r.Offer.Terms.DepositOutpoints {
				if bytes.Equal(deposits.Get([]byte(point)),
					r.Offer.Hash[:]) {

					if err := deposits.Delete(
						[]byte(point),
					); err != nil {

						return err
					}
				}
			}
		}
		return records.Put(r.Offer.Hash[:], payload)
	})
}

// ListActive includes terminal records whose deposit release needs a retry.
func (s *BoltStore) ListActive(ctx context.Context) ([]*Record, error) {
	var records []*Record
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(recordBucket).ForEach(func(_, v []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			r, err := decodeRecord(v)
			if err != nil {
				return err
			}
			if !r.State.Terminal() || !r.DepositsReleased {
				records = append(records, r)
			}
			return nil
		})
	})
	return records, err
}

func decodeRecord(payload []byte) (*Record, error) {
	if payload == nil {
		return nil, ErrNotFound
	}
	var r Record
	if err := json.Unmarshal(payload, &r); err != nil {
		return nil, err
	}
	if err := validateRecord(&r); err != nil {
		return nil, err
	}
	if r.DepositState == "" {
		r.DepositState = inferredDepositState(&r)
	}
	return &r, nil
}

func validateRecord(r *Record) error {
	if r == nil || r.Revision < 0 || len(r.InvoiceIntent) == 0 {
		return ErrInvalid
	}
	if err := validateOffer(r.Offer); err != nil {
		return err
	}
	if r.DepositState != "" &&
		r.DepositState != inferredDepositState(r) {

		return ErrInvalid
	}
	if r.DepositsReleased && !r.State.Terminal() ||
		r.InvoiceCreating && !r.Reserved ||
		r.Invoice.PaymentRequest != "" && (!r.InvoiceCreating ||
			len(r.Invoice.PaymentPlan) == 0) ||
		len(r.ServerFallback) > 0 && (r.Invoice.PaymentRequest == "" ||
			len(r.Fallback.Bundle) == 0 ||
			len(r.Fallback.ClaimSignatures) == 0) ||
		r.SignaturesExposed && (len(r.ServerFallback) == 0 ||
			len(r.AcceptedEvidence) == 0) ||
		r.HasPreimage && (!r.SignaturesExposed ||
			sha256.Sum256(r.Preimage[:]) != r.Offer.Hash) ||
		len(r.CooperativeSignature) > 0 &&
			(len(r.CooperativeRequest) == 0 || !r.HasPreimage) {

		return ErrInvalid
	}
	switch r.State {
	case InitHtlcTx:
		if r.Invoice.PaymentRequest != "" || r.SignaturesExposed {
			return ErrInvalid
		}
	case SignHtlcTx:
		if r.Invoice.PaymentRequest == "" || r.SignaturesExposed {
			return ErrInvalid
		}
	case MonitorInvoiceAndHtlcTx:
		if !r.SignaturesExposed {
			return ErrInvalid
		}
	case PaymentReceived:
		if !r.HasPreimage {
			return ErrInvalid
		}
	case SweepHtlcTimeout:
		if !r.CancellationRequested || !r.SignaturesExposed {
			return ErrInvalid
		}
	case HtlcTimeoutSwept:
		if !r.CancellationRequested || !r.SignaturesExposed ||
			len(r.RefundEvidence) == 0 {

			return ErrInvalid
		}
	case Succeeded:
		if !r.HasPreimage || len(r.ClaimEvidence) == 0 {
			return ErrInvalid
		}
	case Failed:
		if !r.CancellationRequested || r.SignaturesExposed {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

func validateUpdate(old, next *Record) error {
	if !same(old.Offer, next.Offer) ||
		!bytes.Equal(old.InvoiceIntent, next.InvoiceIntent) ||
		old.Reserved && !next.Reserved ||
		old.DepositsReleased && !next.DepositsReleased ||
		old.InvoiceCreating && !next.InvoiceCreating ||
		old.SignaturesExposed && !next.SignaturesExposed ||
		old.CancellationRequested && !next.CancellationRequested ||
		old.HasPreimage && (!next.HasPreimage ||
			old.Preimage != next.Preimage) ||
		old.Invoice.PaymentRequest != "" &&
			!same(old.Invoice, next.Invoice) ||
		len(old.ServerFallback) > 0 &&
			(!bytes.Equal(
				old.ServerFallback, next.ServerFallback,
			) ||
				!same(old.Fallback, next.Fallback)) {

		return ErrInvalid
	}
	for _, pair := range [][2][]byte{
		{old.AcceptedEvidence, next.AcceptedEvidence},
		{old.CooperativeRequest, next.CooperativeRequest},
		{old.CooperativeSignature, next.CooperativeSignature},
		{old.ClaimEvidence, next.ClaimEvidence},
	} {
		if len(pair[0]) > 0 && !bytes.Equal(pair[0], pair[1]) {
			return ErrInvalid
		}
	}
	// A reorg may relocate a confirmed refund between proof persistence
	// and invoice cancellation. Only the recovery state can replace that
	// evidence, after the controller has reverified the current proof.
	if len(old.RefundEvidence) > 0 &&
		!bytes.Equal(old.RefundEvidence, next.RefundEvidence) &&
		(old.State != SweepHtlcTimeout ||
			next.State != SweepHtlcTimeout ||
			len(next.RefundEvidence) == 0) {

		return ErrInvalid
	}
	if old.State.Terminal() {
		copy := *old
		copy.Revision = next.Revision
		copy.DepositsReleased = next.DepositsReleased
		if !same(&copy, next) {
			return ErrState
		}
		return nil
	}
	if !flow.Allows(flow.ServerAsset, old.State, next.State) {
		return ErrState
	}
	return nil
}
