package instantout

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/instantout/reservation"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// cleanupTestReservationManager supplies reservations and records cleanup
// errors for initialization and failure-path tests.
type cleanupTestReservationManager struct {
	ReservationManager

	reservation *reservation.Reservation
	unlockErr   error
}

func (m *cleanupTestReservationManager) GetReservation(context.Context,
	reservation.ID) (*reservation.Reservation, error) {

	return m.reservation, nil
}

func (m *cleanupTestReservationManager) UnlockReservation(context.Context,
	reservation.ID) error {

	return m.unlockErr
}

// cleanupTestInstantOutClient controls initialization responses and records
// cancellation requests.
type cleanupTestInstantOutClient struct {
	swapserverrpc.InstantSwapServerClient

	canceled   chan *swapserverrpc.CancelInstantSwapRequest
	swapHash   lntypes.Hash
	requestErr error
	senderKey  []byte
}

func (c *cleanupTestInstantOutClient) RequestInstantLoopOut(_ context.Context,
	req *swapserverrpc.InstantLoopOutRequest, _ ...grpc.CallOption) (
	*swapserverrpc.InstantLoopOutResponse, error) {

	swapHash, err := lntypes.MakeHash(req.SwapHash)
	if err != nil {
		return nil, err
	}
	c.swapHash = swapHash
	if c.requestErr != nil {
		return nil, c.requestErr
	}

	return &swapserverrpc.InstantLoopOutResponse{
		SwapInvoice: "test invoice",
		SenderKey:   c.senderKey,
	}, nil
}

func (c *cleanupTestInstantOutClient) CancelInstantSwap(_ context.Context,
	req *swapserverrpc.CancelInstantSwapRequest, _ ...grpc.CallOption) (
	*swapserverrpc.CancelInstantSwapResponse, error) {

	c.canceled <- req
	return &swapserverrpc.CancelInstantSwapResponse{}, nil
}

// initCleanupLightningClient returns a controlled decoded invoice whose hash
// matches the request observed by the server mock.
type initCleanupLightningClient struct {
	lndclient.LightningClient

	server        *cleanupTestInstantOutClient
	invoiceAmount lnwire.MilliSatoshi
}

func (c *initCleanupLightningClient) DecodePaymentRequest(_ context.Context,
	_ string) (*lndclient.PaymentRequest, error) {

	return &lndclient.PaymentRequest{
		Hash:  c.server.swapHash,
		Value: c.invoiceAmount,
	}, nil
}

// initCleanupWallet supplies the key and fee estimate needed to initialize an
// Instant Out swap.
type initCleanupWallet struct {
	lndclient.WalletKitClient

	pubKey *btcec.PublicKey
}

func (w *initCleanupWallet) DeriveNextKey(_ context.Context, _ int32) (
	*keychain.KeyDescriptor, error) {

	return &keychain.KeyDescriptor{PubKey: w.pubKey}, nil
}

func (w *initCleanupWallet) EstimateFeeRate(_ context.Context, _ int32) (
	chainfee.SatPerKWeight, error) {

	return chainfee.SatPerKWeight(1000), nil
}

// initCleanupStore accepts the initialized swap so the successful path can be
// tested without a database.
type initCleanupStore struct {
	InstantLoopOutStore
}

func (s *initCleanupStore) CreateInstantLoopOut(context.Context,
	*InstantOut) error {

	return nil
}

func newInitCleanupTestFSM(t *testing.T,
	invoiceAmount lnwire.MilliSatoshi, requestErr error) (
	*FSM, *cleanupTestInstantOutClient, *InitInstantOutCtx) {

	t.Helper()

	const (
		swapAmount = btcutil.Amount(100_000)
		maxSwapFee = btcutil.Amount(200)
	)

	_, pubKey := btcec.PrivKeyFromBytes([]byte{1})
	sweepAddress, err := btcutil.NewAddressWitnessPubKeyHash(
		make([]byte, 20), &chaincfg.TestNet3Params,
	)
	require.NoError(t, err)

	cancelClient := &cleanupTestInstantOutClient{
		canceled:   make(chan *swapserverrpc.CancelInstantSwapRequest, 1),
		requestErr: requestErr,
		senderKey:  pubKey.SerializeCompressed(),
	}
	reservationID := reservation.ID{1}
	instantOutFSM, err := NewFSM(
		&Config{
			Store: &initCleanupStore{},
			LndClient: &initCleanupLightningClient{
				server:        cancelClient,
				invoiceAmount: invoiceAmount,
			},
			Wallet:           &initCleanupWallet{pubKey: pubKey},
			InstantOutClient: cancelClient,
			ReservationManager: &cleanupTestReservationManager{
				reservation: &reservation.Reservation{
					ID:     reservationID,
					State:  reservation.Confirmed,
					Value:  swapAmount,
					Expiry: 1000,
				},
			},
		}, ProtocolVersionFullReservation,
	)
	require.NoError(t, err)

	return instantOutFSM, cancelClient, &InitInstantOutCtx{
		cltvExpiry:      100,
		reservations:    []reservation.ID{reservationID},
		initationHeight: 0,
		sweepAddress:    sweepAddress,
		maxSwapFee:      maxSwapFee,
	}
}

// TestCleanupPreservesActionError verifies that an unlock failure doesn't
// replace the action failure or prevent the cancellation notification.
func TestCleanupPreservesActionError(t *testing.T) {
	actionErr := errors.New("action failed")
	cancelClient := &cleanupTestInstantOutClient{
		canceled: make(chan *swapserverrpc.CancelInstantSwapRequest, 1),
	}
	instantOutFSM := &FSM{
		StateMachine: &fsm.StateMachine{},
		cfg: &Config{
			ReservationManager: &cleanupTestReservationManager{
				unlockErr: errors.New("unlock failed"),
			},
			InstantOutClient: cancelClient,
		},
		InstantOut: &InstantOut{
			Reservations: []*reservation.Reservation{
				{ID: reservation.ID{1}},
			},
		},
	}

	event := instantOutFSM.handleErrorAndUnlockReservations(
		t.Context(), actionErr,
	)
	require.Equal(t, fsm.OnError, event)
	require.ErrorIs(t, instantOutFSM.LastActionError, actionErr)
	require.Eventually(t, func() bool {
		select {
		case <-cancelClient.canceled:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond)
}

// TestInitFailureCancelsServerSwap verifies that a client-side validation
// failure releases the reservations that the server locked while creating the
// swap.
func TestInitFailureCancelsServerSwap(t *testing.T) {
	const (
		swapAmount = btcutil.Amount(100_000)
		maxSwapFee = btcutil.Amount(200)
	)

	instantOutFSM, cancelClient, initCtx := newInitCleanupTestFSM(
		t, lnwire.NewMSatFromSatoshis(swapAmount+maxSwapFee+1), nil,
	)

	event := instantOutFSM.InitInstantOutAction(
		t.Context(), initCtx,
	)
	require.Equal(t, fsm.OnError, event)
	require.ErrorContains(
		t, instantOutFSM.LastActionError, "exceeds maximum",
	)

	select {
	case cancelReq := <-cancelClient.canceled:
		require.Equal(t, cancelClient.swapHash[:], cancelReq.SwapHash)

	case <-time.After(time.Second):
		t.Fatal("server swap was not canceled")
	}
}

// TestInitRequestFailureCancelsServerSwap verifies that cleanup is attempted
// when the request may have succeeded server-side but its response was lost.
func TestInitRequestFailureCancelsServerSwap(t *testing.T) {
	requestErr := errors.New("response lost")
	instantOutFSM, cancelClient, initCtx := newInitCleanupTestFSM(
		t, 0, requestErr,
	)

	event := instantOutFSM.InitInstantOutAction(t.Context(), initCtx)
	require.Equal(t, fsm.OnError, event)
	require.ErrorIs(t, instantOutFSM.LastActionError, requestErr)

	select {
	case cancelReq := <-cancelClient.canceled:
		require.Equal(t, cancelClient.swapHash[:], cancelReq.SwapHash)

	case <-time.After(time.Second):
		t.Fatal("server swap was not canceled")
	}
}

// TestInitSuccessDoesNotCancelServerSwap verifies that successful local
// persistence disarms initialization cleanup.
func TestInitSuccessDoesNotCancelServerSwap(t *testing.T) {
	const (
		swapAmount = btcutil.Amount(100_000)
		maxSwapFee = btcutil.Amount(200)
	)

	instantOutFSM, cancelClient, initCtx := newInitCleanupTestFSM(
		t, lnwire.NewMSatFromSatoshis(swapAmount+maxSwapFee), nil,
	)

	event := instantOutFSM.InitInstantOutAction(t.Context(), initCtx)
	require.Equal(t, OnInit, event)
	require.Never(t, func() bool {
		return len(cancelClient.canceled) != 0
	}, 100*time.Millisecond, time.Millisecond)
}
