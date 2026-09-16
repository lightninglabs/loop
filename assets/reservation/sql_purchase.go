package reservation

import (
	"context"
	"database/sql"
	"errors"
	"math"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"google.golang.org/protobuf/proto"
)

func updatePurchase(ctx context.Context, q *sqlc.Queries,
	r *Reservation) error {

	args := sqlc.UpdateAssetReservationPurchaseParams{
		ReservationID:         r.ID[:],
		Fee:                   int64(r.Fee),
		CsvDelay:              int32(r.CSVDelay),
		RequiredConfirmations: int32(r.RequiredConfirmations),
		ExecutionDelta:        int32(r.ExecutionDelta),
		MinUsableBlocks:       int32(r.MinUsableBlocks),
		MainProbe:             int32(r.Probes.Main),
		SkipProbe:             r.SkipProbe,
		MaxRouteFeeMsat:       int64(r.MaxRouteFeeMsat),
		PrepayRouteFeeMsat:    int64(r.Probes.PrepayFeeMsat),
		MainRouteFeeMsat:      int64(r.Probes.MainFeeMsat),
		ConfirmationHeight:    int64(r.ConfirmationHeight),
		PrepayCredit:          int64(r.PrepayCredit),
		DepositProof:          r.ReservationProof,
	}
	if !r.Probes.CheckedAt.IsZero() {
		args.ProbesCheckedAt = sql.NullTime{
			Time:  r.Probes.CheckedAt,
			Valid: true,
		}
	}
	if r.PaymentRequest != nil {
		args.PaymentHash = r.PaymentHash[:]
		args.PayingNodeKey = r.PayingNodeKey.SerializeCompressed()
	}
	if r.FundingOutpoint != nil {
		args.FundingOutpoint = sql.NullString{
			String: r.FundingOutpoint.String(),
			Valid:  true,
		}
	}
	for _, field := range []struct {
		message proto.Message
		column  *[]byte
	}{
		{
			r.Quote,
			&args.Quote,
		},
		{
			r.PaymentRequest,
			&args.PaymentRequest,
		},
		{
			r.PaymentResult,
			&args.PaymentResult,
		},
	} {
		if !field.message.ProtoReflect().IsValid() {
			continue
		}
		data, err := proto.Marshal(field.message)
		if err != nil {
			return err
		}
		*field.column = data
	}
	count, err := q.UpdateAssetReservationPurchase(ctx, args)
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrNotFound
	}
	return nil
}

func readPurchase(row sqlc.AssetReservation, r *Reservation) error {
	r.SkipProbe = row.SkipProbe
	r.MaxRouteFeeMsat = uint64(row.MaxRouteFeeMsat)
	r.Probes = ProbeResults{
		Main:          ProbeOutcome(row.MainProbe),
		PrepayFeeMsat: uint64(row.PrepayRouteFeeMsat),
		MainFeeMsat:   uint64(row.MainRouteFeeMsat),
	}
	if row.ProbesCheckedAt.Valid {
		r.Probes.CheckedAt = row.ProbesCheckedAt.Time.UTC()
	}
	if len(row.Quote) != 0 {
		r.Quote = &swapserverrpc.AssetReservationQuote{}
		if err := proto.Unmarshal(row.Quote, r.Quote); err != nil {
			return err
		}
	}
	if len(row.PaymentRequest) != 0 {
		if len(row.PaymentHash) != 32 || len(row.PayingNodeKey) != 33 {
			return errors.New("invalid saved payment identity")
		}
		var err error
		r.PayingNodeKey, err = btcec.ParsePubKey(row.PayingNodeKey)
		if err != nil {
			return err
		}
		r.PaymentHash = lntypes.Hash(row.PaymentHash)
		r.PaymentRequest = &routerrpc.SendPaymentRequest{}
		if err := proto.Unmarshal(row.PaymentRequest, r.PaymentRequest); err != nil {
			return err
		}
	}
	if len(row.PaymentResult) != 0 {
		r.PaymentResult = &lnrpc.Payment{}
		if err := proto.Unmarshal(row.PaymentResult, r.PaymentResult); err != nil {
			return err
		}
	}
	if row.FundingOutpoint.Valid {
		var err error
		r.FundingOutpoint, err = ParseOutpoint(row.FundingOutpoint.String)
		if err != nil {
			return err
		}
	}
	if row.ConfirmationHeight < 0 || row.ConfirmationHeight > math.MaxInt32 {
		return errors.New("invalid saved confirmation height")
	}
	r.ConfirmationHeight = uint32(row.ConfirmationHeight)
	r.PrepayCredit = uint64(row.PrepayCredit)
	r.ReservationProof = row.DepositProof
	return nil
}
