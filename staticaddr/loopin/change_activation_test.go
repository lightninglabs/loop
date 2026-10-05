package loopin

import (
	"context"
	"errors"
	"testing"

	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/loop/test"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestInitHtlcChangeActivation verifies that rejected initialization attempts
// do not activate change watches, and accepted swaps persist the exact prepared
// address before advancing to the signing state.
func TestInitHtlcChangeActivation(t *testing.T) {
	t.Parallel()

	rejectedErr := errors.New("initialization rejected")
	lostReplyErr := status.Error(
		codes.DeadlineExceeded, "initialization reply lost",
	)
	activationErr := errors.New("unable to activate change watch")
	storeErr := errors.New("unable to save initialized swap")
	for _, tc := range []struct {
		name           string
		serverErr      error
		feeTier        string
		invalidExpiry  bool
		missingFunding bool
		activationErr  error
		storeErr       error
		fullAmount     bool
		wantActivate   int
		wantStore      int
		wantAbort      int
		wantErr        error
		wantErrText    string
	}{
		{
			name: "accepted partial swap", wantActivate: 1, wantStore: 1,
		},
		{
			name: "server rejects before activation", serverErr: rejectedErr,
			wantErr: rejectedErr,
		},
		{
			name:      "lost server response leaves prepared address inactive",
			serverErr: lostReplyErr, wantErr: lostReplyErr,
		},
		{
			name: "standard fee guard", feeTier: "standard", wantAbort: 1,
			wantErr: ErrFeeTooHigh,
		},
		{
			name: "high fee guard", feeTier: "high", wantAbort: 1,
			wantErr: ErrFeeTooHigh,
		},
		{
			name: "extreme fee guard", feeTier: "extreme", wantAbort: 1,
			wantErr: ErrFeeTooHigh,
		},
		{
			name: "deposit expiry guard", invalidExpiry: true, wantAbort: 1,
			wantErrText: "before htlc expiry",
		},
		{
			name: "funding snapshot guard", missingFunding: true, wantAbort: 1,
			wantErrText: "unconfirmed funding transactions",
		},
		{
			name:          "activation failure prevents swap persistence",
			activationErr: activationErr, wantActivate: 1, wantAbort: 1,
			wantErr: activationErr,
		},
		{
			name:     "swap storage failure prevents signing",
			storeErr: storeErr, wantActivate: 1, wantStore: 1, wantAbort: 1,
			wantErr: storeErr,
		},
		{
			name: "full swap has no change activation", fullAmount: true,
			wantStore: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			mockLnd := test.NewMockLnd()
			_, inputKey := test.CreateKey(51)
			_, changeKey := test.CreateKey(52)
			_, serverKey := test.CreateKey(53)
			dep := &deposit.Deposit{
				Value: 500_000,
				AddressParams: &address.AddressParameters{
					ClientPubkey: inputKey,
					ServerPubkey: serverKey,
					PkScript:     []byte{0x51, 0x20, 0x01},
					Expiry:       2_000,
				},
			}
			addTestFundingTransactions(mockLnd, dep)
			if tc.missingFunding {
				mockLnd.Transactions = nil
			}
			prepared := &address.AddressParameters{
				ClientPubkey: changeKey,
				ServerPubkey: serverKey,
				PkScript:     []byte{0x51, 0x20, 0x02},
				Expiry:       2_000,
			}
			loopIn := &StaticAddressLoopIn{
				Deposits:              []*deposit.Deposit{dep},
				DepositOutpoints:      []string{dep.String()},
				SelectedAmount:        300_000,
				QuotedSwapFee:         1_000,
				InitiationHeight:      uint32(mockLnd.Height),
				PaymentTimeoutSeconds: DefaultPaymentTimeoutSeconds,
			}
			if tc.fullAmount {
				loopIn.SelectedAmount = 0
			}
			response := testStaticAddressLoopInResponse(
				serverKey.SerializeCompressed(),
			)
			if tc.invalidExpiry {
				response.HtlcExpiry = 4_000
			}
			excessiveFee := &swapserverrpc.ServerHtlcSigningInfo{
				FeeRate: 1_000_000,
			}
			switch tc.feeTier {
			case "standard":
				response.StandardHtlcInfo = excessiveFee
			case "high":
				response.HighFeeHtlcInfo = excessiveFee
			case "extreme":
				response.ExtremeFeeHtlcInfo = excessiveFee
			}

			var order []string
			server := &changeActivationServer{
				response: response, err: tc.serverErr,
				requestHook: func(req *swapserverrpc.ServerStaticAddressLoopInRequest) {
					order = append(order, "request")
					require.Zero(t, prepared.ID)
					if tc.fullAmount {
						require.Nil(t, req.ChangeOutput)
						return
					}
					require.NotNil(t, req.ChangeOutput)
					require.EqualValues(t, 200_000, req.ChangeOutput.Amount)
					require.Equal(t, prepared.PkScript,
						req.ChangeOutput.StaticAddress.PkScript)
					require.Equal(t, prepared.ClientPubkey.SerializeCompressed(),
						req.ChangeOutput.StaticAddress.Pubkey)
				},
			}
			addresses := &changeActivationAddresses{
				params: prepared, activationErr: tc.activationErr,
				prepareHook: func() { order = append(order, "prepare") },
				activateHook: func(params *address.AddressParameters) {
					order = append(order, "activate")
					require.Same(t, prepared, params)
					require.Zero(t, params.ID)
					require.NotNil(t, server.request)
					require.Equal(t, server.request.ChangeOutput.StaticAddress.PkScript,
						params.PkScript)
				},
			}
			storeCalls := 0
			store := &changeActivationStore{
				create: func(swap *StaticAddressLoopIn) error {
					order = append(order, "store")
					storeCalls++
					require.Empty(t, server.pushedSigs)
					if tc.fullAmount {
						require.Nil(t, swap.ChangeAddressParams)
					} else {
						require.Same(t, prepared, swap.ChangeAddressParams)
						require.EqualValues(t, 42, swap.ChangeAddressParams.ID)
						require.Equal(t, 1, addresses.activations)
					}

					return tc.storeErr
				},
			}
			f := &FSM{
				StateMachine: &fsm.StateMachine{},
				cfg: &Config{
					Server:                               server,
					AddressManager:                       addresses,
					DepositManager:                       &noopDepositManager{},
					LndClient:                            mockLnd.Client,
					InvoicesClient:                       mockLnd.LndServices.Invoices,
					WalletKit:                            mockLnd.WalletKit,
					ChainParams:                          mockLnd.ChainParams,
					Store:                                store,
					ValidateLoopInContract:               testValidateLoopInContract,
					MaxStaticAddrHtlcFeePercentage:       0.01,
					MaxStaticAddrHtlcBackupFeePercentage: 0.01,
				},
				loopIn: loopIn,
			}
			event := f.InitHtlcAction(t.Context(), nil)
			if tc.wantErr != nil || tc.wantErrText != "" {
				require.Equal(t, fsm.OnError, event)
				if tc.wantErr != nil {
					require.ErrorIs(t, f.LastActionError, tc.wantErr)
				} else {
					require.ErrorContains(t, f.LastActionError, tc.wantErrText)
				}
				select {
				case canceled := <-mockLnd.FailInvoiceChannel:
					require.Equal(t, loopIn.SwapHash, canceled)
				default:
					t.Fatal("failed initialization left its invoice open")
				}
			} else {
				require.Equal(t, OnHtlcInitiated, event)
				require.Nil(t, f.LastActionError)
				require.Empty(t, mockLnd.FailInvoiceChannel)
			}
			wantPrepares := 1
			wantOrder := []string{"prepare", "request"}
			if tc.fullAmount {
				wantPrepares = 0
				wantOrder = []string{"request"}
			}
			if tc.wantActivate > 0 {
				wantOrder = append(wantOrder, "activate")
			}
			if tc.wantStore > 0 {
				wantOrder = append(wantOrder, "store")
			}
			require.Equal(t, wantOrder, order)
			require.Equal(t, wantPrepares, addresses.prepares)
			require.Equal(t, tc.wantActivate, addresses.activations)
			require.Equal(t, tc.wantStore, storeCalls)
			require.Len(t, server.pushedSigs, tc.wantAbort)
			for _, sigs := range server.pushedSigs {
				require.Nil(t, sigs.StandardHtlcInfo)
				require.Nil(t, sigs.HighFeeHtlcInfo)
				require.Nil(t, sigs.ExtremeFeeHtlcInfo)
			}
		})
	}
}

type changeActivationAddresses struct {
	mockAddressManager

	prepares      int
	activations   int
	activationErr error
	prepareHook   func()
	activateHook  func(*address.AddressParameters)
}

func (m *changeActivationAddresses) PrepareChangeAddress(context.Context,
	[][]byte) (
	*address.AddressParameters, error) {

	m.prepares++
	m.prepareHook()

	return m.params, nil
}

func (m *changeActivationAddresses) ActivateChangeAddress(_ context.Context,
	params *address.AddressParameters) error {

	m.activations++
	m.activateHook(params)
	if m.activationErr != nil {
		return m.activationErr
	}
	params.ID = 42

	return nil
}

type changeActivationStore struct {
	mockStore

	create func(*StaticAddressLoopIn) error
}

func (s *changeActivationStore) CreateLoopIn(_ context.Context,
	swap *StaticAddressLoopIn) error {

	return s.create(swap)
}

type changeActivationServer struct {
	swapserverrpc.StaticAddressServerClient

	response    *swapserverrpc.ServerStaticAddressLoopInResponse
	err         error
	request     *swapserverrpc.ServerStaticAddressLoopInRequest
	requestHook func(*swapserverrpc.ServerStaticAddressLoopInRequest)
	pushedSigs  []*swapserverrpc.PushStaticAddressHtlcSigsRequest
}

func (s *changeActivationServer) ServerStaticAddressLoopIn(_ context.Context,
	req *swapserverrpc.ServerStaticAddressLoopInRequest, _ ...grpc.CallOption) (
	*swapserverrpc.ServerStaticAddressLoopInResponse, error) {

	s.request = req
	s.requestHook(req)

	return s.response, s.err
}

func (s *changeActivationServer) PushStaticAddressHtlcSigs(_ context.Context,
	req *swapserverrpc.PushStaticAddressHtlcSigsRequest, _ ...grpc.CallOption) (
	*swapserverrpc.PushStaticAddressHtlcSigsResponse, error) {

	s.pushedSigs = append(s.pushedSigs, req)

	return &swapserverrpc.PushStaticAddressHtlcSigsResponse{}, nil
}
