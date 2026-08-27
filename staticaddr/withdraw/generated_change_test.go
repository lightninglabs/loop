package withdraw

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type generatedChangeTestAddressManager struct {
	params *address.Parameters
	err    error
	calls  int
}

func (m *generatedChangeTestAddressManager) GetStaticAddressParameters(
	context.Context) (*script.Parameters, error) {

	return nil, nil
}

func (m *generatedChangeTestAddressManager) GetStaticAddress(
	context.Context) (*script.StaticAddress, error) {

	return nil, nil
}

func (m *generatedChangeTestAddressManager) NewChangeAddress(
	context.Context) (*address.Parameters, error) {

	m.calls++

	return m.params, m.err
}

// GetParameters satisfies the address manager interface used by the
// withdrawal replacement monitor later in the multi-address stack.
func (m *generatedChangeTestAddressManager) GetParameters(
	pkScript []byte) *address.Parameters {

	if m.params == nil || !bytes.Equal(m.params.PkScript, pkScript) {
		return nil
	}

	return m.params
}

type generatedChangeTestServer struct {
	swapserverrpc.StaticAddressServerClient

	request *swapserverrpc.ServerPsbtWithdrawRequest
	err     error
	calls   int
}

func (s *generatedChangeTestServer) ServerPsbtWithdrawDeposits(
	_ context.Context, request *swapserverrpc.ServerPsbtWithdrawRequest,
	_ ...grpc.CallOption) (*swapserverrpc.ServerPsbtWithdrawResponse, error) {

	s.calls++
	s.request = request

	return nil, s.err
}

// TestCreateFinalizedWithdrawalTxUsesGeneratedChange verifies that only a
// non-dust partial withdrawal derives a fresh static address and that the
// exact generated script is included in the PSBT sent to the server.
func TestCreateFinalizedWithdrawalTxUsesGeneratedChange(t *testing.T) {
	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	depositPkScript := testTaprootPkScript(1)
	changePkScript := testTaprootPkScript(2)
	changeParams := &address.Parameters{
		ClientPubkey: clientKey.PubKey(),
		ServerPubkey: serverKey.PubKey(),
		PkScript:     changePkScript,
	}
	deposits := []*deposit.Deposit{{
		OutPoint: wire.OutPoint{Index: 1},
		Value:    100_000,
		AddressParams: &address.Parameters{
			ClientPubkey: clientKey.PubKey(),
			ServerPubkey: serverKey.PubKey(),
			Expiry:       144,
			PkScript:     depositPkScript,
		},
	}}

	withdrawalAddress, err := btcutil.NewAddressWitnessPubKeyHash(
		make([]byte, 20), &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	withdrawalPkScript, err := txscript.PayToAddrScript(withdrawalAddress)
	require.NoError(t, err)

	serverErr := errors.New("stop after recording withdrawal request")
	addressErr := errors.New("change address unavailable")
	tests := []struct {
		name             string
		selectedAmount   int64
		addressErr       error
		wantAddressCalls int
		wantServerCalls  int
		wantOutputs      []*wire.TxOut
	}{
		{
			name:             "partial non-dust change",
			selectedAmount:   50_000,
			wantAddressCalls: 1,
			wantServerCalls:  1,
			wantOutputs: []*wire.TxOut{
				{
					Value:    50_000,
					PkScript: withdrawalPkScript,
				},
				{
					Value:    50_000,
					PkScript: changePkScript,
				},
			},
		},
		{
			name:            "full withdrawal",
			wantServerCalls: 1,
			wantOutputs: []*wire.TxOut{{
				Value:    100_000,
				PkScript: withdrawalPkScript,
			}},
		},
		{
			name:             "dust change",
			selectedAmount:   99_900,
			wantServerCalls:  1,
			wantAddressCalls: 0,
			wantOutputs: []*wire.TxOut{{
				Value:    99_900,
				PkScript: withdrawalPkScript,
			}},
		},
		{
			name:             "address generation failure",
			selectedAmount:   50_000,
			addressErr:       addressErr,
			wantAddressCalls: 1,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			addressManager := &generatedChangeTestAddressManager{
				params: changeParams,
				err:    testCase.addressErr,
			}
			server := &generatedChangeTestServer{err: serverErr}
			manager := &Manager{cfg: &ManagerConfig{
				StaticAddressServerClient: server,
				AddressManager:            addressManager,
				Signer:                    &withdrawalCleanupSigner{},
			}}

			_, _, err := manager.CreateFinalizedWithdrawalTx(
				t.Context(), deposits, withdrawalAddress, 0,
				testCase.selectedAmount,
				lnrpc.CommitmentType_UNKNOWN_COMMITMENT_TYPE,
			)
			if testCase.addressErr != nil {
				require.ErrorIs(t, err, testCase.addressErr)
			} else {
				require.ErrorIs(t, err, serverErr)
			}

			require.Equal(
				t, testCase.wantAddressCalls, addressManager.calls,
			)
			require.Equal(t, testCase.wantServerCalls, server.calls)
			if testCase.wantServerCalls == 0 {
				require.Nil(t, server.request)
				return
			}

			require.NotNil(t, server.request)
			packet, err := psbt.NewFromRawBytes(
				bytes.NewReader(server.request.WithdrawalPsbt), false,
			)
			require.NoError(t, err)
			require.Equal(
				t, testCase.wantOutputs, packet.UnsignedTx.TxOut,
			)
		})
	}
}

func testTaprootPkScript(value byte) []byte {
	return append(
		[]byte{txscript.OP_1, 32}, bytes.Repeat([]byte{value}, 32)...,
	)
}
