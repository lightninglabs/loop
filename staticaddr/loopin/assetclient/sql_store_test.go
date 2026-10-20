package assetclient_test

import (
	"context"
	"errors"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/staticaddr/loopin"
	"github.com/lightninglabs/loop/staticaddr/loopin/assetclient"
	"github.com/lightninglabs/loop/staticaddr/loopin/assetexec"
	"github.com/lightninglabs/loop/swapkit/bridge"
	"github.com/lightningnetwork/lnd/clock"
)

type (
	Adapter       = assetclient.Adapter
	Server        = assetclient.Server
	Session       = assetclient.Session
	InvoiceIntent = assetclient.InvoiceIntent
)

var (
	OfferFromRPC    = assetclient.OfferFromRPC
	PaymentTerms    = assetclient.PaymentTerms
	NewServerClient = assetclient.NewServerClient
)

// sqlClientStore supplies real normal-store restart coverage while admitting
// the raw controller offers used by protocol-only test fixtures.
type sqlClientStore struct {
	*loopin.SqlStore

	db *loopdb.SqliteSwapStore
}

func openAssetClientStore(path string) (*sqlClientStore, error) {
	db, err := loopdb.NewSqliteStore(
		&loopdb.SqliteConfig{DatabaseFileName: path},
		&chaincfg.RegressionNetParams,
	)
	if err != nil {
		return nil, err
	}
	return &sqlClientStore{
		SqlStore: loopin.NewSqlStore(
			loopdb.NewTypedStore[loopin.Querier](db),
			clock.NewDefaultClock(),
			&chaincfg.RegressionNetParams,
		),
		db: db,
	}, nil
}
func (s *sqlClientStore) Close() error { return s.db.Close() }

func (s *sqlClientStore) Create(
	ctx context.Context,
	r *assetexec.Record,
) error {

	_, err := s.GetAssetJob(ctx, r.Offer.Terms.RequestID)
	if err != nil && !errors.Is(err, assetexec.ErrNotFound) {
		return err
	}
	if errors.Is(err, assetexec.ErrNotFound) {
		terms := r.Offer.Terms
		if len(terms.DepositOutpoints) != 1 {
			return assetexec.ErrInvalid
		}
		d := bridge.Deposit{
			Outpoint: terms.DepositOutpoints[0],
			GroupKey: terms.GroupKey,
			Amount:   terms.DepositAmount,
			Contract: terms.Contract,
		}
		if err := s.AddAssetDeposit(ctx, d); err != nil {
			return err
		}
		job := &loopin.AssetJob{
			ID:     terms.RequestID,
			Offer:  &r.Offer,
			Intent: r.InvoiceIntent,
			Swap: &loopin.AssetSwapRequest{
				RequestID: terms.RequestID,
				GroupKey:  terms.GroupKey,
				Outpoint:  d.Outpoint,
				Amount:    terms.PayoutAmount,
				MaxDebit:  terms.MaxDebit,
				MaxFee:    terms.DepositAmount - terms.PayoutAmount,
			},
		}
		if err := s.PutAssetJob(ctx, job); err != nil {
			return err
		}
	}
	return s.SqlStore.Create(ctx, r)
}
