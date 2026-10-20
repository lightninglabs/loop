package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/lightninglabs/loop/swapkit/tapsdk/walletsigner"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type scriptWallet struct {
	signrpc.SignerClient
	walletrpc.WalletKitClient
	signature []byte
	failure   error
	calls     int
}

func (w *scriptWallet) DeriveKey(_ context.Context,
	key *signrpc.KeyLocator, _ ...grpc.CallOption) (
	*signrpc.KeyDescriptor, error) {

	return &signrpc.KeyDescriptor{
		KeyLoc: key, RawKeyBytes: bindingKey(2).PubKey().SerializeCompressed(),
	}, nil
}

func (w *scriptWallet) SignOutputRaw(context.Context, *signrpc.SignReq,
	...grpc.CallOption) (*signrpc.SignResp, error) {

	w.calls++
	if w.failure != nil {
		err := w.failure
		w.failure = nil
		return nil, err
	}
	return &signrpc.SignResp{RawSigs: [][]byte{w.signature}}, nil
}

// TestScriptSpendRecovery checks the shared unilateral signing sequence across
// lost wallet replies and failed final-transaction persistence. Neither failure
// can produce success before the exact final transaction survives a reopen.
func TestScriptSpendRecovery(t *testing.T) {
	for _, failure := range []string{"wallet reply", "journal", "none"} {
		t.Run(failure, func(t *testing.T) {
			journal := filepath.Join(t.TempDir(), "runtime.db")
			r := openScanRuntime(t, journal, nil)
			t.Cleanup(func() { require.NoError(t, r.Close()) })
			reopen := func() {
				signer := r.cfg.Signer
				require.NoError(t, r.Close())
				r = openScanRuntime(t, journal, nil)
				r.cfg.Signer = signer
			}
			tx := confirmationPackage(t, asset.V1)
			tx.Final = nil
			require.NoError(t, r.put("script-spend", tx))
			requests, err := tx.Package.SigningRequests()
			require.NoError(t, err)
			require.Len(t, requests.ScriptPath, 1)
			request := requests.ScriptPath[0]
			signature, err := schnorr.Sign(
				bindingKey(2), request.Sighash[:],
			)
			require.NoError(t, err)
			lost := errors.New("simulated lost operation")
			wallet := &scriptWallet{signature: signature.Serialize()}
			if failure == "wallet reply" {
				wallet.failure = lost
			}
			signerPath := filepath.Join(t.TempDir(), "signer.db")
			openSigner := func() {
				r.cfg.Signer, err = walletsigner.Open(
					signerPath, wallet, wallet,
				)
				require.NoError(t, err)
			}
			openSigner()
			t.Cleanup(func() {
				require.NoError(t, r.cfg.Signer.Close())
			})
			save := func() error {
				return r.put("script-spend", tx)
			}
			saveFirst := save
			if failure == "journal" {
				saveFirst = func() error { return lost }
			}
			err = r.signScriptSpend(t.Context(), nil, &tx,
				keychain.KeyLocator{}, nil, saveFirst)
			if failure != "none" {
				require.ErrorIs(t, err, lost)
				tx = transaction{}
				reopen()
				require.NoError(t, r.get("script-spend", &tx))
				require.Empty(t, tx.Final)
				require.NoError(t, r.cfg.Signer.Close())
				openSigner()
				err = r.signScriptSpend(t.Context(), nil, &tx,
					keychain.KeyLocator{}, nil, save)
			}
			require.NoError(t, err)
			final := tx.Final
			require.NotEmpty(t, final)
			tx = transaction{}
			reopen()
			require.NoError(t, r.get("script-spend", &tx))
			require.Equal(t, final, tx.Final)
			calls := wallet.calls
			err = r.signScriptSpend(t.Context(), nil, &tx,
				keychain.KeyLocator{}, nil, nil)
			require.NoError(t, err)
			require.Equal(t, calls, wallet.calls)
		})
	}
}
