package loopin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMonitorInvoiceAndHtlcTxRemapsReclaimedDeposits tests that a swap whose
// payment deadline passed before its htlc confirmed, and whose deposits were
// then selected by another swap, maps the deposits back to itself when its
// htlc confirms after all. Otherwise the deposits stay mapped to the other
// swap, which doesn't spend them.
func TestMonitorInvoiceAndHtlcTxRemapsReclaimedDeposits(t *testing.T) {
	// Create the database before the timeout starts, since creating a
	// Postgres database takes a while.
	testDb := loopdb.NewTestDB(t)
	defer testDb.Close()

	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	depositStore := deposit.NewSqlStore(testDb.BaseDB)
	swapStore := NewSqlStore(
		loopdb.NewTypedStore[Querier](testDb),
		clock.NewTestClock(time.Now()), &chaincfg.RegressionNetParams,
	)

	newDeposit := func(hash byte) *deposit.Deposit {
		depositID, err := deposit.GetRandomDepositID()
		require.NoError(t, err)
		d := &deposit.Deposit{
			ID: depositID,
			OutPoint: wire.OutPoint{
				Hash:  chainhash.Hash{hash},
				Index: 0,
			},
			Value:                btcutil.Amount(500_000),
			TimeOutSweepPkScript: []byte{0x00, 0x14, 0x01},
		}
		d.SetState(deposit.LoopingIn)
		require.NoError(t, depositStore.CreateDeposit(ctx, d))

		return d
	}

	// Swap A selects d1 and d2. Swap B later selects d2 and d3.
	d1, d2, d3 := newDeposit(7), newDeposit(8), newDeposit(9)

	mockLnd := test.NewMockLnd()
	_, clientPubKey := test.CreateKey(1)
	_, serverPubKey := test.CreateKey(2)
	addr, err := btcutil.DecodeAddress(P2wkhAddr, nil)
	require.NoError(t, err)

	newLoopIn := func(swapHash lntypes.Hash,
		deposits ...*deposit.Deposit) *StaticAddressLoopIn {

		outpoints := make([]string, 0, len(deposits))
		for _, d := range deposits {
			outpoints = append(outpoints, d.OutPoint.String())
		}

		loopIn := &StaticAddressLoopIn{
			SwapHash:                swapHash,
			SwapPreimage:            lntypes.Preimage(swapHash),
			HtlcCltvExpiry:          2_000,
			InitiationHeight:        uint32(mockLnd.Height),
			InitiationTime:          time.Now().Add(-time.Hour),
			ProtocolVersion:         version.ProtocolVersion_V0,
			ClientPubkey:            clientPubKey,
			ServerPubkey:            serverPubKey,
			HtlcTimeoutSweepAddress: addr,
			PaymentTimeoutSeconds:   1,
			DepositOutpoints:        outpoints,
			Deposits:                deposits,
		}
		loopIn.SetState(MonitorInvoiceAndHtlcTx)

		return loopIn
	}

	swapA := newLoopIn(lntypes.Hash{0xa}, d1, d2)
	require.NoError(t, swapStore.CreateLoopIn(ctx, swapA))

	// The server accepted the risk an hour ago, so the payment deadline has
	// passed. The decision is set directly instead of being sent, since a
	// sent decision is read back from the store with its stored time.
	swapA.ConfirmationRiskDecision = ConfirmationRiskDecisionAccepted
	swapA.ConfirmationRiskDecisionTime = time.Now().Add(-time.Hour)

	mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  swapA.SwapHash,
		State: invoices.ContractOpen,
	})

	// The deposit manager moves the deposits to the requested state.
	depositMgr := &recordingDepositManager{
		transitionChan: make(chan depositTransition, 2),
		transition: func(deposits []*deposit.Deposit, _ fsm.EventType,
			state fsm.StateType) error {

			for _, d := range deposits {
				d.SetState(state)
			}

			return nil
		},
	}

	cfg := &Config{
		AddressManager: &mockAddressManager{
			params: &script.Parameters{
				ClientPubkey:    clientPubKey,
				ServerPubkey:    serverPubKey,
				ProtocolVersion: version.ProtocolVersion_V0,
			},
		},
		ChainNotifier:       mockLnd.ChainNotifier,
		DepositManager:      depositMgr,
		InvoicesClient:      mockLnd.LndServices.Invoices,
		LndClient:           mockLnd.Client,
		ChainParams:         mockLnd.ChainParams,
		NotificationManager: &mockNotificationManager{},
		Store:               swapStore,
	}

	f, err := NewFSM(ctx, swapA, cfg, false)
	require.NoError(t, err)

	resultChan := make(chan fsm.EventType, 1)
	go func() {
		resultChan <- f.MonitorInvoiceAndHtlcTxAction(ctx, nil)
	}()

	select {
	case <-mockLnd.SingleInvoiceSubcribeChannel:
	case <-ctx.Done():
		t.Fatalf("invoice subscription not registered: %v", ctx.Err())
	}

	var confRegistration *test.ConfRegistration
	select {
	case confRegistration = <-mockLnd.RegisterConfChannel:
	case <-ctx.Done():
		t.Fatalf("htlc conf registration not received: %v", ctx.Err())
	}

	// The payment deadline passed before the htlc confirmed, so swap A
	// gives the deposits back.
	select {
	case <-mockLnd.FailInvoiceChannel:
	case <-ctx.Done():
		t.Fatalf("invoice was not canceled: %v", ctx.Err())
	}

	select {
	case transition := <-depositMgr.transitionChan:
		require.Equal(t, deposit.Deposited, transition.state)

	case <-ctx.Done():
		t.Fatalf("deposits were not given back: %v", ctx.Err())
	}

	// Swap B selects d2 and d3, which maps them to swap B.
	d2.SetState(deposit.LoopingIn)
	swapB := newLoopIn(lntypes.Hash{0xb}, d2, d3)
	require.NoError(t, swapStore.CreateLoopIn(ctx, swapB))

	// Swap A's htlc confirms after all, so swap A takes its deposits back
	// to sweep the htlc timeout path.
	confRegistration.ConfChan <- nil

	select {
	case transition := <-depositMgr.transitionChan:
		require.Equal(t, deposit.SweepHtlcTimeout, transition.state)

	case <-ctx.Done():
		t.Fatalf("deposits were not taken back: %v", ctx.Err())
	}

	// Swap A loads d1 and d2 from the store, and swap B only d3.
	depositIDs := func(swapHash lntypes.Hash) []deposit.ID {
		loaded, err := swapStore.GetLoopInByHash(ctx, swapHash)
		require.NoError(t, err)

		ids := make([]deposit.ID, 0, len(loaded.Deposits))
		for _, d := range loaded.Deposits {
			ids = append(ids, d.ID)
		}

		return ids
	}
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.ElementsMatch(
			c, []deposit.ID{d1.ID, d2.ID},
			depositIDs(swapA.SwapHash),
		)
	}, time.Second, 10*time.Millisecond)
	require.Equal(t, []deposit.ID{d3.ID}, depositIDs(swapB.SwapHash))

	cancel()
	select {
	case <-resultChan:
	case <-time.After(time.Second):
		t.Fatal("monitor action did not exit")
	}
}

// TestMonitorInvoiceAndHtlcTxMapsOnlyLockedDeposits tests that a swap whose
// invoice was canceled maps the deposits to itself when its htlc confirms only
// once they are locked for its htlc timeout sweep. A deposit that another swap
// completed with stays mapped to that swap, and a deposit that is already
// locked is mapped again, in case loopd stopped before mapping it.
func TestMonitorInvoiceAndHtlcTxMapsOnlyLockedDeposits(t *testing.T) {
	tests := []struct {
		name  string
		state fsm.StateType
		owner byte
	}{
		{
			name:  "completed by the other swap",
			state: deposit.LoopedIn,
			owner: 0xb,
		},
		{
			name:  "already locked for the htlc timeout sweep",
			state: deposit.SweepHtlcTimeout,
			owner: 0xa,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			testDb := loopdb.NewTestDB(t)
			defer testDb.Close()

			ctx, cancel := context.WithTimeout(
				t.Context(), testTimeout,
			)
			defer cancel()

			depositStore := deposit.NewSqlStore(testDb.BaseDB)
			swapStore := NewSqlStore(
				loopdb.NewTypedStore[Querier](testDb),
				clock.NewTestClock(time.Now()),
				&chaincfg.RegressionNetParams,
			)

			depositID, err := deposit.GetRandomDepositID()
			require.NoError(t, err)
			d := &deposit.Deposit{
				ID: depositID,
				OutPoint: wire.OutPoint{
					Hash:  chainhash.Hash{8},
					Index: 0,
				},
				Value:                btcutil.Amount(500_000),
				TimeOutSweepPkScript: []byte{0x00, 0x14, 0x01},
			}
			d.SetState(deposit.LoopingIn)
			require.NoError(t, depositStore.CreateDeposit(ctx, d))

			mockLnd := test.NewMockLnd()
			_, clientPubKey := test.CreateKey(1)
			_, serverPubKey := test.CreateKey(2)
			addr, err := btcutil.DecodeAddress(P2wkhAddr, nil)
			require.NoError(t, err)

			newLoopIn := func(swapHash lntypes.Hash) *StaticAddressLoopIn {
				loopIn := &StaticAddressLoopIn{
					SwapHash:       swapHash,
					SwapPreimage:   lntypes.Preimage(swapHash),
					HtlcCltvExpiry: 2_000,
					InitiationHeight: uint32(
						mockLnd.Height,
					),
					InitiationTime:          time.Now(),
					ProtocolVersion:         version.ProtocolVersion_V0,
					ClientPubkey:            clientPubKey,
					ServerPubkey:            serverPubKey,
					HtlcTimeoutSweepAddress: addr,
					DepositOutpoints: []string{
						d.OutPoint.String(),
					},
					Deposits: []*deposit.Deposit{d},
				}
				loopIn.SetState(MonitorInvoiceAndHtlcTx)

				return loopIn
			}

			// Swap A gave the deposit back, and swap B selected it.
			swapA := newLoopIn(lntypes.Hash{0xa})
			require.NoError(t, swapStore.CreateLoopIn(ctx, swapA))
			swapB := newLoopIn(lntypes.Hash{0xb})
			require.NoError(t, swapStore.CreateLoopIn(ctx, swapB))
			d.SetState(tc.state)

			// Swap A's invoice was canceled.
			mockLnd.SetInvoice(&lndclient.Invoice{
				Hash:  swapA.SwapHash,
				State: invoices.ContractCanceled,
			})

			// The deposit manager rejects moving a completed
			// deposit.
			depositMgr := &recordingDepositManager{
				transitionChan: make(chan depositTransition, 4),
				transition: func(deposits []*deposit.Deposit,
					_ fsm.EventType, state fsm.StateType) error {

					for _, d := range deposits {
						if d.IsInState(deposit.LoopedIn) {
							return errors.New("final state")
						}
						d.SetState(state)
					}

					return nil
				},
			}

			cfg := &Config{
				AddressManager: &mockAddressManager{
					params: &script.Parameters{
						ClientPubkey:    clientPubKey,
						ServerPubkey:    serverPubKey,
						ProtocolVersion: version.ProtocolVersion_V0,
					},
				},
				ChainNotifier:       mockLnd.ChainNotifier,
				DepositManager:      depositMgr,
				InvoicesClient:      mockLnd.LndServices.Invoices,
				LndClient:           mockLnd.Client,
				ChainParams:         mockLnd.ChainParams,
				NotificationManager: &mockNotificationManager{},
				Store:               swapStore,
			}

			f, err := NewFSM(ctx, swapA, cfg, false)
			require.NoError(t, err)

			resultChan := make(chan fsm.EventType, 1)
			go func() {
				resultChan <- f.MonitorInvoiceAndHtlcTxAction(
					ctx, nil,
				)
			}()

			var confRegistration *test.ConfRegistration
			select {
			case confRegistration = <-mockLnd.RegisterConfChannel:
			case <-ctx.Done():
				t.Fatalf("htlc conf registration not received: "+
					"%v", ctx.Err())
			}

			// Something confirms at swap A's htlc script.
			confRegistration.ConfChan <- nil

			owner := func() []byte {
				var owners []byte
				for _, hash := range []lntypes.Hash{
					swapA.SwapHash, swapB.SwapHash,
				} {
					loaded, err := swapStore.GetLoopInByHash(
						ctx, hash,
					)
					require.NoError(t, err)
					if len(loaded.Deposits) > 0 {
						owners = append(owners, hash[0])
					}
				}

				return owners
			}
			// A completed deposit can't be moved, so the monitor
			// retries once the attempt failed. A locked deposit
			// isn't moved again, and the monitor keeps running.
			if tc.state == deposit.LoopedIn {
				select {
				case event := <-resultChan:
					require.Equal(t, OnRecover, event)

				case <-ctx.Done():
					t.Fatalf("monitor action did not "+
						"return: %v", ctx.Err())
				}
				require.Equal(t, []byte{tc.owner}, owner())

				return
			}

			require.EventuallyWithT(t, func(c *assert.CollectT) {
				assert.Equal(c, []byte{tc.owner}, owner())
			}, 2*time.Second, 10*time.Millisecond)

			cancel()
			select {
			case <-resultChan:
			case <-time.After(3 * time.Second):
				t.Fatal("monitor action did not exit")
			}
		})
	}
}

// TestMonitorInvoiceAndHtlcTxNilDeposit tests that a nil deposit makes the
// htlc timeout transition fail before any deposit is moved, so that the
// monitor retries, instead of panicking or moving only the other deposits.
func TestMonitorInvoiceAndHtlcTxNilDeposit(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()

	mockLnd := test.NewMockLnd()
	_, clientPubKey := test.CreateKey(1)
	_, serverPubKey := test.CreateKey(2)

	d := &deposit.Deposit{
		OutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{9},
			Index: 0,
		},
		Value: btcutil.Amount(500_000),
	}
	d.SetState(deposit.LoopingIn)

	loopIn := &StaticAddressLoopIn{
		SwapHash:         lntypes.Hash{0xc},
		HtlcCltvExpiry:   2_000,
		InitiationHeight: uint32(mockLnd.Height),
		InitiationTime:   time.Now(),
		ProtocolVersion:  version.ProtocolVersion_V0,
		ClientPubkey:     clientPubKey,
		ServerPubkey:     serverPubKey,
		Deposits:         []*deposit.Deposit{d, nil},
	}
	loopIn.SetState(MonitorInvoiceAndHtlcTx)

	mockLnd.SetInvoice(&lndclient.Invoice{
		Hash:  loopIn.SwapHash,
		State: invoices.ContractCanceled,
	})

	depositMgr := &recordingDepositManager{}
	cfg := &Config{
		AddressManager: &mockAddressManager{
			params: &script.Parameters{
				ClientPubkey:    clientPubKey,
				ServerPubkey:    serverPubKey,
				ProtocolVersion: version.ProtocolVersion_V0,
			},
		},
		ChainNotifier:       mockLnd.ChainNotifier,
		DepositManager:      depositMgr,
		InvoicesClient:      mockLnd.LndServices.Invoices,
		LndClient:           mockLnd.Client,
		ChainParams:         mockLnd.ChainParams,
		NotificationManager: &mockNotificationManager{},
		Store:               &mockStore{},
	}

	f, err := NewFSM(ctx, loopIn, cfg, false)
	require.NoError(t, err)

	resultChan := make(chan fsm.EventType, 1)
	go func() {
		resultChan <- f.MonitorInvoiceAndHtlcTxAction(ctx, nil)
	}()

	select {
	case confRegistration := <-mockLnd.RegisterConfChannel:
		confRegistration.ConfChan <- nil

	case <-ctx.Done():
		t.Fatalf("htlc conf registration not received: %v", ctx.Err())
	}

	select {
	case event := <-resultChan:
		require.Equal(t, OnRecover, event)

	case <-ctx.Done():
		t.Fatalf("monitor action did not return: %v", ctx.Err())
	}

	// No deposit was moved, not even the one that isn't nil.
	require.Empty(t, depositMgr.transitions)
}

// failingMapStore is a store that fails to map deposits to swaps.
type failingMapStore struct {
	mockStore
}

// BatchMapDepositsToSwapHashes returns an error.
func (s *failingMapStore) BatchMapDepositsToSwapHashes(context.Context,
	map[deposit.ID]lntypes.Hash) error {

	return errors.New("map failed")
}

// TestMonitorInvoiceAndHtlcTxRetriesFailedMapping tests that the monitor
// retries if it can't map the deposits to the swap, both after locking them
// and if they were already locked, instead of moving on to the htlc timeout
// sweep.
func TestMonitorInvoiceAndHtlcTxRetriesFailedMapping(t *testing.T) {
	tests := []struct {
		name  string
		state fsm.StateType
	}{
		{
			name:  "after locking",
			state: deposit.LoopingIn,
		},
		{
			name:  "already locked",
			state: deposit.SweepHtlcTimeout,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(
				t.Context(), testTimeout,
			)
			defer cancel()

			mockLnd := test.NewMockLnd()
			_, clientPubKey := test.CreateKey(1)
			_, serverPubKey := test.CreateKey(2)

			d := &deposit.Deposit{
				OutPoint: wire.OutPoint{
					Hash:  chainhash.Hash{10},
					Index: 0,
				},
				Value: btcutil.Amount(500_000),
			}
			d.SetState(tc.state)

			loopIn := &StaticAddressLoopIn{
				SwapHash:         lntypes.Hash{0xd},
				HtlcCltvExpiry:   2_000,
				InitiationHeight: uint32(mockLnd.Height),
				InitiationTime:   time.Now(),
				ProtocolVersion:  version.ProtocolVersion_V0,
				ClientPubkey:     clientPubKey,
				ServerPubkey:     serverPubKey,
				Deposits:         []*deposit.Deposit{d},
			}
			loopIn.SetState(MonitorInvoiceAndHtlcTx)

			mockLnd.SetInvoice(&lndclient.Invoice{
				Hash:  loopIn.SwapHash,
				State: invoices.ContractCanceled,
			})

			depositMgr := &recordingDepositManager{
				transition: func(deposits []*deposit.Deposit,
					_ fsm.EventType, state fsm.StateType) error {

					for _, d := range deposits {
						d.SetState(state)
					}

					return nil
				},
			}

			cfg := &Config{
				AddressManager: &mockAddressManager{
					params: &script.Parameters{
						ClientPubkey:    clientPubKey,
						ServerPubkey:    serverPubKey,
						ProtocolVersion: version.ProtocolVersion_V0,
					},
				},
				ChainNotifier:       mockLnd.ChainNotifier,
				DepositManager:      depositMgr,
				InvoicesClient:      mockLnd.LndServices.Invoices,
				LndClient:           mockLnd.Client,
				ChainParams:         mockLnd.ChainParams,
				NotificationManager: &mockNotificationManager{},
				Store:               &failingMapStore{},
			}

			f, err := NewFSM(ctx, loopIn, cfg, false)
			require.NoError(t, err)

			resultChan := make(chan fsm.EventType, 1)
			go func() {
				resultChan <- f.MonitorInvoiceAndHtlcTxAction(
					ctx, nil,
				)
			}()

			select {
			case confRegistration := <-mockLnd.RegisterConfChannel:
				confRegistration.ConfChan <- nil

			case <-ctx.Done():
				t.Fatalf("htlc conf registration not received: "+
					"%v", ctx.Err())
			}

			select {
			case event := <-resultChan:
				require.Equal(t, OnRecover, event)

			case <-ctx.Done():
				t.Fatalf("monitor action did not return: %v",
					ctx.Err())
			}

			// The deposit stays locked for the htlc timeout sweep.
			require.True(t, d.IsInState(deposit.SweepHtlcTimeout))
		})
	}
}
