package deposit

import (
	"context"
	"math"
	"net/url"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/rpcutils"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// testAddressGenesis returns a valid genesis for address contract tests.
func testAddressGenesis() asset.Genesis {
	return asset.Genesis{
		FirstPrevOut: wire.OutPoint{Hash: chainhash.Hash{1}, Index: 1},
		Tag:          "address contract test", Type: asset.Normal,
	}
}

// newAddrResponse models tapd's address construction and RPC serialization.
func newAddrResponse(req *taprpc.NewAddrRequest, genesis asset.Genesis,
	groupKey *btcec.PublicKey) (*taprpc.Addr, error) {

	scriptKey, err := rpcutils.UnmarshalScriptKey(req.ScriptKey)
	if err != nil {
		return nil, err
	}
	internalKey, err := rpcutils.UnmarshalKeyDescriptor(req.InternalKey)
	if err != nil {
		return nil, err
	}
	sibling, _, err := commitment.MaybeDecodeTapscriptPreimage(req.TapscriptSibling)
	if err != nil {
		return nil, err
	}
	assetVersion, err := rpcutils.UnmarshalAssetVersion(req.AssetVersion)
	if err != nil {
		return nil, err
	}
	addrVersion, err := address.UnmarshalVersion(req.AddressVersion)
	if err != nil {
		return nil, err
	}
	a := &address.Tap{
		Version: addrVersion, AssetVersion: assetVersion,
		ChainParams: &address.RegressionNetTap, AssetID: genesis.ID(),
		GroupKey: groupKey, ScriptKey: *scriptKey.PubKey,
		InternalKey: *internalKey.PubKey, Amount: req.Amt,
		TapscriptSibling: sibling,
		ProofCourierAddr: url.URL{Scheme: "universerpc", Host: "localhost:10029"},
	}
	a.AttachGenesis(genesis)
	return marshalTestAddr(a)
}

// marshalTestAddr serializes an address and its expanded RPC fields.
func marshalTestAddr(a *address.Tap) (*taprpc.Addr, error) {
	encoded, err := a.EncodeAddress()
	if err != nil {
		return nil, err
	}
	outputKey, err := a.TaprootOutputKey()
	if err != nil {
		return nil, err
	}
	sibling, _, err := commitment.MaybeEncodeTapscriptPreimage(a.TapscriptSibling)
	if err != nil {
		return nil, err
	}
	assetVersion, err := rpcutils.MarshalAssetVersion(a.AssetVersion)
	if err != nil {
		return nil, err
	}
	addrVersion, err := address.MarshalVersion(a.Version)
	if err != nil {
		return nil, err
	}
	result := &taprpc.Addr{
		Encoded: encoded, AssetId: a.AssetID[:], Amount: a.Amount,
		AssetVersion: assetVersion, AddressVersion: addrVersion,
		AssetType:        taprpc.AssetType(a.AssetType()),
		ScriptKey:        a.ScriptKey.SerializeCompressed(),
		InternalKey:      a.InternalKey.SerializeCompressed(),
		TapscriptSibling: sibling,
		TaprootOutputKey: schnorr.SerializePubKey(outputKey),
		ProofCourierAddr: a.ProofCourierAddr.String(),
	}
	if a.GroupKey != nil {
		result.GroupKey = a.GroupKey.SerializeCompressed()
	}
	return result, nil
}

type addressClient struct {
	AddressProofClient

	genesis  asset.Genesis
	groupKey *btcec.PublicKey
	mutate   func(*taprpc.Addr) *taprpc.Addr
	response *taprpc.Addr
	calls    int
}

// NewAddr constructs an address response and applies any configured mutation.
func (c *addressClient) NewAddr(_ context.Context, req *taprpc.NewAddrRequest,
	_ ...grpc.CallOption) (*taprpc.Addr, error) {

	c.calls++
	response, err := newAddrResponse(req, c.genesis, c.groupKey)
	if err != nil {
		return nil, err
	}
	if c.mutate != nil {
		response = c.mutate(response)
	}
	c.response = response
	return response, nil
}

// newAddressKit creates a deposit kit with trusted address metadata.
func newAddressKit(t *testing.T, genesis *asset.Genesis,
	groupKey *btcec.PublicKey) *Kit {

	t.Helper()
	_, sender := scalarKey(t, 1)
	_, receiver := scalarKey(t, 2)
	_, senderInternal := scalarKey(t, 3)
	_, receiverInternal := scalarKey(t, 4)
	kit, err := NewKit(Params{
		SenderInternalPubKey: senderInternal, ReceiverInternalPubKey: receiverInternal,
		SenderScriptKey: sender, ReceiverScriptKey: receiver,
		AssetID: genesis.ID(), Genesis: genesis, GroupKey: groupKey,
		CsvExpiry: 144, ChainParams: &address.RegressionNetTap,
	})
	require.NoError(t, err)
	return kit
}

// TestAddressValidation accepts both contract types and derives the Bitcoin
// output key using trusted metadata, for normal, collectible and grouped assets.
func TestAddressValidation(t *testing.T) {
	for _, kind := range []string{"normal", "collectible", "grouped"} {
		t.Run(kind, func(t *testing.T) {
			genesis := testAddressGenesis()
			amount := uint64(1000)
			var group *btcec.PublicKey
			if kind == "collectible" {
				genesis.Type = asset.Collectible
				amount = 1
			}
			if kind == "grouped" {
				_, group = scalarKey(t, 42)
			}
			kit := newAddressKit(t, &genesis, group)
			client := &addressClient{genesis: genesis, groupKey: group}
			deposit, err := kit.NewAddr(t.Context(), client, amount)
			require.NoError(t, err)
			decoded, err := kit.ValidateAddr(deposit.Encoded, amount)
			require.NoError(t, err)
			key, err := decoded.TaprootOutputKey()
			require.NoError(t, err)
			require.Equal(t, deposit.TaprootOutputKey, schnorr.SerializePubKey(key))
			hash := lntypes.Hash{1}
			htlcAddr, _, err := kit.NewHtlcAddr(t.Context(), client, amount, hash, 144)
			require.NoError(t, err)
			decoded, err = kit.ValidateHtlcAddr(htlcAddr.Encoded, amount, hash, 144)
			require.NoError(t, err)
			key, err = decoded.TaprootOutputKey()
			require.NoError(t, err)
			require.Equal(t, htlcAddr.TaprootOutputKey, schnorr.SerializePubKey(key))
			_, err = kit.ValidateAddr(htlcAddr.Encoded, amount)
			require.Error(t, err)
			_, err = kit.ValidateHtlcAddr(htlcAddr.Encoded, amount, lntypes.Hash{2}, 144)
			require.Error(t, err)
			_, err = kit.ValidateHtlcAddr(htlcAddr.Encoded, amount, hash, 145)
			require.Error(t, err)
		})
	}
}

// TestAddressResponsesRejectMismatches covers malformed responses and fields
// that disagree with the encoded address, independently of the contract checks.
func TestAddressResponsesRejectMismatches(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*taprpc.Addr) *taprpc.Addr
	}{
		{"nil", func(*taprpc.Addr) *taprpc.Addr { return nil }},
		{"empty", func(*taprpc.Addr) *taprpc.Addr { return &taprpc.Addr{} }},
		{"encoding", func(a *taprpc.Addr) *taprpc.Addr { a.Encoded = "invalid"; return a }},
		{"asset ID", func(a *taprpc.Addr) *taprpc.Addr { a.AssetId[0] ^= 1; return a }},
		{"amount", func(a *taprpc.Addr) *taprpc.Addr { a.Amount++; return a }},
		{"asset type", func(a *taprpc.Addr) *taprpc.Addr { a.AssetType = taprpc.AssetType_COLLECTIBLE; return a }},
		{"asset version", func(a *taprpc.Addr) *taprpc.Addr {
			a.AssetVersion = taprpc.AssetVersion_ASSET_VERSION_V0
			return a
		}},
		{"address version", func(a *taprpc.Addr) *taprpc.Addr {
			a.AddressVersion = taprpc.AddrVersion_ADDR_VERSION_V0
			return a
		}},
		{"group key", func(a *taprpc.Addr) *taprpc.Addr { a.GroupKey = []byte{1}; return a }},
		{"script key", func(a *taprpc.Addr) *taprpc.Addr { a.ScriptKey[1] ^= 1; return a }},
		{"internal key", func(a *taprpc.Addr) *taprpc.Addr { a.InternalKey[1] ^= 1; return a }},
		{"sibling", func(a *taprpc.Addr) *taprpc.Addr { a.TapscriptSibling = nil; return a }},
		{"output key", func(a *taprpc.Addr) *taprpc.Addr { a.TaprootOutputKey[0] ^= 1; return a }},
		{"missing output key", func(a *taprpc.Addr) *taprpc.Addr { a.TaprootOutputKey = nil; return a }},
		{"courier", func(a *taprpc.Addr) *taprpc.Addr { a.ProofCourierAddr = "universerpc://other:1"; return a }},
	}
	for _, htlc := range []bool{false, true} {
		path := "deposit"
		if htlc {
			path = "htlc"
		}
		for _, test := range tests {
			t.Run(path+"/"+test.name, func(t *testing.T) {
				genesis := testAddressGenesis()
				kit := newAddressKit(t, &genesis, nil)
				client := &addressClient{genesis: genesis, mutate: test.mutate}
				if htlc {
					addr, contract, err := kit.NewHtlcAddr(t.Context(), client, 1000, lntypes.Hash{1}, 144)
					require.Error(t, err)
					require.Nil(t, addr)
					require.Nil(t, contract)
				} else {
					addr, err := kit.NewAddr(t.Context(), client, 1000)
					require.Error(t, err)
					require.Nil(t, addr)
				}
			})
		}
	}
}

// TestEncodedAddressesRejectWrongContracts rejects a fully consistent RPC
// response as well as a peer address when its encoding describes another
// contract. Expanded-field consistency alone cannot establish correctness.
func TestEncodedAddressesRejectWrongContracts(t *testing.T) {
	_, otherKey := scalarKey(t, 42)
	tests := []struct {
		name   string
		mutate func(*address.Tap)
	}{
		{"network", func(a *address.Tap) { a.ChainParams = &address.MainNetTap }},
		{"address version", func(a *address.Tap) { a.Version = address.V0 }},
		{"asset version", func(a *address.Tap) { a.AssetVersion = asset.V0 }},
		{"asset ID", func(a *address.Tap) { a.AssetID[0] ^= 1 }},
		{"amount", func(a *address.Tap) { a.Amount++ }},
		{"script key", func(a *address.Tap) { a.ScriptKey = *otherKey }},
		{"internal key", func(a *address.Tap) { a.InternalKey = *otherKey }},
		{"group key", func(a *address.Tap) { a.GroupKey = otherKey }},
		{"missing sibling", func(a *address.Tap) { a.TapscriptSibling = nil }},
	}
	for _, isHtlc := range []bool{false, true} {
		path := "deposit"
		if isHtlc {
			path = "htlc"
		}
		for _, test := range tests {
			t.Run(path+"/"+test.name, func(t *testing.T) {
				genesis := testAddressGenesis()
				kit := newAddressKit(t, &genesis, nil)
				client := &addressClient{
					genesis: genesis,
					mutate: func(response *taprpc.Addr) *taprpc.Addr {
						a, err := address.DecodeAddress(
							response.Encoded, &address.RegressionNetTap,
						)
						require.NoError(t, err)
						a.AttachGenesis(genesis)
						test.mutate(a)
						result, err := marshalTestAddr(a)
						require.NoError(t, err)
						return result
					},
				}
				var err error
				if isHtlc {
					_, _, err = kit.NewHtlcAddr(
						t.Context(), client, 1000, lntypes.Hash{1}, 144,
					)
				} else {
					_, err = kit.NewAddr(t.Context(), client, 1000)
				}
				require.Error(t, err)
				if isHtlc {
					_, err = kit.ValidateHtlcAddr(
						client.response.Encoded, 1000, lntypes.Hash{1}, 144,
					)
				} else {
					_, err = kit.ValidateAddr(client.response.Encoded, 1000)
				}
				require.Error(t, err)
			})
		}
	}
}

// TestAddressMetadataIsolation checks trusted constructor inputs, returned
// decoded addresses and retained RPC response buffers cannot change the kit or
// a previously validated response.
func TestAddressMetadataIsolation(t *testing.T) {
	genesis := testAddressGenesis()
	_, groupKey := scalarKey(t, 42)
	kit := newAddressKit(t, &genesis, groupKey)
	groupCopy := *groupKey
	client := &addressClient{genesis: genesis, groupKey: &groupCopy}
	_, replacement := scalarKey(t, 43)
	genesis.Tag = "changed genesis"
	*groupKey = *replacement
	result, err := kit.NewAddr(t.Context(), client, 1000)
	require.NoError(t, err)
	decoded, err := kit.ValidateAddr(result.Encoded, 1000)
	require.NoError(t, err)
	*decoded.GroupKey = *replacement
	decoded.ChainParams.TapHRP = "changed network"
	decoded.ChainParams.Params.Name = "changed chain"
	clear(client.response.TaprootOutputKey)
	clear(client.response.InternalKey)
	client.response.Encoded = "changed response"
	decoded, err = kit.ValidateAddr(result.Encoded, 1000)
	require.NoError(t, err)
	key, err := decoded.TaprootOutputKey()
	require.NoError(t, err)
	require.Equal(t, schnorr.SerializePubKey(key), result.TaprootOutputKey)
	require.Equal(t, decoded.InternalKey.SerializeCompressed(), result.InternalKey)
}

// TestAddressMetadataRequired keeps proof-only kits compatible while requiring
// enough trusted information to check addresses before invoking the RPC.
func TestAddressMetadataRequired(t *testing.T) {
	genesis := testAddressGenesis()
	kit := newAddressKit(t, &genesis, nil)
	kit.genesis = nil
	client := &addressClient{genesis: genesis}
	_, err := kit.NewAddr(t.Context(), client, 1000)
	require.ErrorContains(t, err, "trusted asset genesis")
	_, _, err = kit.NewHtlcAddr(t.Context(), client, 1000, lntypes.Hash{1}, 144)
	require.ErrorContains(t, err, "trusted asset genesis")
	require.Zero(t, client.calls)
	kit = newAddressKit(t, &genesis, nil)
	for _, amount := range []uint64{0, math.MaxInt64 + 1} {
		_, err = kit.NewAddr(t.Context(), client, amount)
		require.ErrorContains(t, err, "invalid address amount")
	}
	require.Zero(t, client.calls)
}

type siblingMutatingClient struct {
	*addressClient

	kit *Kit
}

// NewAddr replaces the request's sibling and returns an address matching that
// replacement, to check validation is bound to independent contract data.
func (c *siblingMutatingClient) NewAddr(ctx context.Context,
	req *taprpc.NewAddrRequest, opts ...grpc.CallOption) (*taprpc.Addr, error) {

	otherKit, err := c.kit.newHtlcSwapKit(1000, lntypes.Hash{2}, 144)
	if err != nil {
		return nil, err
	}
	preimage, err := otherKit.GetSiblingPreimage()
	if err != nil {
		return nil, err
	}
	sibling, _, err := commitment.MaybeEncodeTapscriptPreimage(&preimage)
	if err != nil {
		return nil, err
	}
	copy(req.TapscriptSibling, sibling)
	return c.addressClient.NewAddr(ctx, req, opts...)
}

// TestAddressRequestSiblingIsolation rejects a response whose sibling was
// replaced through a request slice retained by the RPC client.
func TestAddressRequestSiblingIsolation(t *testing.T) {
	genesis := testAddressGenesis()
	kit := newAddressKit(t, &genesis, nil)
	client := &siblingMutatingClient{
		addressClient: &addressClient{genesis: genesis}, kit: kit,
	}
	_, _, err := kit.NewHtlcAddr(t.Context(), client, 1000, lntypes.Hash{1}, 144)
	require.ErrorContains(t, err, "sibling mismatch")
}
