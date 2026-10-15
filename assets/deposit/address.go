package deposit

import (
	"bytes"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/lightninglabs/loop/assets/internal/policy"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"google.golang.org/protobuf/proto"
)

// validateAddressAsset requires trusted metadata for deriving an address's
// output key. The asset ID alone cannot supply the genesis/type or group key.
func (d *Kit) validateAddressAsset(amount uint64) error {
	if d == nil {
		return fmt.Errorf("deposit kit is required")
	}
	if d.genesis == nil {
		return fmt.Errorf("trusted asset genesis is required for addresses")
	}
	if amount == 0 || amount > math.MaxInt64 ||
		(d.genesis.Type == asset.Collectible && amount != 1) {

		return fmt.Errorf("invalid address amount")
	}
	return nil
}

// ValidateAddr validates an encoded deposit address, including one received
// from a peer, against this kit and the agreed amount. Genesis and GroupKey
// must come from trusted metadata supplied to NewKit. The returned address has
// its genesis attached so its output key can be independently derived.
// Proof courier selection remains a caller policy.
func (d *Kit) ValidateAddr(encoded string, amount uint64) (*address.Tap, error) {
	if err := d.validateAddressAsset(amount); err != nil {
		return nil, err
	}
	sibling, err := d.encodedTimeoutPathSibling()
	if err != nil {
		return nil, err
	}
	return d.validateAddr(encoded, amount, d.muSig2Key.PreTweakedKey, sibling)
}

// ValidateHtlcAddr validates an encoded HTLC address against the deposit's
// parties and the agreed swap terms. It applies the same contract checks as
// NewHtlcAddr and accepts addresses received from peers.
func (d *Kit) ValidateHtlcAddr(encoded string, amount uint64,
	swapHash lntypes.Hash, csvExpiry uint32) (*address.Tap, error) {

	if err := d.validateAddressAsset(amount); err != nil {
		return nil, err
	}
	kit, err := d.newHtlcSwapKit(amount, swapHash, csvExpiry)
	if err != nil {
		return nil, err
	}
	internalKey, err := kit.GetAggregateKey()
	if err != nil {
		return nil, err
	}
	sibling, err := kit.GetSiblingPreimage()
	if err != nil {
		return nil, err
	}
	siblingBytes, _, err := commitment.MaybeEncodeTapscriptPreimage(&sibling)
	if err != nil {
		return nil, err
	}
	return d.validateAddr(encoded, amount, internalKey, siblingBytes)
}

// validateAddr treats the encoded address as authoritative and binds all
// contract fields before attaching trusted metadata to derive the output key.
func (d *Kit) validateAddr(encoded string, amount uint64,
	internalKey *btcec.PublicKey, sibling []byte) (*address.Tap, error) {

	a, err := address.DecodeAddress(
		encoded, policy.CloneAddressParams(*d.chainParams),
	)
	if err != nil {
		return nil, fmt.Errorf("decode contract address: %w", err)
	}
	if a.Version != policy.AddressVersion || a.AssetVersion != policy.AssetVersion {
		return nil, fmt.Errorf("contract address version mismatch")
	}
	if a.AssetID != d.assetID || a.Amount != amount {
		return nil, fmt.Errorf("contract address asset or amount mismatch")
	}
	if !equalOptionalKey(a.GroupKey, d.groupKey) {
		return nil, fmt.Errorf("contract address group key mismatch")
	}
	scriptKey, err := d.TapScriptKey()
	if err != nil {
		return nil, err
	}
	if !a.ScriptKey.IsEqual(scriptKey.PubKey) ||
		!a.InternalKey.IsEqual(internalKey) {

		return nil, fmt.Errorf("contract address key mismatch")
	}
	siblingBytes, _, err := commitment.MaybeEncodeTapscriptPreimage(
		a.TapscriptSibling,
	)
	if err != nil {
		return nil, fmt.Errorf("invalid address sibling: %w", err)
	}
	if !bytes.Equal(siblingBytes, sibling) {
		return nil, fmt.Errorf("contract address sibling mismatch")
	}
	a.AttachGenesis(*d.genesis)
	if _, err := a.TaprootOutputKey(); err != nil {
		return nil, fmt.Errorf("derive contract address output key: %w", err)
	}
	return a, nil
}

// validateAddrResponse snapshots the RPC response and checks every expanded
// address field against the validated encoding, including the derived key.
func (d *Kit) validateAddrResponse(response *taprpc.Addr, amount uint64,
	internalKey *btcec.PublicKey, sibling []byte) (*taprpc.Addr, error) {

	if response == nil {
		return nil, fmt.Errorf("address response is required")
	}
	result := proto.Clone(response).(*taprpc.Addr)
	a, err := d.validateAddr(result.Encoded, amount, internalKey, sibling)
	if err != nil {
		return nil, err
	}
	var groupKey []byte
	if a.GroupKey != nil {
		groupKey = a.GroupKey.SerializeCompressed()
	}
	if result.AssetVersion != policy.RPCAssetVersion ||
		result.AddressVersion != policy.RPCAddressVersion ||
		!bytes.Equal(result.AssetId, a.AssetID[:]) ||
		result.Amount != a.Amount ||
		result.AssetType != taprpc.AssetType(d.genesis.Type) ||
		!bytes.Equal(result.GroupKey, groupKey) ||
		!bytes.Equal(result.ScriptKey, a.ScriptKey.SerializeCompressed()) ||
		!bytes.Equal(result.InternalKey, a.InternalKey.SerializeCompressed()) ||
		!bytes.Equal(result.TapscriptSibling, sibling) ||
		result.ProofCourierAddr != a.ProofCourierAddr.String() {

		return nil, fmt.Errorf("address response disagrees with encoding")
	}
	outputKey, err := a.TaprootOutputKey()
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(result.TaprootOutputKey, schnorr.SerializePubKey(outputKey)) {
		return nil, fmt.Errorf("address response output key mismatch")
	}
	return result, nil
}

// equalOptionalKey compares optional public keys, including their parity.
func equalOptionalKey(a, b *btcec.PublicKey) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.IsEqual(b)
}
