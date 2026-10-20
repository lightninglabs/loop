package assetclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightninglabs/loop/assets/assetln"
	"github.com/lightninglabs/loop/staticaddr/loopin/assetexec"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/taproot-assets/taprpc/tapchannelrpc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

var errLost = errors.New("lost RPC response")

type testServer struct {
	Server
	create func(*swapserverrpc.CreateAssetLoopInRequest) (
		*swapserverrpc.AssetLoopInStatus, error)
	prepare  func(*swapserverrpc.PrepareAssetLoopInRequest) error
	fallback func() ([]byte, error)
}

func (s *testServer) CreateAssetLoopIn(_ context.Context,
	r *swapserverrpc.CreateAssetLoopInRequest, _ ...grpc.CallOption) (
	*swapserverrpc.AssetLoopInStatus, error) {

	return s.create(r)
}

func (s *testServer) PrepareAssetLoopIn(_ context.Context,
	r *swapserverrpc.PrepareAssetLoopInRequest, _ ...grpc.CallOption) (
	*swapserverrpc.AssetLoopInStatus, error) {

	return &swapserverrpc.AssetLoopInStatus{}, s.prepare(r)
}

func (s *testServer) AssetLoopInFallback(context.Context,
	*swapserverrpc.AssetLoopInReference, ...grpc.CallOption) (
	*swapserverrpc.AssetLoopInFallbackResponse, error) {

	bundle, err := s.fallback()
	return &swapserverrpc.AssetLoopInFallbackResponse{
		RecoveryBundle: bundle,
	}, err
}

func offerFixture(t *testing.T) (*swapserverrpc.CreateAssetLoopInRequest,
	*swapserverrpc.AssetLoopInStatus, assetexec.Offer) {

	t.Helper()
	_, pub := btcec.PrivKeyFromBytes([]byte{1})
	group := pub.SerializeCompressed()
	group[0] = 3
	txid := make([]byte, 32)
	txid[0], txid[31] = 1, 2
	request := &swapserverrpc.CreateAssetLoopInRequest{
		ProtocolVersion: 1, GroupKey: group,
		RequestId: bytes.Repeat([]byte{1}, 32),
		Deposits: []*swapserverrpc.AssetDepositInput{{
			Txid: txid, OutputIndex: 2, Proof: []byte("proof"),
		}},
		PayoutAmount: 90, MaxAssetFee: 5, MaxAssetDebit: 95,
		Contract: []byte("approved-deposit-contract"),
	}
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(request)
	require.NoError(t, err)
	digest := sha256.Sum256(raw)
	clone := proto.Clone(request.Deposits[0])
	input := clone.(*swapserverrpc.AssetDepositInput)
	response := &swapserverrpc.AssetLoopInStatus{
		Offer: &swapserverrpc.AssetLoopInOffer{
			ProtocolVersion: 1,
			PaymentHash:     bytes.Repeat([]byte{2}, 32),
			GroupKey: bytes.Clone(
				group,
			), RequestId: request.RequestId,
			RequestDigest: digest[:],
			Deposits: []*swapserverrpc.AssetDepositInput{
				input,
			},
			DepositAmount: 100, PayoutAmount: 90, AssetFee: 5,
			ChangeAmount: 5, MaxAssetDebit: 95, RefundHeight: 100,
			MinPayoutExpiry: 121,
			RecoveryDelta:   10,
			ExecutionDelta:  10,
			OfferExpiry:     time.Now().Add(time.Hour).Unix(),
			Contract:        bytes.Clone(request.Contract),
		},
	}
	offer, err := OfferFromRPC([32]byte{1}, response)
	require.NoError(t, err)
	return request, response, offer
}

func TestRequestOfferBindsExactSelectionAndContract(t *testing.T) {
	request, response, expected := offerFixture(t)
	adapter := Adapter{Server: &testServer{
		create: func(*swapserverrpc.CreateAssetLoopInRequest) (
			*swapserverrpc.AssetLoopInStatus, error) {

			return response, nil
		},
	}}
	offer, err := adapter.RequestOffer(t.Context(), [32]byte{1}, request)
	require.NoError(t, err)
	require.Equal(t, expected, offer)
	require.EqualValues(t, 3, offer.Terms.GroupKey[0])
	require.Equal(t, offer.Terms.GroupKey,
		PaymentTerms(offer, 10).GroupKey)
	reversed := bytes.Clone(request.Deposits[0].Txid)
	for i := range len(reversed) / 2 {
		j := len(reversed) - 1 - i
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	require.Equal(t, []string{fmt.Sprintf("%x:2", reversed)},
		offer.Terms.DepositOutpoints)

	for _, test := range []struct {
		name   string
		mutate func(*swapserverrpc.AssetLoopInOffer)
	}{
		{"contract", func(o *swapserverrpc.AssetLoopInOffer) {
			o.Contract = []byte("substituted")
		}},
		{"outpoint", func(o *swapserverrpc.AssetLoopInOffer) {
			o.Deposits[0].Txid[0]++
		}},
		{"index", func(o *swapserverrpc.AssetLoopInOffer) {
			o.Deposits[0].OutputIndex++
		}},
		{"duplicate", func(o *swapserverrpc.AssetLoopInOffer) {
			o.Deposits = append(o.Deposits, o.Deposits[0])
		}},
		{"group parity", func(o *swapserverrpc.AssetLoopInOffer) {
			o.GroupKey[0] = 2
		}},
		{"digest", func(o *swapserverrpc.AssetLoopInOffer) {
			o.RequestDigest[0]++
		}},
		{"empty id", func(o *swapserverrpc.AssetLoopInOffer) {
			o.RequestId = make([]byte, 32)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cloned := proto.Clone(response)
			candidate := cloned.(*swapserverrpc.AssetLoopInStatus)
			test.mutate(candidate.Offer)
			adapter.Server = &testServer{create: func(
				*swapserverrpc.CreateAssetLoopInRequest) (
				*swapserverrpc.AssetLoopInStatus, error) {

				return candidate, nil
			}}
			_, err := adapter.RequestOffer(
				t.Context(),
				[32]byte{1},
				request,
			)
			require.ErrorIs(t, err, assetexec.ErrInvalid)
		})
	}
}

type testLightning struct {
	lnrpc.LightningClient
	invoice   *lnrpc.Invoice
	decoded   *lnrpc.PayReq
	lookupErr error
}

func (l *testLightning) LookupInvoice(context.Context, *lnrpc.PaymentHash,
	...grpc.CallOption) (*lnrpc.Invoice, error) {

	return l.invoice, l.lookupErr
}

func (l *testLightning) DecodePayReq(context.Context, *lnrpc.PayReqString,
	...grpc.CallOption) (*lnrpc.PayReq, error) {

	return l.decoded, nil
}

type testAssets struct {
	tapchannelrpc.TaprootAssetChannelsClient
	add func(*tapchannelrpc.AddInvoiceRequest) error
}

func (a *testAssets) AddInvoice(_ context.Context,
	r *tapchannelrpc.AddInvoiceRequest, _ ...grpc.CallOption) (
	*tapchannelrpc.AddInvoiceResponse, error) {

	return nil, a.add(r)
}

func TestAdapterRecoversLostInvoiceCreationWithoutNewQuote(t *testing.T) {
	_, _, offer := offerFixture(t)
	ln := &testLightning{lookupErr: status.Error(codes.NotFound, "missing")}
	calls := 0
	receiver := &assetln.Receiver{Lightning: ln}
	receiver.Assets = &testAssets{add: func(
		req *tapchannelrpc.AddInvoiceRequest) error {

		calls++
		require.Equal(t, offer.Hash[:], req.HodlInvoice.PaymentHash)
		require.Equal(t, offer.Terms.GroupKey[:], req.GroupKey)
		ln.lookupErr = nil
		ln.invoice = &lnrpc.Invoice{
			RHash:          offer.Hash[:],
			PaymentRequest: "original-invoice",
			State:          lnrpc.Invoice_OPEN,
		}
		ln.decoded = &lnrpc.PayReq{
			PaymentHash: hex.EncodeToString(offer.Hash[:]),
			NumMsat:     100,
			CltvExpiry:  int64(req.InvoiceRequest.CltvExpiry),
			Timestamp:   time.Now().Unix(), Expiry: 7200,
		}
		return errLost
	}}
	adapter := Adapter{
		Receiver: receiver, HoldExpiryDelta: 10,
		Height: func(context.Context) (uint32, error) {
			return 10, nil
		},
	}
	_, err := adapter.LookupInvoice(t.Context(), offer)
	require.ErrorIs(t, err, assetexec.ErrNotFound)
	intent, err := json.Marshal(InvoiceIntent{Peer: []byte("peer")})
	require.NoError(t, err)
	_, err = adapter.CreateInvoice(t.Context(), offer, intent)
	require.ErrorIs(t, err, errLost)
	invoice, err := adapter.LookupInvoice(t.Context(), offer)
	require.NoError(t, err)
	require.Equal(t, "original-invoice", invoice.PaymentRequest)
	retry, err := adapter.CreateInvoice(t.Context(), offer, intent)
	require.NoError(t, err)
	require.Equal(t, invoice, retry)
	require.Equal(t, 1, calls)
	ln.lookupErr = status.Error(codes.Unavailable, "offline")
	_, err = adapter.CreateInvoice(t.Context(), offer, intent)
	require.Equal(t, codes.Unavailable, status.Code(err))
	require.Equal(t, 1, calls)
}

type sessionActions struct {
	assetexec.Actions
	creates, pushes, cancels int
}

func (*sessionActions) CurrentHeight(context.Context) (uint32, error) {
	return 0, nil
}

func (*sessionActions) ValidateOffer(context.Context, assetexec.Offer) error {
	return nil
}
func (*sessionActions) ReserveDeposits(context.Context, assetexec.Offer) error {
	return nil
}
func (*sessionActions) ReleaseDeposits(context.Context, assetexec.Offer) error {
	return nil
}
func (a *sessionActions) CancelInvoice(context.Context, assetexec.Offer) error {
	a.cancels++
	return nil
}
func (a *sessionActions) CreateInvoice(context.Context, assetexec.Offer,
	[]byte) (assetexec.Invoice, error) {

	a.creates++
	return assetexec.Invoice{
		PaymentRequest: "held-invoice", PaymentPlan: []byte("rfq"),
	}, nil
}
func (*sessionActions) VerifyFinalizeFallback(context.Context, assetexec.Offer,
	[]byte) (assetexec.Fallback, error) {

	return assetexec.Fallback{
		Bundle: []byte(
			"signed-sdk-bundle",
		), ClaimSignatures: []byte("sig"),
	}, nil
}
func (*sessionActions) Prepared(context.Context, assetexec.Offer) (
	[]byte, error) {

	return []byte("local-accepted-evidence"), nil
}
func (a *sessionActions) PushSignatures(context.Context, assetexec.Offer,
	[]byte) error {

	a.pushes++
	return nil
}

func TestSessionRecoveryResumesLostHandshakeResponses(t *testing.T) {
	for _, lostPhase := range []string{"prepare", "fallback"} {
		t.Run(lostPhase, func(t *testing.T) {
			testSessionRecoveryResumesLostResponses(t, lostPhase)
		})
	}
}

type snapshotStore struct {
	assetexec.Store
	record *assetexec.Record
}

func (s snapshotStore) Get(context.Context, [32]byte) (
	*assetexec.Record, error) {

	return s.record, nil
}

func TestAdapterSettlementUsesPersistedAcceptedCircuit(t *testing.T) {
	_, _, offer := offerFixture(t)
	evidence := assetln.Evidence{
		Hash: offer.Hash, GroupKey: offer.Terms.GroupKey,
		Amount: offer.Terms.PayoutAmount, ChannelID: 42, HTLCIndex: 7,
		ExpiryHeight: 200, RFQID: [32]byte{3},
	}
	raw, err := json.Marshal(evidence)
	require.NoError(t, err)
	invoice := &lnrpc.Invoice{
		RHash: offer.Hash[:], State: lnrpc.Invoice_SETTLED,
		Htlcs: []*lnrpc.InvoiceHTLC{{
			ChanId:    42,
			HtlcIndex: 7,
			State:     lnrpc.InvoiceHTLCState_SETTLED,
		}},
	}
	adapter := Adapter{
		Receiver: &assetln.Receiver{Lightning: &testLightning{
			invoice: invoice,
		}},
		Store: snapshotStore{record: &assetexec.Record{
			AcceptedEvidence: raw,
		}},
		HoldExpiryDelta: 10,
	}
	// No ListChannels implementation exists: settled evidence must survive
	// a channel close while remaining bound to the exact accepted circuit.
	settled, err := adapter.InvoiceSettled(t.Context(), offer)
	require.NoError(t, err)
	require.True(t, settled)
	invoice.Htlcs[0].ChanId++
	_, err = adapter.InvoiceSettled(t.Context(), offer)
	require.ErrorIs(t, err, assetln.ErrEvidence)
}

func TestSessionRecoveryCancelsExpiredUnexposedOffer(t *testing.T) {
	_, _, offer := offerFixture(t)
	offer.Terms.OfferExpiry = time.Now().Add(-time.Minute)
	store, err := assetexec.OpenBoltStore(
		filepath.Join(t.TempDir(), "client.db"),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	actions := &sessionActions{}
	controller := assetexec.NewController(store, actions, func() time.Time {
		return offer.Terms.OfferExpiry.Add(-time.Hour)
	})
	require.NoError(
		t,
		controller.Start(t.Context(), offer, []byte("intent")),
	)
	session := Session{
		Controller: controller,
		Adapter:    &Adapter{Store: store, Actions: actions},
	}
	require.NoError(t, session.Recover(t.Context(), offer.Hash))
	require.Equal(t, 1, actions.cancels)
	require.Zero(t, actions.creates)
	require.NoError(t, session.Recover(t.Context(), offer.Hash))
	require.Equal(t, 1, actions.cancels)
}

func TestRequestOfferRejectsDuplicateSelectionBeforeRPC(t *testing.T) {
	request, _, _ := offerFixture(t)
	request.Deposits = append(request.Deposits, request.Deposits[0])
	adapter := Adapter{}
	_, err := adapter.RequestOffer(t.Context(), [32]byte{1}, request)
	require.ErrorIs(t, err, assetexec.ErrInvalid)
}

func TestPreimageRecoveryUsesAuthenticatedLocalSettledInvoice(t *testing.T) {
	_, _, offer := offerFixture(t)
	preimage := [32]byte{41}
	offer.Hash = sha256.Sum256(preimage[:])
	ln := &testLightning{invoice: &lnrpc.Invoice{
		RHash: offer.Hash[:], RPreimage: preimage[:],
		State: lnrpc.Invoice_SETTLED,
	}}
	// No peer or chain adapter exists: a settled local invoice must recover
	// the secret independently of either external disclosure channel.
	adapter := Adapter{Receiver: &assetln.Receiver{Lightning: ln}}
	secret, found, err := adapter.LookupPreimage(t.Context(), offer)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, preimage, secret)

	for _, name := range []string{"wrong hash", "short", "wrong secret"} {
		t.Run(name, func(t *testing.T) {
			ln.invoice = &lnrpc.Invoice{
				RHash: offer.Hash[:], RPreimage: preimage[:],
				State: lnrpc.Invoice_SETTLED,
			}
			switch name {
			case "wrong hash":
				ln.invoice.RHash = bytes.Repeat([]byte{3}, 32)
			case "short":
				ln.invoice.RPreimage = []byte{41}
			case "wrong secret":
				ln.invoice.RPreimage = bytes.Repeat(
					[]byte{42},
					32,
				)
			}
			_, found, err := adapter.LookupPreimage(
				t.Context(),
				offer,
			)
			require.ErrorIs(t, err, assetexec.ErrInvalid)
			require.False(t, found)
		})
	}
}

func testSessionRecoveryResumesLostResponses(t *testing.T, lostPhase string) {
	t.Helper()
	_, _, offer := offerFixture(t)
	path := filepath.Join(t.TempDir(), "client.db")
	store, err := assetexec.OpenBoltStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	actions := &sessionActions{}
	prepareCalls, fallbackCalls := 0, 0
	server := &testServer{
		prepare: func(
			req *swapserverrpc.PrepareAssetLoopInRequest,
		) error {
			prepareCalls++
			require.Equal(
				t,
				offer.Hash[:],
				req.PaymentHash,
			)
			require.Equal(
				t,
				"held-invoice",
				req.AssetHoldInvoice,
			)
			require.Equal(
				t,
				[]byte("immutable-request"),
				req.RecoveryRequest,
			)
			if lostPhase == "prepare" &&
				prepareCalls == 1 {
				return errLost
			}
			return nil
		},
		fallback: func() ([]byte, error) {
			fallbackCalls++
			if lostPhase == "fallback" &&
				fallbackCalls == 1 {
				return nil, errLost
			}
			return []byte("server-fallback"), nil
		},
	}
	adapter := &Adapter{
		Actions: actions,
		Store:   store,
		Server:  server,
	}
	controller := assetexec.NewController(
		store,
		actions,
		nil,
	)
	session := Session{
		Controller: controller,
		Adapter:    adapter,
	}
	intent, err := json.Marshal(InvoiceIntent{
		RecoveryRequest: []byte("immutable-request"),
	})
	require.NoError(t, err)
	require.NoError(
		t,
		controller.Start(t.Context(), offer, intent),
	)
	require.ErrorIs(
		t,
		session.Recover(t.Context(), offer.Hash),
		errLost,
	)
	require.Zero(t, actions.pushes)
	require.NoError(t, store.Close())
	store, err = assetexec.OpenBoltStore(path)
	require.NoError(t, err)
	adapter.Store = store
	session.Controller = assetexec.NewController(
		store,
		actions,
		nil,
	)
	require.NoError(
		t,
		session.Recover(t.Context(), offer.Hash),
	)
	require.Equal(t, 1, actions.creates)
	require.Equal(t, 1, actions.pushes)
	record, err := store.Get(t.Context(), offer.Hash)
	require.NoError(t, err)
	require.True(t, record.SignaturesExposed)
	require.Equal(
		t,
		[]byte("signed-sdk-bundle"),
		record.Fallback.Bundle,
	)
}
