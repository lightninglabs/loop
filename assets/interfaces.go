package assets

import (
	"context"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightninglabs/taproot-assets/taprpc/assetwalletrpc"
	wrpc "github.com/lightninglabs/taproot-assets/taprpc/assetwalletrpc"
	"github.com/lightninglabs/taproot-assets/taprpc/mintrpc"
	"github.com/lightninglabs/taproot-assets/taprpc/tapchannelrpc"
	"github.com/lightninglabs/taproot-assets/taprpc/tapdevrpc"
	"github.com/lightninglabs/taproot-assets/taprpc/universerpc"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

const (
	// DefaultSwapCSVExpiry is the default expiry for a swap in blocks.
	DefaultSwapCSVExpiry = int32(24)

	defaultHtlcFeeConfTarget   = 3
	defaultHtlcConfRequirement = 2

	AssetKeyFamily = 696969
)

// TapdClient is an interface that groups the methods required to interact with
// the taproot-assets server and the wallet.
type AssetClient interface {
	taprpc.TaprootAssetsClient
	wrpc.AssetWalletClient
	mintrpc.MintClient
	universerpc.UniverseClient
	tapdevrpc.TapDevClient

	// FundAndSignVpacket funds ands signs a vpacket.
	FundAndSignVpacket(ctx context.Context,
		vpkt *tappsbt.VPacket) (*tappsbt.VPacket, error)

	// PrepareAndCommitVirtualPsbts prepares and commits virtual psbts.
	PrepareAndCommitVirtualPsbts(ctx context.Context,
		vpkt *tappsbt.VPacket, feeRateSatPerKVByte chainfee.SatPerVByte,
		changeKeyDesc *keychain.KeyDescriptor, params *chaincfg.Params) (
		*psbt.Packet, []*tappsbt.VPacket, []*tappsbt.VPacket,
		*assetwalletrpc.CommitVirtualPsbtsResponse, error)

	// LogAndPublish logs and publishes the virtual psbts.
	LogAndPublish(ctx context.Context, btcPkt *psbt.Packet,
		activeAssets []*tappsbt.VPacket, passiveAssets []*tappsbt.VPacket,
		commitResp *wrpc.CommitVirtualPsbtsResponse) (*taprpc.SendAssetResponse,
		error)

	// GetAssetBalance returns the balance of the given asset.
	GetAssetBalance(ctx context.Context, assetId []byte) (
		uint64, error)

	// DeriveNewKeys derives a new internal and script key.
	DeriveNewKeys(ctx context.Context) (asset.ScriptKey,
		keychain.KeyDescriptor, error)

	// AddHoldInvoice adds a new hold invoice.
	AddHoldInvoice(ctx context.Context, pHash lntypes.Hash,
		assetId []byte, assetAmt uint64, memo string) (
		*tapchannelrpc.AddInvoiceResponse, error)

	// ImportProofFile imports the proof file and returns the last proof.
	ImportProofFile(ctx context.Context, rawProofFile []byte) (
		*proof.Proof, error)

	// SendPayment pays a payment request.
	SendPayment(ctx context.Context,
		invoice string, assetId []byte) (chan *tapchannelrpc.SendPaymentResponse,
		chan error, error)
}

// SwapStore is an interface that groups the methods required to store swap
// information.
type SwapStore interface {
	// CreateAssetSwapOut creates a new swap out in the store.
	CreateAssetSwapOut(ctx context.Context, swap *SwapOut) error

	// UpdateAssetSwapHtlcOutpoint updates the htlc outpoint of a swap out.
	UpdateAssetSwapHtlcOutpoint(ctx context.Context, swapHash lntypes.Hash,
		outpoint *wire.OutPoint, confirmationHeight int32) error

	// UpdateAssetSwapOutProof updates the proof of a swap out.
	UpdateAssetSwapOutProof(ctx context.Context, swapHash lntypes.Hash,
		rawProof []byte) error

	// UpdateAssetSwapOutSweepTx updates the sweep tx of a swap out.
	UpdateAssetSwapOutSweepTx(ctx context.Context,
		swapHash lntypes.Hash, sweepTxid chainhash.Hash,
		confHeight int32, sweepPkscript []byte) error

	// InsertAssetSwapUpdate inserts a new swap update in the store.
	InsertAssetSwapUpdate(ctx context.Context,
		swapHash lntypes.Hash, state fsm.StateType) error

	UpdateAssetSwapOutPreimage(ctx context.Context,
		swapHash lntypes.Hash, preimage lntypes.Preimage) error
}

// BlockHeightSubscriber is responsible for subscribing to the expiry height
// of a swap, as well as getting the current block height.
type BlockHeightSubscriber interface {
	// SubscribeExpiry subscribes to the expiry of a swap. It returns true
	// if the expiry is already past. Otherwise, it returns false and calls
	// the expiryFunc when the expiry height is reached.
	SubscribeExpiry(swapHash [32]byte,
		expiryHeight int32, expiryFunc func()) bool
	// GetBlockHeight returns the current block height.
	GetBlockHeight() int32
}

// InvoiceSubscriber is responsible for subscribing to an invoice.
type InvoiceSubscriber interface {
	// SubscribeInvoice subscribes to an invoice. The update callback is
	// called when the invoice is updated and the error callback is called
	// when an error occurs.
	SubscribeInvoice(ctx context.Context, invoiceHash lntypes.Hash,
		updateCallback func(lndclient.InvoiceUpdate, error)) error
}

// TxConfirmationSubscriber is responsible for subscribing to the confirmation
// of a transaction.
type TxConfirmationSubscriber interface {

	// SubscribeTxConfirmation subscribes to the confirmation of a
	// pkscript on the chain. The callback is called when the pkscript is
	// confirmed or when an error occurs.
	SubscribeTxConfirmation(ctx context.Context, swapHash lntypes.Hash,
		txid *chainhash.Hash, pkscript []byte, numConfs int32,
		eightHint int32, cb func(*chainntnfs.TxConfirmation, error)) error
}

// ExchangeRateProvider is responsible for providing the exchange rate between
// assets.
type ExchangeRateProvider interface {
	// GetSatsPerAssetUnit returns the amount of satoshis per asset unit.
	GetSatsPerAssetUnit(assetId []byte) (btcutil.Amount, error)
}
