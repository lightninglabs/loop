package reservation

import (
	"context"
	"encoding/hex"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var (
	defaultReservationId = mustDecodeID("17cecc61ab4aafebdc0542dabdae0d0cb8907ec1c9c8ae387bc5a3309ca8b600")
)

func TestManager(t *testing.T) {
	ctxb := t.Context()

	testContext := newManagerTestContext(t)

	initChan := make(chan struct{})
	// Start the manager.
	go func() {
		err := testContext.manager.Run(ctxb, testContext.mockLnd.Height, initChan)
		require.NoError(t, err)
	}()

	// We'll now wait for the manager to be initialized.
	<-initChan

	// Create a new reservation.
	reservationFSM, err := testContext.manager.newReservationFromNtfn(
		ctxb, uint32(testContext.mockLnd.Height),
		&swapserverrpc.ServerReservationNotification{
			ReservationId: defaultReservationId[:],
			Value:         uint64(defaultValue),
			ServerKey:     defaultPubkeyBytes,
			Expiry:        uint32(testContext.mockLnd.Height) + defaultExpiry,
		},
	)
	require.NoError(t, err)

	// We'll expect the spendConfirmation to be sent to the server.
	pkScript, err := reservationFSM.reservation.GetPkScript()
	require.NoError(t, err)

	confReg := <-testContext.mockLnd.RegisterConfChannel
	require.Equal(t, confReg.PkScript, pkScript)

	confTx := &wire.MsgTx{
		TxOut: []*wire.TxOut{
			{
				Value:    int64(defaultValue),
				PkScript: pkScript,
			},
		},
	}
	// We'll now confirm the spend.
	confReg.ConfChan <- &chainntnfs.TxConfirmation{
		BlockHeight: uint32(testContext.mockLnd.Height),
		Tx:          confTx,
	}

	// We'll now expect the reservation to be confirmed.
	err = reservationFSM.DefaultObserver.WaitForState(ctxb, 5*time.Second, Confirmed)
	require.NoError(t, err)

	// We'll now expect a spend registration.
	var spendReg atomic.Pointer[test.SpendRegistration]
	spendReg.Store(<-testContext.mockLnd.RegisterSpendChannel)
	require.Equal(t, spendReg.Load().PkScript, pkScript)

	go func() {
		// We'll expect a second spend registration.
		spendReg.Store(<-testContext.mockLnd.RegisterSpendChannel)
		require.Equal(t, spendReg.Load().PkScript, pkScript)
	}()

	// We'll now try to lock the reservation.
	err = testContext.manager.LockReservation(ctxb, defaultReservationId)
	require.NoError(t, err)

	// We'll try to lock the reservation again, which should fail.
	err = testContext.manager.LockReservation(ctxb, defaultReservationId)
	require.Error(t, err)

	testContext.mockLnd.SpendChannel <- &chainntnfs.SpendDetail{
		SpentOutPoint: spendReg.Load().Outpoint,
	}

	// We'll now expect the reservation to be expired.
	err = reservationFSM.DefaultObserver.WaitForState(ctxb, 5*time.Second, Spent)
	require.NoError(t, err)

	testContext.manager.Lock()
	_, ok := testContext.manager.activeReservations[defaultReservationId]
	testContext.manager.Unlock()
	require.False(t, ok)
}

// TestManagerContinuesAfterInvalidNotification verifies that a malformed
// server notification doesn't stop the reservation manager from processing
// later notifications.
func TestManagerContinuesAfterInvalidNotification(t *testing.T) {
	testContext := newManagerTestContext(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	initChan := make(chan struct{})
	errChan := make(chan error, 1)
	go func() {
		errChan <- testContext.manager.Run(
			ctx, testContext.mockLnd.Height, initChan,
		)
	}()

	<-initChan

	// A malformed ID is rejected by newReservationFromNtfn. The manager
	// should log the error and continue processing the stream.
	testContext.reservationNotificationChan <- &swapserverrpc.ServerReservationNotification{
		ReservationId: []byte{1},
	}

	testContext.reservationNotificationChan <- &swapserverrpc.ServerReservationNotification{
		ReservationId: defaultReservationId[:],
		Value:         uint64(defaultValue),
		ServerKey:     defaultPubkeyBytes,
		Expiry: uint32(testContext.mockLnd.Height) +
			defaultExpiry,
	}

	select {
	case <-testContext.mockLnd.RegisterConfChannel:
	case err := <-errChan:
		require.NoError(t, err)
		t.Fatal("reservation manager stopped after malformed notification")
	case <-time.After(5 * time.Second):
		t.Fatal("valid reservation notification was not processed")
	}

	cancel()
	require.NoError(t, <-errChan)
}

// TestManagerRejectsDuplicateReservation verifies that a duplicate server
// notification cannot replace the active FSM for an existing reservation.
func TestManagerRejectsDuplicateReservation(t *testing.T) {
	testContext := newManagerTestContext(t)
	ctx := t.Context()
	req := &swapserverrpc.ServerReservationNotification{
		ReservationId: defaultReservationId[:],
		Value:         uint64(defaultValue),
		ServerKey:     defaultPubkeyBytes,
		Expiry: uint32(testContext.mockLnd.Height) +
			defaultExpiry,
	}

	firstFSM, err := testContext.manager.newReservationFromNtfn(
		ctx, uint32(testContext.mockLnd.Height), req,
	)
	require.NoError(t, err)

	secondFSM, err := testContext.manager.newReservationFromNtfn(
		ctx, uint32(testContext.mockLnd.Height), req,
	)
	require.ErrorIs(t, err, ErrReservationAlreadyExists)
	require.Nil(t, secondFSM)
	require.Same(
		t, firstFSM,
		testContext.manager.activeReservations[defaultReservationId],
	)
}

// TestManagerLimitsActiveReservations verifies that server notifications
// cannot grow the active FSM set without bound.
func TestManagerLimitsActiveReservations(t *testing.T) {
	testContext := newManagerTestContext(t)

	for i := range maxActiveReservations {
		var id ID
		id[0] = byte(i)
		id[1] = byte(i >> 8)
		testContext.manager.activeReservations[id] = NewFSM(
			testContext.manager.cfg, ProtocolVersionServerInitiated,
		)
	}

	reservationFSM, err := testContext.manager.newReservationFromNtfn(
		t.Context(), uint32(testContext.mockLnd.Height),
		&swapserverrpc.ServerReservationNotification{
			ReservationId: defaultReservationId[:],
			Value:         uint64(defaultValue),
			ServerKey:     defaultPubkeyBytes,
			Expiry: uint32(testContext.mockLnd.Height) +
				defaultExpiry,
		},
	)
	require.ErrorIs(t, err, ErrTooManyActiveReservations)
	require.Nil(t, reservationFSM)
	require.Len(
		t, testContext.manager.activeReservations,
		maxActiveReservations,
	)
}

// ManagerTestContext is a helper struct that contains all the necessary
// components to test the reservation manager.
type ManagerTestContext struct {
	manager                     *Manager
	context                     test.Context
	mockLnd                     *test.LndMockServices
	reservationNotificationChan chan *swapserverrpc.ServerReservationNotification
	mockReservationClient       *mockReservationClient
}

// newManagerTestContext creates a new test context for the reservation manager.
func newManagerTestContext(t *testing.T) *ManagerTestContext {
	mockLnd := test.NewMockLnd()
	lndContext := test.NewContext(t, mockLnd)

	dbFixture := loopdb.NewTestDB(t)

	store := NewSQLStore(loopdb.NewTypedStore[Querier](dbFixture))

	mockReservationClient := new(mockReservationClient)

	sendChan := make(chan *swapserverrpc.ServerReservationNotification)

	mockReservationClient.On(
		"OpenReservation", mock.Anything, mock.Anything, mock.Anything,
	).Return(
		&swapserverrpc.ServerOpenReservationResponse{}, nil,
	)

	mockNtfnManager := &mockNtfnManager{
		sendChan: sendChan,
	}

	cfg := &Config{
		Store:               store,
		Wallet:              mockLnd.WalletKit,
		ChainNotifier:       mockLnd.ChainNotifier,
		ReservationClient:   mockReservationClient,
		NotificationManager: mockNtfnManager,
	}

	manager := NewManager(cfg)

	return &ManagerTestContext{
		manager:                     manager,
		context:                     lndContext,
		mockLnd:                     mockLnd,
		mockReservationClient:       mockReservationClient,
		reservationNotificationChan: sendChan,
	}
}

type mockNtfnManager struct {
	sendChan chan *swapserverrpc.ServerReservationNotification
}

func (m *mockNtfnManager) SubscribeReservations(
	ctx context.Context,
) <-chan *swapserverrpc.ServerReservationNotification {

	return m.sendChan
}

func mustDecodeID(id string) ID {
	bytes, err := hex.DecodeString(id)
	if err != nil {
		panic(err)
	}
	var decoded ID
	copy(decoded[:], bytes)
	return decoded
}
