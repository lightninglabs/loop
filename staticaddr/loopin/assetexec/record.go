package assetexec

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"

	"github.com/lightninglabs/loop/staticaddr/loopin/flow"
)

// DecodeRecord validates and decodes a persisted execution snapshot.
func DecodeRecord(payload []byte) (*Record, error) {
	if payload == nil {
		return nil, ErrNotFound
	}
	var r Record
	if err := json.Unmarshal(payload, &r); err != nil {
		return nil, err
	}
	if err := ValidateRecord(&r); err != nil {
		return nil, err
	}
	if r.DepositState == "" {
		r.DepositState = inferredDepositState(&r)
	}
	return &r, nil
}

// ValidateRecord checks the execution record before persistence.
func ValidateRecord(r *Record) error {
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
			sha256.Sum256(r.Preimage[:]) != r.Offer.Hash) {

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

// ValidateUpdate checks immutable and monotonic recovery state.
func ValidateUpdate(old, next *Record) error {
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
		prior := *old
		prior.Revision = next.Revision
		prior.DepositsReleased = next.DepositsReleased
		if !same(&prior, next) {
			return ErrState
		}
		return nil
	}
	if !flow.Allows(flow.ServerAsset, old.State, next.State) {
		return ErrState
	}
	return nil
}

// ValidateCreate admits only a pristine write-ahead execution record.
func ValidateCreate(r *Record) error {
	if r == nil || r.Revision != 0 || r.State != InitHtlcTx || r.Reserved ||
		r.DepositsReleased ||
		r.InvoiceCreating ||
		r.SignaturesExposed ||
		r.HasPreimage ||
		r.CancellationRequested ||
		r.Invoice.PaymentRequest != "" ||
		len(r.ServerFallback) != 0 {

		return ErrInvalid
	}
	return ValidateRecord(r)
}
