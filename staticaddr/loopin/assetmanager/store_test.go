package assetmanager

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/loopin/assetclient"
	"github.com/lightninglabs/loop/staticaddr/loopin/assetexec"
	"github.com/lightninglabs/loop/swapkit/bridge"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

func testDeposit(t *testing.T) bridge.Deposit {
	t.Helper()
	_, pub := btcec.PrivKeyFromBytes([]byte{1})
	var group [33]byte
	copy(group[:], pub.SerializeCompressed())
	return bridge.Deposit{
		Outpoint: wire.OutPoint{Index: 1}.String(), GroupKey: group,
		Amount: 1000, ConfirmationHeight: 3,
		Proof: []byte(
			"authenticated proof",
		), Contract: []byte("contract"),
	}
}

func testSwap(d bridge.Deposit, id byte) *Job {
	requestID := [32]byte{id}
	return &Job{
		ID: requestID, ServerRequest: []byte("exact original request"),
		Swap: &SwapRequest{
			RequestID: requestID, GroupKey: d.GroupKey,
			Outpoint: d.Outpoint,
			Amount:   d.Amount,
			MaxDebit: d.Amount,
		},
	}
}

func TestOversizedProofDoesNotReserveDeposit(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "inventory.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	d := testDeposit(t)
	d.Proof = make([]byte, bridge.PublicRPCMaxBytes)
	require.NoError(t, store.addDeposit(d))
	manager := &Manager{cfg: Config{Store: store}}
	require.ErrorIs(t, manager.Start(t.Context(), *testSwap(d, 1).Swap),
		assetexec.ErrInvalid)
	jobs, err := store.Jobs(t.Context())
	require.NoError(t, err)
	require.Empty(t, jobs)
	entries, err := store.Inventory(t.Context())
	require.NoError(t, err)
	require.Zero(t, entries[0].RequestID)
	require.Zero(t, entries[0].Hash)
	require.False(t, entries[0].Consumed)
}

func TestWholeDepositAllowsBoundedNativeFee(t *testing.T) {
	for _, test := range []struct {
		name               string
		payout, fee, debit uint64
		valid              bool
	}{
		{"zero fee", 1000, 0, 1000, true},
		{"exact fee cap", 990, 10, 990, true},
		{"larger fee cap", 990, 11, 995, true},
		{"fee exceeds cap", 990, 9, 1000, false},
		{"payout exceeds deposit", 1001, 1000, 1001, false},
		{"zero payout", 0, 1000, 1000, false},
		{"debit below payout", 990, 10, 989, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, err := OpenStore(
				filepath.Join(t.TempDir(), "inventory.db"),
			)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			d := testDeposit(t)
			require.NoError(t, store.addDeposit(d))
			request := *testSwap(d, 1).Swap
			request.Amount, request.MaxFee = test.payout, test.fee
			request.MaxDebit = test.debit
			manager := &Manager{cfg: Config{Store: store}}
			err = manager.Start(t.Context(), request)
			if !test.valid {
				require.ErrorIs(t, err, assetexec.ErrInvalid)
				job := testSwap(d, 1)
				job.Swap = &request
				require.ErrorIs(
					t,
					store.PutJob(t.Context(), job),
					assetexec.ErrConflict,
				)
				entries, err := store.Inventory(t.Context())
				require.NoError(t, err)
				require.Zero(t, entries[0].RequestID)
				return
			}
			require.NoError(t, err)
			job, err := store.getJob(t.Context(), request.RequestID)
			require.NoError(t, err)
			var rpc swapserverrpc.CreateAssetLoopInRequest
			require.NoError(
				t,
				proto.Unmarshal(job.ServerRequest, &rpc),
			)
			require.Equal(t, test.payout, rpc.PayoutAmount)
			require.Equal(t, test.fee, rpc.MaxAssetFee)
			require.Equal(t, test.debit, rpc.MaxAssetDebit)
			require.Len(t, rpc.Deposits, 1)
			require.Equal(t, d.Proof, rpc.Deposits[0].Proof)
		})
	}
}

func TestReservationSurvivesRestartAndCompetingRequests(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inventory.db")
	store, err := OpenStore(path)
	require.NoError(t, err)
	d := testDeposit(t)
	require.NoError(t, store.addDeposit(d))
	first, second := testSwap(d, 1), testSwap(d, 2)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, job := range []*Job{first, second} {
		wg.Go(func() { results <- store.PutJob(t.Context(), job) })
	}
	wg.Wait()
	close(results)
	var succeeded, conflicted int
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, assetexec.ErrConflict):
			conflicted++
		default:
			t.Fatal(err)
		}
	}
	require.Equal(t, 1, succeeded)
	require.Equal(t, 1, conflicted)
	require.NoError(t, store.Close())
	store, err = OpenStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	jobs, err := store.Jobs(t.Context())
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	inventory, err := store.Inventory(t.Context())
	require.NoError(t, err)
	require.Equal(t, jobs[0].ID, inventory[0].RequestID)
	require.Equal(
		t,
		[]byte("exact original request"),
		jobs[0].ServerRequest,
	)

	// An ambiguous caller response replays the stored bytes, even if the
	// caller has acquired a refreshed proof since its original invocation.
	retry := jobs[0]
	retry.ServerRequest = []byte("different refreshed proof")
	require.NoError(t, store.PutJob(t.Context(), &retry))
	stored, err := store.getJob(t.Context(), retry.ID)
	require.NoError(t, err)
	require.Equal(t, jobs[0].ServerRequest, stored.ServerRequest)
	retry.Swap.Amount--
	require.ErrorIs(
		t,
		store.PutJob(t.Context(), &retry),
		assetexec.ErrConflict,
	)
}

func TestFailedOfferCanReserveAgainButConsumedAnchorCannot(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "inventory.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	d := testDeposit(t)
	require.NoError(t, store.addDeposit(d))
	first := testSwap(d, 1)
	require.NoError(t, store.PutJob(t.Context(), first))
	offer := assetexec.Offer{
		Hash: [32]byte{11}, Terms: assetexec.Terms{
			RequestID:        first.ID,
			DepositOutpoints: []string{d.Outpoint},
		},
	}
	require.NoError(t, store.bind(t.Context(), offer, false, false))
	require.NoError(t, store.bind(t.Context(), offer, true, false))
	second := testSwap(d, 2)
	require.NoError(t, store.PutJob(t.Context(), second))
	// Replay the old controller's cleanup after a new request took the
	// inventory lock; its durable receipt must not release the new owner.
	require.NoError(t, store.bind(t.Context(), offer, true, false))
	entries, err := store.Inventory(t.Context())
	require.NoError(t, err)
	require.Equal(t, second.ID, entries[0].RequestID)
	offer.Hash = [32]byte{22}
	offer.Terms.RequestID = second.ID
	require.NoError(t, store.bind(t.Context(), offer, false, false))
	require.NoError(t, store.bind(t.Context(), offer, true, true))
	require.ErrorIs(t, store.PutJob(t.Context(), testSwap(d, 3)),
		assetexec.ErrConflict)
}

type runtimeFunc func(context.Context, bridge.Method, any, any) error

func (f runtimeFunc) Call(ctx context.Context, method bridge.Method,
	input, output any) error {

	return f(ctx, method, input, output)
}

func TestDepositLostResponseReplaysPersistedFundingIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inventory.db")
	store, err := OpenStore(path)
	require.NoError(t, err)
	d := testDeposit(t)
	job := &Job{
		ID: [32]byte{7}, Deposit: &bridge.CreateDepositRequest{
			RequestID: [32]byte{
				7,
			}, GroupKey: d.GroupKey, Amount: d.Amount,
		},
	}
	require.NoError(t, store.PutJob(t.Context(), job))
	var calls int
	var original []byte
	runtime := runtimeFunc(func(_ context.Context, method bridge.Method,
		input, output any) error {

		require.Equal(t, bridge.CreateDeposit, method)
		raw, err := json.Marshal(input)
		require.NoError(t, err)
		calls++
		if calls == 1 {
			original = raw
			return errors.New("response lost after publish")
		}
		require.Equal(t, original, raw)
		*(output.(*bridge.Deposit)) = d
		return nil
	})
	manager := &Manager{cfg: Config{
		Store: store, Runtime: runtime, Deposits: &deposit.Manager{},
	}}
	require.Error(t, manager.resume(t.Context(), job))
	require.NoError(t, store.Close())
	store, err = OpenStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	manager.cfg.Store = store
	job, err = store.getJob(t.Context(), job.ID)
	require.NoError(t, err)
	require.False(t, job.Complete)
	require.NoError(t, manager.resume(t.Context(), job))
	require.True(t, job.Complete)
	inventory, err := store.Inventory(t.Context())
	require.NoError(t, err)
	require.Len(t, inventory, 1)
	require.Equal(t, d.Outpoint, inventory[0].Deposit.Outpoint)
}

func TestOriginalRefundCannotRacePendingOffer(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "inventory.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	d := testDeposit(t)
	d.RefundHeight = 100
	require.NoError(t, store.addDeposit(d))
	var height uint32 = 99
	manager := &Manager{cfg: Config{
		Store: store, Runtime: runtimeFunc(func(_ context.Context,
			method bridge.Method, _, output any) error {

			require.Equal(t, bridge.Height, method)
			*(output.(*uint32)) = height
			return nil
		}),
	}}
	_, err = manager.RefundDeposit(t.Context(), d.Outpoint)
	require.ErrorIs(t, err, assetexec.ErrPending)
	pending := testSwap(d, 1)
	require.NoError(t, store.PutJob(t.Context(), pending))
	height = 100
	require.NoError(t, manager.expireDeposits(t.Context()))
	_, err = manager.RefundDeposit(t.Context(), d.Outpoint)
	require.ErrorIs(t, err, assetexec.ErrConflict)
	jobs, err := store.Jobs(t.Context())
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	require.Nil(t, jobs[0].Refund)
}

func TestOriginalRefundPersistsUntilConfirmedProof(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inventory.db")
	store, err := OpenStore(path)
	require.NoError(t, err)
	d := testDeposit(t)
	d.RefundHeight = 100
	require.NoError(t, store.addDeposit(d))
	confirmed := false
	manager := &Manager{cfg: Config{
		Store: store, Runtime: runtimeFunc(func(_ context.Context,
			method bridge.Method, input, output any) error {

			switch method {
			case bridge.Height:
				*(output.(*uint32)) = 100
			case bridge.RefundDeposit:
				require.Equal(t, bridge.RefundDepositRequest{
					Outpoint: d.Outpoint,
				}, input)
				if !confirmed {
					return bridge.ErrPending
				}
				*(output.(*[]byte)) = []byte(
					"confirmed refund proof",
				)
			default:
				t.Fatalf("unexpected method %s", method)
			}
			return nil
		}),
	}}
	require.NoError(t, manager.expireDeposits(t.Context()))
	jobs, err := store.Jobs(t.Context())
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	require.ErrorIs(t, manager.resume(t.Context(), &jobs[0]),
		bridge.ErrPending)
	require.ErrorIs(t, store.PutJob(t.Context(), testSwap(d, 1)),
		assetexec.ErrConflict)
	require.NoError(t, store.Close())
	store, err = OpenStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	manager.cfg.Store = store
	jobs, err = store.Jobs(t.Context())
	require.NoError(t, err)
	confirmed = true
	require.NoError(t, manager.resume(t.Context(), &jobs[0]))
	require.True(t, jobs[0].Complete)
	require.NotEmpty(t, jobs[0].RefundProof)
	inventory, err := store.Inventory(t.Context())
	require.NoError(t, err)
	require.True(t, inventory[0].Consumed)
	require.Equal(t, [32]byte{}, inventory[0].RequestID)
	id, err := manager.RefundDeposit(t.Context(), d.Outpoint)
	require.NoError(t, err)
	require.Equal(t, jobs[0].ID, id)
}

func TestExpiredOfferBeforeControllerCreationReleasesSafely(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "inventory.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	journal, err := assetexec.OpenBoltStore(
		filepath.Join(dir, "journal.db"),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, journal.Close()) })
	d := testDeposit(t)
	require.NoError(t, store.addDeposit(d))
	job := testSwap(d, 1)
	require.NoError(t, store.PutJob(t.Context(), job))
	job.Offer = &assetexec.Offer{Hash: [32]byte{9}}
	require.NoError(t, store.updateJob(t.Context(), job))
	manager := &Manager{cfg: Config{Store: store, Journal: journal}}
	require.NoError(t, manager.resume(t.Context(), job))
	require.True(t, job.Canceled)
	inventory, err := store.Inventory(t.Context())
	require.NoError(t, err)
	require.Equal(t, [32]byte{}, inventory[0].RequestID)
	require.False(t, inventory[0].Consumed)
	require.NoError(t, store.PutJob(t.Context(), testSwap(d, 2)))
}

type lostOfferServer struct {
	assetclient.Server
	calls int
}

func (s *lostOfferServer) CreateAssetLoopIn(_ context.Context,
	_ *swapserverrpc.CreateAssetLoopInRequest, _ ...grpc.CallOption) (
	*swapserverrpc.AssetLoopInStatus, error) {

	s.calls++
	return nil, errors.New("response lost after server offer creation")
}

func TestLostOfferResponseRefundsOnlyAfterOriginalMaturity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inventory.db")
	store, err := OpenStore(path)
	require.NoError(t, err)
	journal, err := assetexec.OpenBoltStore(
		filepath.Join(dir, "journal.db"),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, journal.Close()) })
	d := testDeposit(t)
	d.RefundHeight = 100
	require.NoError(t, store.addDeposit(d))
	var height uint32 = 99
	server := &lostOfferServer{}
	manager := &Manager{cfg: Config{
		Store: store, Journal: journal, Owner: [32]byte{3},
		FetchL402: func(context.Context) error { return nil },
		Runtime: runtimeFunc(
			func(_ context.Context, method bridge.Method,
				_, output any) error {

				require.Equal(t, bridge.Height, method)
				*(output.(*uint32)) = height
				return nil
			},
		),
	}, Session: &assetclient.Session{Adapter: &assetclient.Adapter{
		Server: server,
	}}}
	request := *testSwap(d, 1).Swap
	require.NoError(t, manager.Start(t.Context(), request))
	job, err := store.getJob(t.Context(), request.RequestID)
	require.NoError(t, err)
	require.Error(t, manager.resume(t.Context(), job))
	require.Equal(t, 1, server.calls)
	require.Nil(t, job.Offer)
	_, err = manager.RefundDeposit(t.Context(), d.Outpoint)
	require.ErrorIs(t, err, assetexec.ErrPending)
	require.NoError(t, store.Close())
	store, err = OpenStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	manager.cfg.Store = store
	job, err = store.getJob(t.Context(), request.RequestID)
	require.NoError(t, err)
	height = 100
	require.NoError(t, manager.resume(t.Context(), job))
	require.True(t, job.Canceled)
	require.Equal(t, 1, server.calls, "never recreate a canceled offer")
	require.NoError(t, manager.expireDeposits(t.Context()))
	jobs, err := store.Jobs(t.Context())
	require.NoError(t, err)
	require.Len(t, jobs, 2)
}

type activeJournal struct {
	assetexec.Store
	record *assetexec.Record
}

func (s activeJournal) ListActive(context.Context) (
	[]*assetexec.Record, error) {

	return []*assetexec.Record{s.record}, nil
}

func TestUnknownOfferCannotReleaseExposedJournal(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "inventory.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	d := testDeposit(t)
	d.RefundHeight = 100
	require.NoError(t, store.addDeposit(d))
	job := testSwap(d, 1)
	require.NoError(t, store.PutJob(t.Context(), job))
	manager := &Manager{cfg: Config{
		Store: store, Journal: activeJournal{record: &assetexec.Record{
			SignaturesExposed: true, Offer: assetexec.Offer{
				Terms: assetexec.Terms{RequestID: job.ID},
			},
		}},
		Runtime: runtimeFunc(func(_ context.Context, _ bridge.Method,
			_, output any) error {

			*(output.(*uint32)) = 100
			return nil
		}),
	}}
	_, err = manager.cancelExpiredUnknownOffer(t.Context(), job)
	require.ErrorIs(t, err, assetexec.ErrState)
	inventory, err := store.Inventory(t.Context())
	require.NoError(t, err)
	require.Equal(t, job.ID, inventory[0].RequestID)
	require.False(t, job.Canceled)
}
