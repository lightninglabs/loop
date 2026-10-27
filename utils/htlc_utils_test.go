package utils

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
)

// TestLoopOutPaymentAddr covers legacy invoices, blinded invoice secrets and
// inconsistent separately supplied secrets.
func TestLoopOutPaymentAddr(t *testing.T) {
	t.Parallel()
	params := &chaincfg.TestNet3Params
	addr := [32]byte{1, 2, 3}
	other := [32]byte{4}
	key, _ := test.CreateKey(1)
	invoice, err := zpay32.NewInvoice(
		params, lntypes.Hash{1}, time.Now(),
		zpay32.Description("ordinary"), zpay32.PaymentAddr(addr),
	)
	require.NoError(t, err)
	ordinary, err := invoice.Encode(zpay32.MessageSigner{
		SignCompact: func(msg []byte) ([]byte, error) {
			return ecdsa.SignCompact(key, chainhash.HashB(msg), true), nil
		},
	})
	require.NoError(t, err)
	blinded, err := test.CreateBlindedInvoice(params, lntypes.Hash{2}, 100)
	require.NoError(t, err)

	cases := []struct {
		name    string
		invoice string
		secret  *[32]byte
		wantErr string
	}{
		{name: "legacy", invoice: ordinary},
		{name: "matching", invoice: ordinary, secret: &addr},
		{name: "mismatch", invoice: ordinary, secret: &other,
			wantErr: "differs from invoice"},
		{name: "blinded", invoice: blinded, secret: &addr},
		{name: "missing", invoice: blinded,
			wantErr: "missing blinded swap payment address"},
		{name: "invalid invoice", invoice: "invalid", wantErr: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ObtainLoopOutPaymentAddr(
				&loopdb.LoopOutContract{
					SwapInvoice: tc.invoice, PaymentAddr: tc.secret,
				}, params,
			)
			if tc.name == "invalid invoice" {
				require.Error(t, err)
				return
			}
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, addr, *got)
		})
	}
}
