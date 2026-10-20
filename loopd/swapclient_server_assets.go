package loopd

import (
	"bytes"
	"context"
	"slices"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/staticaddr/loopin"
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

	if !s.staticLoopInManager.AssetsEnabled() {
		return nil, assetOperationError(nil)
	}
	id, group, err := assetRequestIdentity(
		request.GetRequestId(), request.GetGroupKey(),
	)
	if err != nil {
		return nil, err
	}
	err = s.staticLoopInManager.CreateAssetDeposit(
		ctx, id, group, request.GetAmount(),
	)
	if err != nil {
		return nil, assetOperationError(err)
	}
	return &looprpc.AssetRequestStatus{
		RequestId: id[:], State: "queued", DepositRequest: true,
	}, nil
}

// listAssetDeposits reports native quantities and durable reservation owners.
func (s *swapClientServer) listAssetDeposits(ctx context.Context,
	request *looprpc.ListStaticAddressDepositsRequest) (
	*looprpc.ListStaticAddressDepositsResponse, error) {

	if request.StateFilter != looprpc.DepositState_UNKNOWN_STATE {
		return nil, status.Error(codes.InvalidArgument,
			"asset deposits do not support Bitcoin state filters")
	}
	for _, raw := range request.Outpoints {
		point, err := wire.NewOutPointFromString(raw)
		if err != nil || point.String() != raw {
			return nil, status.Error(codes.InvalidArgument,
				"invalid asset deposit outpoint")
		}
	}

	if !s.staticLoopInManager.AssetsEnabled() {
		return nil, assetOperationError(nil)
	}
	entries, err := s.staticLoopInManager.AssetInventory(ctx)
	if err != nil {
		return nil, assetOperationError(err)
	}
	result := &looprpc.ListStaticAddressDepositsResponse{AssetsOnly: true}
	for _, entry := range entries {
		if len(request.Outpoints) > 0 && !slices.Contains(
			request.Outpoints, entry.Deposit.Outpoint,
		) {

			continue
		}
		result.AssetDeposits = append(result.AssetDeposits,
			&looprpc.AssetDeposit{
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

// assetLoopInRequest validates the asset variant before admission. Bitcoin
// options cannot silently change meaning when native units are selected.
func assetLoopInRequest(request *looprpc.StaticAddressLoopInRequest) (
	loopin.AssetSwapRequest, error) {

	params := request.GetAssetParams()
	if params == nil || len(request.GetOutpoints()) != 0 ||
		request.GetAmount() != 0 || request.GetMaxSwapFeeSatoshis() != 0 ||
		len(request.GetLastHop()) != 0 || request.GetLabel() != "" ||
		request.GetInitiator() != "" || len(request.GetRouteHints()) != 0 ||
		request.GetPrivate() || request.GetPaymentTimeoutSeconds() != 0 ||
		request.GetFast() {

		return loopin.AssetSwapRequest{}, status.Error(
			codes.InvalidArgument, "invalid asset request options",
		)
	}
	id, group, err := assetRequestIdentity(
		params.GetRequestId(), params.GetGroupKey(),
	)
	if err != nil {
		return loopin.AssetSwapRequest{}, err
	}
	outpoint := params.Outpoint
	point, err := wire.NewOutPointFromString(outpoint)
	if err != nil || point.String() != outpoint {
		return loopin.AssetSwapRequest{}, status.Error(
			codes.InvalidArgument, "invalid asset deposit outpoint",
		)
	}
	if params.Amount == 0 || params.MaxAssetDebit < params.Amount {
		return loopin.AssetSwapRequest{}, status.Error(
			codes.InvalidArgument, "invalid native asset amounts",
		)
	}
	return loopin.AssetSwapRequest{
		RequestID: id, GroupKey: group, Outpoint: outpoint,
		Amount: params.Amount, MaxDebit: params.MaxAssetDebit,
		MaxFee: params.MaxAssetFee,
	}, nil
}

// assetStaticLoopIn queues an immutable native request through the normal
// static loop-in RPC. The server may not have assigned its payment hash yet.
func (s *swapClientServer) assetStaticLoopIn(ctx context.Context,
	request *looprpc.StaticAddressLoopInRequest) (
	*looprpc.StaticAddressLoopInResponse, error) {

	intent, err := assetLoopInRequest(request)
	if err != nil {
		return nil, err
	}
	if !s.staticLoopInManager.AssetsEnabled() {
		return nil, assetOperationError(nil)
	}
	if err := s.staticLoopInManager.StartAssetLoopIn(ctx, intent); err != nil {
		return nil, assetOperationError(err)
	}
	return &looprpc.StaticAddressLoopInResponse{
		AssetRequest: &looprpc.AssetRequestStatus{
			RequestId: intent.RequestID[:], State: "queued",
		},
	}, nil
}

// listAssetStaticLoopIns reports creation and recovered session progress.
func (s *swapClientServer) listAssetStaticLoopIns(ctx context.Context) (
	*looprpc.ListStaticAddressSwapsResponse, error) {

	if !s.staticLoopInManager.AssetsEnabled() {
		return nil, assetOperationError(nil)
	}
	jobs, err := s.staticLoopInManager.AssetJobs(ctx)
	if err != nil {
		return nil, assetOperationError(err)
	}
	result := &looprpc.ListStaticAddressSwapsResponse{AssetsOnly: true}
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
			record, err := s.staticLoopInManager.AssetStatus(
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
		result.AssetRequests = append(result.AssetRequests, entry)
	}
	return result, nil
}

// RefundAssetDeposit queues the original CSV recovery of an expired deposit.
func (s *swapClientServer) RefundAssetDeposit(ctx context.Context,
	request *looprpc.RefundAssetDepositRequest) (
	*looprpc.AssetRequestStatus, error) {

	if !s.staticLoopInManager.AssetsEnabled() {
		return nil, assetOperationError(nil)
	}
	id, err := s.staticLoopInManager.RefundAssetDeposit(
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
