package runtime

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/loop/swapkit/bridge"
)

// Chain supplies authenticated Bitcoin observations and idempotent broadcast.
// Implementations must include the mempool in Unspent and honor cancellation.
type Chain interface {
	Height(context.Context) (uint32, error)
	Unspent(context.Context, wire.OutPoint) (bool, error)
	Block(context.Context, uint32) (*wire.MsgBlock, error)
	Publish(context.Context, *wire.MsgTx) error
}

// RPCChain connects to Bitcoin Core or btcd's HTTP JSON-RPC interface.
// Client configures TLS trust; credentials are never included in errors.
type RPCChain struct {
	URL, User, Password string
	Client              *http.Client
	sequence            atomic.Uint64
}

func (c *RPCChain) call(ctx context.Context, method string,
	params []any, output any) error {

	id := c.sequence.Add(1)
	body, err := json.Marshal(struct {
		Version string `json:"jsonrpc"`
		ID      uint64 `json:"id"`
		Method  string `json:"method"`
		Params  []any  `json:"params"`
	}{"1.0", id, method, params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.URL,
		bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid Bitcoin RPC endpoint")
	}
	req.SetBasicAuth(c.User, c.Password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("Bitcoin RPC unavailable")
	}
	defer resp.Body.Close()
	var reply struct {
		ID     uint64          `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&reply)
	if err != nil {
		return errors.New("invalid Bitcoin RPC response")
	}
	if reply.ID != id || reply.Error != nil || resp.StatusCode != 200 {
		return fmt.Errorf("Bitcoin RPC %s failed", method)
	}
	return json.Unmarshal(reply.Result, output)
}

// Height returns the current best-chain height.
func (c *RPCChain) Height(ctx context.Context) (uint32, error) {
	var height uint32
	err := c.call(ctx, "getblockcount", []any{}, &height)
	return height, err
}

// Unspent includes both confirmed and mempool spends.
func (c *RPCChain) Unspent(ctx context.Context,
	point wire.OutPoint) (bool, error) {

	var output json.RawMessage
	err := c.call(ctx, "gettxout", []any{
		point.Hash.String(), point.Index, true,
	}, &output)
	return err == nil && !bytes.Equal(output, []byte("null")), err
}

// Block reads the best-chain block at a height, avoiding stale cached proofs.
func (c *RPCChain) Block(ctx context.Context,
	height uint32) (*wire.MsgBlock, error) {

	var hash, raw string
	err := c.call(ctx, "getblockhash", []any{height}, &hash)
	if err != nil {
		return nil, err
	}
	err = c.call(ctx, "getblock", []any{hash, 0}, &raw)
	if err != nil {
		return nil, err
	}
	data, err := hex.DecodeString(raw)
	if err != nil {
		return nil, err
	}
	block := new(wire.MsgBlock)
	err = block.Deserialize(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if block.BlockHash().String() != hash {
		return nil, bridge.ErrInvalid
	}
	return block, nil
}

// Publish retries an exact transaction and accepts an already-known txid.
func (c *RPCChain) Publish(ctx context.Context, tx *wire.MsgTx) error {
	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		return err
	}
	var txid string
	err := c.call(ctx, "sendrawtransaction",
		[]any{hex.EncodeToString(buf.Bytes())}, &txid)
	if err == nil {
		if txid != tx.TxHash().String() {
			return bridge.ErrInvalid
		}
		return nil
	}
	var existing string
	lookup := c.call(ctx, "getrawtransaction",
		[]any{tx.TxHash().String(), false}, &existing)
	if lookup != nil {
		return err
	}
	raw, decodeErr := hex.DecodeString(existing)
	if decodeErr != nil {
		return bridge.ErrInvalid
	}
	var found wire.MsgTx
	if found.Deserialize(bytes.NewReader(raw)) != nil ||
		found.TxHash() != tx.TxHash() {
		return bridge.ErrInvalid
	}
	return nil
}
