package assets

import (
	"context"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/assets/htlc"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
)

// BaseDB is the interface that contains all the queries generated
// by sqlc for the instantout table.
type BaseDB interface {
	// ExecTx allows for executing a function in the context of a database
	// transaction.
	ExecTx(ctx context.Context, txOptions loopdb.TxOptions,
		txBody func(*sqlc.Queries) error) error

	CreateAssetSwap(ctx context.Context, arg sqlc.CreateAssetSwapParams) error
	CreateAssetOutSwap(ctx context.Context, swapHash []byte) error
	GetAllAssetOutSwaps(ctx context.Context) ([]sqlc.GetAllAssetOutSwapsRow, error)
	GetAssetOutSwap(ctx context.Context, swapHash []byte) (sqlc.GetAssetOutSwapRow, error)
	InsertAssetSwapUpdate(ctx context.Context, arg sqlc.InsertAssetSwapUpdateParams) error
	UpdateAssetSwapHtlcTx(ctx context.Context, arg sqlc.UpdateAssetSwapHtlcTxParams) error
	UpdateAssetSwapOutPreimage(ctx context.Context, arg sqlc.UpdateAssetSwapOutPreimageParams) error
	UpdateAssetSwapOutProof(ctx context.Context, arg sqlc.UpdateAssetSwapOutProofParams) error
	UpdateAssetSwapSweepTx(ctx context.Context, arg sqlc.UpdateAssetSwapSweepTxParams) error
}

// PostgresStore is the backing store for the instant out manager.
type PostgresStore struct {
	queries BaseDB
	clock   clock.Clock
}

// NewPostgresStore creates a new PostgresStore.
func NewPostgresStore(queries BaseDB) *PostgresStore {
	return &PostgresStore{
		queries: queries,
		clock:   clock.NewDefaultClock(),
	}
}

// CreateAssetSwapOut creates a new asset swap out in the database.
func (p *PostgresStore) CreateAssetSwapOut(ctx context.Context,
	swap *SwapOut) error {

	params := sqlc.CreateAssetSwapParams{
		SwapHash:         swap.SwapHash[:],
		AssetID:          swap.AssetID,
		Amt:              int64(swap.Amount),
		SenderPubkey:     swap.SenderPubKey.SerializeCompressed(),
		ReceiverPubkey:   swap.ReceiverPubKey.SerializeCompressed(),
		CsvExpiry:        int32(swap.CsvExpiry),
		InitiationHeight: int32(swap.InitiationHeight),
		CreatedTime:      p.clock.Now(),
		ServerKeyFamily:  int64(swap.ClientKeyLocator.Family),
		ServerKeyIndex:   int64(swap.ClientKeyLocator.Index),
	}

	return p.queries.ExecTx(
		ctx, &loopdb.SqliteTxOptions{}, func(q *sqlc.Queries) error {
			err := q.CreateAssetSwap(ctx, params)
			if err != nil {
				return err
			}

			return q.CreateAssetOutSwap(ctx, swap.SwapHash[:])
		},
	)
}

// UpdateAssetSwapHtlcOutpoint updates the htlc outpoint of the swap out in the
// database.
func (p *PostgresStore) UpdateAssetSwapHtlcOutpoint(ctx context.Context,
	swapHash lntypes.Hash, outpoint *wire.OutPoint, confirmationHeight int32) error {

	return p.queries.ExecTx(
		ctx, &loopdb.SqliteTxOptions{}, func(q *sqlc.Queries) error {
			return q.UpdateAssetSwapHtlcTx(
				ctx, sqlc.UpdateAssetSwapHtlcTxParams{
					SwapHash:               swapHash[:],
					HtlcTxid:               outpoint.Hash[:],
					HtlcVout:               int32(outpoint.Index),
					HtlcConfirmationHeight: confirmationHeight,
				})
		},
	)
}

// UpdateAssetSwapOutProof updates the raw proof of the swap out in the
// database.
func (p *PostgresStore) UpdateAssetSwapOutProof(ctx context.Context,
	swapHash lntypes.Hash, rawProof []byte) error {

	return p.queries.ExecTx(
		ctx, &loopdb.SqliteTxOptions{}, func(q *sqlc.Queries) error {
			return q.UpdateAssetSwapOutProof(
				ctx, sqlc.UpdateAssetSwapOutProofParams{
					SwapHash:     swapHash[:],
					RawProofFile: rawProof,
				})
		},
	)
}

// UpdateAssetSwapOutPreimage updates the preimage of the swap out in the
// database.
func (p *PostgresStore) UpdateAssetSwapOutPreimage(ctx context.Context,
	swapHash lntypes.Hash, preimage lntypes.Preimage) error {

	return p.queries.ExecTx(
		ctx, &loopdb.SqliteTxOptions{}, func(q *sqlc.Queries) error {
			return q.UpdateAssetSwapOutPreimage(
				ctx, sqlc.UpdateAssetSwapOutPreimageParams{
					SwapHash:     swapHash[:],
					SwapPreimage: preimage[:],
				})
		},
	)
}

// UpdateAssetSwapOutSweepTx updates the sweep tx of the swap out in the
// database.
func (p *PostgresStore) UpdateAssetSwapOutSweepTx(ctx context.Context,
	swapHash lntypes.Hash, sweepTxid chainhash.Hash, confHeight int32,
	sweepPkscript []byte) error {

	return p.queries.ExecTx(
		ctx, &loopdb.SqliteTxOptions{}, func(q *sqlc.Queries) error {
			return q.UpdateAssetSwapSweepTx(
				ctx, sqlc.UpdateAssetSwapSweepTxParams{
					SwapHash:                swapHash[:],
					SweepTxid:               sweepTxid[:],
					SweepConfirmationHeight: confHeight,
					SweepPkscript:           sweepPkscript,
				})
		},
	)
}

// InsertAssetSwapUpdate inserts a new swap update in the database.
func (p *PostgresStore) InsertAssetSwapUpdate(ctx context.Context,
	swapHash lntypes.Hash, state fsm.StateType) error {

	return p.queries.ExecTx(
		ctx, &loopdb.SqliteTxOptions{}, func(q *sqlc.Queries) error {
			return q.InsertAssetSwapUpdate(
				ctx, sqlc.InsertAssetSwapUpdateParams{
					SwapHash:        swapHash[:],
					UpdateState:     string(state),
					UpdateTimestamp: p.clock.Now(),
				})
		},
	)
}

// GetAllAssetOuts returns all the asset outs from the database.
func (p *PostgresStore) GetAllAssetOuts(ctx context.Context) ([]*SwapOut, error) {
	dbAssetOuts, err := p.queries.GetAllAssetOutSwaps(ctx)
	if err != nil {
		return nil, err
	}

	assetOuts := make([]*SwapOut, 0, len(dbAssetOuts))
	for _, dbAssetOut := range dbAssetOuts {
		assetOut, err := newSwapOutFromDB(
			dbAssetOut.AssetSwap, dbAssetOut.AssetOutSwap,
			dbAssetOut.UpdateState,
		)
		if err != nil {
			return nil, err
		}
		assetOuts = append(assetOuts, assetOut)
	}
	return assetOuts, nil
}

// GetActiveAssetOuts returns all the active asset outs from the database.
func (p *PostgresStore) GetActiveAssetOuts(ctx context.Context) ([]*SwapOut,
	error) {

	dbAssetOuts, err := p.queries.GetAllAssetOutSwaps(ctx)
	if err != nil {
		return nil, err
	}

	assetOuts := make([]*SwapOut, 0)
	for _, dbAssetOut := range dbAssetOuts {
		if IsFinishedState(fsm.StateType(dbAssetOut.UpdateState)) {
			continue
		}

		assetOut, err := newSwapOutFromDB(
			dbAssetOut.AssetSwap, dbAssetOut.AssetOutSwap,
			dbAssetOut.UpdateState,
		)
		if err != nil {
			return nil, err
		}
		assetOuts = append(assetOuts, assetOut)
	}

	return assetOuts, nil
}

// newSwapOutFromDB creates a new SwapOut from the databse rows.
func newSwapOutFromDB(assetSwap sqlc.AssetSwap,
	assetOutSwap sqlc.AssetOutSwap, state string) (
	*SwapOut, error) {

	swapHash, err := lntypes.MakeHash(assetSwap.SwapHash)
	if err != nil {
		return nil, err
	}

	var swapPreimage lntypes.Preimage
	if assetSwap.SwapPreimage != nil {
		swapPreimage, err = lntypes.MakePreimage(assetSwap.SwapPreimage)
		if err != nil {
			return nil, err
		}
	}

	senderPubkey, err := btcec.ParsePubKey(assetSwap.SenderPubkey)
	if err != nil {
		return nil, err
	}

	receiverPubkey, err := btcec.ParsePubKey(assetSwap.ReceiverPubkey)
	if err != nil {
		return nil, err
	}

	var htlcOutpoint *wire.OutPoint
	if assetSwap.HtlcTxid != nil {
		htlcHash, err := chainhash.NewHash(assetSwap.HtlcTxid)
		if err != nil {
			return nil, err
		}
		htlcOutpoint = wire.NewOutPoint(
			htlcHash, uint32(assetSwap.HtlcVout),
		)
	}

	var sweepOutpoint *wire.OutPoint
	if assetSwap.SweepTxid != nil {
		sweepHash, err := chainhash.NewHash(assetSwap.SweepTxid)
		if err != nil {
			return nil, err
		}
		sweepOutpoint = wire.NewOutPoint(
			sweepHash, 0,
		)
	}

	return &SwapOut{
		SwapKit: htlc.SwapKit{
			SwapHash:       swapHash,
			Amount:         uint64(assetSwap.Amt),
			SenderPubKey:   senderPubkey,
			ReceiverPubKey: receiverPubkey,
			CsvExpiry:      uint32(assetSwap.CsvExpiry),
			AssetID:        assetSwap.AssetID,
		},
		SwapPreimage:     swapPreimage,
		State:            fsm.StateType(state),
		InitiationHeight: uint32(assetSwap.InitiationHeight),
		ClientKeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(
				assetSwap.ServerKeyFamily,
			),
			Index: uint32(assetSwap.ServerKeyIndex),
		},
		HtlcOutPoint:            htlcOutpoint,
		HtlcConfirmationHeight:  uint32(assetSwap.HtlcConfirmationHeight),
		SweepOutpoint:           sweepOutpoint,
		SweepConfirmationHeight: uint32(assetSwap.SweepConfirmationHeight),
		SweepPkscript:           assetSwap.SweepPkscript,
		RawHtlcProof:            assetOutSwap.RawProofFile,
	}, nil
}
