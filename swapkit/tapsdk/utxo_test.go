package tapsdk

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/swapkit"
	"github.com/stretchr/testify/require"
)

func TestBitcoinUTXOMempoolGate(t *testing.T) {
	point := wire.OutPoint{Hash: chainhash.Hash{1}, Index: 2}
	result := fmt.Sprintf(`{"bestblock":%q,"confirmations":1,`+
		`"value":0.0001,"scriptPubKey":{"hex":"51"}}`,
		chainhash.Hash{2}.String())
	for _, test := range []struct {
		name, body string
		unspent    bool
		failure    bool
	}{
		{"unspent", `{"id":"loop-asset-utxo","result":` +
			result + `}`, true, false},
		{"spent", `{"id":"loop-asset-utxo","result":null}`,
			false, false},
		{"missing result", `{"id":"loop-asset-utxo"}`, false, true},
		{"wrong id", `{"id":"other","result":null}`, false, true},
		{"malformed output", `{"id":"loop-asset-utxo","result":{}}`,
			false, true},
		{"backend error", `{"id":"loop-asset-utxo","result":null,` +
			`"error":{"code":-1,"message":"private cause"}}`, false, true},
		{"oversized", strings.Repeat(" ", 1<<20) + `{}`, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, req *http.Request) {
					var request struct {
						Method string `json:"method"`
						Params []any  `json:"params"`
					}
					require.NoError(t, json.NewDecoder(req.Body).
						Decode(&request))
					require.Equal(t, "gettxout", request.Method)
					require.Equal(t, []any{point.Hash.String(),
						float64(point.Index), true}, request.Params)
					user, password, ok := req.BasicAuth()
					require.True(t, ok)
					require.Equal(t, "user", user)
					require.Equal(t, "secret", password)
					_, _ = w.Write([]byte(test.body))
				}))
			defer server.Close()
			backend := BitcoinUTXO{URL: server.URL, User: "user",
				Password: "secret"}
			unspent, err := backend.Unspent(t.Context(), point)
			require.Equal(t, test.unspent, unspent)
			if test.failure {
				require.ErrorIs(t, err, swapkit.ErrInvalid)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestBitcoinUTXORejectsRedirects(t *testing.T) {
	var reached atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) { reached.Store(true) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
		}))
	defer server.Close()
	backend := BitcoinUTXO{URL: server.URL, Client: server.Client()}
	unspent, err := backend.Unspent(t.Context(), wire.OutPoint{})
	require.False(t, unspent)
	require.ErrorIs(t, err, swapkit.ErrInvalid)
	require.False(t, reached.Load())
}

func TestBitcoinUTXOCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, req *http.Request) {
			close(started)
			<-release
		}))
	defer server.Close()
	defer close(release)
	ctx, cancel := context.WithCancel(t.Context())
	backend := BitcoinUTXO{URL: server.URL}
	result := make(chan error, 1)
	go func() {
		_, err := backend.Unspent(ctx, wire.OutPoint{})
		result <- err
	}()
	<-started
	cancel()
	require.ErrorIs(t, <-result, context.Canceled)
}
