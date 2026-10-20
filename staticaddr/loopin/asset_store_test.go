package loopin

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/loopin/assetclient"
	"github.com/lightninglabs/loop/staticaddr/loopin/assetexec"
	"github.com/lightninglabs/loop/swapkit"
	assetkit "github.com/lightninglabs/loop/swapkit/tapsdk"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

func testDeposit(t *testing.T) swapkit.Deposit {
	t.Helper()
	_, pub := btcec.PrivKeyFromBytes([]byte{1})
	var group [33]byte
	copy(group[:], pub.SerializeCompressed())
	return swapkit.Deposit{
		Outpoint: wire.OutPoint{Index: 1}.String(), GroupKey: group,
		Amount: 1000, ConfirmationHeight: 3,
		Proof: []byte(
			"authenticated proof",
		), Contract: []byte("contract"),
	}
}

func testSwap(d swapkit.Deposit, id byte) *AssetJob {
	requestID := [32]byte{id}
	return &AssetJob{
		ID: requestID, ServerRequest: []byte("exact original request"),
		Swap: &AssetSwapRequest{
			RequestID: requestID, GroupKey: d.GroupKey,
			Outpoint: d.Outpoint,
			Amount:   d.Amount,
			MaxDebit: d.Amount,
		},
	}
}

func TestOversizedProofDoesNotReserveDeposit(t *testing.T) {
	store, err := openAssetTestStore(filepath.Join(t.TempDir(), "inventory.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	d := testDeposit(t)
	d.Proof = make([]byte, swapkit.PublicRPCMaxBytes)
	require.NoError(t, store.AddAssetDeposit(t.Context(), d))
	manager := &Manager{assetCfg: &AssetConfig{Store: store}}
	require.ErrorIs(
		t,
		manager.StartAssetLoopIn(t.Context(), *testSwap(d, 1).Swap),
		assetexec.ErrInvalid,
	)
	jobs, err := store.AssetJobs(t.Context())
	require.NoError(t, err)
	require.Empty(t, jobs)
	entries, err := store.AssetInventory(t.Context())
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
			store, err := openAssetTestStore(
				filepath.Join(t.TempDir(), "inventory.db"),
			)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			d := testDeposit(t)
			require.NoError(t, store.AddAssetDeposit(t.Context(), d))
			request := *testSwap(d, 1).Swap
			request.Amount, request.MaxFee = test.payout, test.fee
			request.MaxDebit = test.debit
			manager := &Manager{assetCfg: &AssetConfig{Store: store}}
			err = manager.StartAssetLoopIn(t.Context(), request)
			if !test.valid {
				require.ErrorIs(t, err, assetexec.ErrInvalid)
				job := testSwap(d, 1)
				job.Swap = &request
				require.ErrorIs(
					t,
					store.PutAssetJob(t.Context(), job),
					assetexec.ErrConflict,
				)
				entries, err := store.AssetInventory(t.Context())
				require.NoError(t, err)
				require.Zero(t, entries[0].RequestID)
				return
			}
			require.NoError(t, err)
			job, err := store.GetAssetJob(t.Context(), request.RequestID)
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
	store, err := openAssetTestStore(path)
	require.NoError(t, err)
	d := testDeposit(t)
	require.NoError(t, store.AddAssetDeposit(t.Context(), d))
	first, second := testSwap(d, 1), testSwap(d, 2)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, job := range []*AssetJob{first, second} {
		wg.Go(func() { results <- store.PutAssetJob(t.Context(), job) })
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
	store, err = openAssetTestStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	jobs, err := store.AssetJobs(t.Context())
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	inventory, err := store.AssetInventory(t.Context())
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
	require.NoError(t, store.PutAssetJob(t.Context(), &retry))
	stored, err := store.GetAssetJob(t.Context(), retry.ID)
	require.NoError(t, err)
	require.Equal(t, jobs[0].ServerRequest, stored.ServerRequest)
	retry.Swap.Amount--
	require.ErrorIs(
		t,
		store.PutAssetJob(t.Context(), &retry),
		assetexec.ErrConflict,
	)
}

func TestAssetReleaseCommitsWithRecoveryRecord(t *testing.T) {
	store, err := openAssetTestStore(filepath.Join(t.TempDir(), "loop.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	d := testDeposit(t)
	require.NoError(t, store.AddAssetDeposit(t.Context(), d))
	job := testSwap(d, 1)
	require.NoError(t, store.PutAssetJob(t.Context(), job))
	r := assetRecord(d, job.ID)
	job.Offer = &r.Offer
	job.Intent = r.InvoiceIntent
	require.NoError(t, store.UpdateAssetJob(t.Context(), job))
	require.NoError(t, store.Create(t.Context(), r))
	r.CancellationRequested = true
	r.State = assetexec.Failed
	r.Revision++
	require.NoError(t, store.Update(t.Context(), r, 0))
	// A terminal record alone does not release a legacy reservation. Its
	// cleanup checkpoint and ownership advance together.
	require.ErrorIs(
		t,
		store.PutAssetJob(t.Context(), testSwap(d, 2)),
		assetexec.ErrConflict,
	)
	r.DepositsReleased = true
	r.Revision++
	require.NoError(t, store.Update(t.Context(), r, 1))
	require.NoError(t, store.PutAssetJob(t.Context(), testSwap(d, 2)))
	entries, err := store.AssetInventory(t.Context())
	require.NoError(t, err)
	require.Equal(t, [32]byte{2}, entries[0].RequestID)
}

func assetRecord(d swapkit.Deposit, id [32]byte) *assetexec.Record {
	return &assetexec.Record{
		State:         assetexec.InitHtlcTx,
		DepositState:  "Deposited",
		InvoiceIntent: []byte("intent"),
		Offer: assetexec.Offer{
			Hash: [32]byte{8},
			Terms: assetexec.Terms{
				Version:          1,
				RequestID:        id,
				RequestDigest:    [32]byte{3},
				Owner:            [32]byte{4},
				GroupKey:         d.GroupKey,
				DepositOutpoints: []string{d.Outpoint},
				DepositAmount:    d.Amount,
				PayoutAmount:     d.Amount,
				MaxDebit:         d.Amount,
				RefundHeight:     100,
				MinPayoutExpiry:  121,
				RecoveryDelta:    10,
				ExecutionDelta:   10,
				OfferExpiry:      time.Now().Add(time.Hour),
				Contract:         d.Contract,
			},
		},
	}
}

type assetTestDriver struct {
	AssetDriver

	height func() uint32
	create func(swapkit.CreateDepositRequest) (swapkit.Deposit, error)
	refund func(swapkit.RefundDepositRequest) ([]byte, error)
}

func (d assetTestDriver) Height(context.Context) (uint32, error) {
	return d.height(), nil
}
func (d assetTestDriver) CreateDeposit(_ context.Context,
	request swapkit.CreateDepositRequest, _ *assetkit.DepositFunding,
	_ func() error) (swapkit.Deposit, error) {

	return d.create(request)
}
func (d assetTestDriver) RefundDeposit(_ context.Context,
	request swapkit.RefundDepositRequest, _ *assetkit.DepositFunding,
	_ func() error) ([]byte, error) {

	return d.refund(request)
}

func TestDepositLostResponseReplaysPersistedFundingIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inventory.db")
	store, err := openAssetTestStore(path)
	require.NoError(t, err)
	d := testDeposit(t)
	job := &AssetJob{
		ID: [32]byte{7}, Deposit: &swapkit.CreateDepositRequest{
			RequestID: [32]byte{
				7,
			}, GroupKey: d.GroupKey, Amount: d.Amount,
		},
	}
	require.NoError(t, store.PutAssetJob(t.Context(), job))
	var calls int
	var original []byte
	driver := assetTestDriver{create: func(
		input swapkit.CreateDepositRequest) (swapkit.Deposit, error) {

		raw, err := json.Marshal(input)
		require.NoError(t, err)
		calls++
		if calls == 1 {
			original = raw
			return swapkit.Deposit{}, errors.New("response lost after publish")
		}
		require.Equal(t, original, raw)
		return d, nil
	}}
	manager := &Manager{assetCfg: &AssetConfig{
		Store: store, Driver: driver, Deposits: &deposit.Manager{},
	}}
	require.Error(t, manager.resumeAsset(t.Context(), job))
	require.NoError(t, store.Close())
	store, err = openAssetTestStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	manager.assetCfg.Store = store
	job, err = store.GetAssetJob(t.Context(), job.ID)
	require.NoError(t, err)
	require.False(t, job.Complete)
	require.NoError(t, manager.resumeAsset(t.Context(), job))
	require.True(t, job.Complete)
	inventory, err := store.AssetInventory(t.Context())
	require.NoError(t, err)
	require.Len(t, inventory, 1)
	require.Equal(t, d.Outpoint, inventory[0].Deposit.Outpoint)
}

func TestOriginalRefundCannotRacePendingOffer(t *testing.T) {
	store, err := openAssetTestStore(filepath.Join(t.TempDir(), "inventory.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	d := testDeposit(t)
	d.RefundHeight = 100
	require.NoError(t, store.AddAssetDeposit(t.Context(), d))
	var height uint32 = 99
	manager := &Manager{assetCfg: &AssetConfig{
		Store: store, Driver: assetTestDriver{
			height: func() uint32 { return height },
		},
	}}
	_, err = manager.RefundAssetDeposit(t.Context(), d.Outpoint)
	require.ErrorIs(t, err, assetexec.ErrPending)
	pending := testSwap(d, 1)
	require.NoError(t, store.PutAssetJob(t.Context(), pending))
	height = 100
	require.NoError(t, manager.expireDeposits(t.Context()))
	_, err = manager.RefundAssetDeposit(t.Context(), d.Outpoint)
	require.ErrorIs(t, err, assetexec.ErrConflict)
	jobs, err := store.AssetJobs(t.Context())
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	require.Nil(t, jobs[0].Refund)
}

func TestOriginalRefundPersistsUntilConfirmedProof(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inventory.db")
	store, err := openAssetTestStore(path)
	require.NoError(t, err)
	d := testDeposit(t)
	d.RefundHeight = 100
	require.NoError(t, store.AddAssetDeposit(t.Context(), d))
	confirmed := false
	manager := &Manager{assetCfg: &AssetConfig{
		Store: store, Driver: assetTestDriver{
			height: func() uint32 { return 100 },
			refund: func(request swapkit.RefundDepositRequest) ([]byte, error) {
				require.Equal(t, swapkit.RefundDepositRequest{
					Outpoint: d.Outpoint,
				}, request)
				if !confirmed {
					return nil, swapkit.ErrPending
				}
				return []byte("confirmed refund proof"), nil
			},
		},
	}}
	require.NoError(t, manager.expireDeposits(t.Context()))
	jobs, err := store.AssetJobs(t.Context())
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	require.ErrorIs(t, manager.resumeAsset(t.Context(), &jobs[0]),
		swapkit.ErrPending)
	require.ErrorIs(t, store.PutAssetJob(t.Context(), testSwap(d, 1)),
		assetexec.ErrConflict)
	require.NoError(t, store.Close())
	store, err = openAssetTestStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	manager.assetCfg.Store = store
	jobs, err = store.AssetJobs(t.Context())
	require.NoError(t, err)
	confirmed = true
	require.NoError(t, manager.resumeAsset(t.Context(), &jobs[0]))
	require.True(t, jobs[0].Complete)
	require.NotEmpty(t, jobs[0].RefundProof)
	inventory, err := store.AssetInventory(t.Context())
	require.NoError(t, err)
	require.True(t, inventory[0].Consumed)
	require.Equal(t, [32]byte{}, inventory[0].RequestID)
	id, err := manager.RefundAssetDeposit(t.Context(), d.Outpoint)
	require.NoError(t, err)
	require.Equal(t, jobs[0].ID, id)
}

func TestExpiredOfferBeforeControllerCreationReleasesSafely(t *testing.T) {
	dir := t.TempDir()
	store, err := openAssetTestStore(filepath.Join(dir, "inventory.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	d := testDeposit(t)
	require.NoError(t, store.AddAssetDeposit(t.Context(), d))
	job := testSwap(d, 1)
	require.NoError(t, store.PutAssetJob(t.Context(), job))
	job.Offer = &assetexec.Offer{Hash: [32]byte{9}}
	require.NoError(t, store.UpdateAssetJob(t.Context(), job))
	manager := &Manager{assetCfg: &AssetConfig{Store: store}}
	require.NoError(t, manager.resumeAsset(t.Context(), job))
	require.True(t, job.Canceled)
	inventory, err := store.AssetInventory(t.Context())
	require.NoError(t, err)
	require.Equal(t, [32]byte{}, inventory[0].RequestID)
	require.False(t, inventory[0].Consumed)
	require.NoError(t, store.PutAssetJob(t.Context(), testSwap(d, 2)))
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
	store, err := openAssetTestStore(path)
	require.NoError(t, err)
	d := testDeposit(t)
	d.RefundHeight = 100
	require.NoError(t, store.AddAssetDeposit(t.Context(), d))
	var height uint32 = 99
	server := &lostOfferServer{}
	manager := &Manager{assetCfg: &AssetConfig{
		Store: store, Owner: [32]byte{3},
		FetchL402: func(context.Context) error { return nil },
		Driver:    assetTestDriver{height: func() uint32 { return height }},
	}, assetSession: &assetclient.Session{Adapter: &assetclient.Adapter{
		Server: server,
	}}}
	request := *testSwap(d, 1).Swap
	require.NoError(t, manager.StartAssetLoopIn(t.Context(), request))
	job, err := store.GetAssetJob(t.Context(), request.RequestID)
	require.NoError(t, err)
	require.Error(t, manager.resumeAsset(t.Context(), job))
	require.Equal(t, 1, server.calls)
	require.Nil(t, job.Offer)
	_, err = manager.RefundAssetDeposit(t.Context(), d.Outpoint)
	require.ErrorIs(t, err, assetexec.ErrPending)
	require.NoError(t, store.Close())
	store, err = openAssetTestStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	manager.assetCfg.Store = store
	job, err = store.GetAssetJob(t.Context(), request.RequestID)
	require.NoError(t, err)
	height = 100
	require.NoError(t, manager.resumeAsset(t.Context(), job))
	require.True(t, job.Canceled)
	require.Equal(t, 1, server.calls, "never recreate a canceled offer")
	require.NoError(t, manager.expireDeposits(t.Context()))
	jobs, err := store.AssetJobs(t.Context())
	require.NoError(t, err)
	require.Len(t, jobs, 2)
}

type activeJournal struct {
	AssetStore

	record *assetexec.Record
}

func (s activeJournal) ListActive(context.Context) (
	[]*assetexec.Record, error) {

	return []*assetexec.Record{s.record}, nil
}

func TestUnknownOfferCannotReleaseExposedJournal(t *testing.T) {
	store, err := openAssetTestStore(filepath.Join(t.TempDir(), "inventory.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	d := testDeposit(t)
	d.RefundHeight = 100
	require.NoError(t, store.AddAssetDeposit(t.Context(), d))
	job := testSwap(d, 1)
	require.NoError(t, store.PutAssetJob(t.Context(), job))
	manager := &Manager{assetCfg: &AssetConfig{
		Store: activeJournal{AssetStore: store, record: &assetexec.Record{
			SignaturesExposed: true, Offer: assetexec.Offer{
				Terms: assetexec.Terms{RequestID: job.ID},
			},
		}},
		Driver: assetTestDriver{height: func() uint32 { return 100 }},
	}}
	_, err = manager.cancelExpiredUnknownOffer(t.Context(), job)
	require.ErrorIs(t, err, assetexec.ErrState)
	inventory, err := store.AssetInventory(t.Context())
	require.NoError(t, err)
	require.Equal(t, job.ID, inventory[0].RequestID)
	require.False(t, job.Canceled)
}

// assetTestStore opens the same migrated SQLite store used by loopd, including
// synchronous full fsync and foreign-key checks.
type assetTestStore struct {
	*SqlStore

	db *loopdb.SqliteSwapStore
}

func openAssetTestStore(path string) (*assetTestStore, error) {
	db, err := loopdb.NewSqliteStore(
		&loopdb.SqliteConfig{DatabaseFileName: path},
		&chaincfg.RegressionNetParams,
	)
	if err != nil {
		return nil, err
	}
	return &assetTestStore{
		SqlStore: NewSqlStore(
			loopdb.NewTypedStore[Querier](db),
			clock.NewDefaultClock(),
			&chaincfg.RegressionNetParams,
		),
		db: db,
	}, nil
}
func (s *assetTestStore) Close() error { return s.db.Close() }
