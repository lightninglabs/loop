package tapsdk

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/swapkit"
)

// BitcoinUTXO supplies only the mempool-aware gettxout check that LND's wallet
// API cannot provide for externally controlled asset anchors. The normal LND
// services continue to own block retrieval and spend notifications.
type BitcoinUTXO struct {
	URL, User string
	// #nosec G117 -- Configured Bitcoin RPC credential, never serialized.
	Password string
	Client   *http.Client
}

// Unspent checks both the current UTXO set and mempool. Backend failures never
// authorize signing, and request cancellation interrupts the HTTP operation.
func (b *BitcoinUTXO) Unspent(ctx context.Context,
	point wire.OutPoint) (bool, error) {

	const id = "loop-asset-utxo"
	body, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      string `json:"id"`
		Method  string `json:"method"`
		Params  []any  `json:"params"`
	}{"1.0", id, "gettxout", []any{
		point.Hash.String(), point.Index, true,
	}})
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		b.URL, bytes.NewReader(body))
	if err != nil {
		return false, swapkit.ErrInvalid
	}
	req.SetBasicAuth(b.User, b.Password)
	req.Header.Set("Content-Type", "application/json")
	client := b.Client
	if client == nil {
		client = http.DefaultClient
	}
	transport := *client
	transport.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	// #nosec G704 -- Operator-configured Bitcoin RPC; redirects are blocked.
	resp, err := transport.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, swapkit.ErrInvalid
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, swapkit.ErrInvalid
	}
	var reply struct {
		ID     string            `json:"id"`
		Result json.RawMessage   `json:"result"`
		Error  *btcjson.RPCError `json:"error"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&reply)
	if err != nil || reply.ID != id || reply.Error != nil ||
		len(reply.Result) == 0 {

		return false, swapkit.ErrInvalid
	}
	var result *btcjson.GetTxOutResult
	if json.Unmarshal(reply.Result, &result) != nil {
		return false, swapkit.ErrInvalid
	}
	if result == nil {
		return false, nil
	}
	hash, err := chainhash.NewHashFromStr(result.BestBlock)
	if err != nil || hash.String() != result.BestBlock {
		return false, swapkit.ErrInvalid
	}
	script, err := hex.DecodeString(result.ScriptPubKey.Hex)
	if err != nil || len(script) == 0 ||
		hex.EncodeToString(script) != result.ScriptPubKey.Hex ||
		result.Value < 0 || result.Confirmations < 0 {

		return false, swapkit.ErrInvalid
	}
	return true, nil
}
