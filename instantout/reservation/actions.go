package reservation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnrpc"
)

const (
	// Define route independent max routing fees. We have currently no way
	// to get a reliable estimate of the routing fees. Best we can do is
	// the minimum routing fees, which is not very indicative.
	maxRoutingFeeBase = btcutil.Amount(10)

	maxRoutingFeeRate = int64(20000)
)

var (
	// The allowed delta between what we accept as the expiry height and
	// the actual expiry height.
	expiryDelta = uint32(3)

	// defaultPrepayTimeout is the default timeout for the prepayment.
	DefaultPrepayTimeout = time.Minute * 120
)

// ClientRequestedInitContext contains the request parameters for a reservation.
type ClientRequestedInitContext struct {
	value            btcutil.Amount
	relativeExpiry   uint32
	heightHint       uint32
	maxPrepaymentAmt btcutil.Amount
}

// InitFromClientRequestAction is the action that is executed when the
// reservation state machine is initialized from a client request. It creates
// the reservation in the database and sends the reservation request to the
// server.
func (f *FSM) InitFromClientRequestAction(ctx context.Context,
	eventCtx fsm.EventContext) fsm.EventType {

	// Check if the context is of the correct type.
	reservationRequest, ok := eventCtx.(*ClientRequestedInitContext)
	if !ok {
		return f.HandleError(fsm.ErrInvalidContextType)
	}

	// Create the reservation in the database.
	keyRes, err := f.cfg.Wallet.DeriveNextKey(ctx, KeyFamily)
	if err != nil {
		return f.HandleError(err)
	}

	// Send the request to the server.
	requestResponse, err := f.cfg.ReservationClient.RequestReservation(
		ctx, &swapserverrpc.RequestReservationRequest{
			Value:     uint64(reservationRequest.value),
			Expiry:    reservationRequest.relativeExpiry,
			ClientKey: keyRes.PubKey.SerializeCompressed(),
		},
	)
	if err != nil {
		return f.HandleError(err)
	}

	expectedExpiry := reservationRequest.relativeExpiry +
		reservationRequest.heightHint

	// Check that the expiry is in the delta.
	if requestResponse.Expiry < expectedExpiry-expiryDelta ||
		requestResponse.Expiry > expectedExpiry+expiryDelta {

		return f.HandleError(
			fmt.Errorf("unexpected expiry height: %v, expected %v",
				requestResponse.Expiry, expectedExpiry))
	}

	prepayment, err := f.cfg.LightningClient.DecodePaymentRequest(
		ctx, requestResponse.Invoice,
	)
	if err != nil {
		return f.HandleError(err)
	}

	if prepayment.Value.ToSatoshis() > reservationRequest.maxPrepaymentAmt {
		return f.HandleError(
			errors.New("prepayment amount too high"))
	}

	serverKey, err := btcec.ParsePubKey(requestResponse.ServerKey)
	if err != nil {
		return f.HandleError(err)
	}

	var Id ID
	copy(Id[:], requestResponse.ReservationId)

	reservation, err := NewReservation(
		Id, serverKey, keyRes.PubKey, reservationRequest.value,
		requestResponse.Expiry, reservationRequest.heightHint,
		keyRes.KeyLocator, ProtocolVersionClientInitiated,
	)
	if err != nil {
		return f.HandleError(err)
	}
	reservation.PrepayInvoice = requestResponse.Invoice
	f.reservation = reservation

	// Create the reservation in the database.
	err = f.cfg.Store.CreateReservation(ctx, reservation)
	if err != nil {
		return f.HandleError(err)
	}

	return OnClientInitialized
}

// SendPrepayment is the action that is executed when the reservation
// is initialized from a client request. It dispatches the prepayment to the
// server and wait for it to be settled, signaling confirmation of the
// reservation.
func (f *FSM) SendPrepayment(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	prepayment, err := f.cfg.LightningClient.DecodePaymentRequest(
		ctx, f.reservation.PrepayInvoice,
	)
	if err != nil {
		return f.HandleError(err)
	}

	payReq := lndclient.SendPaymentRequest{
		Invoice: f.reservation.PrepayInvoice,
		Timeout: DefaultPrepayTimeout,
		MaxFee:  getMaxRoutingFee(prepayment.Value.ToSatoshis()),
	}
	// Send the prepayment to the server.
	payChan, errChan, err := f.cfg.RouterClient.SendPayment(
		ctx, payReq,
	)
	if err != nil {
		return f.HandleError(err)
	}

	for {
		select {
		case <-ctx.Done():
			return fsm.NoOp

		case err := <-errChan:
			return f.HandleError(err)

		case prepayResp := <-payChan:
			if prepayResp.State == lnrpc.Payment_FAILED {
				return f.HandleError(
					fmt.Errorf("prepayment failed: %v",
						prepayResp.FailureReason))
			}
			if prepayResp.State == lnrpc.Payment_SUCCEEDED {
				return OnBroadcast
			}
		}
	}
}

// ServerRequestedInitContext contains the request parameters for a reservation.
type ServerRequestedInitContext struct {
	reservationID ID
	serverPubkey  *btcec.PublicKey
	value         btcutil.Amount
	expiry        uint32
	heightHint    uint32
}

// InitFromServerRequestAction is the action that is executed when the
// reservation state machine is initialized from  a server request. It creates
// the reservation in the database and dispatches the payment to the server.
func (f *FSM) InitFromServerRequestAction(ctx context.Context,
	eventCtx fsm.EventContext) fsm.EventType {

	// Check if the context is of the correct type.
	reservationRequest, ok := eventCtx.(*ServerRequestedInitContext)
	if !ok {
		return f.HandleError(fsm.ErrInvalidContextType)
	}

	keyRes, err := f.cfg.Wallet.DeriveNextKey(ctx, KeyFamily)
	if err != nil {
		return f.HandleError(err)
	}

	// Send the client reservation details to the server.
	log.Debugf("Dispatching reservation to server: %x",
		reservationRequest.reservationID)

	request := &swapserverrpc.ServerOpenReservationRequest{
		ReservationId: reservationRequest.reservationID[:],
		ClientKey:     keyRes.PubKey.SerializeCompressed(),
	}

	_, err = f.cfg.ReservationClient.OpenReservation(ctx, request)
	if err != nil {
		return f.HandleError(err)
	}

	reservation, err := NewReservation(
		reservationRequest.reservationID,
		reservationRequest.serverPubkey,
		keyRes.PubKey,
		reservationRequest.value,
		reservationRequest.expiry,
		reservationRequest.heightHint,
		keyRes.KeyLocator,
		ProtocolVersionServerInitiated,
	)
	if err != nil {
		return f.HandleError(err)
	}

	f.reservation = reservation

	// Create the reservation in the database.
	err = f.cfg.Store.CreateReservation(ctx, reservation)
	if err != nil {
		return f.HandleError(err)
	}

	return OnBroadcast
}

// SubscribeToConfirmationAction is the action that is executed when the
// reservation is waiting for confirmation. It subscribes to the confirmation
// of the reservation transaction.
func (f *FSM) SubscribeToConfirmationAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	pkscript, err := f.reservation.GetPkScript()
	if err != nil {
		return f.HandleError(err)
	}

	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Subscribe to the confirmation of the reservation transaction.
	log.Debugf("Subscribing to conf for reservation: %x pkscript: %x, "+
		"initiation height: %v", f.reservation.ID, pkscript,
		f.reservation.InitiationHeight)

	confChan, errConfChan, err := f.cfg.ChainNotifier.RegisterConfirmationsNtfn(
		callCtx, nil, pkscript, DefaultConfTarget,
		f.reservation.InitiationHeight,
	)
	if err != nil {
		f.Errorf("unable to subscribe to conf notification: %v", err)
		return f.HandleError(err)
	}

	blockChan, errBlockChan, err := f.cfg.ChainNotifier.RegisterBlockEpochNtfn(
		callCtx,
	)
	if err != nil {
		f.Errorf("unable to subscribe to block notifications: %v", err)
		return f.HandleError(err)
	}

	// We'll now wait for the confirmation of the reservation transaction.
	for {
		select {
		case err := <-errConfChan:
			f.Errorf("conf subscription error: %v", err)
			return f.HandleError(err)

		case err := <-errBlockChan:
			f.Errorf("block subscription error: %v", err)
			return f.HandleError(err)

		case confInfo := <-confChan:
			f.Debugf("confirmed in tx: %v", confInfo.Tx)
			outpoint, err := f.reservation.findReservationOutput(
				confInfo.Tx,
			)
			if err != nil {
				return f.HandleError(err)
			}

			f.reservation.ConfirmationHeight = confInfo.BlockHeight
			f.reservation.Outpoint = outpoint

			return OnConfirmed

		case block := <-blockChan:
			f.Debugf("block received: %v expiry: %v", block,
				f.reservation.Expiry)

			if uint32(block) >= f.reservation.Expiry {
				return OnTimedOut
			}

		case <-ctx.Done():
			return fsm.NoOp
		}
	}
}

// AsyncWaitForExpiredOrSweptAction waits for the reservation to be either
// expired or swept. This is non-blocking and can be used to wait for the
// reservation to expire while expecting other events.
func (f *FSM) AsyncWaitForExpiredOrSweptAction(ctx context.Context,
	_ fsm.EventContext) fsm.EventType {

	notifCtx, cancel := context.WithCancel(ctx)

	blockHeightChan, errEpochChan, err := f.cfg.ChainNotifier.
		RegisterBlockEpochNtfn(notifCtx)
	if err != nil {
		cancel()
		return f.HandleError(err)
	}

	pkScript, err := f.reservation.GetPkScript()
	if err != nil {
		cancel()
		return f.HandleError(err)
	}

	spendChan, errSpendChan, err := f.cfg.ChainNotifier.RegisterSpendNtfn(
		notifCtx, f.reservation.Outpoint, pkScript,
		f.reservation.InitiationHeight,
	)
	if err != nil {
		cancel()
		return f.HandleError(err)
	}

	go func() {
		defer cancel()
		op, err := f.handleSubcriptions(
			notifCtx, blockHeightChan, spendChan, errEpochChan,
			errSpendChan,
		)
		if err != nil {
			f.handleAsyncError(ctx, err)
			return
		}
		if op == fsm.NoOp {
			return
		}
		err = f.SendEvent(ctx, op, nil)
		if err != nil {
			f.Errorf("Error sending %s event: %v", op, err)
		}
	}()

	return fsm.NoOp
}

func (f *FSM) handleSubcriptions(ctx context.Context,
	blockHeightChan <-chan int32, spendChan <-chan *chainntnfs.SpendDetail,
	errEpochChan <-chan error, errSpendChan <-chan error,
) (fsm.EventType, error) {

	for {
		select {
		case err := <-errEpochChan:
			return fsm.OnError, err

		case err := <-errSpendChan:
			return fsm.OnError, err

		case blockHeight := <-blockHeightChan:
			expired := blockHeight >= int32(f.reservation.Expiry)

			if expired {
				f.Debugf("Reservation expired")
				return OnTimedOut, nil
			}

		case <-spendChan:
			return OnSpent, nil

		case <-ctx.Done():
			return fsm.NoOp, nil
		}
	}
}

func (f *FSM) handleAsyncError(ctx context.Context, err error) {
	f.LastActionError = err
	f.Errorf("Error on async action: %v", err)
	err2 := f.SendEvent(ctx, fsm.OnError, err)
	if err2 != nil {
		f.Errorf("Error sending event: %v", err2)
	}
}

func getMaxRoutingFee(amt btcutil.Amount) btcutil.Amount {
	return swap.CalcFee(amt, maxRoutingFeeBase, maxRoutingFeeRate)
}
