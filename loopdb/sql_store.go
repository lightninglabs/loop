package loopdb

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/routing/route"
)

// FetchLoopOutSwaps returns all swaps currently in the store.
func (db *BaseDB) FetchLoopOutSwaps(ctx context.Context) ([]*LoopOut,
	error) {

	var loopOuts []*LoopOut

	err := db.ExecTx(ctx, NewSqlReadOpts(), func(tx *sqlc.Queries) error {
		swaps, err := tx.GetLoopOutSwaps(ctx)
		if err != nil {
			return err
		}

		loopOuts = make([]*LoopOut, len(swaps))

		for i, swap := range swaps {
			updates, err := tx.GetSwapUpdates(
				ctx, swap.SwapHash,
			)
			if err != nil {
				return err
			}

			loopOut, err := ConvertLoopOutRow(
				db.network, sqlc.GetLoopOutSwapRow(swap),
				updates,
			)
			if err != nil {
				return err
			}

			loopOuts[i] = loopOut
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return loopOuts, nil
}

// FetchLoopOutSwap returns the loop out swap with the given hash.
func (db *BaseDB) FetchLoopOutSwap(ctx context.Context,
	hash lntypes.Hash) (*LoopOut, error) {

	var loopOut *LoopOut

	err := db.ExecTx(ctx, NewSqlReadOpts(), func(tx *sqlc.Queries) error {
		swap, err := tx.GetLoopOutSwap(ctx, hash[:])
		if err != nil {
			return err
		}

		updates, err := tx.GetSwapUpdates(ctx, swap.SwapHash)
		if err != nil {
			return err
		}

		loopOut, err = ConvertLoopOutRow(
			db.network, swap, updates,
		)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return loopOut, nil
}

// CreateLoopOut adds an initiated swap to the store.
func (db *BaseDB) CreateLoopOut(ctx context.Context, hash lntypes.Hash,
	swap *LoopOutContract) error {

	writeOpts := NewSqlWriteOpts()
	return db.ExecTx(ctx, writeOpts, func(tx *sqlc.Queries) error {
		insertArgs := loopToInsertArgs(
			hash, &swap.SwapContract,
		)

		// First we'll insert the swap itself.
		err := tx.InsertSwap(ctx, insertArgs)
		if err != nil {
			return err
		}

		htlcKeyInsertArgs := swapToHtlcKeysInsertArgs(
			hash, &swap.SwapContract,
		)

		// Next insert the htlc keys.
		err = tx.InsertHtlcKeys(ctx, htlcKeyInsertArgs)
		if err != nil {
			return err
		}

		loopOutInsertArgs := loopOutToInsertArgs(hash, swap)

		// Next insert the loop out relevant data.
		err = tx.InsertLoopOut(ctx, loopOutInsertArgs)
		if err != nil {
			return err
		}

		// If the loop is an asset loop out, we'll also insert the
		// asset details.
		if swap.AssetSwapInfo != nil {
			assetInfo := swap.AssetSwapInfo
			err = tx.InsertLoopOutAsset(
				ctx, sqlc.InsertLoopOutAssetParams{
					SwapHash:    hash[:],
					AssetID:     assetInfo.AssetId,
					SwapRfqID:   assetInfo.SwapRfqId,
					PrepayRfqID: assetInfo.PrepayRfqId,
				},
			)
			if err != nil {
				return err
			}
		}

		return nil
	})
}

// UpdateLoopOutAssetInfo updates the offchain send amounts of the prepay and
// swap payment for an asset loop out swap.
func (db *BaseDB) UpdateLoopOutAssetInfo(ctx context.Context, hash lntypes.Hash,
	asset *LoopOutAssetSwap) error {

	return db.UpdateLoopOutAssetOffchainPayments(
		ctx, sqlc.UpdateLoopOutAssetOffchainPaymentsParams{
			SwapHash:           hash[:],
			AssetAmtPaidSwap:   int64(asset.SwapPaidAmt),
			AssetAmtPaidPrepay: int64(asset.PrepayPaidAmt),
		})
}

// BatchCreateLoopOut adds multiple initiated swaps to the store.
func (db *BaseDB) BatchCreateLoopOut(ctx context.Context,
	swaps map[lntypes.Hash]*LoopOutContract) error {

	writeOpts := NewSqlWriteOpts()
	return db.ExecTx(ctx, writeOpts, func(tx *sqlc.Queries) error {
		for swapHash, swap := range swaps {
			swap := swap

			insertArgs := loopToInsertArgs(
				swapHash, &swap.SwapContract,
			)

			// First we'll insert the swap itself.
			err := tx.InsertSwap(ctx, insertArgs)
			if err != nil {
				return err
			}

			htlcKeyInsertArgs := swapToHtlcKeysInsertArgs(
				swapHash, &swap.SwapContract,
			)

			// Next insert the htlc keys.
			err = tx.InsertHtlcKeys(ctx, htlcKeyInsertArgs)
			if err != nil {
				return err
			}

			loopOutInsertArgs := loopOutToInsertArgs(swapHash, swap)

			// Next insert the loop out relevant data.
			err = tx.InsertLoopOut(ctx, loopOutInsertArgs)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// UpdateLoopOut stores a new event for a target loop out swap. This
// appends to the event log for a particular swap as it goes through
// the various stages in its lifetime.
func (db *BaseDB) UpdateLoopOut(ctx context.Context, hash lntypes.Hash,
	time time.Time, state SwapStateData) error {

	return db.updateLoop(ctx, hash, time, state)
}

// FetchLoopInSwaps returns all swaps currently in the store.
func (db *BaseDB) FetchLoopInSwaps(ctx context.Context) (
	[]*LoopIn, error) {

	var loopIns []*LoopIn

	err := db.ExecTx(ctx, NewSqlReadOpts(), func(tx *sqlc.Queries) error {
		swaps, err := tx.GetLoopInSwaps(ctx)
		if err != nil {
			return err
		}

		loopIns = make([]*LoopIn, len(swaps))

		for i, swap := range swaps {
			updates, err := tx.GetSwapUpdates(ctx, swap.SwapHash)
			if err != nil {
				return err
			}

			loopIn, err := db.convertLoopInRow(
				swap, updates,
			)
			if err != nil {
				return err
			}

			loopIns[i] = loopIn
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return loopIns, nil
}

// CreateLoopIn adds an initiated swap to the store.
func (db *BaseDB) CreateLoopIn(ctx context.Context, hash lntypes.Hash,
	swap *LoopInContract) error {

	writeOpts := NewSqlWriteOpts()
	return db.ExecTx(ctx, writeOpts, func(tx *sqlc.Queries) error {
		insertArgs := loopToInsertArgs(
			hash, &swap.SwapContract,
		)

		// First we'll insert the swap itself.
		err := tx.InsertSwap(ctx, insertArgs)
		if err != nil {
			return err
		}
		htlcKeyInsertArgs := swapToHtlcKeysInsertArgs(
			hash, &swap.SwapContract,
		)

		// Next insert the htlc keys.
		err = tx.InsertHtlcKeys(ctx, htlcKeyInsertArgs)
		if err != nil {
			return err
		}

		loopInInsertArgs := loopInToInsertArgs(hash, swap)

		// Next insert the loop out relevant data.
		err = tx.InsertLoopIn(ctx, loopInInsertArgs)
		if err != nil {
			return err
		}

		return nil
	})
}

// BatchCreateLoopIn adds multiple initiated swaps to the store.
func (db *BaseDB) BatchCreateLoopIn(ctx context.Context,
	swaps map[lntypes.Hash]*LoopInContract) error {

	writeOpts := NewSqlWriteOpts()
	return db.ExecTx(ctx, writeOpts, func(tx *sqlc.Queries) error {
		for swapHash, swap := range swaps {
			swap := swap

			insertArgs := loopToInsertArgs(
				swapHash, &swap.SwapContract,
			)

			// First we'll insert the swap itself.
			err := tx.InsertSwap(ctx, insertArgs)
			if err != nil {
				return err
			}

			htlcKeyInsertArgs := swapToHtlcKeysInsertArgs(
				swapHash, &swap.SwapContract,
			)

			// Next insert the htlc keys.
			err = tx.InsertHtlcKeys(ctx, htlcKeyInsertArgs)
			if err != nil {
				return err
			}

			loopInInsertArgs := loopInToInsertArgs(swapHash, swap)

			// Next insert the loop in relevant data.
			err = tx.InsertLoopIn(ctx, loopInInsertArgs)
			if err != nil {
				return err
			}
		}

		return nil
	})
}

// UpdateLoopIn stores a new event for a target loop in swap. This
// appends to the event log for a particular swap as it goes through
// the various stages in its lifetime.
func (db *BaseDB) UpdateLoopIn(ctx context.Context, hash lntypes.Hash,
	time time.Time, state SwapStateData) error {

	return db.updateLoop(ctx, hash, time, state)
}

// PutLiquidityParams writes the serialized `manager.Parameters` bytes
// into the bucket.
//
// NOTE: it's the caller's responsibility to encode the param. Atm,
// it's encoding using the proto package's `Marshal` method.
func (db *BaseDB) PutLiquidityParams(ctx context.Context,
	params []byte) error {

	err := db.Queries.UpsertLiquidityParams(ctx, params)
	if err != nil {
		return err
	}

	return nil
}

// FetchLiquidityParams reads the serialized `manager.Parameters` bytes
// from the bucket.
//
// NOTE: it's the caller's responsibility to decode the param. Atm,
// it's decoding using the proto package's `Unmarshal` method.
func (db *BaseDB) FetchLiquidityParams(ctx context.Context) ([]byte,
	error) {

	var params []byte
	params, err := db.Queries.FetchLiquidityParams(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return params, nil
	} else if err != nil {
		return nil, err
	}

	return params, nil
}

// A compile time assertion to ensure that SqliteStore satisfies the
// SwapStore interface.
var _ SwapStore = (*BaseDB)(nil)

// updateLoop updates the swap with the given hash by inserting a new update
// in the swap_updates table.
func (db *BaseDB) updateLoop(ctx context.Context, hash lntypes.Hash,
	time time.Time, state SwapStateData) error {

	writeOpts := NewSqlWriteOpts()
	return db.ExecTx(ctx, writeOpts, func(tx *sqlc.Queries) error {
		updateParams := sqlc.InsertSwapUpdateParams{
			SwapHash:        hash[:],
			UpdateTimestamp: time.UTC(),
			UpdateState:     int32(state.State),
			ServerCost:      int64(state.Cost.Server),
			OnchainCost:     int64(state.Cost.Onchain),
			OffchainCost:    int64(state.Cost.Offchain),
		}

		if state.HtlcTxHash != nil {
			updateParams.HtlcTxhash = state.HtlcTxHash.String()
		}
		// First we insert the swap update.
		err := tx.InsertSwapUpdate(ctx, updateParams)
		if err != nil {
			return err
		}

		return nil
	})
}

// BatchInsertUpdate inserts multiple swap updates to the store.
func (db *BaseDB) BatchInsertUpdate(ctx context.Context,
	updateData map[lntypes.Hash][]BatchInsertUpdateData) error {

	writeOpts := NewSqlWriteOpts()
	return db.ExecTx(ctx, writeOpts, func(tx *sqlc.Queries) error {
		for swapHash, updates := range updateData {
			for _, update := range updates {
				updateParams := sqlc.InsertSwapUpdateParams{
					SwapHash:        swapHash[:],
					UpdateTimestamp: update.Time.UTC(),
					UpdateState:     int32(update.State.State),
					ServerCost:      int64(update.State.Cost.Server),
					OnchainCost:     int64(update.State.Cost.Onchain),
					OffchainCost:    int64(update.State.Cost.Offchain),
				}

				if update.State.HtlcTxHash != nil {
					updateParams.HtlcTxhash = update.State.HtlcTxHash.String()
				}
				// First we insert the swap update.
				err := tx.InsertSwapUpdate(ctx, updateParams)
				if err != nil {
					return err
				}
			}
		}

		return nil
	})
}

// BatchUpdateLoopOutSwapCosts updates the swap costs for a batch of loop out
// swaps.
func (db *BaseDB) BatchUpdateLoopOutSwapCosts(ctx context.Context,
	costs map[lntypes.Hash]SwapCost) error {

	writeOpts := NewSqlWriteOpts()
	return db.ExecTx(ctx, writeOpts, func(tx *sqlc.Queries) error {
		for swapHash, cost := range costs {
			lastUpdateID, err := tx.GetLastUpdateID(
				ctx, swapHash[:],
			)
			if err != nil {
				return err
			}

			err = tx.OverrideSwapCosts(
				ctx, sqlc.OverrideSwapCostsParams{
					ID:           lastUpdateID,
					ServerCost:   int64(cost.Server),
					OnchainCost:  int64(cost.Onchain),
					OffchainCost: int64(cost.Offchain),
				},
			)
			if err != nil {
				return err
			}
		}

		return nil
	})
}

// HasMigration returns true if the migration with the given ID has been done.
func (db *BaseDB) HasMigration(ctx context.Context, migrationID string) (
	bool, error) {

	migration, err := db.GetMigration(ctx, migrationID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}

	return migration.MigrationTs.Valid, nil
}

// SetMigration marks the migration with the given ID as done.
func (db *BaseDB) SetMigration(ctx context.Context, migrationID string) error {
	return db.InsertMigration(ctx, sqlc.InsertMigrationParams{
		MigrationID: migrationID,
		MigrationTs: sql.NullTime{
			Time:  time.Now().UTC(),
			Valid: true,
		},
	})
}

// loopToInsertArgs converts a SwapContract struct to the arguments needed to
// insert it into the database.
func loopToInsertArgs(hash lntypes.Hash,
	swap *SwapContract) sqlc.InsertSwapParams {

	return sqlc.InsertSwapParams{
		SwapHash:         hash[:],
		Preimage:         swap.Preimage[:],
		InitiationTime:   swap.InitiationTime.UTC(),
		AmountRequested:  int64(swap.AmountRequested),
		CltvExpiry:       swap.CltvExpiry,
		MaxSwapFee:       int64(swap.MaxSwapFee),
		MaxMinerFee:      int64(swap.MaxMinerFee),
		InitiationHeight: swap.InitiationHeight,
		ProtocolVersion:  int32(swap.ProtocolVersion),
		Label:            swap.Label,
	}
}

// loopOutToInsertArgs converts a LoopOutContract struct to the arguments
// needed to insert it into the database.
func loopOutToInsertArgs(hash lntypes.Hash,
	loopOut *LoopOutContract) sqlc.InsertLoopOutParams {

	return sqlc.InsertLoopOutParams{
		SwapHash:            hash[:],
		DestAddress:         loopOut.DestAddr.String(),
		SingleSweep:         loopOut.IsExternalAddr,
		SwapInvoice:         loopOut.SwapInvoice,
		MaxSwapRoutingFee:   int64(loopOut.MaxSwapRoutingFee),
		SweepConfTarget:     loopOut.SweepConfTarget,
		HtlcConfirmations:   int32(loopOut.HtlcConfirmations),
		OutgoingChanSet:     loopOut.OutgoingChanSet.String(),
		PrepayInvoice:       loopOut.PrepayInvoice,
		MaxPrepayRoutingFee: int64(loopOut.MaxPrepayRoutingFee),
		PublicationDeadline: loopOut.SwapPublicationDeadline.UTC(),
		PaymentTimeout:      int32(loopOut.PaymentTimeout.Seconds()),
	}
}

// loopInToInsertArgs converts a LoopInContract struct to the arguments needed
// to insert it into the database.
func loopInToInsertArgs(hash lntypes.Hash,
	loopIn *LoopInContract) sqlc.InsertLoopInParams {

	loopInInsertParams := sqlc.InsertLoopInParams{
		SwapHash:       hash[:],
		HtlcConfTarget: loopIn.HtlcConfTarget,
		ExternalHtlc:   loopIn.ExternalHtlc,
	}

	if loopIn.LastHop != nil {
		loopInInsertParams.LastHop = loopIn.LastHop[:]
	}

	return loopInInsertParams
}

// swapToHtlcKeysInsertArgs extracts the htlc keys from a SwapContract struct
// and converts them to the arguments needed to insert them into the database.
func swapToHtlcKeysInsertArgs(hash lntypes.Hash,
	swap *SwapContract) sqlc.InsertHtlcKeysParams {

	return sqlc.InsertHtlcKeysParams{
		SwapHash:               hash[:],
		SenderScriptPubkey:     swap.HtlcKeys.SenderScriptKey[:],
		ReceiverScriptPubkey:   swap.HtlcKeys.ReceiverScriptKey[:],
		SenderInternalPubkey:   swap.HtlcKeys.SenderInternalPubKey[:],
		ReceiverInternalPubkey: swap.HtlcKeys.ReceiverInternalPubKey[:],
		ClientKeyFamily: int32(
			swap.HtlcKeys.ClientScriptKeyLocator.Family,
		),
		ClientKeyIndex: int32(
			swap.HtlcKeys.ClientScriptKeyLocator.Index,
		),
	}
}

// ConvertLoopOutRow converts a database row containing a loop out swap to a
// LoopOut struct.
func ConvertLoopOutRow(network *chaincfg.Params, row sqlc.GetLoopOutSwapRow,
	updates []sqlc.SwapUpdate) (*LoopOut,
	error) {

	htlcKeys, err := fetchHtlcKeys(
		row.SenderScriptPubkey, row.ReceiverScriptPubkey,
		row.SenderInternalPubkey, row.ReceiverInternalPubkey,
		row.ClientKeyFamily, row.ClientKeyIndex,
	)
	if err != nil {
		return nil, err
	}

	preimage, err := lntypes.MakePreimage(row.Preimage)
	if err != nil {
		return nil, err
	}

	destAddress, err := btcutil.DecodeAddress(row.DestAddress, network)
	if err != nil {
		return nil, err
	}

	swapHash, err := lntypes.MakeHash(row.SwapHash)
	if err != nil {
		return nil, err
	}

	loopOut := &LoopOut{
		Contract: &LoopOutContract{
			SwapContract: SwapContract{
				Preimage:         preimage,
				AmountRequested:  btcutil.Amount(row.AmountRequested),
				HtlcKeys:         htlcKeys,
				CltvExpiry:       row.CltvExpiry,
				MaxSwapFee:       btcutil.Amount(row.MaxSwapFee),
				MaxMinerFee:      btcutil.Amount(row.MaxMinerFee),
				InitiationHeight: row.InitiationHeight,
				InitiationTime:   row.InitiationTime,
				Label:            row.Label,
				ProtocolVersion:  ProtocolVersion(row.ProtocolVersion),
			},
			DestAddr:                destAddress,
			IsExternalAddr:          row.SingleSweep,
			SwapInvoice:             row.SwapInvoice,
			MaxSwapRoutingFee:       btcutil.Amount(row.MaxSwapRoutingFee),
			SweepConfTarget:         row.SweepConfTarget,
			HtlcConfirmations:       uint32(row.HtlcConfirmations),
			PrepayInvoice:           row.PrepayInvoice,
			MaxPrepayRoutingFee:     btcutil.Amount(row.MaxPrepayRoutingFee),
			SwapPublicationDeadline: row.PublicationDeadline,
			PaymentTimeout: time.Duration(
				row.PaymentTimeout,
			) * time.Second,
		},
		Loop: Loop{
			Hash: swapHash,
		},
	}

	if row.AssetID != nil {
		loopOut.Contract.AssetSwapInfo = &LoopOutAssetSwap{
			AssetId:     row.AssetID,
			SwapRfqId:   row.SwapRfqID,
			PrepayRfqId: row.PrepayRfqID,
			SwapPaidAmt: uint64(
				unmarshalSqlInt64(row.AssetAmtPaidSwap),
			),
			PrepayPaidAmt: uint64(
				unmarshalSqlInt64(row.AssetAmtPaidPrepay),
			),
		}
	}

	if row.OutgoingChanSet != "" {
		chanSet, err := ConvertOutgoingChanSet(row.OutgoingChanSet)
		if err != nil {
			return nil, err
		}

		loopOut.Contract.OutgoingChanSet = chanSet
	}

	// If we don't have any updates yet we can return early
	if len(updates) == 0 {
		return loopOut, nil
	}

	events, err := getSwapEvents(updates)
	if err != nil {
		return nil, err
	}

	loopOut.Events = events

	return loopOut, nil
}

// convertLoopInRow converts a database row containing a loop in swap to a
// LoopIn struct.
func (db *BaseDB) convertLoopInRow(row sqlc.GetLoopInSwapsRow,
	updates []sqlc.SwapUpdate) (*LoopIn, error) {

	htlcKeys, err := fetchHtlcKeys(
		row.SenderScriptPubkey, row.ReceiverScriptPubkey,
		row.SenderInternalPubkey, row.ReceiverInternalPubkey,
		row.ClientKeyFamily, row.ClientKeyIndex,
	)
	if err != nil {
		return nil, err
	}

	preimage, err := lntypes.MakePreimage(row.Preimage)
	if err != nil {
		return nil, err
	}

	swapHash, err := lntypes.MakeHash(row.SwapHash)
	if err != nil {
		return nil, err
	}

	loopIn := &LoopIn{
		Contract: &LoopInContract{
			SwapContract: SwapContract{
				Preimage:         preimage,
				AmountRequested:  btcutil.Amount(row.AmountRequested),
				HtlcKeys:         htlcKeys,
				CltvExpiry:       row.CltvExpiry,
				MaxSwapFee:       btcutil.Amount(row.MaxSwapFee),
				MaxMinerFee:      btcutil.Amount(row.MaxMinerFee),
				InitiationHeight: row.InitiationHeight,
				InitiationTime:   row.InitiationTime,
				Label:            row.Label,
				ProtocolVersion:  ProtocolVersion(row.ProtocolVersion),
			},
			HtlcConfTarget: row.HtlcConfTarget,
			ExternalHtlc:   row.ExternalHtlc,
		},
		Loop: Loop{
			Hash: swapHash,
		},
	}

	if row.LastHop != nil {
		lastHop, err := route.NewVertexFromBytes(row.LastHop)
		if err != nil {
			return nil, err
		}

		loopIn.Contract.LastHop = &lastHop
	}

	// If we don't have any updates yet we can return early
	if len(updates) == 0 {
		return loopIn, nil
	}

	events, err := getSwapEvents(updates)
	if err != nil {
		return nil, err
	}

	loopIn.Events = events

	return loopIn, nil
}

// getSwapEvents returns a slice of LoopEvents for the swap.
func getSwapEvents(updates []sqlc.SwapUpdate) ([]*LoopEvent, error) {
	events := make([]*LoopEvent, len(updates))

	for i := 0; i < len(events); i++ {
		events[i] = &LoopEvent{
			SwapStateData: SwapStateData{
				State: SwapState(updates[i].UpdateState),
				Cost: SwapCost{
					Server:   btcutil.Amount(updates[i].ServerCost),
					Onchain:  btcutil.Amount(updates[i].OnchainCost),
					Offchain: btcutil.Amount(updates[i].OffchainCost),
				},
			},
			Time: updates[i].UpdateTimestamp.UTC(),
		}

		if updates[i].HtlcTxhash != "" {
			chainHash, err := chainhash.NewHashFromStr(updates[i].HtlcTxhash)
			if err != nil {
				return nil, err
			}

			events[i].HtlcTxHash = chainHash
		}
	}

	return events, nil
}

// ConvertOutgoingChanSet converts a comma separated string of channel IDs into
// a ChannelSet.
func ConvertOutgoingChanSet(outgoingChanSet string) (ChannelSet, error) {
	// Split the string into a slice of strings
	chanStrings := strings.Split(outgoingChanSet, ",")
	channels := make([]uint64, len(chanStrings))

	// Iterate over the chanStrings slice and convert each string to ChannelID
	for i, chanString := range chanStrings {
		chanID, err := strconv.ParseInt(chanString, 10, 64)
		if err != nil {
			return nil, err
		}
		channels[i] = uint64(chanID)
	}

	return NewChannelSet(channels)
}

// fetchHtlcKeys converts the blob encoded htlc keys into a HtlcKeys struct.
func fetchHtlcKeys(senderScriptPubkey, receiverScriptPubkey,
	senderInternalPubkey, receiverInternalPubkey []byte,
	clientKeyFamily, clientKeyIndex int32) (HtlcKeys, error) {

	senderScriptKey, err := blobTo33ByteSlice(senderScriptPubkey)
	if err != nil {
		return HtlcKeys{}, err
	}

	receiverScriptKey, err := blobTo33ByteSlice(receiverScriptPubkey)
	if err != nil {
		return HtlcKeys{}, err
	}

	htlcKeys := HtlcKeys{
		SenderScriptKey:   senderScriptKey,
		ReceiverScriptKey: receiverScriptKey,
		ClientScriptKeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(clientKeyFamily),
			Index:  uint32(clientKeyIndex),
		},
	}

	if senderInternalPubkey != nil {
		senderInternalPubkey, err := blobTo33ByteSlice(
			senderInternalPubkey,
		)
		if err != nil {
			return HtlcKeys{}, err
		}
		htlcKeys.SenderInternalPubKey = senderInternalPubkey
	}

	if receiverInternalPubkey != nil {
		receiverInternalPubkey, err := blobTo33ByteSlice(
			receiverInternalPubkey,
		)
		if err != nil {
			return HtlcKeys{}, err
		}
		htlcKeys.ReceiverInternalPubKey = receiverInternalPubkey
	}

	return htlcKeys, nil
}

// blobTo33ByteSlice converts a blob encoded 33 byte public key into a
// [33]byte.
func blobTo33ByteSlice(blob []byte) ([33]byte, error) {
	if len(blob) != 33 {
		return [33]byte{}, errors.New("blob is not 33 bytes")
	}

	var key [33]byte
	copy(key[:], blob)

	return key, nil
}

func unmarshalSqlInt64(data sql.NullInt64) int64 {
	if !data.Valid {
		return 0
	}

	return data.Int64
}
