package tapsdk

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// mockClient is a full mock of the Client interface for testing Wallet
// high-level methods. Only methods needed by the tests are implemented;
// the rest panic to catch unexpected calls.
type mockClient struct {
	mock.Mock
}

// --- WalletClient (TaprootAssets service) ---

func (m *mockClient) GetInfo(ctx context.Context) (*Info, error) {
	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*Info), args.Error(1)
}

func (m *mockClient) ListAssetRecords(ctx context.Context,
	req *ListAssetsRequest) ([]*AssetRecord, error) {

	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).([]*AssetRecord), args.Error(1)
}

func (m *mockClient) ListBalances(ctx context.Context,
	req *ListBalancesRequest) (*ListBalancesResponse,
	error) {

	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*ListBalancesResponse), args.Error(1)
}

func (m *mockClient) ListTransfers(ctx context.Context,
	req *ListTransfersRequest) ([]*AssetTransfer, error) {

	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).([]*AssetTransfer), args.Error(1)
}

func (m *mockClient) SendAsset(ctx context.Context,
	req *SendAssetRequest) (*AssetTransfer, error) {

	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*AssetTransfer), args.Error(1)
}

func (m *mockClient) NewAddr(ctx context.Context,
	req *NewAddressRequest) (*Address, error) {

	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*Address), args.Error(1)
}

func (m *mockClient) DecodeAddr(ctx context.Context,
	addr string) (*Address, error) {

	args := m.Called(ctx, addr)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*Address), args.Error(1)
}

func (m *mockClient) QueryAddrs(ctx context.Context,
	query *AddressQuery) ([]*Address, error) {

	args := m.Called(ctx, query)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).([]*Address), args.Error(1)
}

func (m *mockClient) AddrReceives(ctx context.Context,
	query *AddressReceivesQuery) ([]*AddressEvent,
	error) {

	args := m.Called(ctx, query)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).([]*AddressEvent), args.Error(1)
}

// --- ProofClient ---

func (m *mockClient) ExportProof(ctx context.Context,
	ref AssetRef, scriptKey PubKey,
	outpoint *Outpoint) (*ProofFile, error) {

	args := m.Called(ctx, ref, scriptKey, outpoint)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*ProofFile), args.Error(1)
}

func (m *mockClient) UnpackProofFile(ctx context.Context,
	rawProofFile []byte) ([][]byte, error) {

	args := m.Called(ctx, rawProofFile)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).([][]byte), args.Error(1)
}

func (m *mockClient) DecodeProof(ctx context.Context,
	rawProof []byte) (*DecodedProof, error) {

	args := m.Called(ctx, rawProof)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*DecodedProof), args.Error(1)
}

func (m *mockClient) RegisterTransfer(ctx context.Context,
	assetRef AssetRef, scriptKey PubKey,
	outpoint Outpoint) (*RegisteredAsset, error) {

	args := m.Called(ctx, assetRef, scriptKey, outpoint)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*RegisteredAsset), args.Error(1)
}

// --- WalletKitClient (AssetWallet service) ---

func (m *mockClient) CustomAnchorCapabilities(
	ctx context.Context) (*CustomAnchorCapabilities, error) {

	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*CustomAnchorCapabilities), args.Error(1)
}

func (m *mockClient) DeriveScriptKey(
	ctx context.Context) (*ScriptKey, error) {

	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*ScriptKey), args.Error(1)
}

func (m *mockClient) DeriveInternalKey(
	ctx context.Context) (*InternalKey, error) {

	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*InternalKey), args.Error(1)
}

func (m *mockClient) FundTransfer(ctx context.Context,
	recipients []Recipient,
	inputs []PrevID) (*FundedTransfer, error) {

	args := m.Called(ctx, recipients, inputs)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*FundedTransfer), args.Error(1)
}

func (m *mockClient) FundInteractivePsbt(ctx context.Context,
	psbt []byte) (*FundedTransfer, error) {

	args := m.Called(ctx, psbt)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*FundedTransfer), args.Error(1)
}

func (m *mockClient) SignVirtualPsbt(ctx context.Context,
	fundedPsbt []byte) ([]byte, error) {

	args := m.Called(ctx, fundedPsbt)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).([]byte), args.Error(1)
}

func (m *mockClient) CommitVirtualPsbts(ctx context.Context,
	virtualPsbts [][]byte, passivePsbts [][]byte,
	feeRate FeeRate) (*CommittedTransfer, error) {

	args := m.Called(ctx, virtualPsbts, passivePsbts, feeRate)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*CommittedTransfer), args.Error(1)
}

func (m *mockClient) AnchorVirtualPsbts(ctx context.Context,
	signedPsbts [][]byte) (*AssetTransfer, error) {

	args := m.Called(ctx, signedPsbts)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*AssetTransfer), args.Error(1)
}

func (m *mockClient) PublishAndLogTransfer(ctx context.Context,
	anchorPsbt []byte, virtualPsbts [][]byte, passivePsbts [][]byte,
	skipAnchorTxBroadcast bool) (*AssetPacket, error) {

	args := m.Called(
		ctx, anchorPsbt, virtualPsbts, passivePsbts,
		skipAnchorTxBroadcast,
	)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*AssetPacket), args.Error(1)
}

// --- UniverseClient ---

func (m *mockClient) InsertProof(ctx context.Context, rawProof []byte,
	decoded *DecodedProof) error {

	args := m.Called(ctx, rawProof, decoded)
	return args.Error(0)
}

// --- MintClient ---

func (m *mockClient) MintAsset(ctx context.Context,
	req *MintAssetRequest) (*MintingBatch, error) {

	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*MintingBatch), args.Error(1)
}

func (m *mockClient) MintIssuance(ctx context.Context,
	req *MintIssuanceRequest) (*MintingBatch, error) {

	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*MintingBatch), args.Error(1)
}

func (m *mockClient) FundBatch(ctx context.Context,
	req *FundBatchRequest) (*VerboseMintingBatch,
	error) {

	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*VerboseMintingBatch), args.Error(1)
}

func (m *mockClient) SealBatch(ctx context.Context,
	req *SealBatchRequest) (*MintingBatch, error) {

	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*MintingBatch), args.Error(1)
}

func (m *mockClient) FinalizeBatch(ctx context.Context,
	req *FinalizeBatchRequest) (*MintingBatch, error) {

	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*MintingBatch), args.Error(1)
}

func (m *mockClient) CancelBatch(
	ctx context.Context) (*CancelBatchResponse, error) {

	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*CancelBatchResponse), args.Error(1)
}

func (m *mockClient) ListBatches(ctx context.Context,
	req *ListBatchesRequest) ([]*VerboseMintingBatch,
	error) {

	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).([]*VerboseMintingBatch), args.Error(1)
}

// --- WalletClient extras (PR #30 additions) ---

func (m *mockClient) ListUtxos(ctx context.Context,
	req *ListUtxosRequest) (
	map[string]*ManagedUtxo, error) {

	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(map[string]*ManagedUtxo),
		args.Error(1)
}

func (m *mockClient) ListAssetGroups(
	ctx context.Context) ([]AssetGroupRecord, error) {

	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).([]AssetGroupRecord), args.Error(1)
}

func (m *mockClient) BurnAsset(ctx context.Context,
	req *BurnAssetRequest) (
	*BurnAssetResponse, error) {

	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*BurnAssetResponse), args.Error(1)
}

func (m *mockClient) ListBurns(ctx context.Context,
	req *ListBurnsRequest) ([]*BurnRecord, error) {

	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).([]*BurnRecord), args.Error(1)
}

func (m *mockClient) FetchAssetMeta(ctx context.Context,
	req *FetchAssetMetaRequest) (
	*AssetMeta, error) {

	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*AssetMeta), args.Error(1)
}

func (m *mockClient) VerifyProof(ctx context.Context,
	rawProofFile []byte) (
	*VerifyProofResponse, error) {

	args := m.Called(ctx, rawProofFile)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*VerifyProofResponse), args.Error(1)
}

// --- WalletKitClient extras (PR #30 additions) ---

func (m *mockClient) QueryInternalKey(ctx context.Context,
	internalKey []byte) (*KeyDescriptor, error) {

	args := m.Called(ctx, internalKey)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*KeyDescriptor), args.Error(1)
}

func (m *mockClient) QueryScriptKey(ctx context.Context,
	tweakedScriptKey []byte) (*ScriptKey, error) {

	args := m.Called(ctx, tweakedScriptKey)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*ScriptKey), args.Error(1)
}

func (m *mockClient) ProveAssetOwnership(ctx context.Context,
	req *ProveOwnershipRequest) (
	*OwnershipProof, error) {

	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*OwnershipProof), args.Error(1)
}

func (m *mockClient) VerifyAssetOwnership(ctx context.Context,
	req *VerifyOwnershipRequest) (
	*VerifyOwnershipResponse, error) {

	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*VerifyOwnershipResponse),
		args.Error(1)
}

func (m *mockClient) RemoveUTXOLease(ctx context.Context,
	outpoint Outpoint) error {

	args := m.Called(ctx, outpoint)
	return args.Error(0)
}

func (m *mockClient) DeclareScriptKey(ctx context.Context,
	req *DeclareScriptKeyRequest) (
	*ScriptKey, error) {

	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*ScriptKey), args.Error(1)
}

func (m *mockClient) ExportBackup(ctx context.Context,
	mode BackupMode) ([]byte, error) {

	args := m.Called(ctx, mode)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).([]byte), args.Error(1)
}

func (m *mockClient) ImportBackup(ctx context.Context,
	backup []byte) (uint32, error) {

	args := m.Called(ctx, backup)
	return uint32(args.Int(0)), args.Error(1)
}

// --- UniverseClient (PR #31 additions) ---

func (m *mockClient) AssetRoots(ctx context.Context,
	req *AssetRootRequest) (
	map[string]*UniverseRoot, error) {

	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(map[string]*UniverseRoot),
		args.Error(1)
}

func (m *mockClient) QueryAssetRoots(ctx context.Context,
	id *UniverseID) (*QueryRootResponse, error) {

	args := m.Called(ctx, id)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*QueryRootResponse), args.Error(1)
}

func (m *mockClient) DeleteAssetRoot(ctx context.Context,
	id *UniverseID) error {

	args := m.Called(ctx, id)
	return args.Error(0)
}

func (m *mockClient) AssetLeafKeys(ctx context.Context,
	req *AssetLeafKeysRequest) (
	[]AssetLeafKey, error) {

	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).([]AssetLeafKey), args.Error(1)
}

func (m *mockClient) AssetLeaves(ctx context.Context,
	id *UniverseID) ([]AssetLeaf, error) {

	args := m.Called(ctx, id)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).([]AssetLeaf), args.Error(1)
}

func (m *mockClient) QueryProof(ctx context.Context,
	key *UniverseKey) (*AssetProofResponse,
	error) {

	args := m.Called(ctx, key)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*AssetProofResponse), args.Error(1)
}

func (m *mockClient) UniverseStats(
	ctx context.Context) (*UniverseStats, error) {

	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*UniverseStats), args.Error(1)
}

func (m *mockClient) QueryAssetStats(ctx context.Context,
	req *AssetStatsQuery) (
	[]AssetStatsSnapshot, error) {

	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).([]AssetStatsSnapshot), args.Error(1)
}

func (m *mockClient) QueryEvents(ctx context.Context,
	req *QueryEventsRequest) (
	[]GroupedUniverseEvents, error) {

	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).([]GroupedUniverseEvents),
		args.Error(1)
}

func (m *mockClient) ListFederationServers(
	ctx context.Context) ([]FederationServer, error) {

	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).([]FederationServer), args.Error(1)
}

func (m *mockClient) AddFederationServer(ctx context.Context,
	servers []FederationServer) error {

	args := m.Called(ctx, servers)
	return args.Error(0)
}

func (m *mockClient) DeleteFederationServer(ctx context.Context,
	servers []FederationServer) error {

	args := m.Called(ctx, servers)
	return args.Error(0)
}

func (m *mockClient) SetFederationSyncConfig(ctx context.Context,
	global []GlobalFederationSyncConfig,
	asset []AssetFederationSyncConfig) error {

	args := m.Called(ctx, global, asset)
	return args.Error(0)
}

func (m *mockClient) QueryFederationSyncConfig(ctx context.Context,
	ids []UniverseID) (*FederationSyncConfig,
	error) {

	args := m.Called(ctx, ids)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*FederationSyncConfig),
		args.Error(1)
}

func (m *mockClient) Info(
	ctx context.Context) (*UniverseInfo, error) {

	args := m.Called(ctx)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*UniverseInfo), args.Error(1)
}

func (m *mockClient) SyncUniverse(ctx context.Context,
	req *SyncRequest) ([]SyncedUniverse, error) {

	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).([]SyncedUniverse), args.Error(1)
}

// --- EventClient ---

func (m *mockClient) SubscribeReceiveEvents(ctx context.Context,
	req *SubscribeReceiveEventsRequest) (
	<-chan *ReceiveEventRecord, <-chan error, error) {

	panic("SubscribeReceiveEvents not expected in unit tests")
}

func (m *mockClient) SubscribeSendEvents(ctx context.Context,
	req *SubscribeSendEventsRequest) (
	<-chan *SendEventRecord, <-chan error, error) {

	panic("SubscribeSendEvents not expected in unit tests")
}

func (m *mockClient) SubscribeMintEvents(ctx context.Context,
	req *SubscribeMintEventsRequest) (
	<-chan *MintEvent, <-chan error, error) {

	panic("SubscribeMintEvents not expected in unit tests")
}

func (m *mockClient) Close() error {
	args := m.Called()
	return args.Error(0)
}

// --- Tests ---

// testKey derives a valid compressed secp256k1 public key from a small
// scalar, so fixture addresses the SDK decodes locally actually parse.
func testKey(t *testing.T, scalar byte) PubKey {
	t.Helper()

	priv, _ := btcec.PrivKeyFromBytes([]byte{scalar})
	pk, err := ParsePubKey(priv.PubKey().SerializeCompressed())
	require.NoError(t, err)
	return pk
}

// sendTestAssetID returns a deterministic asset ID for fixture addresses.
func sendTestAssetID() AssetID {
	var id AssetID
	for i := range id {
		id[i] = byte(i + 1)
	}
	return id
}

// encodeV2NoAmount builds a real bech32m-encoded V2 Tap address with
// no embedded amount so Wallet.Send's local decoder actually parses.
// seed distinguishes otherwise-identical addresses across recipients
// in a single test.
func encodeV2NoAmount(t *testing.T, seed ...byte) string {
	t.Helper()

	s := byte(13)
	if len(seed) == 1 {
		s = seed[0]
	}

	addr := &Address{
		AddressVersion:   AddressVersionV2,
		AssetVersion:     AssetVersionV0,
		AssetRef:         AssetRefFromGroupKey(testKey(t, s)),
		ScriptKey:        testKey(t, 7),
		InternalKey:      testKey(t, 11),
		ProofCourierAddr: testAuthmailboxUniverserpcLocalhost10029,
	}

	encoded, err := EncodeAddress(
		addr, NetworkRegtest,
	)
	require.NoError(t, err)
	return encoded
}

func encodeV2NoAmountForRef(t *testing.T, ref AssetRef,
	seed byte) string {

	t.Helper()

	addr := &Address{
		AddressVersion:   AddressVersionV2,
		AssetVersion:     AssetVersionV0,
		AssetRef:         ref,
		ScriptKey:        testKey(t, seed),
		InternalKey:      testKey(t, seed+1),
		ProofCourierAddr: testAuthmailboxUniverserpcLocalhost10029,
	}

	encoded, err := EncodeAddress(
		addr, NetworkRegtest,
	)
	require.NoError(t, err)
	return encoded
}

func encodeV2EmbeddedForRef(t *testing.T, ref AssetRef, amount uint64,
	seed byte) string {

	t.Helper()

	addr := &Address{
		AddressVersion:   AddressVersionV2,
		AssetVersion:     AssetVersionV0,
		AssetRef:         ref,
		Amount:           amount,
		ScriptKey:        testKey(t, seed),
		InternalKey:      testKey(t, seed+1),
		ProofCourierAddr: testAuthmailboxUniverserpcLocalhost10029,
	}

	encoded, err := EncodeAddress(
		addr, NetworkRegtest,
	)
	require.NoError(t, err)
	return encoded
}

func recipientAmountIs(recipient Recipient, amount uint64) bool {
	got, ok := recipient.Amount()
	return ok && got == amount
}

func recipientUsesEmbeddedAmount(recipient Recipient) bool {
	_, ok := recipient.Amount()
	return !ok
}

// encodeEmbedded builds an address carrying an embedded amount so the
// legacy RecipientsV1 path is exercised.
func encodeEmbedded(t *testing.T, amount uint64,
	version AddressVersion) string {

	t.Helper()

	s := byte(17)

	id := sendTestAssetID()
	// Mutate the first byte so the AssetRef differs between seeds.
	id[0] = s

	addr := &Address{
		AddressVersion: version,
		AssetVersion:   AssetVersionV0,
		AssetRef:       AssetRefFromAssetID(id),
		Amount:         amount,
		ScriptKey:      testKey(t, 7),
		InternalKey:    testKey(t, 11),
	}

	encoded, err := EncodeAddress(
		addr, NetworkRegtest,
	)
	require.NoError(t, err)
	return encoded
}

func TestSend_WithAmount(t *testing.T) {
	mc := new(mockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	addr := encodeV2NoAmount(t)
	amount := uint64(500)
	feeRate := mustFeeRateSatPerVByte(t, 25)
	label := "test-send"

	expectedTransfer := &AssetTransfer{
		AnchorTxid: "abc123",
		Label:      label,
	}

	mc.On("SendAsset", ctx, mock.MatchedBy(
		func(req *SendAssetRequest) bool {
			return len(req.Recipients) == 1 &&
				req.Recipients[0].Address == addr &&
				recipientAmountIs(req.Recipients[0], amount) &&
				req.FeeRate == feeRate &&
				req.Label == label
		}),
	).Return(expectedTransfer, nil)

	transfer, err := w.Send(
		ctx, addr,
		WithAmount(amount),
		WithFeeRate(feeRate), WithLabel(label),
	)
	require.NoError(t, err)
	require.Equal(t, expectedTransfer, transfer)

	mc.AssertExpectations(t)
}

// TestSend_NoAmountOption_UsesAddressEmbedded verifies that an
// embedded-amount address (V0/V1, or V2 with a baked-in amount) sends
// correctly when the caller does not pass WithAmount.
func TestSend_NoAmountOption_UsesAddressEmbedded(t *testing.T) {
	mc := new(mockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	addr := encodeEmbedded(t, 42, AddressVersionV0)

	expectedTransfer := &AssetTransfer{AnchorTxid: "def456"}

	mc.On("SendAsset", ctx, mock.MatchedBy(
		func(req *SendAssetRequest) bool {
			return len(req.Recipients) == 1 &&
				req.Recipients[0].Address == addr &&
				recipientUsesEmbeddedAmount(req.Recipients[0])
		}),
	).Return(expectedTransfer, nil)

	transfer, err := w.Send(ctx, addr)
	require.NoError(t, err)
	require.Equal(t, expectedTransfer, transfer)

	mc.AssertExpectations(t)
}

// TestSend_V2_AmountRequired rejects a V2 address with no embedded
// amount when the caller does not specify WithAmount.
func TestSend_V2_AmountRequired(t *testing.T) {
	mc := new(mockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	addr := encodeV2NoAmount(t)

	_, err := w.Send(ctx, addr)
	require.ErrorIs(t, err, ErrAmountRequired)

	mc.AssertExpectations(t)
}

// TestSend_AmountMismatch rejects an explicit amount that contradicts
// the address-embedded amount.
func TestSend_AmountMismatch(t *testing.T) {
	mc := new(mockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	addr := encodeEmbedded(t, 100, AddressVersionV1)

	_, err := w.Send(ctx, addr, WithAmount(250))
	require.ErrorIs(t, err, ErrAmountMismatch)

	mc.AssertExpectations(t)
}

// TestSend_AmountMatchesEmbedded accepts an explicit amount that
// matches the embedded value and routes through the explicit
// Recipients path — the caller's intent is to supply the amount, so
// the SDK preserves that on the wire.
func TestSend_AmountMatchesEmbedded(t *testing.T) {
	mc := new(mockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	addr := encodeEmbedded(t, 100, AddressVersionV1)
	mc.On("SendAsset", ctx, mock.MatchedBy(
		func(req *SendAssetRequest) bool {
			return len(req.Recipients) == 1 &&
				req.Recipients[0].Address == addr &&
				recipientAmountIs(req.Recipients[0], 100)
		}),
	).Return(&AssetTransfer{AnchorTxid: "match"}, nil)

	transfer, err := w.Send(ctx, addr, WithAmount(100))
	require.NoError(t, err)
	require.NotNil(t, transfer)

	mc.AssertExpectations(t)
}

// TestSend_DecodeError surfaces an error when the address string is
// not a valid bech32m Tap address.
func TestSend_DecodeError(t *testing.T) {
	mc := new(mockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	_, err := w.Send(ctx, "not-a-tap-address", WithAmount(10))
	require.Error(t, err)

	mc.AssertExpectations(t)
}

func TestSend_Error(t *testing.T) {
	mc := new(mockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	addr := encodeV2NoAmount(t)
	mc.On("SendAsset", ctx, mock.Anything).Return(
		nil, context.DeadlineExceeded,
	)

	_, err := w.Send(ctx, addr, WithAmount(100))
	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	mc.AssertExpectations(t)
}

func TestSend_WithSkipProofCourierPingCheck(t *testing.T) {
	mc := new(mockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	addr := encodeV2NoAmount(t)
	expectedTransfer := &AssetTransfer{AnchorTxid: "ghi789"}

	mc.On("SendAsset", ctx, mock.MatchedBy(
		func(req *SendAssetRequest) bool {
			return req.SkipProofCourierPingCheck
		}),
	).Return(expectedTransfer, nil)

	transfer, err := w.Send(
		ctx, addr,
		WithAmount(42),
		WithSkipProofCourierPingCheck(),
	)
	require.NoError(t, err)
	require.Equal(t, expectedTransfer, transfer)

	mc.AssertExpectations(t)
}

func TestSendMulti_MultipleRecipients(t *testing.T) {
	mc := new(mockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	ref := AssetRefFromGroupKey(testKey(t, 21))
	aliceAddr := encodeV2NoAmountForRef(t, ref, 31)
	bobAddr := encodeV2NoAmountForRef(t, ref, 41)

	recipients := []Recipient{
		RecipientWithAmount(aliceAddr, 100),
		RecipientWithAmount(bobAddr, 200),
	}

	expectedTransfer := &AssetTransfer{AnchorTxid: "multi123"}

	mc.On("SendAsset", ctx, mock.MatchedBy(
		func(req *SendAssetRequest) bool {
			return len(req.Recipients) == 2 &&
				req.Recipients[0].Address == aliceAddr &&
				recipientAmountIs(req.Recipients[0], 100) &&
				req.Recipients[1].Address == bobAddr &&
				recipientAmountIs(req.Recipients[1], 200)
		}),
	).Return(expectedTransfer, nil)

	transfer, err := w.SendMulti(ctx, recipients)
	require.NoError(t, err)
	require.Equal(t, expectedTransfer, transfer)

	mc.AssertExpectations(t)
}

func TestSendMulti_RejectsMixedAssetRefs(t *testing.T) {
	mc := new(mockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	aliceAddr := encodeV2NoAmount(t, 21)
	bobAddr := encodeV2NoAmount(t, 22)

	recipients := []Recipient{
		RecipientWithAmount(aliceAddr, 100),
		RecipientWithAmount(bobAddr, 200),
	}

	_, err := w.SendMulti(ctx, recipients)
	require.ErrorIs(t, err, ErrMixedAssetBatchUnsupported)

	mc.AssertExpectations(t)
}

func TestSendMulti_NoRecipients(t *testing.T) {
	mc := new(mockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	_, err := w.SendMulti(ctx, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNoRecipients)

	_, err = w.SendMulti(ctx, []Recipient{})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNoRecipients)
}

func TestSendMulti_WithOptions(t *testing.T) {
	mc := new(mockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	addr := encodeV2NoAmount(t)
	recipients := []Recipient{
		RecipientWithAmount(addr, 50),
	}

	expectedTransfer := &AssetTransfer{
		AnchorTxid: "opts123",
		Label:      "batch-payment",
	}

	mc.On("SendAsset", ctx, mock.MatchedBy(
		func(req *SendAssetRequest) bool {
			return req.FeeRate == mustFeeRateSatPerVByte(t, 10) &&
				req.Label == "batch-payment" &&
				req.SkipProofCourierPingCheck
		}),
	).Return(expectedTransfer, nil)

	transfer, err := w.SendMulti(
		ctx, recipients,
		WithFeeRate(mustFeeRateSatPerVByte(t, 10)),
		WithLabel("batch-payment"),
		WithSkipProofCourierPingCheck(),
	)
	require.NoError(t, err)
	require.Equal(t, expectedTransfer, transfer)

	mc.AssertExpectations(t)
}

// TestSendMulti_MixedAmountsNormalised feeds SendMulti a batch where
// one recipient has an explicit amount and another uses the embedded amount.
// The low-level SendAsset must still see a uniform shape, so the SDK
// echoes the embedded value into the embedded-amount slot.
func TestSendMulti_MixedAmountsNormalised(t *testing.T) {
	mc := new(mockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	ref := AssetRefFromGroupKey(testKey(t, 41))
	explicitAddr := encodeV2NoAmountForRef(t, ref, 51)
	embeddedAddr := encodeV2EmbeddedForRef(t, ref, 75, 61)

	recipients := []Recipient{
		RecipientWithAmount(explicitAddr, 200),
		RecipientWithEmbeddedAmount(embeddedAddr),
	}

	mc.On("SendAsset", ctx, mock.MatchedBy(
		func(req *SendAssetRequest) bool {
			return len(req.Recipients) == 2 &&
				req.Recipients[0].Address == explicitAddr &&
				recipientAmountIs(req.Recipients[0], 200) &&
				req.Recipients[1].Address == embeddedAddr &&
				recipientAmountIs(req.Recipients[1], 75)
		}),
	).Return(&AssetTransfer{AnchorTxid: "mix"}, nil)

	_, err := w.SendMulti(ctx, recipients)
	require.NoError(t, err)

	mc.AssertExpectations(t)
}

// TestSendMulti_AmountMismatch rejects a recipient whose explicit
// amount disagrees with the address-embedded amount.
func TestSendMulti_AmountMismatch(t *testing.T) {
	mc := new(mockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	addr := encodeEmbedded(t, 75, AddressVersionV1)
	_, err := w.SendMulti(ctx, []Recipient{
		RecipientWithAmount(addr, 200),
	})
	require.ErrorIs(t, err, ErrAmountMismatch)

	mc.AssertExpectations(t)
}

// TestSendMulti_AmountRequired rejects a V2 address with no embedded
// amount when the caller does not specify one.
func TestSendMulti_AmountRequired(t *testing.T) {
	mc := new(mockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	addr := encodeV2NoAmount(t)
	recipients := []Recipient{
		RecipientWithEmbeddedAmount(addr),
	}

	_, err := w.SendMulti(ctx, recipients)
	require.ErrorIs(t, err, ErrAmountRequired)

	mc.AssertExpectations(t)
}

func TestSendMulti_ExplicitZeroAmount(t *testing.T) {
	mc := new(mockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	addr := encodeV2NoAmount(t)
	_, err := w.SendMulti(ctx, []Recipient{
		RecipientWithAmount(addr, 0),
	})
	require.ErrorIs(t, err, ErrZeroAmount)

	mc.AssertExpectations(t)
}
