package reservation

import (
	"bytes"
	"context"
	"errors"
	"slices"

	"github.com/lightninglabs/loop/looprpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// PurchaseManager is the local API's view of the purchase manager.
type PurchaseManager interface {
	NewPurchase(context.Context, ID, [32]byte, uint64, bool) (*Reservation, error)
	Get(context.Context, ID) (*Reservation, error)
	List(context.Context) ([]*Reservation, error)
	Approve(context.Context, ID, *looprpc.ApproveAssetReservationRequest) (
		*Reservation, error)
	RetryProbes(context.Context, ID) (*Reservation, error)
	Cancel(context.Context, ID) (*Reservation, error)
}

// RPCServer serves local calls protected by loopd's macaroon interceptor.
// It never authorizes payment merely because Buy or Get was called.
type RPCServer struct {
	looprpc.UnimplementedAssetReservationsServer

	Manager PurchaseManager
}

func localResult(r *Reservation, err error) (*looprpc.ClientAssetReservation,
	error) {

	if err != nil {
		return nil, status.Error(codes.Unavailable, "reservation unavailable")
	}
	if r == nil {
		return nil, status.Error(codes.Unavailable, "reservation unavailable")
	}
	result := &looprpc.ClientAssetReservation{
		ReservationId:               bytes.Clone(r.ID[:]),
		State:                       string(r.State),
		AssetId:                     bytes.Clone(r.AssetID[:]),
		Amount:                      r.Amount,
		AssetFee:                    r.Fee,
		CsvDelay:                    r.CSVDelay,
		RequiredConfirmations:       r.RequiredConfirmations,
		ExecutionDelta:              r.ExecutionDelta,
		MinUsableBlocks:             r.MinUsableBlocks,
		ConfirmationHeight:          r.ConfirmationHeight,
		PrepayCredit:                r.PrepayCredit,
		ProofVerified:               len(r.ReservationProof) != 0,
		SkipProbe:                   r.SkipProbe,
		MainProbe:                   looprpc.ReservationProbeResult(r.Probes.Main),
		EstimatedPrepayRouteFeeMsat: r.Probes.PrepayFeeMsat,
		MainProbeFeeMsat:            r.Probes.MainFeeMsat,
	}
	if !r.Probes.CheckedAt.IsZero() {
		result.ProbesCheckedAt = r.Probes.CheckedAt.Unix()
	}
	if r.FundingOutpoint != nil {
		result.Outpoint = r.FundingOutpoint.String()
	}
	if r.ConfirmationHeight != 0 {
		lifetime, err := r.Terms.Lifetime(r.ConfirmationHeight)
		if err != nil {
			return nil, status.Error(codes.Internal, "invalid reservation lifetime")
		}
		result.TimeoutHeight = lifetime.TimeoutHeight
		result.ExecutionCutoff = lifetime.ExecutionCutoff
	}
	if r.Quote != nil {
		hash, err := QuoteHash(r.Quote)
		if err != nil {
			return nil, status.Error(codes.Internal, "invalid saved quote")
		}
		result.QuoteHash = hash[:]
		result.EdgeKey = bytes.Clone(r.Quote.EdgeKey)
		result.PrepayAmountMsat = r.Quote.PrepayAmountMsat
		result.EstimatedMainAmountMsat = r.Quote.EstimatedMainAmountMsat
		result.QuoteExpiresAt = r.Quote.ExpiresAt
	}
	return result, nil
}

// Buy starts an unpaid purchase under a caller-supplied idempotency key.
func (s *RPCServer) Buy(ctx context.Context,
	req *looprpc.BuyAssetReservationRequest) (*looprpc.ClientAssetReservation,
	error) {

	if s.Manager == nil {
		return localResult(nil, errors.New("purchases are disabled"))
	}
	if req == nil || len(req.ReservationId) != 32 || len(req.AssetId) != 32 {
		return nil, status.Error(codes.InvalidArgument, "invalid purchase request")
	}
	return localResult(s.Manager.NewPurchase(ctx, ID(req.ReservationId),
		[32]byte(req.AssetId), req.Amount, req.SkipProbe))
}

func (s *RPCServer) resolve(ctx context.Context,
	req *looprpc.ClientAssetReservationSelector) (*Reservation, error) {

	if s.Manager == nil || req == nil {
		return nil, errors.New("missing reservation service or selector")
	}
	switch v := req.Selector.(type) {
	case *looprpc.ClientAssetReservationSelector_ReservationId:
		if len(v.ReservationId) != 32 || ID(v.ReservationId) == (ID{}) {
			return nil, errors.New("invalid reservation ID")
		}
		return s.Manager.Get(ctx, ID(v.ReservationId))

	case *looprpc.ClientAssetReservationSelector_Outpoint:
		point, err := ParseOutpoint(v.Outpoint)
		if err != nil {
			return nil, err
		}
		records, err := s.Manager.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, r := range records {
			if r.FundingOutpoint != nil && *r.FundingOutpoint == *point {
				return r, nil
			}
		}
	}
	return nil, ErrNotFound
}

// Get returns saved progress; it cannot trigger a new payment.
func (s *RPCServer) Get(ctx context.Context,
	req *looprpc.ClientAssetReservationSelector) (*looprpc.ClientAssetReservation,
	error) {

	return localResult(s.resolve(ctx, req))
}

// Approve binds payment to the displayed quote and explicit limits.
func (s *RPCServer) Approve(ctx context.Context,
	req *looprpc.ApproveAssetReservationRequest) (*looprpc.ClientAssetReservation,
	error) {

	if req == nil || len(req.QuoteHash) != 32 {
		return nil, status.Error(codes.InvalidArgument, "invalid approval")
	}
	r, err := s.resolve(ctx, req.Reservation)
	if err != nil {
		return localResult(nil, err)
	}
	return localResult(s.Manager.Approve(ctx, r.ID, req))
}

// RetryProbes checks the same unpaid quote again, without repricing it.
func (s *RPCServer) RetryProbes(ctx context.Context,
	req *looprpc.ClientAssetReservationSelector) (*looprpc.ClientAssetReservation,
	error) {

	r, err := s.resolve(ctx, req)
	if err != nil {
		return localResult(nil, err)
	}
	return localResult(s.Manager.RetryProbes(ctx, r.ID))
}

// Cancel asks the manager to reconcile payment before canceling a purchase.
func (s *RPCServer) Cancel(ctx context.Context,
	req *looprpc.ClientAssetReservationSelector) (*looprpc.ClientAssetReservation,
	error) {

	r, err := s.resolve(ctx, req)
	if err != nil {
		return localResult(nil, err)
	}
	return localResult(s.Manager.Cancel(ctx, r.ID))
}

// List returns one bounded page in stable ID order.
func (s *RPCServer) List(ctx context.Context,
	req *looprpc.ListClientAssetReservationsRequest) (
	*looprpc.ListClientAssetReservationsResponse, error) {

	if s.Manager == nil {
		return nil, status.Error(codes.Unavailable, "reservation unavailable")
	}
	if req == nil || req.Limit > 1000 ||
		(len(req.AfterId) != 0 && len(req.AfterId) != 32) {

		return nil, status.Error(codes.InvalidArgument, "invalid reservation page")
	}
	records, err := s.Manager.List(ctx)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "reservation unavailable")
	}
	slices.SortFunc(records, func(a, b *Reservation) int {
		return bytes.Compare(a.ID[:], b.ID[:])
	})
	limit := req.Limit
	if limit == 0 {
		limit = 100
	}
	result := &looprpc.ListClientAssetReservationsResponse{}
	for _, r := range records {
		if bytes.Compare(r.ID[:], req.AfterId) <= 0 {
			continue
		}
		if len(result.Reservations) == int(limit) {
			last := result.Reservations[len(result.Reservations)-1]
			result.NextAfterId = bytes.Clone(last.ReservationId)
			break
		}
		value, err := localResult(r, nil)
		if err != nil {
			return nil, err
		}
		result.Reservations = append(result.Reservations, value)
	}
	return result, nil
}
