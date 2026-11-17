package loop

import (
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// resumeTestServer supplies the replacement invoice for recovery.
type resumeTestServer struct {
	swapserverrpc.SwapServerClient
}

func (s *resumeTestServer) NewLoopOutSwap(context.Context,
	*swapserverrpc.ServerLoopOutRequest, ...grpc.CallOption) (
	*swapserverrpc.ServerLoopOutResponse, error) {

	return &swapserverrpc.ServerLoopOutResponse{
		SwapInvoice: "replacement",
	}, nil
}

// resumeTestLightning supplies the decoded replacement invoice.
type resumeTestLightning struct {
	lndclient.LightningClient
	invoice *lndclient.PaymentRequest
}

func (l *resumeTestLightning) DecodePaymentRequest(context.Context,
	string) (*lndclient.PaymentRequest, error) {

	return l.invoice, nil
}

// resumeTestRouter captures the actual request sent by the recovery path.
type resumeTestRouter struct {
	lndclient.RouterClient
	request *lndclient.SendPaymentRequest
	err     error
}

func (r *resumeTestRouter) SendPayment(_ context.Context,
	req lndclient.SendPaymentRequest) (chan lndclient.PaymentStatus,
	chan error, error) {

	r.request = &req
	return nil, nil, r.err
}

// TestResumeLoopOutPaymentLimits checks invoice validation and the payment
// parameters passed to LND, rather than a separate request-building helper.
func TestResumeLoopOutPaymentLimits(t *testing.T) {
	tests := []struct {
		name       string
		value      lnwire.MilliSatoshi
		routingFee btcutil.Amount
		channels   loopdb.ChannelSet
		wrongHash  bool
		wantErr    string
	}{
		{
			name: "at invoice ceiling", value: 51_000_000,
			routingFee: 7, channels: loopdb.ChannelSet{42, 99},
		},
		{
			name:  "routing budget above server fee",
			value: 50_500_000, routingFee: 2000,
			channels: loopdb.ChannelSet{42},
		},
		{
			name:  "channel restrictions with equal fee budgets",
			value: 50_500_000, routingFee: 1000,
			channels: loopdb.ChannelSet{42, 99},
		},
		{
			name:  "zero routing budget and unrestricted channels",
			value: 50_000_000,
		},
		{
			name: "one satoshi above ceiling", value: 51_001_000,
			wantErr: "exceeds max allowed",
		},
		{
			name:  "one millisatoshi above ceiling",
			value: 51_000_001, wantErr: "exceeds max allowed",
		},
		{
			name: "old doubled fee ceiling", value: 52_000_000,
			wantErr: "exceeds max allowed",
		},
		{
			name: "wrong payment hash", value: 50_000_000,
			wrongHash: true, wantErr: "does not match swap hash",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hash := lntypes.Hash{1}
			invoice := &lndclient.PaymentRequest{
				Hash: hash, Value: tc.value,
			}
			if tc.wrongHash {
				invoice.Hash = lntypes.Hash{2}
			}
			stop := errors.New("payment captured")
			router := &resumeTestRouter{err: stop}
			manager := &resumeManager{
				swapClient: &resumeTestServer{},
				lnd: &lndclient.GrpcLndServices{
					LndServices: lndclient.LndServices{
						Client: &resumeTestLightning{
							invoice: invoice,
						},
						Router: router,
					},
				},
			}
			swap := &loopdb.LoopOut{
				Loop: loopdb.Loop{Hash: hash},
				Contract: &loopdb.LoopOutContract{
					SwapContract: loopdb.SwapContract{
						AmountRequested: 50_000,
						MaxSwapFee:      1000,
					},
					MaxSwapRoutingFee: tc.routingFee,
					OutgoingChanSet:   tc.channels,
				},
			}
			err := manager.resumeLoopOutPayment(t.Context(), swap)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				require.Nil(t, router.request)
				return
			}
			require.ErrorIs(t, err, stop)
			require.NotNil(t, router.request)
			require.Equal(t, "replacement", router.request.Invoice)
			require.Equal(t, tc.routingFee, router.request.MaxFee)
			require.Zero(t, router.request.MaxFeeMsat)
			require.Equal(t, []uint64(tc.channels),
				router.request.OutgoingChanIds)
		})
	}
}
