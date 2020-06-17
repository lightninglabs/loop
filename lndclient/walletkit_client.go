package lndclient

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"google.golang.org/grpc"
)

// WalletKitClient exposes wallet functionality.
type WalletKitClient interface {
	// ListUnspent returns a list of all utxos spendable by the wallet with
	// a number of confirmations between the specified minimum and maximum.
	ListUnspent(ctx context.Context, minConfs, maxConfs int32) (
		[]*lnwallet.Utxo, error)

	// LeaseOutput locks an output to the given ID, preventing it from being
	// available for any future coin selection attempts. The absolute time
	// of the lock's expiration is returned. The expiration of the lock can
	// be extended by successive invocations of this call. Outputs can be
	// unlocked before their expiration through `ReleaseOutput`.
	LeaseOutput(ctx context.Context, lockID wtxmgr.LockID,
		op wire.OutPoint) (time.Time, error)

	// ReleaseOutput unlocks an output, allowing it to be available for coin
	// selection if it remains unspent. The ID should match the one used to
	// originally lock the output.
	ReleaseOutput(ctx context.Context, lockID wtxmgr.LockID,
		op wire.OutPoint) error

	DeriveNextKey(ctx context.Context, family int32) (
		*keychain.KeyDescriptor, error)

	DeriveKey(ctx context.Context, locator *keychain.KeyLocator) (
		*keychain.KeyDescriptor, error)

	NextAddr(ctx context.Context) (btcutil.Address, error)

	PublishTransaction(ctx context.Context, tx *wire.MsgTx) error

	SendOutputs(ctx context.Context, outputs []*wire.TxOut,
		feeRate chainfee.SatPerKWeight) (*wire.MsgTx, error)

	EstimateFee(ctx context.Context, confTarget int32) (chainfee.SatPerKWeight,
		error)

	// ListSweeps returns a list of sweep transaction ids known to our node.
	// Note that this function only looks up transaction ids, and does not
	// query our wallet for the full set of transactions.
	ListSweeps(ctx context.Context) ([]string, error)
}

type walletKitClient struct {
	client       walletrpc.WalletKitClient
	walletKitMac serializedMacaroon
}

// A compile-time constraint to ensure walletKitclient satisfies the
// WalletKitClient interface.
var _ WalletKitClient = (*walletKitClient)(nil)

func newWalletKitClient(conn *grpc.ClientConn,
	walletKitMac serializedMacaroon) *walletKitClient {

	return &walletKitClient{
		client:       walletrpc.NewWalletKitClient(conn),
		walletKitMac: walletKitMac,
	}
}

// ListUnspent returns a list of all utxos spendable by the wallet with a number
// of confirmations between the specified minimum and maximum.
func (m *walletKitClient) ListUnspent(ctx context.Context, minConfs,
	maxConfs int32) ([]*lnwallet.Utxo, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	rpcCtx = m.walletKitMac.WithMacaroonAuth(rpcCtx)
	resp, err := m.client.ListUnspent(rpcCtx, &walletrpc.ListUnspentRequest{
		MinConfs: minConfs,
		MaxConfs: maxConfs,
	})
	if err != nil {
		return nil, err
	}

	utxos := make([]*lnwallet.Utxo, 0, len(resp.Utxos))
	for _, utxo := range resp.Utxos {
		var addrType lnwallet.AddressType
		switch utxo.AddressType {
		case lnrpc.AddressType_WITNESS_PUBKEY_HASH:
			addrType = lnwallet.WitnessPubKey
		case lnrpc.AddressType_NESTED_PUBKEY_HASH:
			addrType = lnwallet.NestedWitnessPubKey
		default:
			return nil, fmt.Errorf("invalid utxo address type %v",
				utxo.AddressType)
		}

		pkScript, err := hex.DecodeString(utxo.PkScript)
		if err != nil {
			return nil, err
		}

		opHash, err := chainhash.NewHash(utxo.Outpoint.TxidBytes)
		if err != nil {
			return nil, err
		}

		utxos = append(utxos, &lnwallet.Utxo{
			AddressType:   addrType,
			Value:         btcutil.Amount(utxo.AmountSat),
			Confirmations: utxo.Confirmations,
			PkScript:      pkScript,
			OutPoint: wire.OutPoint{
				Hash:  *opHash,
				Index: utxo.Outpoint.OutputIndex,
			},
		})
	}

	return utxos, nil
}

// LeaseOutput locks an output to the given ID, preventing it from being
// available for any future coin selection attempts. The absolute time of the
// lock's expiration is returned. The expiration of the lock can be extended by
// successive invocations of this call. Outputs can be unlocked before their
// expiration through `ReleaseOutput`.
func (m *walletKitClient) LeaseOutput(ctx context.Context, lockID wtxmgr.LockID,
	op wire.OutPoint) (time.Time, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	rpcCtx = m.walletKitMac.WithMacaroonAuth(rpcCtx)
	resp, err := m.client.LeaseOutput(rpcCtx, &walletrpc.LeaseOutputRequest{
		Id: lockID[:],
		Outpoint: &lnrpc.OutPoint{
			TxidBytes:   op.Hash[:],
			OutputIndex: op.Index,
		},
	})
	if err != nil {
		return time.Time{}, err
	}

	return time.Unix(int64(resp.Expiration), 0), nil
}

// ReleaseOutput unlocks an output, allowing it to be available for coin
// selection if it remains unspent. The ID should match the one used to
// originally lock the output.
func (m *walletKitClient) ReleaseOutput(ctx context.Context,
	lockID wtxmgr.LockID, op wire.OutPoint) error {

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	rpcCtx = m.walletKitMac.WithMacaroonAuth(rpcCtx)
	_, err := m.client.ReleaseOutput(rpcCtx, &walletrpc.ReleaseOutputRequest{
		Id: lockID[:],
		Outpoint: &lnrpc.OutPoint{
			TxidBytes:   op.Hash[:],
			OutputIndex: op.Index,
		},
	})
	return err
}

func (m *walletKitClient) DeriveNextKey(ctx context.Context, family int32) (
	*keychain.KeyDescriptor, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	rpcCtx = m.walletKitMac.WithMacaroonAuth(rpcCtx)
	resp, err := m.client.DeriveNextKey(rpcCtx, &walletrpc.KeyReq{
		KeyFamily: family,
	})
	if err != nil {
		return nil, err
	}

	key, err := btcec.ParsePubKey(resp.RawKeyBytes, btcec.S256())
	if err != nil {
		return nil, err
	}

	return &keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(resp.KeyLoc.KeyFamily),
			Index:  uint32(resp.KeyLoc.KeyIndex),
		},
		PubKey: key,
	}, nil
}

func (m *walletKitClient) DeriveKey(ctx context.Context, in *keychain.KeyLocator) (
	*keychain.KeyDescriptor, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	rpcCtx = m.walletKitMac.WithMacaroonAuth(rpcCtx)
	resp, err := m.client.DeriveKey(rpcCtx, &signrpc.KeyLocator{
		KeyFamily: int32(in.Family),
		KeyIndex:  int32(in.Index),
	})
	if err != nil {
		return nil, err
	}

	key, err := btcec.ParsePubKey(resp.RawKeyBytes, btcec.S256())
	if err != nil {
		return nil, err
	}

	return &keychain.KeyDescriptor{
		KeyLocator: *in,
		PubKey:     key,
	}, nil
}

func (m *walletKitClient) NextAddr(ctx context.Context) (
	btcutil.Address, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	rpcCtx = m.walletKitMac.WithMacaroonAuth(rpcCtx)
	resp, err := m.client.NextAddr(rpcCtx, &walletrpc.AddrRequest{})
	if err != nil {
		return nil, err
	}

	addr, err := btcutil.DecodeAddress(resp.Addr, nil)
	if err != nil {
		return nil, err
	}

	return addr, nil
}

func (m *walletKitClient) PublishTransaction(ctx context.Context,
	tx *wire.MsgTx) error {

	txHex, err := swap.EncodeTx(tx)
	if err != nil {
		return err
	}

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	rpcCtx = m.walletKitMac.WithMacaroonAuth(rpcCtx)
	_, err = m.client.PublishTransaction(rpcCtx, &walletrpc.Transaction{
		TxHex: txHex,
	})

	return err
}

func (m *walletKitClient) SendOutputs(ctx context.Context,
	outputs []*wire.TxOut, feeRate chainfee.SatPerKWeight) (
	*wire.MsgTx, error) {

	rpcOutputs := make([]*signrpc.TxOut, len(outputs))
	for i, output := range outputs {
		rpcOutputs[i] = &signrpc.TxOut{
			PkScript: output.PkScript,
			Value:    output.Value,
		}
	}

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	rpcCtx = m.walletKitMac.WithMacaroonAuth(rpcCtx)
	resp, err := m.client.SendOutputs(rpcCtx, &walletrpc.SendOutputsRequest{
		Outputs:  rpcOutputs,
		SatPerKw: int64(feeRate),
	})
	if err != nil {
		return nil, err
	}

	tx, err := swap.DecodeTx(resp.RawTx)
	if err != nil {
		return nil, err
	}

	return tx, nil
}

func (m *walletKitClient) EstimateFee(ctx context.Context, confTarget int32) (
	chainfee.SatPerKWeight, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	rpcCtx = m.walletKitMac.WithMacaroonAuth(rpcCtx)
	resp, err := m.client.EstimateFee(rpcCtx, &walletrpc.EstimateFeeRequest{
		ConfTarget: int32(confTarget),
	})
	if err != nil {
		return 0, err
	}

	return chainfee.SatPerKWeight(resp.SatPerKw), nil
}

// ListSweeps returns a list of sweep transaction ids known to our node.
// Note that this function only looks up transaction ids (Verbose=false), and
// does not query our wallet for the full set of transactions.
func (m *walletKitClient) ListSweeps(ctx context.Context) ([]string, error) {
	rpcCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()

	resp, err := m.client.ListSweeps(
		m.walletKitMac.WithMacaroonAuth(rpcCtx),
		&walletrpc.ListSweepsRequest{
			Verbose: false,
		},
	)
	if err != nil {
		return nil, err
	}

	// Since we have requested the abbreviated response from lnd, we can
	// just get our response to a list of sweeps and return it.
	sweeps := resp.GetTransactionIds()
	return sweeps.TransactionIds, nil
}
