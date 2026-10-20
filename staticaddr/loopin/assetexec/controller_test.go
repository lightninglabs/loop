package assetexec

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/require"
)

var errUnavailable = errors.New("RPC response lost")

type testActions struct {
	t                 *testing.T
	store             Store
	invoice           Invoice
	createErr         error
	preparedErr       error
	preparedEvidence  []byte
	onPrepared        func()
	pushErr           error
	settleErr         error
	settled           bool
	preimage          [32]byte
	preimageFound     bool
	height            uint32
	refundCalls       int
	settledErr        error
	refund            []byte
	claim             []byte
	creates           int
	lookups           int
	prepared          int
	pushes            int
	settles           int
	signs             int
	cancels           int
	lookupPreimageErr error
	blockPreimage     bool
	preimageLookups   int
	cancelErr         error
}

func (a *testActions) record(ctx context.Context, o Offer) *Record {
	r, err := a.store.Get(ctx, o.Hash)
	require.NoError(a.t, err)
	return r
}

func (a *testActions) CurrentHeight(context.Context) (uint32, error) {
	return a.height, nil
}

func (a *testActions) ValidateOffer(context.Context, Offer) error { return nil }

func (a *testActions) CreateInvoice(ctx context.Context, o Offer,
	intent []byte) (Invoice, error) {

	r := a.record(ctx, o)
	require.True(a.t, r.InvoiceCreating)
	require.True(a.t, r.Reserved)
	require.Equal(a.t, intent, r.InvoiceIntent)
	a.creates++
	a.invoice = Invoice{"hold-invoice", []byte("original-rfq")}
	return a.invoice, a.createErr
}

func (a *testActions) LookupInvoice(context.Context, Offer) (Invoice, error) {
	a.lookups++
	if a.invoice.PaymentRequest == "" {
		return Invoice{}, ErrNotFound
	}
	return a.invoice, nil
}

func (a *testActions) VerifyFinalizeFallback(context.Context, Offer,
	[]byte) (Fallback, error) {

	return Fallback{Bundle: []byte("full-sdk-package"),
		ClaimSignatures: []byte("claim-sigs")}, nil
}

func (a *testActions) Prepared(context.Context, Offer) ([]byte, error) {
	a.prepared++
	if a.onPrepared != nil {
		a.onPrepared()
	}
	if a.preparedEvidence != nil {
		return a.preparedEvidence, a.preparedErr
	}
	return []byte("local-accepted-asset-htlc"), a.preparedErr
}

func (a *testActions) PushSignatures(ctx context.Context, o Offer,
	signatures []byte) error {

	r := a.record(ctx, o)
	require.True(a.t, r.SignaturesExposed)
	require.NotEmpty(a.t, r.AcceptedEvidence)
	require.Equal(a.t, []byte("full-sdk-package"), r.Fallback.Bundle)
	require.Equal(a.t, r.Fallback.ClaimSignatures, signatures)
	a.pushes++
	return a.pushErr
}

func (a *testActions) LookupPreimage(ctx context.Context, _ Offer) (
	[32]byte, bool, error) {

	a.preimageLookups++
	if a.blockPreimage {
		<-ctx.Done()
		return [32]byte{}, false, ctx.Err()
	}
	return a.preimage, a.preimageFound, a.lookupPreimageErr
}

func (a *testActions) SettleInvoice(ctx context.Context, o Offer,
	preimage [32]byte) error {

	r := a.record(ctx, o)
	require.True(a.t, r.HasPreimage)
	require.Equal(a.t, preimage, r.Preimage)
	a.settles++
	return a.settleErr
}

func (a *testActions) InvoiceSettled(context.Context, Offer) (bool, error) {
	return a.settled, a.settledErr
}

func (a *testActions) SignCooperative(ctx context.Context, o Offer,
	request []byte) ([]byte, error) {

	r := a.record(ctx, o)
	require.Equal(a.t, PaymentReceived, r.State)
	require.Equal(a.t, request, r.CooperativeRequest)
	require.True(a.t, a.settled)
	a.signs++
	return []byte("cooperative-signature"), nil
}

func (a *testActions) RecoverRefund(ctx context.Context, o Offer,
	_ Fallback) ([]byte, error) {

	require.NoError(a.t, ctx.Err())
	r := a.record(ctx, o)
	require.True(a.t, r.CancellationRequested)
	require.Equal(a.t, SweepHtlcTimeout, r.State)
	a.refundCalls++
	return a.refund, nil
}

func (a *testActions) InspectClaim(context.Context, Offer, Fallback) (
	[]byte, error) {

	return a.claim, nil
}

func (a *testActions) CancelInvoice(ctx context.Context, o Offer) error {
	r := a.record(ctx, o)
	require.True(a.t, r.CancellationRequested)
	if r.SignaturesExposed {
		require.NotEmpty(a.t, r.RefundEvidence)
		require.Equal(a.t, a.refund, r.RefundEvidence,
			"persist the current proof before canceling the invoice")
	}
	a.cancels++
	return a.cancelErr
}

type fixture struct {
	t        *testing.T
	store    *memoryStore
	c        *Controller
	a        *testActions
	offer    Offer
	preimage [32]byte
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t}
	f.preimage[0] = 7
	_, group := btcec.PrivKeyFromBytes([]byte{9})
	var key [33]byte
	copy(key[:], group.SerializeCompressed())
	f.offer = Offer{
		Hash: sha256.Sum256(f.preimage[:]),
		Terms: Terms{
			Version: 1, Owner: [32]byte{1}, GroupKey: key,
			RequestID: [32]byte{2}, RequestDigest: [32]byte{3},
			DepositOutpoints: []string{
				strings.Repeat("ab", 32) + ":0",
			},
			DepositAmount: 100, PayoutAmount: 90, AssetFee: 5,
			ChangeAmount: 5, MaxDebit: 95, RefundHeight: 100,
			MinPayoutExpiry: 121,
			RecoveryDelta:   10,
			ExecutionDelta:  10,
			OfferExpiry:     time.Now().Add(time.Hour),
			Contract:        []byte("approved-contract"),
		},
	}
	f.store = newMemoryStore()
	f.a = &testActions{t: t, store: f.store, preimage: f.preimage}
	f.c = NewController(f.store, f.a, nil)
	return f
}

func (f *fixture) reopen() {
	f.t.Helper()
	// Rebuild the controller from independently decoded checkpoints.
	// SQL filesystem restart is exercised by the integration store tests.
	f.a.store = f.store
	f.c = NewController(f.store, f.a, nil)
}

func (f *fixture) ready() {
	f.t.Helper()
	ctx := f.t.Context()
	require.NoError(f.t, f.c.Start(ctx, f.offer, []byte("invoice-intent")))
	_, err := f.c.PrepareInvoice(ctx, f.offer.Hash)
	require.NoError(f.t, err)
	require.NoError(f.t, f.c.ReceiveFallback(
		ctx, f.offer.Hash, []byte("server-package"),
		func(ctx context.Context, raw []byte) (Fallback, error) {
			return f.a.VerifyFinalizeFallback(ctx, f.offer, raw)
		},
	))
}

func TestRecoveryPreservesEveryExposureBoundary(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.ready()
	_, err := f.c.CooperativeSignature(ctx, f.offer.Hash, []byte("sweep"))
	require.ErrorIs(t, err, ErrState)
	f.a.pushErr = errUnavailable
	require.ErrorIs(t, f.c.ExposeClaim(ctx, f.offer.Hash), errUnavailable)
	f.reopen()
	f.a.pushErr = nil
	require.NoError(t, f.c.Recover(ctx, f.offer.Hash))
	require.Equal(t, 2, f.a.prepared)
	require.Equal(t, 2, f.a.pushes)
	require.ErrorIs(t, f.c.ReceivePreimage(
		ctx, f.offer.Hash, [32]byte{3},
	), ErrInvalid)
	f.a.preimageFound = true
	f.a.settleErr = errUnavailable
	require.ErrorIs(t, f.c.Recover(ctx, f.offer.Hash), errUnavailable)
	f.reopen()
	f.a.settleErr = nil
	require.ErrorIs(t, f.c.Recover(ctx, f.offer.Hash), ErrPending)
	f.a.settled = true
	require.NoError(t, f.c.Recover(ctx, f.offer.Hash))
	f.a.settled = false
	_, err = f.c.CooperativeSignature(ctx, f.offer.Hash, []byte("sweep"))
	require.ErrorIs(t, err, ErrPending)
	require.Zero(t, f.a.signs)
	f.a.settled = true
	sig, err := f.c.CooperativeSignature(ctx, f.offer.Hash, []byte("sweep"))
	require.NoError(t, err)
	f.reopen()
	retry, err := f.c.CooperativeSignature(
		ctx,
		f.offer.Hash,
		[]byte("sweep"),
	)
	require.NoError(t, err)
	require.Equal(t, sig, retry)
	require.Equal(t, 1, f.a.signs)
	_, err = f.c.CooperativeSignature(ctx, f.offer.Hash, []byte("changed"))
	require.ErrorIs(t, err, ErrConflict)
	require.ErrorIs(t, f.c.Cancel(ctx, f.offer.Hash), ErrState)
	require.ErrorIs(t, f.c.Recover(ctx, f.offer.Hash), ErrPending)
	require.Zero(t, f.store.releases)
	f.a.claim = []byte("confirmed-claim-with-spendable-asset-proofs")
	f.store.releaseErr = errUnavailable
	require.ErrorIs(t, f.c.Recover(ctx, f.offer.Hash), errUnavailable)
	f.reopen()
	active, err := f.store.ListActive(ctx)
	require.NoError(t, err)
	require.Len(t, active, 1)
	f.store.releaseErr = nil
	require.NoError(t, f.c.Recover(ctx, f.offer.Hash))
	active, err = f.store.ListActive(ctx)
	require.NoError(t, err)
	require.Empty(t, active)
	r, err := f.c.Status(ctx, f.offer.Hash)
	require.NoError(t, err)
	require.Equal(t, Succeeded, r.State)
	require.True(t, r.DepositsReleased)
}

func TestInvoiceIntentSurvivesLostResponse(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	require.NoError(t, f.c.Start(ctx, f.offer, []byte("intent")))
	f.a.createErr = errUnavailable
	_, err := f.c.PrepareInvoice(ctx, f.offer.Hash)
	require.ErrorIs(t, err, errUnavailable)
	f.reopen()
	f.a.createErr = nil
	invoice, err := f.c.PrepareInvoice(ctx, f.offer.Hash)
	require.NoError(t, err)
	require.Equal(t, f.a.invoice, invoice)
	require.Equal(t, 1, f.a.creates)
	require.Equal(t, 1, f.a.lookups)
	require.ErrorIs(
		t,
		f.c.Start(ctx, f.offer, []byte("changed")),
		ErrConflict,
	)
}

func TestRefundRequiresConfirmedAssetProofs(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.ready()
	require.NoError(t, f.c.ExposeClaim(ctx, f.offer.Hash))
	require.ErrorIs(t, f.c.Cancel(ctx, f.offer.Hash), ErrPending)
	f.reopen()
	require.ErrorIs(t, f.c.ExposeClaim(ctx, f.offer.Hash), ErrState)
	require.ErrorIs(t, f.c.Recover(ctx, f.offer.Hash), ErrPending)
	require.Zero(t, f.a.cancels)
	require.Zero(t, f.store.releases)
	f.a.refund = []byte("confirmed-refund-with-spendable-proofs")
	require.NoError(t, f.c.Recover(ctx, f.offer.Hash))
	r, err := f.c.Status(ctx, f.offer.Hash)
	require.NoError(t, err)
	require.Equal(t, HtlcTimeoutSwept, r.State)
	require.Equal(t, 1, f.a.cancels)
	require.Equal(t, 1, f.store.releases)
}

func TestExecutionWinsWhileRefundIsPending(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.ready()
	require.NoError(t, f.c.ExposeClaim(ctx, f.offer.Hash))
	require.ErrorIs(t, f.c.Cancel(ctx, f.offer.Hash), ErrPending)
	f.reopen()
	f.a.preimageFound = true
	f.a.settled = true
	require.NoError(t, f.c.Recover(ctx, f.offer.Hash))
	r, err := f.c.Status(ctx, f.offer.Hash)
	require.NoError(t, err)
	require.Equal(t, PaymentReceived, r.State)
	require.Zero(t, f.a.cancels)
}

func TestLocalAcceptanceIsRequired(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.ready()
	f.a.preparedErr = ErrPending
	require.ErrorIs(t, f.c.ExposeClaim(ctx, f.offer.Hash), ErrPending)
	require.Zero(t, f.a.pushes)
	r, err := f.c.Status(ctx, f.offer.Hash)
	require.NoError(t, err)
	require.False(t, r.SignaturesExposed)
	require.NoError(t, f.c.Cancel(ctx, f.offer.Hash))
	require.Equal(t, 1, f.a.cancels)
}

type failingStore struct {
	Store

	fail func(*Record) bool
}

func (s failingStore) Update(ctx context.Context, r *Record, rev int64) error {
	if s.fail(r) {
		return errUnavailable
	}
	return s.Store.Update(ctx, r, rev)
}

func TestPersistenceFailurePreventsSignatureExposure(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.ready()
	f.c = NewController(failingStore{
		Store: f.store,
		fail:  func(r *Record) bool { return r.SignaturesExposed },
	}, f.a, nil)
	require.ErrorIs(t, f.c.ExposeClaim(ctx, f.offer.Hash), errUnavailable)
	require.Zero(t, f.a.pushes)
	f.reopen()
	f.a.preparedErr = ErrPending
	require.ErrorIs(t, f.c.ExposeClaim(ctx, f.offer.Hash), ErrPending)
	require.Zero(t, f.a.pushes)
}

func TestJournalExclusionAndImmutableData(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.ready()
	other := cloneOffer(f.offer)
	other.Hash[0]++
	require.ErrorIs(t, f.c.Start(ctx, other, []byte("intent")), ErrConflict)
	r, err := f.c.Status(ctx, f.offer.Hash)
	require.NoError(t, err)
	r.Offer.Terms.Contract[0] ^= 1
	r.Revision++
	require.ErrorIs(t, f.store.Update(ctx, r, r.Revision-1), ErrInvalid)
	r, err = f.c.Status(ctx, f.offer.Hash)
	require.NoError(t, err)
	r.Revision++
	require.ErrorIs(t, f.store.Update(ctx, r, r.Revision), ErrInvalid)
	require.Equal(t, []byte("approved-contract"), r.Offer.Terms.Contract)
	require.NoError(t, f.c.Cancel(ctx, f.offer.Hash))
	require.NoError(t, f.c.Start(ctx, other, []byte("intent")))
}

func TestConcurrentExposureUsesOneDurableDecision(t *testing.T) {
	f := newFixture(t)
	f.ready()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			require.NoError(
				t,
				f.c.ExposeClaim(t.Context(), f.offer.Hash),
			)
		})
	}
	wg.Wait()
	require.Equal(t, 8, f.a.prepared)
	require.Equal(t, 8, f.a.pushes)
}

func TestOfferRejectsNoncanonicalOutpoints(t *testing.T) {
	for _, point := range []string{
		strings.Repeat("AB", 32) + ":0",
		strings.Repeat("ab", 32) + ":00",
		strings.Repeat("ab", 32) + ":+1",
		strings.Repeat("ab", 32) + ":4294967296",
	} {
		t.Run(point, func(t *testing.T) {
			f := newFixture(t)
			f.offer.Terms.DepositOutpoints = []string{point}
			require.ErrorIs(t, f.c.Start(
				t.Context(), f.offer, []byte("intent"),
			), ErrInvalid)
		})
	}
}

func TestOfflineServerCannotBlockConfirmedRefund(t *testing.T) {
	f := newFixture(t)
	f.ready()
	ctx := t.Context()
	require.NoError(t, f.c.ExposeClaim(ctx, f.offer.Hash))
	require.ErrorIs(t, f.c.Cancel(ctx, f.offer.Hash), ErrPending)
	f.reopen()
	f.a.lookupPreimageErr = errUnavailable
	f.a.refund = []byte("confirmed refund with spendable assets")
	require.NoError(t, f.c.Recover(ctx, f.offer.Hash))
	r, err := f.c.Status(ctx, f.offer.Hash)
	require.NoError(t, err)
	require.Equal(t, HtlcTimeoutSwept, r.State)
	require.True(t, r.DepositsReleased)
}

func TestKnownPreimageDoesNotPreventFailedPayoutRefund(t *testing.T) {
	f := newFixture(t)
	f.ready()
	ctx := t.Context()
	require.NoError(t, f.c.ExposeClaim(ctx, f.offer.Hash))
	f.a.settleErr = errors.New("incoming HTLC already canceled")
	require.ErrorIs(t, f.c.ReceivePreimage(
		ctx, f.offer.Hash, f.preimage,
	), f.a.settleErr)
	require.ErrorIs(t, f.c.Cancel(ctx, f.offer.Hash), ErrPending)
	f.reopen()
	f.a.refund = []byte("confirmed refund with spendable assets")
	require.NoError(t, f.c.Recover(ctx, f.offer.Hash))
	r, err := f.c.Status(ctx, f.offer.Hash)
	require.NoError(t, err)
	require.Equal(t, HtlcTimeoutSwept, r.State)
	require.True(t, r.DepositsReleased)
	require.True(t, r.HasPreimage)
	require.Equal(t, f.preimage, r.Preimage)
}

func TestKnownPreimageCannotCancelSettledPayout(t *testing.T) {
	f := newFixture(t)
	f.ready()
	ctx := t.Context()
	require.NoError(t, f.c.ExposeClaim(ctx, f.offer.Hash))
	f.a.settleErr = errUnavailable
	require.ErrorIs(t, f.c.ReceivePreimage(
		ctx, f.offer.Hash, f.preimage,
	), errUnavailable)
	// The lost settlement response was successful at the local node.
	f.a.settled = true
	require.ErrorIs(t, f.c.Cancel(ctx, f.offer.Hash), ErrState)
	require.Zero(t, f.a.cancels)
	require.Zero(t, f.store.releases)
	r, err := f.c.Status(ctx, f.offer.Hash)
	require.NoError(t, err)
	require.Equal(t, PaymentReceived, r.State)
}

func TestSettlementWinsCancellationRace(t *testing.T) {
	f := newFixture(t)
	f.ready()
	ctx := t.Context()
	require.NoError(t, f.c.ExposeClaim(ctx, f.offer.Hash))
	f.a.settleErr = errUnavailable
	require.ErrorIs(t, f.c.ReceivePreimage(
		ctx, f.offer.Hash, f.preimage,
	), errUnavailable)
	f.a.refund = []byte("confirmed refund with spendable assets")
	f.a.cancelErr = errors.New("cannot cancel settled invoice")
	require.ErrorIs(t, f.c.Cancel(ctx, f.offer.Hash), f.a.cancelErr)
	require.Zero(t, f.store.releases)
	r, err := f.c.Status(ctx, f.offer.Hash)
	require.NoError(t, err)
	require.Equal(t, SweepHtlcTimeout, r.State)
	require.False(t, r.DepositsReleased)
}

func TestRefundMustRemainConfirmedBeforeCancel(t *testing.T) {
	f := newFixture(t)
	f.ready()
	ctx := t.Context()
	require.NoError(t, f.c.ExposeClaim(ctx, f.offer.Hash))
	f.a.refund = []byte("confirmed refund with spendable assets")
	f.a.cancelErr = errUnavailable
	require.ErrorIs(t, f.c.Cancel(ctx, f.offer.Hash), errUnavailable)
	require.Equal(t, 1, f.a.cancels)

	// The refund proof was journaled, but a restart follows a reorg that
	// removes the refund. A retry must not use that stale proof to cancel.
	f.reopen()
	f.a.refund, f.a.cancelErr = nil, nil
	require.ErrorIs(t, f.c.Recover(ctx, f.offer.Hash), ErrPending)
	require.Equal(t, 1, f.a.cancels)
	require.Zero(t, f.store.releases)
	r, err := f.c.Status(ctx, f.offer.Hash)
	require.NoError(t, err)
	require.NotEmpty(t, r.RefundEvidence)
	require.False(t, r.DepositsReleased)
	require.Equal(t, SweepHtlcTimeout, r.State)

	f.a.refund = []byte("refund confirmed in a replacement block")
	require.NoError(t, f.c.Recover(ctx, f.offer.Hash))
	require.Equal(t, 2, f.a.cancels)
	require.Equal(t, 1, f.store.releases)
	f.reopen()
	r, err = f.c.Status(ctx, f.offer.Hash)
	require.NoError(t, err)
	require.Equal(t, f.a.refund, r.RefundEvidence)
}

type ambiguousExposureStore struct{ Store }

func (s ambiguousExposureStore) Update(ctx context.Context, r *Record,
	previous int64) error {

	if err := s.Store.Update(ctx, r, previous); err != nil {
		return err
	}
	if r.SignaturesExposed {
		return errUnavailable
	}
	return nil
}

func TestExposureRetryCannotGrantAuthorityAfterPayoutChanges(t *testing.T) {
	for _, name := range []string{"expired", "failed", "new circuit"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.ready()
			ctx := t.Context()
			// Crash after durable intent but before the first send.
			// The server has no client signature.
			f.c = NewController(
				ambiguousExposureStore{f.store},
				f.a,
				nil,
			)
			require.ErrorIs(t, f.c.ExposeClaim(ctx, f.offer.Hash),
				errUnavailable)
			require.Zero(t, f.a.pushes)
			f.reopen()
			var expected error
			switch name {
			case "expired":
				f.c.now = func() time.Time {
					return f.offer.Terms.OfferExpiry
				}
				expected = ErrPending
			case "failed":
				f.a.preparedErr = ErrPending
				expected = ErrPending
			case "new circuit":
				f.a.preparedEvidence = []byte(
					"replacement circuit",
				)
				expected = ErrConflict
			}
			require.ErrorIs(
				t,
				f.c.Recover(ctx, f.offer.Hash),
				expected,
			)
			require.Zero(t, f.a.pushes)
			require.Zero(t, f.store.releases)
			r, err := f.c.Status(ctx, f.offer.Hash)
			require.NoError(t, err)
			require.True(t, r.SignaturesExposed)
			require.False(t, r.DepositsReleased)
			require.Equal(t, []byte("local-accepted-asset-htlc"),
				r.AcceptedEvidence)
			other := cloneOffer(f.offer)
			other.Hash[0]++
			f.c.now = time.Now
			require.ErrorIs(t, f.c.Start(ctx, other,
				[]byte("replacement intent")), ErrConflict)
		})
	}
}

func TestRecoveryDrivesRefundAfterDeadlineWhileServerOffline(t *testing.T) {
	for _, deadline := range []string{"offer", "height"} {
		t.Run(deadline, func(t *testing.T) {
			f := newFixture(t)
			f.ready()
			ctx := t.Context()
			require.NoError(t, f.c.ExposeClaim(ctx, f.offer.Hash))
			f.a.lookupPreimageErr = errUnavailable
			f.reopen()
			if deadline == "offer" {
				f.c.now = func() time.Time {
					return f.offer.Terms.OfferExpiry
				}
			} else {
				f.a.height = f.offer.Terms.RefundHeight
			}

			// Recovery itself drives publication. Until
			// confirmation,
			// neither invoice cancellation nor lock release is
			// allowed.
			require.ErrorIs(
				t,
				f.c.Recover(ctx, f.offer.Hash),
				ErrPending,
			)
			require.Equal(t, 1, f.a.refundCalls)
			require.Zero(t, f.a.cancels)
			require.Zero(t, f.store.releases)
			r, err := f.c.Status(ctx, f.offer.Hash)
			require.NoError(t, err)
			require.True(t, r.CancellationRequested)
			require.Equal(t, SweepHtlcTimeout, r.State)

			// Intent survives restart after the wall clock moves
			// back. The adapter must resume the same signed bundle.
			f.reopen()
			f.a.refund = []byte(
				"current confirmed spendable refund",
			)
			require.NoError(t, f.c.Recover(ctx, f.offer.Hash))
			require.Equal(t, 2, f.a.refundCalls)
			require.Equal(t, 1, f.a.cancels)
			require.Equal(t, 1, f.store.releases)
		})
	}
}

func TestRecoveryPrefersPreimageOverExpiredOffer(t *testing.T) {
	f := newFixture(t)
	f.ready()
	ctx := t.Context()
	require.NoError(t, f.c.ExposeClaim(ctx, f.offer.Hash))
	f.a.height = f.offer.Terms.RefundHeight
	f.a.preimageFound, f.a.settled = true, true
	require.NoError(t, f.c.Recover(ctx, f.offer.Hash))
	require.Equal(t, 1, f.a.settles)
	require.Zero(t, f.a.refundCalls)
	r, err := f.c.Status(ctx, f.offer.Hash)
	require.NoError(t, err)
	require.True(t, r.HasPreimage)
	require.Equal(t, PaymentReceived, r.State)
}

func TestRefundRecoveryRequiresAuthoritativeUnsettledInvoice(t *testing.T) {
	for _, state := range []string{"settled", "unavailable"} {
		t.Run(state, func(t *testing.T) {
			f := newFixture(t)
			f.ready()
			ctx := t.Context()
			require.NoError(t, f.c.ExposeClaim(ctx, f.offer.Hash))
			f.a.height = f.offer.Terms.RefundHeight
			f.a.lookupPreimageErr = errUnavailable
			if state == "settled" {
				f.a.settled = true
			} else {
				f.a.settledErr = errUnavailable
			}
			require.Error(t, f.c.Recover(ctx, f.offer.Hash))
			require.Zero(t, f.a.refundCalls)
			require.Zero(t, f.a.cancels)
			require.Zero(t, f.store.releases)
			r, err := f.c.Status(ctx, f.offer.Hash)
			require.NoError(t, err)
			require.False(t, r.HasPreimage)
			require.Equal(t, MonitorInvoiceAndHtlcTx, r.State)
		})
	}
}

func TestControllerLockWaitHonorsCancellation(t *testing.T) {
	f := newFixture(t)
	f.ready()
	unlock, err := f.c.lock(t.Context(), f.offer.Hash)
	require.NoError(t, err)
	defer unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- f.c.ExposeClaim(ctx, f.offer.Hash) }()
	select {
	case err := <-result:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(time.Second):
		t.Fatal("canceled operation remained blocked on the swap lock")
	}
	require.Zero(t, f.a.prepared)
	require.Zero(t, f.a.pushes)
}

func TestBlockedPreimageLookupCannotStarveRefund(t *testing.T) {
	f := newFixture(t)
	f.ready()
	require.NoError(t, f.c.ExposeClaim(t.Context(), f.offer.Hash))
	f.a.height = f.offer.Terms.RefundHeight
	f.a.blockPreimage = true
	f.a.refund = []byte("current confirmed spendable refund")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, f.c.Recover(ctx, f.offer.Hash))
	require.NoError(t, ctx.Err())
	require.Equal(t, 1, f.a.preimageLookups)
	require.Equal(t, 1, f.a.refundCalls)
	require.Equal(t, 1, f.a.cancels)
}

func TestExpiredUnexposedRecoveryNeverQueriesPreimage(t *testing.T) {
	f := newFixture(t)
	f.ready()
	f.a.blockPreimage = true
	f.c.now = func() time.Time { return f.offer.Terms.OfferExpiry }
	require.NoError(t, f.c.Recover(t.Context(), f.offer.Hash))
	require.Zero(t, f.a.preimageLookups)
	require.Zero(t, f.a.refundCalls)
	require.Equal(t, 1, f.a.cancels)
}

type delayedExposureStore struct {
	Store

	afterWrite func()
}

func (s delayedExposureStore) Update(ctx context.Context, r *Record,
	previous int64) error {

	if err := s.Store.Update(ctx, r, previous); err != nil {
		return err
	}
	if r.SignaturesExposed {
		s.afterWrite()
	}
	return nil
}

func TestExposureRechecksDeadlinesAfterBlockingOperations(t *testing.T) {
	for _, phase := range []string{"evidence", "journal"} {
		for _, cause := range []string{
			"wall", "height", "overflow", "cancel",
		} {
			t.Run(phase+"/"+cause, func(t *testing.T) {
				testExposureRechecksDeadlines(t, phase, cause)
			})
		}
	}
}

func testExposureRechecksDeadlines(t *testing.T, phase, cause string) {
	t.Helper()
	f := newFixture(t)
	f.ready()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	changed := func() {
		switch cause {
		case "wall":
			f.c.now = func() time.Time {
				return f.offer.Terms.OfferExpiry
			}
		case "height":
			f.a.height = f.offer.Terms.RefundHeight -
				f.offer.Terms.ExecutionDelta
		case "overflow":
			f.a.height = ^uint32(0)
		case "cancel":
			cancel()
		}
	}
	if phase == "evidence" {
		f.a.onPrepared = changed
	} else {
		f.c.store = delayedExposureStore{f.store, changed}
	}
	err := f.c.ExposeClaim(ctx, f.offer.Hash)
	if cause == "cancel" {
		require.ErrorIs(
			t,
			err,
			context.Canceled,
		)
	} else {
		require.ErrorIs(t, err, ErrState)
	}
	require.Zero(t, f.a.pushes)
	r, err := f.c.Status(t.Context(), f.offer.Hash)
	require.NoError(t, err)
	require.Equal(
		t,
		phase == "journal",
		r.SignaturesExposed,
	)
	require.False(t, r.DepositsReleased)
}
