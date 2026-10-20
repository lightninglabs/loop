package loopd

import (
	"bytes"
	"context"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/staticaddr/loopin/assetmanager"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// assetRequestIdentity validates fixed-size binary identifiers at the boundary.
func assetRequestIdentity(id, group []byte) ([32]byte, [33]byte, error) {
	var request [32]byte
	var key [33]byte
	if len(id) != len(request) || len(group) != len(key) {
		return request, key, status.Error(
			codes.InvalidArgument, "invalid asset request",
		)
	}
	copy(request[:], id)
	copy(key[:], group)
	parsed, err := btcec.ParsePubKey(group)
	if request == ([32]byte{}) || err != nil ||
		!bytes.Equal(parsed.SerializeCompressed(), group) {

		return request, key, status.Error(
			codes.InvalidArgument, "invalid asset request",
		)
	}
	return request, key, nil
}

// assetOperationError does not disclose lookup, locking or storage failures.
func assetOperationError(err error) error {
	if err != nil {
		warnf("Asset client operation failed: %v", err)
	}
	return status.Error(codes.Unavailable, "asset operation unavailable")
}

// CreateAssetDeposit durably queues funding with registered static keys.
func (s *swapClientServer) CreateAssetDeposit(ctx context.Context,
	request *looprpc.CreateAssetDepositRequest) (
	*looprpc.AssetRequestStatus, error) {

	if s.assetLoopInManager == nil {
		return nil, assetOperationError(nil)
	}
	id, group, err := assetRequestIdentity(
		request.GetRequestId(), request.GetGroupKey(),
	)
	if err != nil {
		return nil, err
	}
	err = s.assetLoopInManager.CreateDeposit(
		ctx, id, group, request.GetAmount(),
	)
	if err != nil {
		return nil, assetOperationError(err)
	}
	return &looprpc.AssetRequestStatus{
		RequestId: id[:], State: "queued", DepositRequest: true,
	}, nil
}

// ListAssetDeposits reports native quantities and durable reservation owners.
func (s *swapClientServer) ListAssetDeposits(ctx context.Context,
	_ *looprpc.ListAssetDepositsRequest) (
	*looprpc.ListAssetDepositsResponse, error) {

	if s.assetLoopInManager == nil {
		return nil, assetOperationError(nil)
	}
	entries, err := s.assetLoopInManager.Inventory(ctx)
	if err != nil {
		return nil, assetOperationError(err)
	}
	result := &looprpc.ListAssetDepositsResponse{}
	for _, entry := range entries {
		result.Deposits = append(result.Deposits, &looprpc.AssetDeposit{
			Outpoint:           entry.Deposit.Outpoint,
			GroupKey:           entry.Deposit.GroupKey[:],
			Amount:             entry.Deposit.Amount,
			ConfirmationHeight: entry.Deposit.ConfirmationHeight,
			RefundHeight:       entry.Deposit.RefundHeight,
			RequestId:          entry.RequestID[:],
			Consumed:           entry.Consumed,
		})
	}
	return result, nil
}

// AssetStaticLoopIn reserves one whole isolated asset deposit and queues its
// immutable offer request before contacting the server.
func (s *swapClientServer) AssetStaticLoopIn(ctx context.Context,
	request *looprpc.AssetStaticLoopInRequest) (
	*looprpc.AssetRequestStatus, error) {

	if s.assetLoopInManager == nil {
		return nil, assetOperationError(nil)
	}
	id, group, err := assetRequestIdentity(
		request.GetRequestId(), request.GetGroupKey(),
	)
	if err != nil {
		return nil, err
	}
	err = s.assetLoopInManager.Start(ctx, assetmanager.SwapRequest{
		RequestID: id, GroupKey: group, Outpoint: request.GetOutpoint(),
		Amount:   request.GetAmount(),
		MaxDebit: request.GetMaxAssetDebit(),
		MaxFee:   request.GetMaxAssetFee(),
	})
	if err != nil {
		return nil, assetOperationError(err)
	}
	return &looprpc.AssetRequestStatus{
		RequestId: id[:], State: "queued",
	}, nil
}

// ListAssetStaticLoopIns reports creation and recovered session progress.
func (s *swapClientServer) ListAssetStaticLoopIns(ctx context.Context,
	_ *looprpc.ListAssetStaticLoopInsRequest) (
	*looprpc.ListAssetStaticLoopInsResponse, error) {

	if s.assetLoopInManager == nil {
		return nil, assetOperationError(nil)
	}
	jobs, err := s.assetLoopInManager.Jobs(ctx)
	if err != nil {
		return nil, assetOperationError(err)
	}
	result := &looprpc.ListAssetStaticLoopInsResponse{}
	for _, job := range jobs {
		entry := &looprpc.AssetRequestStatus{
			RequestId: job.ID[:], State: "queued",
			DepositRequest: job.Deposit != nil,
			RefundRequest:  job.Refund != nil,
		}
		if job.Deposit != nil && job.Complete {
			entry.State = "funded"
		}
		if job.Refund != nil && job.Complete {
			entry.State = "refunded"
		}
		if job.Offer != nil {
			entry.PaymentHash = job.Offer.Hash[:]
			controller := s.assetLoopInManager.Session.Controller
			record, err := controller.Status(
				ctx,
				job.Offer.Hash,
			)
			if err == nil {
				entry.State = string(record.State)
			} else {
				entry.State = "preparing"
			}
		}
		if job.Canceled {
			entry.State = "canceled"
		}
		result.Requests = append(result.Requests, entry)
	}
	return result, nil
}

// RefundAssetDeposit queues the original CSV recovery of an expired deposit.
func (s *swapClientServer) RefundAssetDeposit(ctx context.Context,
	request *looprpc.RefundAssetDepositRequest) (
	*looprpc.AssetRequestStatus, error) {

	if s.assetLoopInManager == nil {
		return nil, assetOperationError(nil)
	}
	id, err := s.assetLoopInManager.RefundDeposit(
		ctx,
		request.GetOutpoint(),
	)
	if err != nil {
		return nil, assetOperationError(err)
	}
	return &looprpc.AssetRequestStatus{
		RequestId: id[:], State: "queued", RefundRequest: true,
	}, nil
}
