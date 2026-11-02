package tapsdk

import (
	"context"
	"fmt"
	"sync"

	"github.com/lightninglabs/loop/tapsdk/internal/vpsbt"
)

const opExecute = "Execute"

// InteractiveTxBuilder builds interactive Taproot Asset transfers where the
// receiver provides their keys directly (rather than a Taproot Asset address).
//
// Interactive transfers require manual proof delivery after completion.
// The returned AssetTransfer contains the proofs that must be sent to
// the receiver.
type InteractiveTxBuilder struct {
	walletKit  WalletKitClient
	listAssets listInteractiveAssets
	networkHRP string
	coinType   uint32

	// Transfer parameters
	assetRef         AssetRef
	amount           uint64
	receiverKeys     *DerivedKeys
	lockTime         uint64
	relativeLockTime uint64
	altLeaves        map[PubKey][][]byte

	// Internal state
	vPacketBytes []byte
	fundedPsbt   []byte
	signedPsbt   []byte
	finished     bool

	mu sync.Mutex
}

type interactiveAssetResolver interface {
	ListAssetRecords(ctx context.Context,
		req *ListAssetsRequest) ([]*AssetRecord, error)
}

type interactiveAssetRowResolver interface {
	listAssetRecords(ctx context.Context,
		req *ListAssetsRequest) ([]*AssetRecord, error)
}

type listInteractiveAssets func(context.Context,
	*ListAssetsRequest) ([]*AssetRecord, error)

// newInteractiveTxBuilder creates a new InteractiveTxBuilder.
func newInteractiveTxBuilder(wallet WalletKitClient,
	networkHRP string, coinType uint32) *InteractiveTxBuilder {

	builder := &InteractiveTxBuilder{
		walletKit:  wallet,
		networkHRP: networkHRP,
		coinType:   coinType,
	}

	if resolver, ok := wallet.(interactiveAssetRowResolver); ok {
		builder.listAssets = resolver.listAssetRecords
	} else if resolver, ok := wallet.(interactiveAssetResolver); ok {
		builder.listAssets = resolver.ListAssetRecords
	}

	return builder
}

// SetAsset specifies which asset and how much to send. Asset-ID refs are used
// directly. Group-key refs are resolved against the wallet's spendable assets
// during Execute, so callers can keep using the SDK's semantic fungible asset
// identifier unless no single issuance/tranche can cover the requested amount.
func (b *InteractiveTxBuilder) SetAsset(ref AssetRef,
	amount uint64) *InteractiveTxBuilder {

	b.mu.Lock()
	defer b.mu.Unlock()

	b.assetRef = ref
	b.amount = amount
	return b
}

// SetReceiverKeys sets the keys derived by the receiver.
// These keys are obtained by the receiver calling Wallet.DeriveKeys().
func (b *InteractiveTxBuilder) SetReceiverKeys(
	keys DerivedKeys) *InteractiveTxBuilder {

	b.mu.Lock()
	defer b.mu.Unlock()

	b.receiverKeys = &keys
	return b
}

// SetLockTime sets the optional lock time for the output.
func (b *InteractiveTxBuilder) SetLockTime(
	lockTime uint64) *InteractiveTxBuilder {

	b.mu.Lock()
	defer b.mu.Unlock()

	b.lockTime = lockTime
	return b
}

// SetRelativeLockTime sets the optional relative lock time for the output.
func (b *InteractiveTxBuilder) SetRelativeLockTime(
	relativeLockTime uint64) *InteractiveTxBuilder {

	b.mu.Lock()
	defer b.mu.Unlock()

	b.relativeLockTime = relativeLockTime
	return b
}

// WithAltLeaves attaches auxiliary Taproot leaves that should be committed to
// the receiver's output.
func (b *InteractiveTxBuilder) WithAltLeaves(
	scriptKey PubKey,
	leaves [][]byte) *InteractiveTxBuilder {

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.altLeaves == nil {
		b.altLeaves = make(map[PubKey][][]byte)
	}

	cloned := make([][]byte, len(leaves))
	for i := range leaves {
		cloned[i] = append([]byte(nil), leaves[i]...)
	}

	b.altLeaves[scriptKey] = cloned
	return b
}

// Execute builds and sends the interactive transfer.
// Returns AssetTransfer with proofs that must be delivered to the receiver.
func (b *InteractiveTxBuilder) Execute(
	ctx context.Context) (*AssetTransfer, error) {

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.finished {
		return nil, ErrBuilderFinished
	}

	// Validate required fields.
	if err := b.validate(); err != nil {
		return nil, err
	}

	assetID, err := b.resolveAssetID(ctx)
	if err != nil {
		return nil, err
	}

	// Build the vPacket.
	if err := b.buildVPacket(assetID); err != nil {
		return nil, wrapErr(opExecute, err)
	}

	// Fund the vPacket.
	if err := b.fund(ctx); err != nil {
		return nil, err
	}

	// Sign the funded PSBT.
	if err := b.sign(ctx); err != nil {
		return nil, err
	}

	// Anchor and complete the transfer.
	result, err := b.complete(ctx)
	if err != nil {
		return nil, err
	}

	b.finished = true
	return result, nil
}

// validate checks that all required fields are set.
func (b *InteractiveTxBuilder) validate() error {
	if b.receiverKeys == nil {
		return &Error{Op: opExecute, Err: ErrNoReceiverKeys}
	}

	if b.assetRef.IsZero() {
		return &Error{Op: opExecute, Err: ErrNoAssetRef}
	}

	if err := b.assetRef.Validate(); err != nil {
		return &Error{Op: opExecute, Err: err}
	}

	if b.amount == 0 {
		return &Error{Op: opExecute, Err: ErrZeroAmount}
	}

	return nil
}

func (b *InteractiveTxBuilder) resolveAssetID(
	ctx context.Context) (AssetID, error) {

	if assetID, ok := b.assetRef.AssetID(); ok {
		var zeroID AssetID
		if assetID == zeroID {
			return AssetID{}, &Error{
				Op:  opExecute,
				Err: ErrNoAssetRef,
			}
		}

		return assetID, nil
	}

	if !b.assetRef.IsGroupRef() {
		return AssetID{}, &Error{
			Op:  opExecute,
			Err: ErrNoAssetRef,
		}
	}

	if b.listAssets == nil {
		return AssetID{}, &Error{
			Op:  opExecute,
			Err: ErrGroupKeyNotSupported,
		}
	}

	assets, err := b.listAssets(ctx, &ListAssetsRequest{
		AssetRef: &b.assetRef,
	})
	if err != nil {
		return AssetID{}, wrapErr("ResolveAssetRef", err)
	}

	var total uint64
	for _, asset := range assets {
		if asset == nil {
			continue
		}

		total = addSaturatingUint64(total, asset.Amount)
		if asset.Amount >= b.amount {
			return asset.Genesis.IssuanceID, nil
		}
	}

	if total == 0 {
		return AssetID{}, wrapErr("ResolveAssetRef", fmt.Errorf(
			"%w: %s", ErrAssetUnknown, b.assetRef,
		))
	}

	return AssetID{}, wrapErr("ResolveAssetRef", fmt.Errorf(
		"%w: requested %d, largest spendable tranche is less than "+
			"that amount", ErrInsufficientBalance, b.amount,
	))
}

// buildVPacket creates the virtual PSBT for the interactive send.
func (b *InteractiveTxBuilder) buildVPacket(assetID AssetID) error {
	var leaves [][]byte
	if b.altLeaves != nil && b.receiverKeys != nil {
		leaves = b.altLeaves[b.receiverKeys.ScriptKey.PubKey]
	}

	vPkt := &vpsbt.InteractiveVPacket{
		AssetID:           assetID,
		Amount:            b.amount,
		ScriptKey:         b.receiverKeys.ScriptKey.PubKey,
		AnchorInternalKey: b.receiverKeys.InternalKey.PubKey,
		AnchorKeyLocator: vpsbt.KeyLocator{
			Family: b.receiverKeys.InternalKey.KeyLocator.Family,
			Index:  b.receiverKeys.InternalKey.KeyLocator.Index,
		},
		AltLeaves:         leaves,
		LockTime:          b.lockTime,
		RelativeLockTime:  b.relativeLockTime,
		AnchorOutputIndex: 0,
		AssetVersion:      vpsbt.AssetVersionV0,
		NetworkHRP:        b.networkHRP,
		CoinType:          b.coinType,
	}

	encoded, err := vPkt.Encode()
	if err != nil {
		return err
	}

	b.vPacketBytes = encoded
	return nil
}

// fund funds the virtual PSBT.
func (b *InteractiveTxBuilder) fund(ctx context.Context) error {
	resp, err := b.walletKit.FundInteractivePsbt(ctx, b.vPacketBytes)
	if err != nil {
		return wrapErr("Fund", err)
	}

	b.fundedPsbt = append([]byte(nil), resp.FundedPsbt...)
	return nil
}

// sign signs the funded PSBT.
func (b *InteractiveTxBuilder) sign(ctx context.Context) error {
	signedPsbt, err := b.walletKit.SignVirtualPsbt(ctx, b.fundedPsbt)
	if err != nil {
		return wrapErr("Sign", err)
	}

	b.signedPsbt = append([]byte(nil), signedPsbt...)
	return nil
}

// complete anchors the signed PSBTs and completes the transfer.
func (b *InteractiveTxBuilder) complete(
	ctx context.Context) (*AssetTransfer, error) {

	result, err := b.walletKit.AnchorVirtualPsbts(
		ctx, [][]byte{b.signedPsbt},
	)
	if err != nil {
		return nil, wrapErr("Complete", err)
	}

	return result, nil
}
