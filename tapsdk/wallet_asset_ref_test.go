package tapsdk

import (
	"context"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// refMockClient is a full mock of the Client interface for testing AssetRef-
// based Wallet methods. Methods not exercised by these tests use panic
// stubs so unexpected calls are caught immediately.
type refMockClient struct {
	mock.Mock
}

// --- WalletClient ---

func (m *refMockClient) GetInfo(ctx context.Context) (*Info,
	error) {

	panic("GetInfo: unexpected call")
}

func (m *refMockClient) ListAssetRecords(ctx context.Context,
	req *ListAssetsRequest) ([]*AssetRecord, error) {

	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).([]*AssetRecord), args.Error(1)
}

func (m *refMockClient) ListBalances(ctx context.Context,
	req *ListBalancesRequest) (*ListBalancesResponse,
	error) {

	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*ListBalancesResponse), args.Error(1)
}

func (m *refMockClient) ListTransfers(ctx context.Context,
	req *ListTransfersRequest) ([]*AssetTransfer,
	error) {

	panic("ListTransfers: unexpected call")
}

func (m *refMockClient) SendAsset(ctx context.Context,
	req *SendAssetRequest) (*AssetTransfer, error) {

	panic("SendAsset: unexpected call")
}

func (m *refMockClient) NewAddr(ctx context.Context,
	req *NewAddressRequest) (*Address, error) {

	args := m.Called(ctx, req)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*Address), args.Error(1)
}

func (m *refMockClient) DecodeAddr(ctx context.Context,
	addr string) (*Address, error) {

	panic("DecodeAddr: unexpected call")
}

func (m *refMockClient) QueryAddrs(ctx context.Context,
	query *AddressQuery) ([]*Address, error) {

	panic("QueryAddrs: unexpected call")
}

func (m *refMockClient) AddrReceives(ctx context.Context,
	query *AddressReceivesQuery) ([]*AddressEvent,
	error) {

	panic("AddrReceives: unexpected call")
}

// --- ProofClient ---

func (m *refMockClient) ExportProof(ctx context.Context,
	ref AssetRef, scriptKey PubKey,
	outpoint *Outpoint) (*ProofFile, error) {

	panic("ExportProof: unexpected call")
}

func (m *refMockClient) UnpackProofFile(ctx context.Context,
	rawProofFile []byte) ([][]byte, error) {

	panic("UnpackProofFile: unexpected call")
}

func (m *refMockClient) DecodeProof(ctx context.Context,
	rawProof []byte) (*DecodedProof, error) {

	panic("DecodeProof: unexpected call")
}

func (m *refMockClient) RegisterTransfer(ctx context.Context,
	assetRef AssetRef, scriptKey PubKey,
	outpoint Outpoint) (*RegisteredAsset, error) {

	panic("RegisterTransfer: unexpected call")
}

// --- WalletKitClient ---

func (m *refMockClient) CustomAnchorCapabilities(
	ctx context.Context) (*CustomAnchorCapabilities, error) {

	panic("CustomAnchorCapabilities: unexpected call")
}

func (m *refMockClient) DeriveScriptKey(
	ctx context.Context) (*ScriptKey, error) {

	panic("DeriveScriptKey: unexpected call")
}

func (m *refMockClient) DeriveInternalKey(
	ctx context.Context) (*InternalKey, error) {

	panic("DeriveInternalKey: unexpected call")
}

func (m *refMockClient) FundTransfer(ctx context.Context,
	recipients []Recipient,
	inputs []PrevID) (*FundedTransfer, error) {

	panic("FundTransfer: unexpected call")
}

func (m *refMockClient) FundInteractivePsbt(ctx context.Context,
	psbt []byte) (*FundedTransfer, error) {

	panic("FundInteractivePsbt: unexpected call")
}

func (m *refMockClient) SignVirtualPsbt(ctx context.Context,
	fundedPsbt []byte) ([]byte, error) {

	panic("SignVirtualPsbt: unexpected call")
}

func (m *refMockClient) CommitVirtualPsbts(ctx context.Context,
	virtualPsbts [][]byte, passivePsbts [][]byte,
	feeRate FeeRate) (*CommittedTransfer, error) {

	panic("CommitVirtualPsbts: unexpected call")
}

func (m *refMockClient) AnchorVirtualPsbts(ctx context.Context,
	signedPsbts [][]byte) (*AssetTransfer, error) {

	panic("AnchorVirtualPsbts: unexpected call")
}

func (m *refMockClient) PublishAndLogTransfer(ctx context.Context,
	anchorPsbt []byte, virtualPsbts [][]byte, passivePsbts [][]byte,
	skipAnchorTxBroadcast bool) (*AssetPacket, error) {

	panic("PublishAndLogTransfer: unexpected call")
}

// --- UniverseClient ---

func (m *refMockClient) InsertProof(ctx context.Context,
	rawProof []byte, decoded *DecodedProof) error {

	panic("InsertProof: unexpected call")
}

// --- MintClient ---

func (m *refMockClient) MintAsset(ctx context.Context,
	req *MintAssetRequest) (*MintingBatch, error) {

	panic("MintAsset: unexpected call")
}

func (m *refMockClient) MintIssuance(ctx context.Context,
	req *MintIssuanceRequest) (*MintingBatch, error) {

	panic("MintIssuance: unexpected call")
}

func (m *refMockClient) FundBatch(ctx context.Context,
	req *FundBatchRequest) (*VerboseMintingBatch,
	error) {

	panic("FundBatch: unexpected call")
}

func (m *refMockClient) SealBatch(ctx context.Context,
	req *SealBatchRequest) (*MintingBatch, error) {

	panic("SealBatch: unexpected call")
}

func (m *refMockClient) FinalizeBatch(ctx context.Context,
	req *FinalizeBatchRequest) (*MintingBatch, error) {

	panic("FinalizeBatch: unexpected call")
}

func (m *refMockClient) CancelBatch(
	ctx context.Context) (*CancelBatchResponse, error) {

	panic("CancelBatch: unexpected call")
}

func (m *refMockClient) ListBatches(ctx context.Context,
	req *ListBatchesRequest) ([]*VerboseMintingBatch,
	error) {

	panic("ListBatches: unexpected call")
}

// --- WalletClient extras (PR #30 additions) ---

func (m *refMockClient) ListUtxos(_ context.Context,
	_ *ListUtxosRequest) (
	map[string]*ManagedUtxo, error) {

	return nil, nil
}

func (m *refMockClient) ListAssetGroups(
	_ context.Context) ([]AssetGroupRecord, error) {

	return nil, nil
}

func (m *refMockClient) BurnAsset(_ context.Context,
	_ *BurnAssetRequest) (
	*BurnAssetResponse, error) {

	return nil, nil
}

func (m *refMockClient) ListBurns(_ context.Context,
	_ *ListBurnsRequest) ([]*BurnRecord, error) {

	return nil, nil
}

func (m *refMockClient) FetchAssetMeta(_ context.Context,
	_ *FetchAssetMetaRequest) (
	*AssetMeta, error) {

	return nil, nil
}

func (m *refMockClient) VerifyProof(_ context.Context,
	_ []byte) (*VerifyProofResponse, error) {

	return nil, nil
}

// --- WalletKitClient extras (PR #30 additions) ---

func (m *refMockClient) QueryInternalKey(_ context.Context,
	_ []byte) (*KeyDescriptor, error) {

	return nil, nil
}

func (m *refMockClient) QueryScriptKey(_ context.Context,
	_ []byte) (*ScriptKey, error) {

	return nil, nil
}

func (m *refMockClient) ProveAssetOwnership(_ context.Context,
	_ *ProveOwnershipRequest) (
	*OwnershipProof, error) {

	return nil, nil
}

func (m *refMockClient) VerifyAssetOwnership(_ context.Context,
	_ *VerifyOwnershipRequest) (
	*VerifyOwnershipResponse, error) {

	return nil, nil
}

func (m *refMockClient) RemoveUTXOLease(_ context.Context,
	_ Outpoint) error {

	return nil
}

func (m *refMockClient) DeclareScriptKey(_ context.Context,
	_ *DeclareScriptKeyRequest) (
	*ScriptKey, error) {

	return nil, nil
}

func (m *refMockClient) ExportBackup(_ context.Context,
	_ BackupMode) ([]byte, error) {

	return nil, nil
}

func (m *refMockClient) ImportBackup(_ context.Context,
	_ []byte) (uint32, error) {

	return 0, nil
}

// --- UniverseClient (PR #31 additions) ---

func (m *refMockClient) AssetRoots(_ context.Context,
	_ *AssetRootRequest) (
	map[string]*UniverseRoot, error) {

	return nil, nil
}

func (m *refMockClient) QueryAssetRoots(ctx context.Context,
	id *UniverseID) (*QueryRootResponse, error) {

	args := m.Called(ctx, id)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}

	return args.Get(0).(*QueryRootResponse), args.Error(1)
}

func (m *refMockClient) DeleteAssetRoot(_ context.Context,
	_ *UniverseID) error {

	return nil
}

func (m *refMockClient) AssetLeafKeys(_ context.Context,
	_ *AssetLeafKeysRequest) (
	[]AssetLeafKey, error) {

	return nil, nil
}

func (m *refMockClient) AssetLeaves(_ context.Context,
	_ *UniverseID) ([]AssetLeaf, error) {

	return nil, nil
}

func (m *refMockClient) QueryProof(_ context.Context,
	_ *UniverseKey) (*AssetProofResponse,
	error) {

	return nil, nil
}

func (m *refMockClient) UniverseStats(
	_ context.Context) (*UniverseStats, error) {

	return nil, nil
}

func (m *refMockClient) QueryAssetStats(_ context.Context,
	_ *AssetStatsQuery) (
	[]AssetStatsSnapshot, error) {

	return nil, nil
}

func (m *refMockClient) QueryEvents(_ context.Context,
	_ *QueryEventsRequest) (
	[]GroupedUniverseEvents, error) {

	return nil, nil
}

func (m *refMockClient) ListFederationServers(
	_ context.Context) ([]FederationServer, error) {

	return nil, nil
}

func (m *refMockClient) AddFederationServer(_ context.Context,
	_ []FederationServer) error {

	return nil
}

func (m *refMockClient) DeleteFederationServer(_ context.Context,
	_ []FederationServer) error {

	return nil
}

func (m *refMockClient) SetFederationSyncConfig(_ context.Context,
	_ []GlobalFederationSyncConfig,
	_ []AssetFederationSyncConfig) error {

	return nil
}

func (m *refMockClient) QueryFederationSyncConfig(_ context.Context,
	_ []UniverseID) (*FederationSyncConfig,
	error) {

	return nil, nil
}

func (m *refMockClient) Info(
	_ context.Context) (*UniverseInfo, error) {

	return nil, nil
}

func (m *refMockClient) SyncUniverse(_ context.Context,
	_ *SyncRequest) ([]SyncedUniverse, error) {

	return nil, nil
}

func (m *refMockClient) SubscribeReceiveEvents(_ context.Context,
	_ *SubscribeReceiveEventsRequest) (
	<-chan *ReceiveEventRecord, <-chan error, error) {

	panic("SubscribeReceiveEvents: unexpected call")
}

func (m *refMockClient) SubscribeSendEvents(_ context.Context,
	_ *SubscribeSendEventsRequest) (
	<-chan *SendEventRecord, <-chan error, error) {

	panic("SubscribeSendEvents: unexpected call")
}

func (m *refMockClient) SubscribeMintEvents(_ context.Context,
	_ *SubscribeMintEventsRequest) (
	<-chan *MintEvent, <-chan error, error) {

	panic("SubscribeMintEvents: unexpected call")
}

func (m *refMockClient) Close() error {
	return nil
}

// --- Test helpers ---

func testRefGroupKey(t *testing.T) PubKey {
	t.Helper()

	b, err := hex.DecodeString(
		"02a1633cafcc01ebfb6d78e39f687a1f0995c62fc95f51ead10" +
			"a02ee0be551b5dc",
	)
	require.NoError(t, err)

	key, err := ParsePubKey(b)
	require.NoError(t, err)

	return key
}

func testRefAssetID() AssetID {
	var id AssetID
	for i := range id {
		id[i] = byte(i)
	}

	return id
}

// --- NewReceiveAddress tests ---

func TestNewReceiveAddress_Fungible(t *testing.T) {
	mc := new(refMockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	groupKey := testRefGroupKey(t)
	ref := AssetRefFromGroupKey(groupKey)

	expectedAddr := &Address{
		Encoded:        "taprt1q_fungible_test",
		AssetRef:       ref,
		AddressVersion: AddressVersionV2,
	}

	v2 := AddressVersionV2
	mc.On("NewAddr", ctx, &NewAddressRequest{
		AssetRef:       ref,
		AddressVersion: &v2,
	}).Return(expectedAddr, nil)

	addr, err := w.NewReceiveAddress(ctx, ref)
	require.NoError(t, err)
	require.Equal(t, expectedAddr, addr)

	mc.AssertExpectations(t)
}

func TestNewReceiveAddress_Collectible(t *testing.T) {
	mc := new(refMockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	assetID := testRefAssetID()
	ref := AssetRefFromAssetID(assetID)

	expectedAddr := &Address{
		Encoded:        "taprt1q_collectible_test",
		AssetRef:       ref,
		AddressVersion: AddressVersionV2,
	}

	v2 := AddressVersionV2
	mc.On("NewAddr", ctx, &NewAddressRequest{
		AssetRef:       ref,
		AddressVersion: &v2,
	}).Return(expectedAddr, nil)

	addr, err := w.NewReceiveAddress(ctx, ref)
	require.NoError(t, err)
	require.Equal(t, expectedAddr, addr)

	mc.AssertExpectations(t)
}

func TestNewReceiveAddress_Error(t *testing.T) {
	mc := new(refMockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	ref := AssetRefFromGroupKey(testRefGroupKey(t))

	mc.On("NewAddr", ctx, mock.Anything).Return(
		nil, fmt.Errorf("connection refused"),
	)

	_, err := w.NewReceiveAddress(ctx, ref)
	require.Error(t, err)
	require.Contains(t, err.Error(), "connection refused")

	mc.AssertExpectations(t)
}

// --- GetBalance tests ---

func TestGetBalance_Fungible(t *testing.T) {
	mc := new(refMockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	groupKey := testRefGroupKey(t)
	ref := AssetRefFromGroupKey(groupKey)

	mc.On("ListBalances", ctx, &ListBalancesRequest{
		AssetRef: &ref,
	}).Return(&ListBalancesResponse{
		Balances: map[string]*Balance{
			ref.String(): {
				AssetRef: ref,
				Balance:  42000,
			},
		},
	}, nil)

	balance, err := w.GetBalance(ctx, ref)
	require.NoError(t, err)
	require.Equal(t, uint64(42000), balance)

	mc.AssertExpectations(t)
}

func TestGetBalance_Collectible(t *testing.T) {
	mc := new(refMockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	assetID := testRefAssetID()
	ref := AssetRefFromAssetID(assetID)

	mc.On("ListBalances", ctx, &ListBalancesRequest{
		AssetRef: &ref,
	}).Return(&ListBalancesResponse{
		Balances: map[string]*Balance{
			ref.String(): {
				AssetRef: ref,
				Balance:  1,
			},
		},
	}, nil)

	balance, err := w.GetBalance(ctx, ref)
	require.NoError(t, err)
	require.Equal(t, uint64(1), balance)

	mc.AssertExpectations(t)
}

// expectEmptyBalances stubs a ListBalances call that returns no entries
// for the given ref — the cold path all subsequent probes are keyed off.
func expectEmptyBalances(mc *refMockClient, ctx context.Context,
	ref AssetRef) {

	mc.On("ListBalances", ctx, &ListBalancesRequest{
		AssetRef: &ref,
	}).Return(&ListBalancesResponse{
		Balances: map[string]*Balance{},
	}, nil)
}

func TestListBalances_ReturnsAllBalances(t *testing.T) {
	mc := new(refMockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	first := AssetRefFromGroupKey(testRefGroupKey(t))
	second := AssetRefFromAssetID(testRefAssetID())

	mc.On("ListBalances", ctx, (*ListBalancesRequest)(nil)).Return(
		&ListBalancesResponse{
			Balances: map[string]*Balance{
				first.String(): {
					AssetRef: first,
					Balance:  21,
				},
				second.String(): {
					AssetRef: second,
					Balance:  1,
				},
			},
			UnconfirmedTransfers: 2,
		}, nil,
	)

	resp, err := w.ListBalances(ctx, nil)
	require.NoError(t, err)
	require.Len(t, resp.Balances, 2)
	require.Equal(t, uint64(21), resp.Balances[first.String()].Balance)
	require.Equal(t, uint64(1), resp.Balances[second.String()].Balance)
	require.Equal(t, uint64(2), resp.UnconfirmedTransfers)

	mc.AssertExpectations(t)
}

func TestListBalances_NormalizesEmptyResponse(t *testing.T) {
	mc := new(refMockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	mc.On("ListBalances", ctx, (*ListBalancesRequest)(nil)).Return(
		&ListBalancesResponse{}, nil,
	)

	resp, err := w.ListBalances(ctx, nil)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.NotNil(t, resp.Balances)
	require.Empty(t, resp.Balances)

	mc.AssertExpectations(t)
}

func TestListBalances_FilterKnownBalance(t *testing.T) {
	mc := new(refMockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	ref := AssetRefFromGroupKey(testRefGroupKey(t))
	req := &ListBalancesRequest{AssetRef: &ref}

	mc.On("ListBalances", ctx, req).Return(&ListBalancesResponse{
		Balances: map[string]*Balance{
			ref.String(): {
				AssetRef: ref,
				Balance:  42000,
			},
		},
	}, nil)

	resp, err := w.ListBalances(ctx, req)
	require.NoError(t, err)
	require.Len(t, resp.Balances, 1)
	require.Equal(t, uint64(42000), resp.Balances[ref.String()].Balance)

	mc.AssertExpectations(t)
}

func TestListBalances_FilterKnownZero(t *testing.T) {
	mc := new(refMockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	ref := AssetRefFromGroupKey(testRefGroupKey(t))
	req := &ListBalancesRequest{AssetRef: &ref}

	expectEmptyBalances(mc, ctx, ref)
	mc.On("QueryAssetRoots", ctx, issuanceRootID(ref)).Return(
		&QueryRootResponse{
			IssuanceRoot: &UniverseRoot{
				ID:        *issuanceRootID(ref),
				AssetName: "known-zero",
			},
		}, nil,
	)

	resp, err := w.ListBalances(ctx, req)
	require.NoError(t, err)
	require.Len(t, resp.Balances, 1)
	require.Equal(t, ref, resp.Balances[ref.String()].AssetRef)
	require.Zero(t, resp.Balances[ref.String()].Balance)

	mc.AssertExpectations(t)
}

func TestListBalances_UnknownAsset(t *testing.T) {
	mc := new(refMockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	ref := AssetRefFromGroupKey(testRefGroupKey(t))
	req := &ListBalancesRequest{AssetRef: &ref}

	expectEmptyBalances(mc, ctx, ref)
	mc.On("QueryAssetRoots", ctx, issuanceRootID(ref)).Return(
		&QueryRootResponse{}, nil,
	)

	resp, err := w.ListBalances(ctx, req)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrAssetUnknown)
	require.Contains(t, err.Error(), ref.String())
	require.Nil(t, resp)

	mc.AssertExpectations(t)
}

// issuanceRootID is the UniverseID the cold-path probe consults.
func issuanceRootID(ref AssetRef) *UniverseID {
	return &UniverseID{
		AssetRef:  ref,
		ProofType: ProofTypeIssuance,
	}
}

// TestGetBalance_ZeroWhenUniverseKnown covers the motivating scenario
// from issue #69: a group ref bootstrapped via SyncUniverse but with no
// received units yet. The issuance root resolves the ref as "known".
func TestGetBalance_ZeroWhenUniverseKnown(t *testing.T) {
	mc := new(refMockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	ref := AssetRefFromGroupKey(testRefGroupKey(t))

	expectEmptyBalances(mc, ctx, ref)
	mc.On("QueryAssetRoots", ctx, issuanceRootID(ref)).Return(
		&QueryRootResponse{
			IssuanceRoot: &UniverseRoot{
				ID:        *issuanceRootID(ref),
				AssetName: "bootstrapped",
			},
		}, nil,
	)

	balance, err := w.GetBalance(ctx, ref)
	require.NoError(t, err)
	require.Zero(t, balance)

	mc.AssertExpectations(t)
}

// TestGetBalance_ZeroWhenUniverseKnowsViaTransfer covers the case
// where the wallet has seen transfer proofs for the ref but no
// issuance root is locally available.
func TestGetBalance_ZeroWhenUniverseKnowsViaTransfer(t *testing.T) {
	mc := new(refMockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	ref := AssetRefFromAssetID(testRefAssetID())

	expectEmptyBalances(mc, ctx, ref)
	mc.On("QueryAssetRoots", ctx, issuanceRootID(ref)).Return(
		&QueryRootResponse{
			TransferRoot: &UniverseRoot{
				ID:        *issuanceRootID(ref),
				AssetName: "transfer-only",
			},
		}, nil,
	)

	balance, err := w.GetBalance(ctx, ref)
	require.NoError(t, err)
	require.Zero(t, balance)

	mc.AssertExpectations(t)
}

// TestGetBalance_UnknownAsset covers a ref the wallet has truly never
// seen: no balance entry and no universe root. The error chain carries
// the ref for debugging.
func TestGetBalance_UnknownAsset(t *testing.T) {
	mc := new(refMockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	ref := AssetRefFromGroupKey(testRefGroupKey(t))

	expectEmptyBalances(mc, ctx, ref)
	mc.On("QueryAssetRoots", ctx, issuanceRootID(ref)).Return(
		&QueryRootResponse{}, nil,
	)

	balance, err := w.GetBalance(ctx, ref)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrAssetUnknown)
	require.Contains(t, err.Error(), ref.String())
	require.Zero(t, balance)

	mc.AssertExpectations(t)
}

func TestGetBalance_ListBalancesError(t *testing.T) {
	mc := new(refMockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	ref := AssetRefFromAssetID(testRefAssetID())

	mc.On("ListBalances", ctx, mock.Anything).Return(
		nil, context.DeadlineExceeded,
	)

	_, err := w.GetBalance(ctx, ref)
	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	mc.AssertExpectations(t)
}

func TestGetBalance_QueryAssetRootsError(t *testing.T) {
	mc := new(refMockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	ref := AssetRefFromAssetID(testRefAssetID())

	expectEmptyBalances(mc, ctx, ref)
	mc.On("QueryAssetRoots", ctx, issuanceRootID(ref)).Return(
		nil, context.DeadlineExceeded,
	)

	_, err := w.GetBalance(ctx, ref)
	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	mc.AssertExpectations(t)
}
