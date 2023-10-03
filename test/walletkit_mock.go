package test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wtxmgr"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// DefaultMockFee is the default value we use for fee estimates when no values
// are set for specific conf targets.
var DefaultMockFee = chainfee.SatPerKWeight(10000)

type mockWalletKit struct {
	lnd      *LndMockServices
	keyIndex int32

	feeEstimateLock sync.Mutex
	feeEstimates    map[int32]chainfee.SatPerKWeight
}

var _ lndclient.WalletKitClient = (*mockWalletKit)(nil)

func (m *mockWalletKit) ListUnspent(ctx context.Context, minConfs,
	maxConfs int32, opts ...lndclient.ListUnspentOption) (
	[]*lnwallet.Utxo, error) {

	return nil, nil
}

func (m *mockWalletKit) ListLeases(
	context.Context) ([]lndclient.LeaseDescriptor, error) {

	return nil, nil
}

func (m *mockWalletKit) LeaseOutput(ctx context.Context, lockID wtxmgr.LockID,
	op wire.OutPoint, duration time.Duration) (time.Time, error) {

	return time.Now(), nil
}

func (m *mockWalletKit) ReleaseOutput(ctx context.Context,
	lockID wtxmgr.LockID, op wire.OutPoint) error {

	return nil
}

func (m *mockWalletKit) DeriveNextKey(ctx context.Context, family int32) (
	*keychain.KeyDescriptor, error) {

	index := m.keyIndex

	_, pubKey := CreateKey(index)
	m.keyIndex++

	return &keychain.KeyDescriptor{
		KeyLocator: keychain.KeyLocator{
			Family: keychain.KeyFamily(family),
			Index:  uint32(index),
		},
		PubKey: pubKey,
	}, nil
}

func (m *mockWalletKit) DeriveKey(ctx context.Context, in *keychain.KeyLocator) (
	*keychain.KeyDescriptor, error) {

	_, pubKey := CreateKey(int32(in.Index))

	return &keychain.KeyDescriptor{
		KeyLocator: *in,
		PubKey:     pubKey,
	}, nil
}

func (m *mockWalletKit) NextAddr(context.Context, string, walletrpc.AddressType,
	bool) (btcutil.Address, error) {

	addr, err := btcutil.NewAddressWitnessPubKeyHash(
		make([]byte, 20), &chaincfg.TestNet3Params,
	)
	if err != nil {
		return nil, err
	}
	return addr, nil
}

func (m *mockWalletKit) PublishTransaction(ctx context.Context, tx *wire.MsgTx,
	_ string) error {

	m.lnd.AddTx(tx)
	m.lnd.TxPublishChannel <- tx
	return nil
}

func (m *mockWalletKit) SendOutputs(ctx context.Context, outputs []*wire.TxOut,
	feeRate chainfee.SatPerKWeight, _ string) (*wire.MsgTx, error) {

	var inputTxHash chainhash.Hash

	tx := wire.MsgTx{}
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  inputTxHash,
			Index: 0,
		},
	})

	for _, out := range outputs {
		tx.AddTxOut(&wire.TxOut{
			PkScript: out.PkScript,
			Value:    out.Value,
		})
	}

	m.lnd.AddTx(&tx)
	m.lnd.SendOutputsChannel <- tx

	return &tx, nil
}

func (m *mockWalletKit) setFeeEstimate(confTarget int32, fee chainfee.SatPerKWeight) {
	m.feeEstimateLock.Lock()
	defer m.feeEstimateLock.Unlock()

	m.feeEstimates[confTarget] = fee
}

func (m *mockWalletKit) EstimateFeeRate(ctx context.Context,
	confTarget int32) (chainfee.SatPerKWeight, error) {

	m.feeEstimateLock.Lock()
	defer m.feeEstimateLock.Unlock()

	if confTarget <= 1 {
		return 0, errors.New("conf target must be greater than 1")
	}

	feeEstimate, ok := m.feeEstimates[confTarget]
	if !ok {
		return DefaultMockFee, nil
	}

	return feeEstimate, nil
}

// ListSweeps returns a list of the sweep transaction ids known to our node.
func (m *mockWalletKit) ListSweeps(_ context.Context) ([]string, error) {
	return m.lnd.Sweeps, nil
}

// ListSweepsVerbose returns a list of sweep transactions known to our node
// with verbose information about each sweep.
func (m *mockWalletKit) ListSweepsVerbose(ctx context.Context) (
	[]lnwallet.TransactionDetail, error) {

	return m.lnd.SweepsVerbose, nil
}

// BumpFee attempts to bump the fee of a transaction by spending one of
// its outputs at the given fee rate. This essentially results in a
// child-pays-for-parent (CPFP) scenario. If the given output has been
// used in a previous BumpFee call, then a transaction replacing the
// previous is broadcast, resulting in a replace-by-fee (RBF) scenario.
func (m *mockWalletKit) BumpFee(context.Context, wire.OutPoint,
	chainfee.SatPerKWeight) error {

	return nil
}

// ListAccounts retrieves all accounts belonging to the wallet by default.
// Optional name and addressType can be provided to filter through all of the
// wallet accounts and return only those matching.
func (m *mockWalletKit) ListAccounts(context.Context, string,
	walletrpc.AddressType) ([]*walletrpc.Account, error) {

	return nil, nil
}

// FundPsbt creates a fully populated PSBT that contains enough inputs
// to fund the outputs specified in the template. There are two ways of
// specifying a template: Either by passing in a PSBT with at least one
// output declared or by passing in a raw TxTemplate message. If there
// are no inputs specified in the template, coin selection is performed
// automatically. If the template does contain any inputs, it is assumed
// that full coin selection happened externally and no additional inputs
// are added. If the specified inputs aren't enough to fund the outputs
// with the given fee rate, an error is returned.
// After either selecting or verifying the inputs, all input UTXOs are
// locked with an internal app ID.
//
// NOTE: If this method returns without an error, it is the caller's
// responsibility to either spend the locked UTXOs (by finalizing and
// then publishing the transaction) or to unlock/release the locked
// UTXOs in case of an error on the caller's side.
func (m *mockWalletKit) FundPsbt(_ context.Context,
	_ *walletrpc.FundPsbtRequest) (*psbt.Packet, int32,
	[]*walletrpc.UtxoLease, error) {

	return nil, 0, nil, nil
}

// SignPsbt expects a partial transaction with all inputs and outputs
// fully declared and tries to sign all unsigned inputs that have all
// required fields (UTXO information, BIP32 derivation information,
// witness or sig scripts) set.
// If no error is returned, the PSBT is ready to be given to the next
// signer or to be finalized if lnd was the last signer.
//
// NOTE: This RPC only signs inputs (and only those it can sign), it
// does not perform any other tasks (such as coin selection, UTXO
// locking or input/output/fee value validation, PSBT finalization). Any
// input that is incomplete will be skipped.
func (m *mockWalletKit) SignPsbt(_ context.Context,
	_ *psbt.Packet) (*psbt.Packet, error) {

	return nil, nil
}

// FinalizePsbt expects a partial transaction with all inputs and
// outputs fully declared and tries to sign all inputs that belong to
// the wallet. Lnd must be the last signer of the transaction. That
// means, if there are any unsigned non-witness inputs or inputs without
// UTXO information attached or inputs without witness data that do not
// belong to lnd's wallet, this method will fail. If no error is
// returned, the PSBT is ready to be extracted and the final TX within
// to be broadcast.
//
// NOTE: This method does NOT publish the transaction once finalized. It
// is the caller's responsibility to either publish the transaction on
// success or unlock/release any locked UTXOs in case of an error in
// this method.
func (m *mockWalletKit) FinalizePsbt(_ context.Context, _ *psbt.Packet,
	_ string) (*psbt.Packet, *wire.MsgTx, error) {

	return nil, nil, nil
}

// ImportPublicKey imports a public key as watch-only into the wallet.
func (m *mockWalletKit) ImportPublicKey(ctx context.Context,
	pubkey *btcec.PublicKey, addrType lnwallet.AddressType) error {

	return fmt.Errorf("unimplemented")
}

// ImportTaprootScript imports a user-provided taproot script into the
// wallet. The imported script will act as a pay-to-taproot address.
func (m *mockWalletKit) ImportTaprootScript(ctx context.Context,
	tapscript *waddrmgr.Tapscript) (btcutil.Address, error) {

	return nil, fmt.Errorf("unimplemented")
}
