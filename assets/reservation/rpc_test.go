package reservation

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/stretchr/testify/require"
)

type localRPCManager struct {
	PurchaseManager

	record    *Reservation
	approval  *looprpc.ApproveAssetReservationRequest
	buys      int
	skipProbe bool
}

func (m *localRPCManager) NewPurchase(_ context.Context, _ ID, _ [32]byte,
	_ uint64, skipProbe bool) (*Reservation, error) {

	m.buys++
	m.skipProbe = skipProbe
	return m.record, nil
}

func (m *localRPCManager) Get(context.Context, ID) (*Reservation, error) {
	return m.record, nil
}

func (m *localRPCManager) List(context.Context) ([]*Reservation, error) {
	return []*Reservation{m.record}, nil
}

func (m *localRPCManager) Approve(_ context.Context, _ ID,
	a *looprpc.ApproveAssetReservationRequest) (*Reservation, error) {

	if err := m.record.CheckApproval(a); err != nil {
		return nil, err
	}
	m.approval = a
	return m.record, nil
}

func TestLocalRPCRequiresSeparateApproval(t *testing.T) {
	r := testReservation()
	r.Quote = testPurchaseQuote(r)
	r.FundingOutpoint = &wire.OutPoint{
		Index: 1,
	}
	r.Probes = ProbeResults{
		Main:          ProbeSucceeded,
		CheckedAt:     time.Now(),
		MainFeeMsat:   10_000,
		PrepayFeeMsat: 11,
	}
	m := &localRPCManager{
		record: r,
	}
	s := &RPCServer{
		Manager: m,
	}
	got, err := s.Buy(t.Context(), &looprpc.BuyAssetReservationRequest{
		ReservationId: r.ID[:],
		AssetId:       r.AssetID[:],
		Amount:        r.Amount,
	})
	require.NoError(t, err)
	require.Equal(t, r.Fee, got.AssetFee)
	require.EqualValues(t, 10_000, got.MainProbeFeeMsat)
	require.EqualValues(t, 11, got.EstimatedPrepayRouteFeeMsat)
	require.Nil(t, m.approval)
	selector := &looprpc.ClientAssetReservationSelector{
		Selector: &looprpc.ClientAssetReservationSelector_Outpoint{
			Outpoint: r.FundingOutpoint.String(),
		},
	}
	_, err = s.Get(t.Context(), selector)
	require.NoError(t, err)
	require.Equal(t, 1, m.buys)
	require.Nil(t, m.approval)

	a := &looprpc.ApproveAssetReservationRequest{
		Reservation:     selector,
		QuoteHash:       got.QuoteHash,
		MaxRouteFeeMsat: 1000,
	}
	a.QuoteHash[0] ^= 1
	_, err = s.Approve(t.Context(), a)
	require.Error(t, err)
	require.Nil(t, m.approval)
	a.QuoteHash[0] ^= 1
	_, err = s.Approve(t.Context(), a)
	require.NoError(t, err)
	require.NotNil(t, m.approval)
}

func TestLocalRPCDisabled(t *testing.T) {
	s := &RPCServer{}
	_, err := s.Buy(t.Context(), &looprpc.BuyAssetReservationRequest{})
	require.Error(t, err)
	_, err = s.List(t.Context(), &looprpc.ListClientAssetReservationsRequest{})
	require.Error(t, err)
}

func TestLocalRPCSkipProbe(t *testing.T) {
	r := testReservation()
	r.Quote = testPurchaseQuote(r)
	r.SkipProbe = true
	m := &localRPCManager{
		record: r,
	}
	s := &RPCServer{
		Manager: m,
	}
	got, err := s.Buy(t.Context(), &looprpc.BuyAssetReservationRequest{
		ReservationId: r.ID[:],
		AssetId:       r.AssetID[:],
		Amount:        r.Amount,
		SkipProbe:     true,
	})
	require.NoError(t, err)
	require.True(t, m.skipProbe)
	require.True(t, got.SkipProbe)
	require.Zero(t, got.MainProbe)
	require.Zero(t, got.ProbesCheckedAt)
	require.Nil(t, m.approval)

	a := &looprpc.ApproveAssetReservationRequest{
		Reservation: &looprpc.ClientAssetReservationSelector{
			Selector: &looprpc.ClientAssetReservationSelector_ReservationId{
				ReservationId: r.ID[:],
			},
		},
		QuoteHash:       got.QuoteHash,
		MaxRouteFeeMsat: 1000,
	}
	_, err = s.Approve(t.Context(), a)
	require.Error(t, err)
	require.Nil(t, m.approval)
	a.SkipProbe = true
	_, err = s.Approve(t.Context(), a)
	require.NoError(t, err)
	require.True(t, m.approval.SkipProbe)
}
