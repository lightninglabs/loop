package grpc

import (
	"context"
	"fmt"
	"time"

	tapsdk "github.com/lightninglabs/loop/tapsdk"
	"github.com/lightninglabs/loop/tapsdk/macaroon"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightninglabs/taproot-assets/taprpc/mintrpc"
	"google.golang.org/grpc"
)

// mintClient is a wrapper around the mintrpc.MintClient.
type mintClient struct {
	client  mintrpc.MintClient
	timeout time.Duration
	mintMac macaroon.SerializedMacaroon
}

// NewMintClient creates a new Mint client.
func NewMintClient(conn grpc.ClientConnInterface, timeout time.Duration,
	mintMac macaroon.SerializedMacaroon) *mintClient {

	return &mintClient{
		client:  mintrpc.NewMintClient(conn),
		timeout: timeout,
		mintMac: mintMac,
	}
}

// mintAssetRequest is the low-level RPC request shape for MintAsset.
// It is internal to this package; callers use MintAssetRequest or
// MintIssuanceRequest instead.
type mintAssetRequest struct {
	asset         *mintAsset
	shortResponse bool
}

// mintAsset describes an asset to add to a minting batch.
type mintAsset struct {
	tapsdk.PendingMintAsset

	groupedAsset            bool
	decimalDisplay          uint32
	externalGroupKey        *tapsdk.ExternalKey
	enableSupplyCommitments bool
}

// mintAssetRPC adds an asset to the pending minting batch using the
// internal request type.
func (m *mintClient) mintAssetRPC(ctx context.Context,
	req *mintAssetRequest) (*tapsdk.MintingBatch, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	rpcCtx = m.mintMac.WithMacaroonAuth(rpcCtx)

	rpcReq := marshalMintAssetRequest(req)
	resp, err := m.client.MintAsset(rpcCtx, rpcReq)
	if err != nil {
		return nil, err
	}

	return unmarshalMintingBatch(resp.PendingBatch)
}

// MintAsset adds a brand-new asset to the pending minting batch.
func (m *mintClient) MintAsset(ctx context.Context,
	req *tapsdk.MintAssetRequest) (*tapsdk.MintingBatch,
	error) {

	return m.mintAssetRPC(
		ctx, mintAssetRequestFromMintAsset(req),
	)
}

// MintIssuance adds an additional issuance to the pending
// minting batch.
func (m *mintClient) MintIssuance(ctx context.Context,
	req *tapsdk.MintIssuanceRequest) (*tapsdk.MintingBatch,
	error) {

	mintReq, err := mintAssetRequestFromMintIssuance(req)
	if err != nil {
		return nil, err
	}

	return m.mintAssetRPC(ctx, mintReq)
}

// FundBatch funds the current pending minting batch.
func (m *mintClient) FundBatch(ctx context.Context,
	req *tapsdk.FundBatchRequest) (*tapsdk.VerboseMintingBatch, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	rpcCtx = m.mintMac.WithMacaroonAuth(rpcCtx)

	rpcReq, err := marshalFundBatchRequest(req)
	if err != nil {
		return nil, err
	}

	resp, err := m.client.FundBatch(rpcCtx, rpcReq)
	if err != nil {
		return nil, err
	}

	return unmarshalVerboseMintingBatch(resp.Batch)
}

// SealBatch seals the current funded mint batch.
func (m *mintClient) SealBatch(ctx context.Context,
	req *tapsdk.SealBatchRequest) (*tapsdk.MintingBatch, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	rpcCtx = m.mintMac.WithMacaroonAuth(rpcCtx)

	rpcReq, err := marshalSealBatchRequest(req)
	if err != nil {
		return nil, err
	}

	resp, err := m.client.SealBatch(rpcCtx, rpcReq)
	if err != nil {
		return nil, err
	}

	return unmarshalMintingBatch(resp.Batch)
}

// FinalizeBatch finalizes the current pending mint batch.
func (m *mintClient) FinalizeBatch(ctx context.Context,
	req *tapsdk.FinalizeBatchRequest) (*tapsdk.MintingBatch, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	rpcCtx = m.mintMac.WithMacaroonAuth(rpcCtx)

	rpcReq, err := marshalFinalizeBatchRequest(req)
	if err != nil {
		return nil, err
	}

	resp, err := m.client.FinalizeBatch(rpcCtx, rpcReq)
	if err != nil {
		return nil, err
	}

	return unmarshalMintingBatch(resp.Batch)
}

// CancelBatch cancels the current mint batch.
func (m *mintClient) CancelBatch(ctx context.Context) (
	*tapsdk.CancelBatchResponse, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	rpcCtx = m.mintMac.WithMacaroonAuth(rpcCtx)

	resp, err := m.client.CancelBatch(rpcCtx, &mintrpc.CancelBatchRequest{})
	if err != nil {
		return nil, err
	}

	batchKey, err := tapsdk.ParsePubKey(resp.BatchKey)
	if err != nil {
		return nil, fmt.Errorf("invalid batch key: %w", err)
	}

	return &tapsdk.CancelBatchResponse{BatchKey: batchKey}, nil
}

// ListBatches lists mint batches known to the daemon.
func (m *mintClient) ListBatches(ctx context.Context,
	req *tapsdk.ListBatchesRequest) ([]*tapsdk.VerboseMintingBatch, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	rpcCtx = m.mintMac.WithMacaroonAuth(rpcCtx)

	rpcReq := marshalListBatchesRequest(req)
	resp, err := m.client.ListBatches(rpcCtx, rpcReq)
	if err != nil {
		return nil, err
	}

	batches := make([]*tapsdk.VerboseMintingBatch, 0, len(resp.Batches))
	for _, rpcBatch := range resp.Batches {
		batch, err := unmarshalVerboseMintingBatch(rpcBatch)
		if err != nil {
			return nil, err
		}

		batches = append(batches, batch)
	}

	return batches, nil
}

func mintAssetRequestFromMintAsset(
	req *tapsdk.MintAssetRequest) *mintAssetRequest {

	if req == nil {
		return nil
	}

	asset := req.Asset
	if asset == nil {
		return &mintAssetRequest{
			shortResponse: req.ShortResponse,
		}
	}

	return &mintAssetRequest{
		shortResponse: req.ShortResponse,
		asset: &mintAsset{
			AssetVersion:            asset.AssetVersion,
			AssetType:               asset.AssetType,
			Name:                    asset.Name,
			AssetMeta:               asset.AssetMeta,
			Amount:                  asset.InitialSupply,
			NewGroupedAsset:         asset.AllowIssuance,
			GroupInternalKey:        asset.GroupInternalKey,
			GroupTapscriptRoot:      asset.GroupTapscriptRoot,
			ScriptKey:               asset.ScriptKey,
			decimalDisplay:          asset.DecimalDisplay,
			externalGroupKey:        asset.ExternalGroupKey,
			enableSupplyCommitments: asset.EnableSupplyCommitments,
		},
	}
}

func mintAssetRequestFromMintIssuance(
	req *tapsdk.MintIssuanceRequest) (*mintAssetRequest,
	error) {

	if req == nil {
		return nil, nil
	}

	issuance := req.Issuance
	if issuance == nil {
		return &mintAssetRequest{
			shortResponse: req.ShortResponse,
		}, nil
	}

	groupKey, ok := issuance.AssetRef.GroupKey()
	if !ok {
		return nil, fmt.Errorf("asset ref must resolve " +
			"to a group key to mint an issuance")
	}

	return &mintAssetRequest{
		shortResponse: req.ShortResponse,
		asset: &mintAsset{
			AssetVersion:     issuance.AssetVersion,
			AssetType:        issuance.AssetType,
			Name:             issuance.Name,
			AssetMeta:        issuance.AssetMeta,
			Amount:           issuance.Amount,
			GroupKey:         &groupKey,
			ScriptKey:        issuance.ScriptKey,
			groupedAsset:     true,
			decimalDisplay:   issuance.DecimalDisplay,
			externalGroupKey: issuance.ExternalGroupKey,
		},
	}, nil
}

func marshalMintAssetRequest(
	req *mintAssetRequest) *mintrpc.MintAssetRequest {

	if req == nil {
		return &mintrpc.MintAssetRequest{}
	}

	return &mintrpc.MintAssetRequest{
		Asset:         marshalMintAsset(req.asset),
		ShortResponse: req.shortResponse,
	}
}

func marshalMintAsset(asset *mintAsset) *mintrpc.MintAsset {
	if asset == nil {
		return nil
	}

	rpcAsset := &mintrpc.MintAsset{
		AssetVersion:       taprpc.AssetVersion(asset.AssetVersion),
		AssetType:          taprpc.AssetType(asset.AssetType),
		Name:               asset.Name,
		Amount:             asset.Amount,
		NewGroupedAsset:    asset.NewGroupedAsset,
		GroupedAsset:       asset.groupedAsset,
		GroupAnchor:        asset.GroupAnchor,
		GroupTapscriptRoot: asset.GroupTapscriptRoot,
		ScriptKey:          marshalScriptKey(asset.ScriptKey),
		DecimalDisplay:     asset.decimalDisplay,
		ExternalGroupKey: marshalExternalKey(
			asset.externalGroupKey,
		),
		EnableSupplyCommitments: asset.enableSupplyCommitments,
		GroupInternalKey: marshalKeyDescriptor(
			asset.GroupInternalKey,
		),
		AssetMeta: marshalAssetMeta(asset.AssetMeta),
	}

	if asset.GroupKey != nil {
		rpcAsset.GroupKey = asset.GroupKey[:]
	}

	return rpcAsset
}

func marshalFundBatchRequest(
	req *tapsdk.FundBatchRequest) (*mintrpc.FundBatchRequest, error) {

	if req == nil {
		return &mintrpc.FundBatchRequest{}, nil
	}

	feeRate, err := feeRateSatPerKWeight(req.FeeRate)
	if err != nil {
		return nil, err
	}

	rpcReq := &mintrpc.FundBatchRequest{
		ShortResponse: req.ShortResponse,
		FeeRate:       feeRate,
	}

	err = marshalBatchSibling(req.BatchSibling, func(fullTree *taprpc.
		TapscriptFullTree) {

		rpcReq.BatchSibling = &mintrpc.FundBatchRequest_FullTree{
			FullTree: fullTree,
		}
	}, func(branch *taprpc.TapBranch) {
		rpcReq.BatchSibling = &mintrpc.FundBatchRequest_Branch{
			Branch: branch,
		}
	})
	if err != nil {
		return nil, err
	}

	return rpcReq, nil
}

func marshalSealBatchRequest(
	req *tapsdk.SealBatchRequest) (*mintrpc.SealBatchRequest, error) {

	if req == nil {
		return &mintrpc.SealBatchRequest{}, nil
	}

	if len(req.GroupWitnesses) != 0 && len(req.SignedGroupVirtualPSBTs) != 0 {
		return nil, fmt.Errorf("seal batch request must choose one witness input")
	}

	rpcReq := &mintrpc.SealBatchRequest{
		ShortResponse:           req.ShortResponse,
		SignedGroupVirtualPsbts: req.SignedGroupVirtualPSBTs,
	}

	for _, witness := range req.GroupWitnesses {
		rpcReq.GroupWitnesses = append(rpcReq.GroupWitnesses,
			&taprpc.GroupWitness{
				GenesisId: witness.GenesisID[:],
				Witness:   witness.Witness,
			},
		)
	}

	return rpcReq, nil
}

func marshalFinalizeBatchRequest(
	req *tapsdk.FinalizeBatchRequest) (*mintrpc.FinalizeBatchRequest, error) {

	if req == nil {
		return &mintrpc.FinalizeBatchRequest{}, nil
	}

	feeRate, err := feeRateSatPerKWeight(req.FeeRate)
	if err != nil {
		return nil, err
	}

	rpcReq := &mintrpc.FinalizeBatchRequest{
		ShortResponse: req.ShortResponse,
		FeeRate:       feeRate,
	}

	err = marshalBatchSibling(req.BatchSibling, func(fullTree *taprpc.
		TapscriptFullTree) {

		rpcReq.BatchSibling = &mintrpc.FinalizeBatchRequest_FullTree{
			FullTree: fullTree,
		}
	}, func(branch *taprpc.TapBranch) {
		rpcReq.BatchSibling = &mintrpc.FinalizeBatchRequest_Branch{
			Branch: branch,
		}
	})
	if err != nil {
		return nil, err
	}

	return rpcReq, nil
}

func marshalListBatchesRequest(
	req *tapsdk.ListBatchesRequest) *mintrpc.ListBatchRequest {

	if req == nil {
		return &mintrpc.ListBatchRequest{}
	}

	rpcReq := &mintrpc.ListBatchRequest{Verbose: req.Verbose}
	if req.BatchKey != nil {
		rpcReq.Filter = &mintrpc.ListBatchRequest_BatchKey{
			BatchKey: req.BatchKey[:],
		}
	}

	return rpcReq
}

func marshalBatchSibling(batchSibling *tapsdk.BatchSibling,
	setFullTree func(*taprpc.TapscriptFullTree),
	setBranch func(*taprpc.TapBranch)) error {

	if batchSibling == nil {
		return nil
	}

	if batchSibling.FullTree != nil && batchSibling.Branch != nil {
		return fmt.Errorf("batch sibling must set exactly one variant")
	}

	if batchSibling.FullTree != nil {
		leaves := make(
			[]*taprpc.TapLeaf, 0, len(batchSibling.FullTree.Leaves),
		)
		for _, leaf := range batchSibling.FullTree.Leaves {
			leaves = append(leaves, &taprpc.TapLeaf{Script: leaf.Script})
		}

		setFullTree(&taprpc.TapscriptFullTree{AllLeaves: leaves})
		return nil
	}

	if batchSibling.Branch != nil {
		setBranch(&taprpc.TapBranch{
			LeftTaphash:  batchSibling.Branch.LeftTapHash[:],
			RightTaphash: batchSibling.Branch.RightTapHash[:],
		})
	}

	return nil
}

func marshalAssetMeta(meta *tapsdk.AssetMeta) *taprpc.AssetMeta {
	if meta == nil {
		return nil
	}

	rpcMeta := &taprpc.AssetMeta{
		Data: meta.Data,
		Type: taprpc.AssetMetaType(meta.Type),
	}

	if meta.MetaHash != (tapsdk.Hash{}) {
		rpcMeta.MetaHash = meta.MetaHash[:]
	}

	return rpcMeta
}

func marshalScriptKey(key *tapsdk.ScriptKey) *taprpc.ScriptKey {
	if key == nil {
		return nil
	}

	rpcKey := &taprpc.ScriptKey{
		PubKey:   key.PubKey[:],
		TapTweak: key.TapTweak,
	}

	if key.KeyDesc != (tapsdk.KeyDescriptor{}) {
		rpcKey.KeyDesc = marshalKeyDescriptor(&key.KeyDesc)
	}

	return rpcKey
}

func marshalKeyDescriptor(desc *tapsdk.KeyDescriptor) *taprpc.KeyDescriptor {
	if desc == nil {
		return nil
	}

	return &taprpc.KeyDescriptor{
		RawKeyBytes: desc.RawKeyBytes[:],
		KeyLoc: &taprpc.KeyLocator{
			KeyFamily: int32(desc.KeyLocator.Family),
			KeyIndex:  int32(desc.KeyLocator.Index),
		},
	}
}

func marshalExternalKey(key *tapsdk.ExternalKey) *taprpc.ExternalKey {
	if key == nil {
		return nil
	}

	return &taprpc.ExternalKey{
		Xpub:              key.XPub,
		MasterFingerprint: key.MasterFingerprint[:],
		DerivationPath:    key.DerivationPath,
	}
}

func unmarshalVerboseMintingBatch(
	rpcBatch *mintrpc.VerboseBatch) (*tapsdk.VerboseMintingBatch, error) {

	if rpcBatch == nil {
		return nil, fmt.Errorf("nil verbose minting batch")
	}

	batch, err := unmarshalMintingBatch(rpcBatch.Batch)
	if err != nil {
		return nil, err
	}

	verboseBatch := &tapsdk.VerboseMintingBatch{Batch: *batch}
	verboseBatch.UnsealedAssets = make(
		[]tapsdk.UnsealedMintAsset, 0, len(rpcBatch.UnsealedAssets),
	)
	for _, rpcAsset := range rpcBatch.UnsealedAssets {
		asset, err := unmarshalUnsealedMintAsset(rpcAsset)
		if err != nil {
			return nil, err
		}

		verboseBatch.UnsealedAssets = append(verboseBatch.UnsealedAssets,
			*asset)
	}

	return verboseBatch, nil
}

func unmarshalUnsealedMintAsset(
	rpcAsset *mintrpc.UnsealedAsset) (*tapsdk.UnsealedMintAsset, error) {

	if rpcAsset == nil {
		return nil, fmt.Errorf("nil unsealed mint asset")
	}

	asset := &tapsdk.UnsealedMintAsset{
		GroupVirtualPSBT: rpcAsset.GroupVirtualPsbt,
	}

	if rpcAsset.Asset != nil {
		pendingAsset, err := unmarshalPendingMintAsset(rpcAsset.Asset)
		if err != nil {
			return nil, err
		}

		asset.Asset = pendingAsset
	}

	if rpcAsset.GroupKeyRequest != nil {
		groupKeyRequest, err := unmarshalGroupKeyRequest(
			rpcAsset.GroupKeyRequest,
		)
		if err != nil {
			return nil, err
		}

		asset.GroupKeyRequest = groupKeyRequest
	}

	if rpcAsset.GroupVirtualTx != nil {
		groupVirtualTx, err := unmarshalGroupVirtualTx(
			rpcAsset.GroupVirtualTx,
		)
		if err != nil {
			return nil, err
		}

		asset.GroupVirtualTx = groupVirtualTx
	}

	return asset, nil
}

func unmarshalMintingBatch(
	rpcBatch *mintrpc.MintingBatch) (*tapsdk.MintingBatch, error) {

	if rpcBatch == nil {
		return nil, fmt.Errorf("nil minting batch")
	}

	batch := &tapsdk.MintingBatch{
		BatchTxid:  rpcBatch.BatchTxid,
		State:      tapsdk.BatchState(rpcBatch.State),
		CreatedAt:  rpcBatch.CreatedAt,
		HeightHint: rpcBatch.HeightHint,
		BatchPSBT:  rpcBatch.BatchPsbt,
	}

	if len(rpcBatch.BatchKey) != 0 {
		batchKey, err := tapsdk.ParsePubKey(rpcBatch.BatchKey)
		if err != nil {
			return nil, fmt.Errorf("invalid batch key: %w", err)
		}

		batch.BatchKey = batchKey
	}

	batch.Assets = make([]tapsdk.PendingMintAsset, 0, len(rpcBatch.Assets))
	for _, rpcAsset := range rpcBatch.Assets {
		asset, err := unmarshalPendingMintAsset(rpcAsset)
		if err != nil {
			return nil, err
		}

		batch.Assets = append(batch.Assets, *asset)
	}

	return batch, nil
}

func unmarshalPendingMintAsset(
	rpcAsset *mintrpc.PendingAsset) (*tapsdk.PendingMintAsset, error) {

	if rpcAsset == nil {
		return nil, fmt.Errorf("nil pending mint asset")
	}

	asset := &tapsdk.PendingMintAsset{
		AssetVersion:       tapsdk.AssetVersion(rpcAsset.AssetVersion),
		AssetType:          tapsdk.AssetType(rpcAsset.AssetType),
		Name:               rpcAsset.Name,
		Amount:             rpcAsset.Amount,
		NewGroupedAsset:    rpcAsset.NewGroupedAsset,
		GroupAnchor:        rpcAsset.GroupAnchor,
		GroupTapscriptRoot: rpcAsset.GroupTapscriptRoot,
	}

	if rpcAsset.AssetMeta != nil {
		meta, err := unmarshalAssetMeta(rpcAsset.AssetMeta)
		if err != nil {
			return nil, err
		}

		asset.AssetMeta = meta
	}

	if len(rpcAsset.GroupKey) != 0 {
		groupKey, err := tapsdk.ParsePubKey(rpcAsset.GroupKey)
		if err != nil {
			return nil, fmt.Errorf("invalid group key: %w", err)
		}

		asset.GroupKey = &groupKey
	}

	if rpcAsset.GroupInternalKey != nil {
		groupInternalKey, err := unmarshalKeyDescriptor(
			rpcAsset.GroupInternalKey,
		)
		if err != nil {
			return nil, fmt.Errorf("invalid group internal key: %w", err)
		}

		asset.GroupInternalKey = groupInternalKey
	}

	if rpcAsset.ScriptKey != nil {
		scriptKey, err := unmarshalScriptKey(rpcAsset.ScriptKey)
		if err != nil {
			return nil, fmt.Errorf("invalid script key: %w", err)
		}

		asset.ScriptKey = scriptKey
	}

	return asset, nil
}

func unmarshalAssetMeta(
	rpcMeta *taprpc.AssetMeta) (*tapsdk.AssetMeta, error) {

	if rpcMeta == nil {
		return nil, fmt.Errorf("nil asset meta")
	}

	metaHash, err := tapsdk.ParseHash(rpcMeta.MetaHash)
	if err != nil {
		return nil, fmt.Errorf("invalid asset meta hash: %w", err)
	}

	return &tapsdk.AssetMeta{
		Data:     rpcMeta.Data,
		Type:     tapsdk.AssetMetaType(rpcMeta.Type),
		MetaHash: metaHash,
	}, nil
}

func unmarshalGroupKeyRequest(
	rpcRequest *taprpc.GroupKeyRequest) (*tapsdk.GroupKeyRequest, error) {

	if rpcRequest == nil {
		return nil, fmt.Errorf("nil group key request")
	}

	request := &tapsdk.GroupKeyRequest{
		TapscriptRoot: rpcRequest.TapscriptRoot,
		NewAsset:      rpcRequest.NewAsset,
	}

	if rpcRequest.RawKey != nil {
		rawKey, err := unmarshalKeyDescriptor(rpcRequest.RawKey)
		if err != nil {
			return nil, fmt.Errorf("invalid raw key: %w", err)
		}

		request.RawKey = rawKey
	}

	if rpcRequest.AnchorGenesis != nil {
		anchorGenesis, err := unmarshalGenesisInfo(rpcRequest.AnchorGenesis)
		if err != nil {
			return nil, err
		}

		request.AnchorGenesis = anchorGenesis
	}

	if rpcRequest.ExternalKey != nil {
		request.ExternalKey = &tapsdk.ExternalKey{
			XPub:           rpcRequest.ExternalKey.Xpub,
			DerivationPath: rpcRequest.ExternalKey.DerivationPath,
		}
		copy(request.ExternalKey.MasterFingerprint[:],
			rpcRequest.ExternalKey.MasterFingerprint)
	}

	return request, nil
}

func unmarshalGenesisInfo(
	rpcGenesis *taprpc.GenesisInfo) (*tapsdk.GenesisInfo, error) {

	if rpcGenesis == nil {
		return nil, fmt.Errorf("nil genesis info")
	}

	metaHash, err := tapsdk.ParseHash(rpcGenesis.MetaHash)
	if err != nil {
		return nil, fmt.Errorf("invalid genesis meta hash: %w", err)
	}

	assetID, err := tapsdk.ParseAssetID(rpcGenesis.AssetId)
	if err != nil {
		return nil, fmt.Errorf("invalid genesis asset ID: %w", err)
	}

	return &tapsdk.GenesisInfo{
		GenesisPoint: rpcGenesis.GenesisPoint,
		Name:         rpcGenesis.Name,
		MetaHash:     metaHash,
		IssuanceID:   assetID,
		AssetType:    tapsdk.AssetType(rpcGenesis.AssetType),
		OutputIndex:  rpcGenesis.OutputIndex,
	}, nil
}

func unmarshalGroupVirtualTx(
	rpcTx *taprpc.GroupVirtualTx) (*tapsdk.GroupVirtualTx, error) {

	if rpcTx == nil {
		return nil, fmt.Errorf("nil group virtual tx")
	}

	tx := &tapsdk.GroupVirtualTx{Transaction: rpcTx.Transaction}

	if rpcTx.PrevOut != nil {
		tx.PrevOut = &tapsdk.TxOut{
			Value:    rpcTx.PrevOut.Value,
			PkScript: rpcTx.PrevOut.PkScript,
		}
	}

	if len(rpcTx.GenesisId) != 0 {
		genesisID, err := tapsdk.ParseAssetID(rpcTx.GenesisId)
		if err != nil {
			return nil, fmt.Errorf("invalid group virtual tx genesis ID: %w",
				err)
		}

		tx.GenesisID = genesisID
	}

	if len(rpcTx.TweakedKey) != 0 {
		tweakedKey, err := tapsdk.ParsePubKey(rpcTx.TweakedKey)
		if err != nil {
			return nil, fmt.Errorf("invalid tweaked key: %w", err)
		}

		tx.TweakedKey = &tweakedKey
	}

	return tx, nil
}
