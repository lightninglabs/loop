package instantout

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/instantout/reservation"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// InstantOutBaseDB is the interface that contains all the queries generated
// by sqlc for the instantout table.
type InstantOutBaseDB interface {
	// InsertSwap inserts a new base swap.
	InsertSwap(ctx context.Context, arg sqlc.InsertSwapParams) error

	// InsertHtlcKeys inserts the htlc keys for a swap.
	InsertHtlcKeys(ctx context.Context, arg sqlc.InsertHtlcKeysParams) error

	// InsertInstantOut inserts a new instant out swap.
	InsertInstantOut(ctx context.Context,
		arg sqlc.InsertInstantOutParams) error

	// InsertInstantOutUpdate inserts a new instant out update.
	InsertInstantOutUpdate(ctx context.Context,
		arg sqlc.InsertInstantOutUpdateParams) error

	// UpdateInstantOut updates an instant out swap.
	UpdateInstantOut(ctx context.Context,
		arg sqlc.UpdateInstantOutParams) error

	// GetInstantOutSwap retrieves an instant out swap.
	GetInstantOutSwap(ctx context.Context,
		swapHash []byte) (sqlc.GetInstantOutSwapRow, error)

	// GetInstantOutSwapUpdates retrieves all instant out swap updates.
	GetInstantOutSwapUpdates(ctx context.Context,
		swapHash []byte) ([]sqlc.InstantoutUpdate, error)

	// GetInstantOutSwaps retrieves all instant out swaps.
	GetInstantOutSwaps(ctx context.Context) ([]sqlc.GetInstantOutSwapsRow,
		error)

	// ExecTx allows for executing a function in the context of a database
	// transaction.
	ExecTx(ctx context.Context, txOptions loopdb.TxOptions,
		txBody func(*sqlc.Queries) error) error
}

// ReservationStore is the interface that is required to load the reservations
// based on the stored reservation ids.
type ReservationStore interface {
	// GetReservation returns the reservation for the given id.
	GetReservation(ctx context.Context, id reservation.ID) (
		*reservation.Reservation, error)
}

type SQLStore struct {
	baseDb           InstantOutBaseDB
	reservationStore ReservationStore
	clock            clock.Clock
	network          *chaincfg.Params
}

// NewSQLStore creates a new SQLStore.
func NewSQLStore(db InstantOutBaseDB, clock clock.Clock,
	reservationStore ReservationStore, network *chaincfg.Params) *SQLStore {

	return &SQLStore{
		baseDb:           db,
		clock:            clock,
		reservationStore: reservationStore,
	}
}

// CreateInstantLoopOut adds a new instant loop out to the store.
func (s *SQLStore) CreateInstantLoopOut(ctx context.Context,
	instantOut *InstantOut) error {

	swapArgs := sqlc.InsertSwapParams{
		SwapHash:         instantOut.SwapHash[:],
		Preimage:         instantOut.swapPreimage[:],
		InitiationTime:   s.clock.Now(),
		AmountRequested:  int64(instantOut.Value),
		CltvExpiry:       instantOut.CltvExpiry,
		MaxMinerFee:      0,
		MaxSwapFee:       0,
		InitiationHeight: instantOut.initiationHeight,
		ProtocolVersion:  int32(instantOut.protocolVersion),
		Label:            "",
	}

	htlcKeyArgs := sqlc.InsertHtlcKeysParams{
		SwapHash: instantOut.SwapHash[:],
		SenderScriptPubkey: instantOut.serverPubkey.
			SerializeCompressed(),
		ReceiverScriptPubkey: instantOut.clientPubkey.
			SerializeCompressed(),
		ClientKeyFamily: int32(instantOut.keyLocator.Family),
		ClientKeyIndex:  int32(instantOut.keyLocator.Index),
	}

	reservationIdByteSlice := reservationIdsToByteSlice(
		instantOut.Reservations,
	)
	instantOutArgs := sqlc.InsertInstantOutParams{
		SwapHash:        instantOut.SwapHash[:],
		Preimage:        instantOut.swapPreimage[:],
		SweepAddress:    instantOut.sweepAddress.String(),
		OutgoingChanSet: instantOut.outgoingChanSet.String(),
		HtlcFeeRate:     int64(instantOut.htlcFeeRate),
		ReservationIds:  reservationIdByteSlice,
		SwapInvoice:     instantOut.swapInvoice,
	}

	updateArgs := sqlc.InsertInstantOutUpdateParams{
		SwapHash:        instantOut.SwapHash[:],
		UpdateTimestamp: s.clock.Now(),
		UpdateState:     string(instantOut.State),
	}

	return s.baseDb.ExecTx(ctx, loopdb.NewSqlWriteOpts(),
		func(q *sqlc.Queries) error {
			err := q.InsertSwap(ctx, swapArgs)
			if err != nil {
				return err
			}

			err = q.InsertHtlcKeys(ctx, htlcKeyArgs)
			if err != nil {
				return err
			}

			err = q.InsertInstantOut(ctx, instantOutArgs)
			if err != nil {
				return err
			}

			return q.InsertInstantOutUpdate(ctx, updateArgs)
		})
}

// UpdateInstantLoopOut updates an existing instant loop out in the
// store.
func (s *SQLStore) UpdateInstantLoopOut(ctx context.Context,
	instantOut *InstantOut) error {

	// Serialize the FinalHtlcTx.
	var finalHtlcTx []byte
	if instantOut.finalizedHtlcTx != nil {
		var buffer bytes.Buffer
		err := instantOut.finalizedHtlcTx.Serialize(
			&buffer,
		)
		if err != nil {
			return err
		}
		finalHtlcTx = buffer.Bytes()
	}

	var finalSweeplessSweepTx []byte
	if instantOut.FinalizedSweeplessSweepTx != nil {
		var buffer bytes.Buffer
		err := instantOut.FinalizedSweeplessSweepTx.Serialize(
			&buffer,
		)
		if err != nil {
			return err
		}
		finalSweeplessSweepTx = buffer.Bytes()
	}

	var sweepTxHash []byte
	if instantOut.SweepTxHash != nil {
		sweepTxHash = instantOut.SweepTxHash[:]
	}

	updateParams := sqlc.UpdateInstantOutParams{
		SwapHash:                  instantOut.SwapHash[:],
		FinalizedHtlcTx:           finalHtlcTx,
		SweepTxHash:               sweepTxHash,
		FinalizedSweeplessSweepTx: finalSweeplessSweepTx,
		SweepConfirmationHeight: serializeNullInt32(
			int32(instantOut.sweepConfirmationHeight),
		),
	}

	updateArgs := sqlc.InsertInstantOutUpdateParams{
		SwapHash:        instantOut.SwapHash[:],
		UpdateTimestamp: s.clock.Now(),
		UpdateState:     string(instantOut.State),
	}

	return s.baseDb.ExecTx(ctx, loopdb.NewSqlWriteOpts(),
		func(q *sqlc.Queries) error {
			err := q.UpdateInstantOut(ctx, updateParams)
			if err != nil {
				return err
			}

			return q.InsertInstantOutUpdate(ctx, updateArgs)
		},
	)
}

// GetInstantLoopOut returns the instant loop out for the given swap
// hash.
func (s *SQLStore) GetInstantLoopOut(ctx context.Context, swapHash []byte) (
	*InstantOut, error) {

	row, err := s.baseDb.GetInstantOutSwap(ctx, swapHash)
	if err != nil {
		return nil, err
	}

	updates, err := s.baseDb.GetInstantOutSwapUpdates(ctx, swapHash)
	if err != nil {
		return nil, err
	}

	return s.sqlInstantOutToInstantOut(ctx, row, updates)
}

// ListInstantLoopOuts returns all instant loop outs that are in the
// store.
func (s *SQLStore) ListInstantLoopOuts(ctx context.Context) ([]*InstantOut,
	error) {

	rows, err := s.baseDb.GetInstantOutSwaps(ctx)
	if err != nil {
		return nil, err
	}

	var instantOuts []*InstantOut
	for _, row := range rows {
		updates, err := s.baseDb.GetInstantOutSwapUpdates(
			ctx, row.SwapHash,
		)
		if err != nil {
			return nil, err
		}

		instantOut, err := s.sqlInstantOutToInstantOut(
			ctx, sqlc.GetInstantOutSwapRow(row), updates,
		)
		if err != nil {
			return nil, err
		}

		instantOuts = append(instantOuts, instantOut)
	}

	return instantOuts, nil
}

// sqlInstantOutToInstantOut converts sql rows to an instant out struct.
func (s *SQLStore) sqlInstantOutToInstantOut(ctx context.Context,
	row sqlc.GetInstantOutSwapRow, updates []sqlc.InstantoutUpdate) (
	*InstantOut, error) {

	swapHash, err := lntypes.MakeHash(row.SwapHash)
	if err != nil {
		return nil, err
	}

	swapPreImage, err := lntypes.MakePreimage(row.Preimage)
	if err != nil {
		return nil, err
	}

	serverKey, err := btcec.ParsePubKey(row.SenderScriptPubkey)
	if err != nil {
		return nil, err
	}

	clientKey, err := btcec.ParsePubKey(row.ReceiverScriptPubkey)
	if err != nil {
		return nil, err
	}

	var finalizedHtlcTx *wire.MsgTx
	if row.FinalizedHtlcTx != nil {
		finalizedHtlcTx = &wire.MsgTx{}
		err := finalizedHtlcTx.Deserialize(bytes.NewReader(
			row.FinalizedHtlcTx,
		))
		if err != nil {
			return nil, err
		}
	}

	var finalizedSweepLessSweepTx *wire.MsgTx
	if row.FinalizedSweeplessSweepTx != nil {
		finalizedSweepLessSweepTx = &wire.MsgTx{}
		err := finalizedSweepLessSweepTx.Deserialize(bytes.NewReader(
			row.FinalizedSweeplessSweepTx,
		))
		if err != nil {
			return nil, err
		}
	}

	var sweepTxHash *chainhash.Hash
	if row.SweepTxHash != nil {
		sweepTxHash, err = chainhash.NewHash(row.SweepTxHash)
		if err != nil {
			return nil, err
		}
	}

	var outgoingChanSet loopdb.ChannelSet
	if row.OutgoingChanSet != "" {
		outgoingChanSet, err = loopdb.ConvertOutgoingChanSet(
			row.OutgoingChanSet,
		)
		if err != nil {
			return nil, err
		}
	}
	reservationIds, err := byteSliceToReservationIds(row.ReservationIds)
	if err != nil {
		return nil, err
	}

	reservations := make([]*reservation.Reservation, 0, len(reservationIds))
	for _, id := range reservationIds {
		reservation, err := s.reservationStore.GetReservation(
			ctx, id,
		)
		if err != nil {
			return nil, err
		}

		reservations = append(reservations, reservation)
	}

	sweepAddress, err := btcutil.DecodeAddress(row.SweepAddress, s.network)
	if err != nil {
		return nil, err
	}

	instantOut := &InstantOut{
		SwapHash:         swapHash,
		swapPreimage:     swapPreImage,
		CltvExpiry:       row.CltvExpiry,
		outgoingChanSet:  outgoingChanSet,
		Reservations:     reservations,
		protocolVersion:  ProtocolVersion(row.ProtocolVersion),
		initiationHeight: row.InitiationHeight,
		Value:            btcutil.Amount(row.AmountRequested),
		keyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(row.ClientKeyFamily),
			Index:  uint32(row.ClientKeyIndex),
		},
		clientPubkey:              clientKey,
		serverPubkey:              serverKey,
		swapInvoice:               row.SwapInvoice,
		htlcFeeRate:               chainfee.SatPerKWeight(row.HtlcFeeRate),
		sweepAddress:              sweepAddress,
		finalizedHtlcTx:           finalizedHtlcTx,
		SweepTxHash:               sweepTxHash,
		FinalizedSweeplessSweepTx: finalizedSweepLessSweepTx,
		sweepConfirmationHeight: uint32(deserializeNullInt32(
			row.SweepConfirmationHeight,
		)),
	}

	if len(updates) > 0 {
		lastUpdate := updates[len(updates)-1]
		instantOut.State = fsm.StateType(lastUpdate.UpdateState)
	}

	return instantOut, nil
}

// reservationIdsToByteSlice converts a slice of reservation ids to a byte
// slice.
func reservationIdsToByteSlice(reservations []*reservation.Reservation) []byte {
	var reservationIds []byte
	for _, reservation := range reservations {
		reservationIds = append(reservationIds, reservation.ID[:]...)
	}

	return reservationIds
}

// byteSliceToReservationIds converts a byte slice to a slice of reservation
// ids.
func byteSliceToReservationIds(byteSlice []byte) ([]reservation.ID, error) {
	if len(byteSlice)%32 != 0 {
		return nil, fmt.Errorf("invalid byte slice length")
	}

	var reservationIds []reservation.ID
	for i := 0; i < len(byteSlice); i += 32 {
		var id reservation.ID
		copy(id[:], byteSlice[i:i+32])
		reservationIds = append(reservationIds, id)
	}

	return reservationIds, nil
}

// serializeNullInt32 serializes an int32 to a sql.NullInt32.
func serializeNullInt32(i int32) sql.NullInt32 {
	return sql.NullInt32{
		Int32: i,
		Valid: true,
	}
}

// deserializeNullInt32 deserializes an int32 from a sql.NullInt32.
func deserializeNullInt32(i sql.NullInt32) int32 {
	if i.Valid {
		return i.Int32
	}

	return 0
}
