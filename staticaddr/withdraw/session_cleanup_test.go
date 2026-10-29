package withdraw

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// TestWithdrawalSessionCleanup verifies that completed sessions are never
// cleaned twice, while a later failure releases both its session and sessions
// whose signing has not started. Failure follows the first combine call rather
// than a particular input, so coverage does not depend on map iteration order.
func TestWithdrawalSessionCleanup(t *testing.T) {
	t.Parallel()

	clientKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	serverKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	withdrawalAddress, err := btcutil.NewAddressWitnessPubKeyHash(
		make([]byte, 20), &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)
	combineErr := errors.New("combine failed")

	for _, testCase := range []struct {
		name       string
		failAt     int
		combineErr error
		wantErr    string
	}{
		{name: "complete success"},
		{
			name: "partial success then combine error", failAt: 2,
			combineErr: combineErr, wantErr: "error combining signature",
		},
		{
			name: "partial success then incomplete combine", failAt: 2,
			wantErr: "expected all signatures to be combined",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			deposits := make([]*deposit.Deposit, 3)
			for i := range deposits {
				deposits[i] = &deposit.Deposit{
					Index: uint32(i),
					Value: 100_000,
					AddressParams: &script.Parameters{
						ClientPubkey: clientKey.PubKey(),
						ServerPubkey: serverKey.PubKey(),
						Expiry:       144,
					},
				}
				staticAddress, err := deposits[i].GetStaticAddressScript()
				require.NoError(t, err)
				pkScript, err := staticAddress.StaticAddressScript()
				require.NoError(t, err)
				deposits[i].AddressParams.PkScript = pkScript
			}
			signer := &withdrawalSessionSigner{
				failAt: testCase.failAt, combineErr: testCase.combineErr,
			}
			manager := &Manager{cfg: &ManagerConfig{
				Signer: signer, StaticAddressServerClient: &withdrawalSessionServer{},
			}}
			tx, _, err := manager.CreateFinalizedWithdrawalTx(
				t.Context(), deposits, withdrawalAddress, 0, 0,
				lnrpc.CommitmentType_UNKNOWN_COMMITMENT_TYPE,
			)
			if testCase.wantErr == "" {
				require.NoError(t, err)
				require.Len(t, signer.combined, len(deposits))
				require.Empty(t, signer.cleaned)
				for _, txIn := range tx.TxIn {
					require.Len(t, txIn.Witness, 1)
				}
			} else {
				require.ErrorContains(t, err, testCase.wantErr)
				if testCase.combineErr != nil {
					require.ErrorIs(t, err, testCase.combineErr)
				}
				require.Nil(t, tx)
				require.Len(t, signer.combined, 1)
				require.Len(t, signer.cleaned, len(deposits)-1)
			}

			for _, combined := range signer.combined {
				require.NotContains(t, signer.cleaned, combined)
			}
			accountedFor := slices.Concat(signer.combined, signer.cleaned)
			require.ElementsMatch(t, signer.created, accountedFor)
			require.Len(t, signer.signCleanup, signer.combineCalls)
			for _, cleanup := range signer.signCleanup {
				require.False(t, cleanup)
			}
		})
	}
}

// withdrawalSessionSigner models lnd's automatic deletion after a complete
// combine and records explicit cleanup separately from those completed sessions.
type withdrawalSessionSigner struct {
	lndclient.SignerClient

	created      [][32]byte
	combined     [][32]byte
	cleaned      [][32]byte
	signCleanup  []bool
	combineCalls int
	failAt       int
	combineErr   error
}

func (s *withdrawalSessionSigner) MuSig2Cleanup(_ context.Context,
	sessionID [32]byte) error {

	s.cleaned = append(s.cleaned, sessionID)
	return nil
}

func (s *withdrawalSessionSigner) MuSig2CreateSession(context.Context,
	input.MuSig2Version, *keychain.KeyLocator, [][]byte,
	...lndclient.MuSig2SessionOpts) (*input.MuSig2SessionInfo, error) {

	sessionID := [32]byte{byte(len(s.created) + 1)}
	s.created = append(s.created, sessionID)
	return &input.MuSig2SessionInfo{SessionID: sessionID}, nil
}

func (s *withdrawalSessionSigner) MuSig2RegisterNonces(context.Context,
	[32]byte, [][musig2.PubNonceSize]byte) (bool, error) {

	return true, nil
}

func (s *withdrawalSessionSigner) MuSig2Sign(_ context.Context,
	_ [32]byte, _ [32]byte, cleanup bool) ([]byte, error) {

	s.signCleanup = append(s.signCleanup, cleanup)
	return make([]byte, input.MuSig2PartialSigSize), nil
}

func (s *withdrawalSessionSigner) MuSig2CombineSig(_ context.Context,
	sessionID [32]byte, _ [][]byte) (bool, []byte, error) {

	s.combineCalls++
	if s.combineCalls == s.failAt {
		return false, nil, s.combineErr
	}
	s.combined = append(s.combined, sessionID)
	return true, bytes.Repeat([]byte{sessionID[0]}, 64), nil
}

type withdrawalSessionServer struct {
	swapserverrpc.StaticAddressServerClient
}

func (*withdrawalSessionServer) ServerPsbtWithdrawDeposits(_ context.Context,
	req *swapserverrpc.ServerPsbtWithdrawRequest, _ ...grpc.CallOption) (
	*swapserverrpc.ServerPsbtWithdrawResponse, error) {

	packet, err := psbt.NewFromRawBytes(bytes.NewReader(req.WithdrawalPsbt), false)
	if err != nil {
		return nil, err
	}
	signingInfo := make(map[string]*swapserverrpc.ServerPsbtWithdrawSigningInfo)
	for _, txIn := range packet.UnsignedTx.TxIn {
		signingInfo[txIn.PreviousOutPoint.String()] =
			&swapserverrpc.ServerPsbtWithdrawSigningInfo{
				Nonce: make([]byte, musig2.PubNonceSize),
				Sig:   make([]byte, input.MuSig2PartialSigSize),
			}
	}
	txID := packet.UnsignedTx.TxHash()
	return &swapserverrpc.ServerPsbtWithdrawResponse{
		Txid: txID[:], SigningInfo: signingInfo,
	}, nil
}
