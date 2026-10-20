package assetmanager

import (
	"context"
	"testing"
	"time"

	"github.com/lightninglabs/loop/staticaddr/loopin/assetclient"
	"github.com/lightninglabs/loop/staticaddr/loopin/assetexec"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type stalledReportServer struct {
	assetclient.Server
}

func (stalledReportServer) CancelAssetLoopIn(ctx context.Context,
	_ *swapserverrpc.AssetLoopInReference, _ ...grpc.CallOption) (
	*swapserverrpc.AssetLoopInStatus, error) {

	<-ctx.Done()
	return nil, ctx.Err()
}

func (stalledReportServer) ReportAssetLoopInRefund(ctx context.Context,
	_ *swapserverrpc.ReportAssetLoopInRefundRequest, _ ...grpc.CallOption) (
	*swapserverrpc.AssetLoopInStatus, error) {

	<-ctx.Done()
	return nil, ctx.Err()
}

type refundedJournal struct{ assetexec.Store }

func (refundedJournal) Get(
	context.Context,
	[32]byte,
) (*assetexec.Record, error) {
	return &assetexec.Record{
		State:          assetexec.HtlcTimeoutSwept,
		RefundEvidence: []byte("confirmed spendable refund"),
	}, nil
}

// TestOptionalReportsKeepRecoveryBudget ensures an unresponsive server cannot
// consume the worker's full action deadline after local recovery succeeded.
func TestOptionalReportsKeepRecoveryBudget(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		name := "refund"
		if canceled {
			name = "cancel"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(
				t.Context(),
				5*time.Second,
			)
			defer cancel()
			manager := &Manager{cfg: Config{
				Server:  stalledReportServer{},
				Journal: refundedJournal{},
			}}
			job := &Job{
				Offer:    &assetexec.Offer{},
				Canceled: canceled,
			}
			started := time.Now()
			require.ErrorIs(t, manager.reportRefund(ctx, job),
				context.DeadlineExceeded)
			require.Less(t, time.Since(started), 3*time.Second)
			require.NoError(t, ctx.Err())
			require.False(t, job.CancellationReported)
			require.False(t, job.RefundReported)
		})
	}
}
