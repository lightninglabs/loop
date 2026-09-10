package reservation

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func testPurchaseQuote(r *Reservation) *swapserverrpc.AssetReservationQuote {
	_, server := btcec.PrivKeyFromBytes([]byte{2})
	_, edge := btcec.PrivKeyFromBytes([]byte{3})
	return &swapserverrpc.AssetReservationQuote{
		ReservationId: r.ID[:],
		Terms: &swapserverrpc.AssetReservationTerms{
			AssetId:               r.AssetID[:],
			Amount:                r.Amount,
			Fee:                   11,
			CsvDelay:              1440,
			RequiredConfirmations: 3,
			ExecutionDelta:        90,
			MinUsableBlocks:       1000,
		},
		ClientKey:               r.ClientKey.PubKey.SerializeCompressed(),
		ServerKey:               server.SerializeCompressed(),
		ReceivingNodeKey:        server.SerializeCompressed(),
		EdgeKey:                 edge.SerializeCompressed(),
		PrepayInvoice:           "prepay",
		ProbeInvoice:            "probe only",
		PrepayAmountMsat:        11000,
		EstimatedMainAmountMsat: 10000000,
		ExpiresAt:               1900000000,
	}
}

func testApproval(t *testing.T, r *Reservation) *looprpc.ApproveAssetReservationRequest {

	t.Helper()
	hash, err := QuoteHash(r.Quote)
	require.NoError(t, err)
	return &looprpc.ApproveAssetReservationRequest{
		QuoteHash:       hash[:],
		MaxRouteFeeMsat: 100,
	}
}

func TestPurchaseStore(t *testing.T) {
	ctx := t.Context()
	db := loopdb.NewTestDB(t)
	store := NewSqlStore(db.BaseDB)
	r := testReservation()
	r.Terms = Terms{
		AssetID: r.AssetID,
		Amount:  r.Amount,
	}
	r.State = "RequestQuote"
	require.NoError(t, store.CreateReservation(ctx, r))
	request := *r
	r.Quote = testPurchaseQuote(r)
	var err error
	r.Terms, err = TermsFromRPC(r.Quote.Terms)
	require.NoError(t, err)
	r.Probes = ProbeResults{
		FeeKnown:  true,
		Main:      ProbeSucceeded,
		CheckedAt: time.Unix(1800000000, 0).UTC(),
	}
	r.ProbeNodeKey = r.ClientKey.PubKey
	r.ProbeRequest = &routerrpc.SendPaymentRequest{
		PaymentRequest: r.Quote.ProbeInvoice,
		TimeoutSeconds: 10, MaxParts: 1,
	}
	r.ProbeDeadline = time.Unix(1800000010, 0).UTC()
	r.ProbeResult = &lnrpc.Payment{
		PaymentHash: ProbeHash(r.ID).String(),
		Status:      lnrpc.Payment_FAILED,
	}
	r.State = "AwaitApproval"
	require.NoError(t, store.UpdateReservation(ctx, r))
	r.MaxRouteFeeMsat = 100
	r.State = PayPrepay
	require.NoError(t, store.UpdateReservation(ctx, r))

	preimage := lntypes.Preimage{9}
	r.PaymentHash = preimage.Hash()
	r.PayingNodeKey = r.ClientKey.PubKey
	r.PaymentRequest = &routerrpc.SendPaymentRequest{
		PaymentRequest: r.Quote.PrepayInvoice,
		FeeLimitMsat:   100,
	}
	r.State = "PayPrepay"
	require.NoError(t, store.UpdateReservation(ctx, r))
	r.PaymentResult = &lnrpc.Payment{
		PaymentHash:     r.PaymentHash.String(),
		PaymentPreimage: preimage.String(), Status: lnrpc.Payment_SUCCEEDED,
	}
	r.FundingOutpoint = &wire.OutPoint{Hash: chainhash.Hash{3}, Index: 2}
	r.ConfirmationHeight = 100
	r.PrepayCredit = 11
	r.ReservationProof = []byte{1, 2, 3}
	r.State = "Ready"
	require.NoError(t, store.UpdateReservation(ctx, r))
	loaded, err := NewSqlStore(db.BaseDB).GetReservation(ctx, r.ID)
	require.NoError(t, err)
	require.True(t, proto.Equal(r.Quote, loaded.Quote))
	require.True(t, proto.Equal(r.PaymentRequest, loaded.PaymentRequest))
	require.True(t, proto.Equal(r.PaymentResult, loaded.PaymentResult))
	require.Equal(t, r.MaxRouteFeeMsat, loaded.MaxRouteFeeMsat)
	require.Equal(t, r.SkipProbe, loaded.SkipProbe)
	require.Equal(t, r.Probes, loaded.Probes)
	require.True(t, proto.Equal(r.ProbeRequest, loaded.ProbeRequest))
	require.True(t, proto.Equal(r.ProbeResult, loaded.ProbeResult))
	require.Equal(t, r.ProbeDeadline, loaded.ProbeDeadline)
	require.True(t, r.ProbeNodeKey.IsEqual(loaded.ProbeNodeKey))
	require.Equal(t, r.FundingOutpoint, loaded.FundingOutpoint)
	require.Equal(t, r.ReservationProof, loaded.ReservationProof)
	require.Equal(t, r.PrepayCredit, loaded.PrepayCredit)

	// A retry of the original unquoted request restores the same purchase.
	require.NoError(t, store.CreateReservation(ctx, &request))
	require.Equal(t, r.State, request.State)
	require.True(t, proto.Equal(r.Quote, request.Quote))
	updates, err := db.GetAssetReservationUpdates(ctx, r.ID[:])
	require.NoError(t, err)
	require.NoError(t, store.UpdateReservation(ctx, loaded))
	after, err := db.GetAssetReservationUpdates(ctx, r.ID[:])
	require.NoError(t, err)
	require.Len(t, after, len(updates))

	for _, mutate := range []func(*Reservation){
		func(v *Reservation) { v.Quote.PrepayInvoice += "changed" },
		func(v *Reservation) { v.MaxRouteFeeMsat++ },
		func(v *Reservation) { v.SkipProbe = true },
		func(v *Reservation) { v.State = AwaitApproval },
		func(v *Reservation) { v.PaymentRequest.TimeoutSeconds++ },
		func(v *Reservation) { v.FundingOutpoint.Index++ },
		func(v *Reservation) { v.PrepayCredit = 0 },
		func(v *Reservation) { v.ReservationProof = nil },
	} {
		changed, err := store.GetReservation(ctx, r.ID)
		require.NoError(t, err)
		mutate(changed)
		require.Error(t, store.UpdateReservation(ctx, changed))
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	loaded.State = "Expired"
	require.Error(t, store.UpdateReservation(canceled, loaded))
	loaded, err = store.GetReservation(ctx, r.ID)
	require.NoError(t, err)
	require.EqualValues(t, "Ready", loaded.State)
}

func TestApprovalBindsQuotedAmounts(t *testing.T) {
	for _, field := range []string{"asset fee", "BTC prepay"} {
		t.Run(field, func(t *testing.T) {
			r := testReservation()
			r.Quote = testPurchaseQuote(r)
			a := testApproval(t, r)
			a.SkipProbe = true
			require.NoError(t, r.CheckApproval(a))

			// Consent to the displayed quote cannot authorize a
			// different amount, even when the caller skips probing.
			switch field {
			case "asset fee":
				r.Quote.Terms.Fee++

			case "BTC prepay":
				r.Quote.PrepayAmountMsat++
			}
			require.Error(t, r.CheckApproval(a))
		})
	}
}

func TestApprovalAndOutpoint(t *testing.T) {
	r := testReservation()
	r.Quote = testPurchaseQuote(r)
	r.Probes = ProbeResults{
		FeeKnown:  true,
		Main:      ProbeFailed,
		CheckedAt: time.Unix(1800000000, 0),
	}
	a := testApproval(t, r)
	// A failed probe needs explicit consent. SkipProbe cannot override limits.
	require.Error(t, r.CheckApproval(a))
	a.SkipProbe = true
	require.NoError(t, r.CheckApproval(a))
	a.MaxRouteFeeMsat = math.MaxUint64
	require.Error(t, r.CheckApproval(a))
	a.MaxRouteFeeMsat = 0
	require.NoError(t, r.CheckApproval(a))
	a.QuoteHash[0]++
	require.Error(t, r.CheckApproval(a))

	text := strings.Repeat("ab", 32) + ":2"
	outpoint, err := ParseOutpoint(text)
	require.NoError(t, err)
	require.Equal(t, text, outpoint.String())
	for _, invalid := range []string{
		strings.ToUpper(text), text[:len(text)-1] + "02", "bad",
	} {
		_, err := ParseOutpoint(invalid)
		require.Error(t, err)
	}
}
