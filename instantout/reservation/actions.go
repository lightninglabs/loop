package reservation

import (
	"context"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/loop/utils"
	"github.com/lightningnetwork/lnd/chainntnfs"
)

// InitReservationContext contains the request parameters for a reservation.
type InitReservationContext struct {
	reservationID ID
	serverPubkey  *btcec.PublicKey
	value         btcutil.Amount
	expiry        uint32
	heightHint    uint32
}

// InitAction is the action that is executed when the reservation state machine
// is initialized. It creates the reservation in the database and dispatches the
// payment to the server.
func (f *FSM) InitAction(ctx context.Context,
	eventCtx fsm.EventContext) fsm.EventType {

	// Check if the context is of the correct type.
	reservationRequest, ok := eventCtx.(*InitReservationContext)
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

	blockChan, errBlockChan, err := utils.RegisterBlockEpochNtfnWithRetry(
		callCtx, f.cfg.ChainNotifier,
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

	blockHeightChan, errEpochChan, err := utils.RegisterBlockEpochNtfnWithRetry(
		notifCtx, f.cfg.ChainNotifier,
	)
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
