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
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type generatedChangeTestAddressManager struct {
	params *address.AddressParameters
	known  map[string]*address.AddressParameters
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
	context.Context) (*address.AddressParameters, error) {

	m.calls++

	return m.params, m.err
}

// GetParameters satisfies the address manager interface used by the
// withdrawal replacement monitor later in the multi-address stack.
func (m *generatedChangeTestAddressManager) GetParameters(
	pkScript []byte) *address.AddressParameters {

	if params := m.known[string(pkScript)]; params != nil {
		return params
	}
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
	changeParams := &address.AddressParameters{
		ClientPubkey: clientKey.PubKey(),
		ServerPubkey: serverKey.PubKey(),
		PkScript:     changePkScript,
	}
	deposits := []*deposit.Deposit{{
		Index: 1,
		Value: 100_000,
		AddressParams: &address.AddressParameters{
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

// TestWithdrawalFeeBumpReusesChange checks that fee bumps retain their change
// address across manager restarts, including legacy root-address change. Each
// attempt must still create a new signing session, and older transactions remain
// unchanged so their confirmations can be accounted for normally.
func TestWithdrawalFeeBumpReusesChange(t *testing.T) {
	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	for _, testCase := range []struct {
		name            string
		family          keychain.KeyFamily
		rootDestination bool
		previousChange  bool
		unknownChange   bool
	}{
		{
			name: "derived change", previousChange: true,
			family: keychain.KeyFamily(swap.StaticAddressChangeKeyFamily),
		},
		{
			name: "legacy root change", previousChange: true,
			family: keychain.KeyFamily(swap.StaticSingleAddressKeyFamily),
		},
		{
			name: "legacy root destination and change", previousChange: true,
			family:          keychain.KeyFamily(swap.StaticSingleAddressKeyFamily),
			rootDestination: true,
		},
		{
			name:   "previous withdrawal without change",
			family: keychain.KeyFamily(swap.StaticAddressChangeKeyFamily),
		},
		{
			name: "unknown previous change is not reused", previousChange: true,
			unknownChange: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			oldChangeScript := testTaprootPkScript(3)
			newChangeScript := testTaprootPkScript(4)
			destinationScript := testTaprootPkScript(5)
			if testCase.rootDestination || !testCase.previousChange {
				destinationScript = oldChangeScript
			}
			withdrawalAddress, err := btcutil.NewAddressTaproot(
				destinationScript[2:], &chaincfg.RegressionNetParams,
			)
			require.NoError(t, err)
			previousTx := wire.NewMsgTx(2)
			previousTx.AddTxIn(&wire.TxIn{
				PreviousOutPoint: wire.OutPoint{Index: 1},
			})
			previousTx.AddTxOut(wire.NewTxOut(50_000, destinationScript))
			if testCase.previousChange {
				previousTx.AddTxOut(wire.NewTxOut(49_000, oldChangeScript))
			}
			oldTxHash := previousTx.TxHash()
			oldTxCopy := previousTx.Copy()
			d := &deposit.Deposit{
				OutPoint:              previousTx.TxIn[0].PreviousOutPoint,
				Value:                 100_000,
				FinalizedWithdrawalTx: previousTx,
				AddressParams: &address.AddressParameters{
					ClientPubkey: clientKey.PubKey(),
					ServerPubkey: serverKey.PubKey(),
					Expiry:       144,
					PkScript:     testTaprootPkScript(1),
				},
			}
			d.SetState(deposit.Withdrawing)
			addressManager := &generatedChangeTestAddressManager{
				params: &address.AddressParameters{PkScript: newChangeScript},
				known: map[string]*address.AddressParameters{
					string(oldChangeScript): {
						PkScript: oldChangeScript,
						KeyLocator: keychain.KeyLocator{
							Family: testCase.family,
						},
					},
				},
			}
			addressManager.params.KeyLocator.Family =
				keychain.KeyFamily(swap.StaticAddressChangeKeyFamily)
			if testCase.unknownChange {
				delete(addressManager.known, string(oldChangeScript))
			}
			signer := &withdrawalSessionSigner{}
			cfg := &ManagerConfig{
				Signer: signer, AddressManager: addressManager,
				StaticAddressServerClient: &withdrawalSessionServer{},
			}
			expectedScript := oldChangeScript
			if !testCase.previousChange || testCase.unknownChange {
				expectedScript = newChangeScript
			}

			var firstTx *wire.MsgTx
			for i := range 2 {
				// Reconstruct the manager for each bump. The persisted
				// transaction provides the change association.
				manager := &Manager{cfg: cfg}
				tx, _, err := manager.CreateFinalizedWithdrawalTx(
					t.Context(), []*deposit.Deposit{d},
					withdrawalAddress, chainfee.SatPerKWeight(1_000*(i+1)),
					50_000, lnrpc.CommitmentType_UNKNOWN_COMMITMENT_TYPE,
				)
				require.NoError(t, err)
				require.Len(t, tx.TxOut, 2)
				require.Equal(t, expectedScript, tx.TxOut[1].PkScript)
				require.Equal(t, destinationScript, tx.TxOut[0].PkScript)
				require.EqualValues(t, 50_000, tx.TxOut[0].Value)
				if i == 0 {
					firstTx = tx
				} else {
					require.Less(t, tx.TxOut[1].Value, firstTx.TxOut[1].Value)
					require.NotEqual(t, firstTx.TxHash(), tx.TxHash())
				}
				d.FinalizedWithdrawalTx = tx
			}
			wantAddressCalls := 0
			if !testCase.previousChange || testCase.unknownChange {
				wantAddressCalls = 1
			}
			require.Equal(t, wantAddressCalls, addressManager.calls)
			require.Equal(t, oldTxHash, previousTx.TxHash())
			require.Equal(t, oldTxCopy, previousTx)
			require.Len(t, signer.created, 2)
			require.NotEqual(t, signer.created[0], signer.created[1])
			require.Equal(t, signer.created, signer.combined)
			require.Empty(t, signer.cleaned)
		})
	}
}

// TestWithdrawalChangeAddressRejectsEmptyPreviousTx rejects malformed pending
// metadata before deriving a replacement change address.
func TestWithdrawalChangeAddressRejectsEmptyPreviousTx(t *testing.T) {
	t.Parallel()

	d := &deposit.Deposit{FinalizedWithdrawalTx: wire.NewMsgTx(2)}
	d.SetState(deposit.Withdrawing)
	addressManager := &generatedChangeTestAddressManager{}
	manager := &Manager{cfg: &ManagerConfig{AddressManager: addressManager}}
	_, err := manager.withdrawalChangeAddress(
		t.Context(), []*deposit.Deposit{d},
	)
	require.ErrorContains(t, err, "previous withdrawal tx has no outputs")
	require.Zero(t, addressManager.calls)
}

func testTaprootPkScript(value byte) []byte {
	return append(
		[]byte{txscript.OP_1, 32}, bytes.Repeat([]byte{value}, 32)...,
	)
}
