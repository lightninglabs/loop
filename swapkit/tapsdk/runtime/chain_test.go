package runtime

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/loop/swapkit/bridge"
	"github.com/stretchr/testify/require"
)

type chainRequest struct {
	ID     uint64            `json:"id"`
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
	User   string
	Pass   string
}

func chainServer(t *testing.T, reply func(chainRequest) (int, any)) (
	*RPCChain, <-chan chainRequest) {

	t.Helper()
	requests := make(chan chainRequest, 10)
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request) {

		var request chainRequest
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		request.User, request.Pass, _ = r.BasicAuth()
		requests <- request
		status, body := reply(request)
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(server.Close)
	return &RPCChain{
		URL: server.URL, User: "rpc-user", Password: "rpc-secret",
		Client: server.Client(),
	}, requests
}

func chainReply(id uint64, result any) map[string]any {
	return map[string]any{"id": id, "result": result, "error": nil}
}

// TestRPCChainUnspent verifies that admission sees mempool spends and that a
// missing output cannot be mistaken for an authenticated unspent observation.
func TestRPCChainUnspent(t *testing.T) {
	for _, live := range []bool{false, true} {
		name := map[bool]string{false: "spent", true: "unspent"}[live]
		t.Run(name, func(t *testing.T) {
			chain, requests := chainServer(t,
				func(req chainRequest) (int, any) {
					var result any
					if live {
						result = map[string]any{
							"confirmations": 3,
						}
					}
					return 200, chainReply(req.ID, result)
				})
			point := wire.OutPoint{Index: 2}
			point.Hash[0] = 1
			unspent, err := chain.Unspent(t.Context(), point)
			require.NoError(t, err)
			require.Equal(t, live, unspent)
			req := <-requests
			require.Equal(t, "gettxout", req.Method)
			require.Equal(t, "rpc-user", req.User)
			require.Equal(t, "rpc-secret", req.Pass)
			require.JSONEq(t, `true`, string(req.Params[2]))
		})
	}
}

// TestRPCChainRejectsUnauthenticatedResults rejects a mismatched request ID,
// an RPC failure carrying a result, and an HTTP authentication failure.
func TestRPCChainRejectsUnauthenticatedResults(t *testing.T) {
	for _, mode := range []string{"id", "rpc", "http"} {
		t.Run(mode, func(t *testing.T) {
			chain, _ := chainServer(t,
				func(req chainRequest) (int, any) {
					body := chainReply(req.ID, 100)
					status := 200
					switch mode {
					case "id":
						body["id"] = req.ID + 1
					case "rpc":
						body["error"] = map[string]any{
							"code":    -1,
							"message": "rpc-secret",
						}
					case "http":
						status = http.StatusUnauthorized
					}
					return status, body
				})
			_, err := chain.Height(t.Context())
			require.Error(t, err)
			require.NotContains(t, err.Error(), "rpc-secret")
		})
	}
}

// TestRPCChainPublicationAmbiguity recovers a lost publication response only
// after observing the exact transaction in the node's transaction index.
func TestRPCChainPublicationAmbiguity(t *testing.T) {
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{Sequence: 1})
	tx.AddTxOut(&wire.TxOut{Value: 500, PkScript: []byte{0x51}})
	for _, known := range []bool{false, true} {
		name := map[bool]string{false: "different", true: "same"}[known]
		t.Run(name, func(t *testing.T) {
			observed := tx.Copy()
			if !known {
				observed.TxOut[0].Value--
			}
			var buf bytes.Buffer
			require.NoError(t, observed.Serialize(&buf))
			chain, requests := chainServer(t,
				func(req chainRequest) (int, any) {
					if req.Method == "sendrawtransaction" {
						body := chainReply(req.ID, nil)
						return 503, body
					}
					return 200, chainReply(req.ID,
						hex.EncodeToString(buf.Bytes()))
				})
			err := chain.Publish(t.Context(), tx)
			if known {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, bridge.ErrInvalid)
			}
			require.Equal(t, "sendrawtransaction",
				(<-requests).Method)
			req := <-requests
			require.Equal(t, "getrawtransaction", req.Method)
			var id string
			require.NoError(t, json.Unmarshal(req.Params[0], &id))
			require.Equal(t, tx.TxHash().String(), id)
		})
	}
}

// TestRPCChainBlockBinding prevents a stale or substituted block body from
// supplying confirmation evidence for the current best-chain block hash.
func TestRPCChainBlockBinding(t *testing.T) {
	block := wire.NewMsgBlock(&wire.BlockHeader{Version: 1})
	var buf bytes.Buffer
	require.NoError(t, block.Serialize(&buf))
	chain, _ := chainServer(t, func(req chainRequest) (int, any) {
		if req.Method == "getblockhash" {
			hash := block.BlockHash()
			hash[0] ^= 1
			return 200, chainReply(req.ID, hash.String())
		}
		return 200, chainReply(req.ID, hex.EncodeToString(buf.Bytes()))
	})
	_, err := chain.Block(t.Context(), 1)
	require.ErrorIs(t, err, bridge.ErrInvalid)
}

// TestRPCChainCancellation ensures a stalled Bitcoin node cannot keep an
// execution action or shutdown blocked beyond its caller's deadline.
func TestRPCChainCancellation(t *testing.T) {
	entered := make(chan struct{})
	stop := make(chan struct{})
	defer close(stop)
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request) {

		close(entered)
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	t.Cleanup(server.Close)
	chain := &RPCChain{URL: server.URL, Client: server.Client()}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := chain.Height(ctx)
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Bitcoin RPC did not start")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("Bitcoin RPC did not honor cancellation")
	}
}
