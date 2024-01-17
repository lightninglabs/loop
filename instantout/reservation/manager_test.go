package reservation

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

var (
	defaultReservationId = mustDecodeID("17cecc61ab4aafebdc0542dabdae0d0cb8907ec1c9c8ae387bc5a3309ca8b600")
)

func TestManager(t *testing.T) {
	ctxb, cancel := context.WithCancel(context.Background())
	defer cancel()

	testContext := newManagerTestContext(t)

	// Start the manager.
	go func() {
		err := testContext.manager.Run(ctxb, testContext.mockLnd.Height)
		require.NoError(t, err)
	}()

	// Create a new reservation.
	fsm, err := testContext.manager.newReservation(
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
	pkScript, err := fsm.reservation.GetPkScript()
	require.NoError(t, err)

	conf := <-testContext.mockLnd.RegisterConfChannel
	require.Equal(t, conf.PkScript, pkScript)

	confTx := &wire.MsgTx{
		TxOut: []*wire.TxOut{
			{
				PkScript: pkScript,
			},
		},
	}
	// We'll now confirm the spend.
	conf.ConfChan <- &chainntnfs.TxConfirmation{
		BlockHeight: uint32(testContext.mockLnd.Height),
		Tx:          confTx,
	}

	// We'll now expect the reservation to be confirmed.
	err = fsm.DefaultObserver.WaitForState(ctxb, 5*time.Second, Confirmed)
	require.NoError(t, err)

	// We'll now expire the reservation.
	err = testContext.mockLnd.NotifyHeight(
		testContext.mockLnd.Height + int32(defaultExpiry),
	)
	require.NoError(t, err)

	// We'll now expect the reservation to be expired.
	err = fsm.DefaultObserver.WaitForState(ctxb, 5*time.Second, TimedOut)
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

	store := NewSQLStore(dbFixture)

	mockReservationClient := new(mockReservationClient)

	sendChan := make(chan *swapserverrpc.ServerReservationNotification)

	mockReservationClient.On(
		"ReservationNotificationStream", mock.Anything, mock.Anything,
		mock.Anything,
	).Return(
		&dummyReservationNotificationServer{
			SendChan: sendChan,
		}, nil,
	)

	mockReservationClient.On(
		"OpenReservation", mock.Anything, mock.Anything, mock.Anything,
	).Return(
		&swapserverrpc.ServerOpenReservationResponse{}, nil,
	)

	cfg := &Config{
		Store:             store,
		Wallet:            mockLnd.WalletKit,
		ChainNotifier:     mockLnd.ChainNotifier,
		FetchL402:         func(context.Context) error { return nil },
		ReservationClient: mockReservationClient,
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

type dummyReservationNotificationServer struct {
	grpc.ClientStream

	// SendChan is the channel that is used to send notifications.
	SendChan chan *swapserverrpc.ServerReservationNotification
}

func (d *dummyReservationNotificationServer) Recv() (
	*swapserverrpc.ServerReservationNotification, error) {

	return <-d.SendChan, nil
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
