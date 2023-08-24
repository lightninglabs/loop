package reservation

import (
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop/fsm"
	looprpc "github.com/lightninglabs/loop/swapserverrpc"
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
func (r *FSM) InitAction(eventCtx fsm.EventContext) fsm.EventType {
	// Check if the context is of the correct type.
	reservationRequest, ok := eventCtx.(*InitReservationContext)
	if !ok {
		return r.HandleError(fsm.ErrInvalidContextType)
	}

	keyRes, err := r.cfg.Wallet.DeriveNextKey(
		r.ctx, KeyFamily,
	)
	if err != nil {
		return r.HandleError(err)
	}

	// Send the client reservation details to the server.
	log.Debugf("Dispatching reservation to server: %x",
		reservationRequest.reservationID)

	request := &looprpc.ServerOpenReservationRequest{
		ReservationId: reservationRequest.reservationID[:],
		ClientKey:     keyRes.PubKey.SerializeCompressed(),
	}

	_, err = r.cfg.ReservationClient.OpenReservation(r.ctx, request)
	if err != nil {
		return r.HandleError(err)
	}

	reservation, err := NewReservation(
		reservationRequest.reservationID,
		reservationRequest.serverPubkey,
		keyRes.PubKey,
		reservationRequest.value,
		reservationRequest.expiry,
		reservationRequest.heightHint,
		keyRes.KeyLocator,
	)
	if err != nil {
		return r.HandleError(err)
	}

	r.reservation = reservation

	// Create the reservation in the database.
	err = r.cfg.Store.CreateReservation(r.ctx, reservation)
	if err != nil {
		return r.HandleError(err)
	}

	return OnBroadcast
}

// SubscribeToConfirmationAction is the action that is executed when the
// reservation is waiting for confirmation. It subscribes to the confirmation
// of the reservation transaction.
func (r *FSM) SubscribeToConfirmationAction(_ fsm.EventContext) fsm.EventType {
	pkscript, err := r.reservation.GetPkScript()
	if err != nil {
		return r.HandleError(err)
	}

	// Subscribe to the confirmation of the reservation transaction.
	log.Debugf("Subscribing to conf for reservation: %x pkscript: %x, "+
		"initiation height: %v", r.reservation.ID, pkscript,
		r.reservation.InitiationHeight)

	confChan, errConfChan, err := r.cfg.ChainNotifier.RegisterConfirmationsNtfn(
		r.ctx, nil, pkscript, 1,
		r.reservation.InitiationHeight-1,
	)
	if err != nil {
		r.Errorf("unable to subscribe to conf notification: %v", err)
		return r.HandleError(err)
	}

	blockChan, errBlockChan, err := r.cfg.ChainNotifier.RegisterBlockEpochNtfn(
		r.ctx,
	)
	if err != nil {
		r.Errorf("unable to subscribe to block notifications: %v", err)
		return r.HandleError(err)
	}

	// We'll now wait for the confirmation of the reservation transaction.
	for {
		select {
		case err := <-errConfChan:
			r.Errorf("conf subscription error: %v", err)
			return r.HandleError(err)

		case err := <-errBlockChan:
			r.Errorf("block subscription error: %v", err)
			return r.HandleError(err)

		case confInfo := <-confChan:
			r.Debugf("reservation confirmed: %v", confInfo)
			outpoint, err := r.reservation.findReservationOutput(
				confInfo.Tx,
			)
			if err != nil {
				return r.HandleError(err)
			}

			r.reservation.ConfirmationHeight = confInfo.BlockHeight
			r.reservation.Outpoint = outpoint

			return OnConfirmed

		case block := <-blockChan:
			r.Debugf("block received: %v expiry: %v", block,
				r.reservation.Expiry)

			if uint32(block) >= r.reservation.Expiry {
				return OnTimedOut
			}

		case <-r.ctx.Done():
			return fsm.NoOp
		}
	}
}

// ReservationConfirmedAction waits for the reservation to be either expired or
// waits for other actions to happen.
func (f *FSM) ReservationConfirmedAction(_ fsm.EventContext) fsm.EventType {
	blockHeightChan, errEpochChan, err := f.cfg.ChainNotifier.
		RegisterBlockEpochNtfn(f.ctx)
	if err != nil {
		return f.HandleError(err)
	}

	pkSCript, err := f.reservation.GetPkScript()
	if err != nil {
		return f.HandleError(err)
	}

	spendChan, errSpendChan, err := f.cfg.ChainNotifier.RegisterSpendNtfn(
		f.ctx, f.reservation.Outpoint, pkSCript,
		f.reservation.InitiationHeight,
	)
	if err != nil {
		return f.HandleError(err)
	}

	for {
		select {
		case err := <-errEpochChan:
			return f.HandleError(err)

		case err := <-errSpendChan:
			return f.HandleError(err)

		case blockHeight := <-blockHeightChan:
			expired := blockHeight >= int32(f.reservation.Expiry)
			if expired {
				f.Debugf("Reservation %v expired",
					f.reservation.ID)

				return OnTimedOut
			}

		case <-spendChan:
			return OnSpent

		case <-f.ctx.Done():
			return fsm.NoOp
		}
	}
}
