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
	ctxb, cancel := context.WithCancel(context.Background())
	defer cancel()

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
	reservationFSM, err := testContext.manager.newReservation(
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
