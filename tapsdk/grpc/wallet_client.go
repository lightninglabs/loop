package grpc

import (
	"context"
	"encoding/hex"
	"fmt"
	"maps"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	tapsdk "github.com/lightninglabs/loop/tapsdk"
	"github.com/lightninglabs/loop/tapsdk/internal/codec"
	"github.com/lightninglabs/loop/tapsdk/macaroon"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"google.golang.org/grpc"
)

type walletClient struct {
	client   taprpc.TaprootAssetsClient
	timeout  time.Duration
	adminMac macaroon.SerializedMacaroon
}

// NewWalletClient creates a new Wallet client.
func NewWalletClient(conn grpc.ClientConnInterface, timeout time.Duration,
	adminMac macaroon.SerializedMacaroon) *walletClient {

	return &walletClient{
		client:   taprpc.NewTaprootAssetsClient(conn),
		timeout:  timeout,
		adminMac: adminMac,
	}
}

func (s *walletClient) GetInfo(ctx context.Context) (*tapsdk.Info, error) {
	rpcCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)
	resp, err := s.client.GetInfo(rpcCtx, &taprpc.GetInfoRequest{})
	if err != nil {
		return nil, err
	}

	pubKey, err := hex.DecodeString(resp.LndIdentityPubkey)
	if err != nil {
		return nil, err
	}

	var pubKeyArray [33]byte
	copy(pubKeyArray[:], pubKey)

	return &tapsdk.Info{
		Version:           resp.Version,
		LndVersion:        resp.LndVersion,
		Network:           resp.Network,
		LndIdentityPubkey: pubKeyArray,
		BlockHeight:       resp.BlockHeight,
		NodeAlias:         resp.NodeAlias,
		SyncedToChain:     resp.SyncToChain,
	}, nil
}

func (s *walletClient) ListAssetRecords(ctx context.Context,
	req *tapsdk.ListAssetsRequest) ([]*tapsdk.AssetRecord, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)

	rpcReq, assetRefFilter, err := marshalListAssetRecordsRequest(req)
	if err != nil {
		return nil, err
	}

	resp, err := s.client.ListAssets(rpcCtx, rpcReq)
	if err != nil {
		return nil, err
	}

	assets := make([]*tapsdk.AssetRecord, 0, len(resp.Assets))
	for _, rpcAsset := range resp.Assets {
		asset, err := unmarshalAsset(rpcAsset)
		if err != nil {
			return nil, err
		}

		if assetRefFilter != nil &&
			!assetRecordMatchesRef(asset, *assetRefFilter) {

			continue
		}

		assets = append(assets, asset)
	}

	return assets, nil
}

func marshalListAssetRecordsRequest(
	req *tapsdk.ListAssetsRequest) (*taprpc.ListAssetRequest,
	*tapsdk.AssetRef, error) {

	rpcReq := &taprpc.ListAssetRequest{}
	var assetRefFilter *tapsdk.AssetRef
	if req == nil {
		return rpcReq, assetRefFilter, nil
	}

	rpcReq.WithWitness = req.WithWitness
	rpcReq.IncludeSpent = req.IncludeSpent
	rpcReq.IncludeLeased = req.IncludeLeased
	rpcReq.IncludeUnconfirmedMints = req.IncludeUnconfirmedMints
	rpcReq.MinAmount = req.MinAmount
	rpcReq.MaxAmount = req.MaxAmount

	if req.AssetRef != nil {
		if err := req.AssetRef.Validate(); err != nil {
			return nil, nil, err
		}

		assetRefFilter = req.AssetRef
		if groupKey, ok := req.AssetRef.GroupKey(); ok {
			rpcReq.GroupKey = groupKey[:]
		}
	}

	if req.AnchorOutpoint != nil {
		anchor := req.AnchorOutpoint
		rpcReq.AnchorOutpoint = &taprpc.OutPoint{
			Txid:        anchor.Txid[:],
			OutputIndex: anchor.Index,
		}
	}

	scriptKeyType, err := marshalScriptKeyTypeQuery(req.ScriptKeyType)
	if err != nil {
		return nil, nil, err
	}
	rpcReq.ScriptKeyType = scriptKeyType

	return rpcReq, assetRefFilter, nil
}

func assetRecordMatchesRef(asset *tapsdk.AssetRecord,
	ref tapsdk.AssetRef) bool {

	if asset == nil {
		return false
	}

	if asset.AssetRef.Equivalent(ref) {
		return true
	}

	assetID, ok := ref.AssetID()
	return ok && asset.Genesis.IssuanceID == assetID
}

func (s *walletClient) ListBalances(ctx context.Context,
	req *tapsdk.ListBalancesRequest) (
	*tapsdk.ListBalancesResponse, error) {

	// Group refs must round-trip through tapd's group-by-group_key
	// mode — the server only honors group_key_filter in that mode,
	// and a wallet that learned about the group through universe
	// bootstrap has no per-asset-id rows yet.
	if req != nil && req.AssetRef != nil && req.AssetRef.IsGroupRef() {
		return s.listGroupBalance(ctx, req)
	}

	rpcCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)
	rpcReq, err := marshalListBalancesRequest(req)
	if err != nil {
		return nil, err
	}

	rawResp, err := s.client.ListBalances(rpcCtx, rpcReq)
	if err != nil {
		return nil, err
	}

	result := &tapsdk.ListBalancesResponse{
		Balances: make(
			map[string]*tapsdk.Balance,
		),
		UnconfirmedTransfers: rawResp.UnconfirmedTransfers,
	}

	for _, ab := range rawResp.AssetBalances {
		row, err := unmarshalBalance(ab)
		if err != nil {
			return nil, fmt.Errorf("balance: %w", err)
		}

		key := row.AssetRef.String()
		balance, ok := result.Balances[key]
		if !ok {
			result.Balances[key] = row
			continue
		}

		balance.Balance += row.Balance
	}

	return result, nil
}

// listGroupBalance queries a single-group balance via tapd's group-by-group_key
// mode.
func (s *walletClient) listGroupBalance(ctx context.Context,
	req *tapsdk.ListBalancesRequest) (*tapsdk.ListBalancesResponse,
	error) {

	groupKey, ok := req.AssetRef.GroupKey()
	if !ok {
		return nil, fmt.Errorf("group balance requires a group asset ref")
	}

	rpcCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)
	scriptKeyType, err := marshalScriptKeyTypeQuery(req.ScriptKeyType)
	if err != nil {
		return nil, err
	}

	rawResp, err := s.client.ListBalances(rpcCtx,
		&taprpc.ListBalancesRequest{
			GroupBy: &taprpc.ListBalancesRequest_GroupKey{
				GroupKey: true,
			},
			GroupKeyFilter: groupKey[:],
			IncludeLeased:  req.IncludeLeased,
			ScriptKeyType:  scriptKeyType,
		},
	)
	if err != nil {
		return nil, err
	}

	result := &tapsdk.ListBalancesResponse{
		Balances:             map[string]*tapsdk.Balance{},
		UnconfirmedTransfers: rawResp.UnconfirmedTransfers,
	}

	groupBalance, ok := rawResp.AssetGroupBalances[hex.EncodeToString(groupKey[:])]
	if !ok || groupBalance == nil || groupBalance.Balance == 0 {
		return result, nil
	}

	result.Balances[req.AssetRef.String()] = &tapsdk.Balance{
		AssetRef: *req.AssetRef,
		Balance:  groupBalance.Balance,
	}

	return result, nil
}

func (s *walletClient) ListTransfers(ctx context.Context,
	req *tapsdk.ListTransfersRequest) ([]*tapsdk.AssetTransfer, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)

	rpcReq := &taprpc.ListTransfersRequest{}
	if req != nil {
		rpcReq.AnchorTxid = req.AnchorTxid
	}

	resp, err := s.client.ListTransfers(rpcCtx, rpcReq)
	if err != nil {
		return nil, err
	}

	transfers := make([]*tapsdk.AssetTransfer, 0, len(resp.Transfers))
	for _, rpcTransfer := range resp.Transfers {
		transfer, err := unmarshalAssetTransfer(rpcTransfer)
		if err != nil {
			return nil, err
		}

		transfers = append(transfers, transfer)
	}

	return transfers, nil
}

// SendAsset performs a one-shot address-based send.
func (s *walletClient) SendAsset(ctx context.Context,
	req *tapsdk.SendAssetRequest) (*tapsdk.AssetTransfer, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)

	rpcReq, err := marshalSendAssetRequest(req)
	if err != nil {
		return nil, err
	}

	resp, err := s.client.SendAsset(rpcCtx, rpcReq)
	if err != nil {
		return nil, err
	}

	return unmarshalAssetTransfer(resp.Transfer)
}

func unmarshalAsset(rpcAsset *taprpc.Asset) (*tapsdk.AssetRecord, error) {
	if rpcAsset == nil {
		return nil, fmt.Errorf("nil asset")
	}
	if rpcAsset.AssetGenesis == nil {
		return nil, fmt.Errorf("missing asset genesis")
	}
	if len(rpcAsset.ScriptKey) != 33 {
		return nil, fmt.Errorf("invalid script key length: %d",
			len(rpcAsset.ScriptKey))
	}

	genesis, err := unmarshalIssuanceGenesis(rpcAsset.AssetGenesis)
	if err != nil {
		return nil, err
	}

	var scriptKeyPub [33]byte
	copy(scriptKeyPub[:], rpcAsset.ScriptKey)

	asset := &tapsdk.AssetRecord{
		Version:          uint8(rpcAsset.Version),
		Genesis:          *genesis,
		Amount:           rpcAsset.Amount,
		LockTime:         uint64(rpcAsset.LockTime),
		RelativeLockTime: uint64(rpcAsset.RelativeLockTime),
		ScriptVersion:    uint16(rpcAsset.ScriptVersion),
		ScriptKey: tapsdk.ScriptKey{
			PubKey: scriptKeyPub,
		},
	}
	if rpcAsset.ChainAnchor != nil &&
		rpcAsset.ChainAnchor.AnchorOutpoint != "" {

		raw := rpcAsset.ChainAnchor.AnchorOutpoint
		point, err := tapsdk.NewOutpointFromStr(raw)
		if err != nil || point.String() != raw {
			return nil, fmt.Errorf(
				"invalid canonical anchor outpoint",
			)
		}
		asset.AnchorOutpoint = &point
	}

	// tapd keys every queryable map (ListBalances, ListAssetGroups,
	// ListAssets filters) by the tweaked group key, so that's what
	// AssetRef must encode.
	var tweakedGroupKey *tapsdk.PubKey
	if rpcAsset.AssetGroup != nil &&
		len(rpcAsset.AssetGroup.TweakedGroupKey) == 33 {

		var gk tapsdk.PubKey
		copy(gk[:], rpcAsset.AssetGroup.TweakedGroupKey)
		tweakedGroupKey = &gk
	}

	asset.AssetRef = tapsdk.AssetRefFromAsset(
		asset.Genesis.IssuanceID, tweakedGroupKey,
	)

	return asset, nil
}

// unmarshalIssuanceGenesis converts tapd's GenesisInfo into the SDK's concrete
// issuance genesis type.
func unmarshalIssuanceGenesis(rpcGenesis *taprpc.GenesisInfo) (
	*tapsdk.IssuanceGenesis, error) {

	if rpcGenesis == nil {
		return nil, fmt.Errorf("nil asset genesis")
	}
	if rpcGenesis.GenesisPoint == "" {
		return nil, fmt.Errorf("missing genesis point")
	}
	if len(rpcGenesis.AssetId) != 32 {
		return nil, fmt.Errorf("invalid asset ID length: %d",
			len(rpcGenesis.AssetId))
	}
	if len(rpcGenesis.MetaHash) != 0 && len(rpcGenesis.MetaHash) != 32 {
		return nil, fmt.Errorf("invalid meta hash length: %d",
			len(rpcGenesis.MetaHash))
	}

	firstPrevOut, err := tapsdk.NewOutpointFromStr(rpcGenesis.GenesisPoint)
	if err != nil {
		return nil, fmt.Errorf("invalid genesis point: %w",
			err)
	}

	var assetID [32]byte
	copy(assetID[:], rpcGenesis.AssetId)

	var metaHash [32]byte
	if len(rpcGenesis.MetaHash) == 32 {
		copy(metaHash[:], rpcGenesis.MetaHash)
	}

	assetType, err := unmarshalAssetType(rpcGenesis.AssetType)
	if err != nil {
		return nil, err
	}

	return &tapsdk.IssuanceGenesis{
		FirstPrevOut: firstPrevOut,
		Tag:          rpcGenesis.Name,
		MetaHash:     metaHash,
		IssuanceID:   assetID,
		OutputIndex:  rpcGenesis.OutputIndex,
		Type:         assetType,
	}, nil
}

func unmarshalAssetType(assetType taprpc.AssetType) (tapsdk.AssetType,
	error) {

	switch assetType {
	case taprpc.AssetType_NORMAL:
		return tapsdk.AssetTypeFungible, nil

	case taprpc.AssetType_COLLECTIBLE:
		return tapsdk.AssetTypeNFT, nil

	default:
		return 0, fmt.Errorf("unknown asset type: %v", assetType)
	}
}

func unmarshalAssetTransfer(rpcTransfer *taprpc.AssetTransfer) (
	*tapsdk.AssetTransfer, error) {

	if rpcTransfer == nil {
		return nil, fmt.Errorf("nil transfer")
	}

	// Unmarshal transfer txid and anchor txid
	var transferTxid [32]byte
	var anchorTxid string
	if len(rpcTransfer.AnchorTxHash) == 32 {
		copy(transferTxid[:], rpcTransfer.AnchorTxHash)

		var h chainhash.Hash
		copy(h[:], rpcTransfer.AnchorTxHash)
		anchorTxid = h.String()
	}

	// Unmarshal outputs
	outputs := make([]tapsdk.TransferOutput, 0, len(rpcTransfer.Outputs))
	for _, out := range rpcTransfer.Outputs {
		assetType, err := unmarshalAssetType(out.AssetType)
		if err != nil {
			return nil, fmt.Errorf("invalid output asset type: %w",
				err)
		}

		output := tapsdk.TransferOutput{
			Amount:    out.Amount,
			AssetType: assetType,
			ProofBlob: out.NewProofBlob,
		}

		if len(out.AssetId) > 0 {
			if len(out.AssetId) != 32 {
				return nil, fmt.Errorf(
					"invalid output asset ID length: %d",
					len(out.AssetId),
				)
			}
			copy(output.IssuanceID[:], out.AssetId)
		}

		// Copy the script key.
		if len(out.ScriptKey) == 33 {
			copy(output.ScriptKey[:], out.ScriptKey)
		}

		// Copy the outpoint from anchor.
		if out.Anchor != nil {
			op, err := tapsdk.NewOutpointFromStr(out.Anchor.Outpoint)
			if err != nil {
				return nil, fmt.Errorf("invalid anchor outpoint: %w", err)
			}

			output.AnchorOutpoint = op
			output.AnchorValue = out.Anchor.Value
		}

		// Group key is empty for ungrouped assets and on responses
		// from older daemons; the SDK treats either as "no group" and
		// falls back to the asset-id ref.
		if len(out.GroupKey) > 0 {
			groupKey, err := tapsdk.ParsePubKey(out.GroupKey)
			if err != nil {
				return nil, fmt.Errorf(
					"invalid output group key: %w", err,
				)
			}
			output.GroupKey = &groupKey
		}

		outputs = append(outputs, output)
	}

	// Unmarshal inputs
	inputs := make([]tapsdk.TransferInput, 0, len(rpcTransfer.Inputs))
	for _, in := range rpcTransfer.Inputs {
		if in == nil {
			return nil, fmt.Errorf("nil transfer input")
		}
		if len(in.AssetId) != 32 {
			return nil, fmt.Errorf(
				"invalid input asset ID length: %d", len(in.AssetId),
			)
		}
		if len(in.ScriptKey) != 33 {
			return nil, fmt.Errorf(
				"invalid input script key length: %d",
				len(in.ScriptKey),
			)
		}

		var assetID [32]byte
		copy(assetID[:], in.AssetId)

		var scriptKey [33]byte
		copy(scriptKey[:], in.ScriptKey)

		assetType, err := unmarshalAssetType(in.AssetType)
		if err != nil {
			return nil, fmt.Errorf("invalid input asset type: %w",
				err)
		}

		anchorPoint, err := tapsdk.NewOutpointFromStr(in.AnchorPoint)
		if err != nil {
			return nil, fmt.Errorf("invalid anchor point: %w", err)
		}

		input := tapsdk.TransferInput{
			AnchorPoint: anchorPoint,
			IssuanceID:  assetID,
			AssetType:   assetType,
			ScriptKey:   scriptKey,
			Amount:      in.Amount,
		}

		// Group key is empty for ungrouped assets and on responses
		// from older daemons; the SDK treats either as "no group" and
		// falls back to the asset-id ref.
		if len(in.GroupKey) > 0 {
			groupKey, err := tapsdk.ParsePubKey(in.GroupKey)
			if err != nil {
				return nil, fmt.Errorf(
					"invalid input group key: %w", err,
				)
			}
			input.GroupKey = &groupKey
		}

		inputs = append(inputs, input)
	}

	var blockHash [32]byte
	var blockHashStr string
	if rpcTransfer.AnchorTxBlockHash != nil {
		blockHashStr = rpcTransfer.AnchorTxBlockHash.HashStr
		if len(rpcTransfer.AnchorTxBlockHash.Hash) == 32 {
			copy(blockHash[:], rpcTransfer.AnchorTxBlockHash.Hash)
		}
	}

	return &tapsdk.AssetTransfer{
		TransferTimestamp:    rpcTransfer.TransferTimestamp,
		TransferTxid:         transferTxid,
		AnchorTxid:           anchorTxid,
		AnchorTxHeightHint:   rpcTransfer.AnchorTxHeightHint,
		AnchorTxChainFees:    rpcTransfer.AnchorTxChainFees,
		Inputs:               inputs,
		Outputs:              outputs,
		AnchorTxBlockHash:    blockHash,
		AnchorTxBlockHashStr: blockHashStr,
		AnchorTxBlockHeight:  rpcTransfer.AnchorTxBlockHeight,
		Label:                rpcTransfer.Label,
		AnchorTx:             rpcTransfer.AnchorTx,
	}, nil
}

// NewAddr creates a new Taproot Asset address for receiving assets.
func (s *walletClient) NewAddr(ctx context.Context,
	req *tapsdk.NewAddressRequest) (*tapsdk.Address, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)

	rpcReq, err := marshalNewAddrRequest(req)
	if err != nil {
		return nil, err
	}

	resp, err := s.client.NewAddr(rpcCtx, rpcReq)
	if err != nil {
		return nil, err
	}

	return unmarshalAddr(resp)
}

// DecodeAddr decodes a bech32m Taproot Asset address string into its
// components.
func (s *walletClient) DecodeAddr(ctx context.Context,
	addr string) (*tapsdk.Address, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)

	resp, err := s.client.DecodeAddr(rpcCtx, &taprpc.DecodeAddrRequest{
		Addr: addr,
	})
	if err != nil {
		return nil, err
	}

	return unmarshalAddr(resp)
}

// QueryAddrs returns addresses that were previously created by this tapd
// instance.
func (s *walletClient) QueryAddrs(ctx context.Context,
	query *tapsdk.AddressQuery) ([]*tapsdk.Address, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)

	rpcReq := &taprpc.QueryAddrRequest{}
	if query != nil {
		rpcReq.CreatedAfter = query.CreatedAfter
		rpcReq.CreatedBefore = query.CreatedBefore
		rpcReq.Limit = query.Limit
		rpcReq.Offset = query.Offset
	}

	resp, err := s.client.QueryAddrs(rpcCtx, rpcReq)
	if err != nil {
		return nil, err
	}

	addrs := make([]*tapsdk.Address, 0, len(resp.Addrs))
	for _, rpcAddr := range resp.Addrs {
		addr, err := unmarshalAddr(rpcAddr)
		if err != nil {
			return nil, err
		}
		addrs = append(addrs, addr)
	}

	return addrs, nil
}

// AddrReceives returns incoming transfer events for addresses created by this
// tapd instance.
func (s *walletClient) AddrReceives(ctx context.Context,
	query *tapsdk.AddressReceivesQuery) ([]*tapsdk.AddressEvent, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)

	rpcReq := &taprpc.AddrReceivesRequest{}
	if query != nil {
		rpcReq.FilterAddr = query.FilterAddr
		rpcReq.FilterStatus = taprpc.AddrEventStatus(query.FilterStatus)
		rpcReq.StartTimestamp = query.StartTimestamp
		rpcReq.EndTimestamp = query.EndTimestamp
		rpcReq.Offset = query.Offset
		rpcReq.Limit = query.Limit
		rpcReq.Direction = taprpc.SortDirection(query.Direction)
	}

	resp, err := s.client.AddrReceives(rpcCtx, rpcReq)
	if err != nil {
		return nil, err
	}

	events := make([]*tapsdk.AddressEvent, 0, len(resp.Events))
	for _, rpcEvent := range resp.Events {
		event, err := unmarshalAddrEvent(rpcEvent)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}

	return events, nil
}

func marshalListBalancesRequest(
	req *tapsdk.ListBalancesRequest) (*taprpc.ListBalancesRequest, error) {

	// Group by asset_id so we get per-tranche balances with genesis info
	// and group keys. Group-ref lookups take a separate code path.
	rpcReq := &taprpc.ListBalancesRequest{
		GroupBy: &taprpc.ListBalancesRequest_AssetId{
			AssetId: true,
		},
	}
	if req == nil {
		return rpcReq, nil
	}

	rpcReq.IncludeLeased = req.IncludeLeased
	scriptKeyType, err := marshalScriptKeyTypeQuery(req.ScriptKeyType)
	if err != nil {
		return nil, err
	}
	rpcReq.ScriptKeyType = scriptKeyType

	if req.AssetRef != nil {
		if assetID, ok := req.AssetRef.AssetID(); ok {
			rpcReq.AssetFilter = assetID[:]
		}
	}

	return rpcReq, nil
}

func marshalScriptKeyTypeQuery(
	query *tapsdk.ScriptKeyTypeQuery) (*taprpc.ScriptKeyTypeQuery, error) {

	if err := query.Validate(); err != nil {
		return nil, err
	}

	if query == nil {
		return nil, nil
	}

	if query.ExplicitType != nil {
		return &taprpc.ScriptKeyTypeQuery{
			Type: &taprpc.ScriptKeyTypeQuery_ExplicitType{
				ExplicitType: taprpc.ScriptKeyType(*query.ExplicitType),
			},
		}, nil
	}

	if query.AllTypes {
		return &taprpc.ScriptKeyTypeQuery{
			Type: &taprpc.ScriptKeyTypeQuery_AllTypes{
				AllTypes: true,
			},
		}, nil
	}

	return nil, nil
}

func unmarshalBalance(
	rpcBalance *taprpc.AssetBalance) (*tapsdk.Balance, error) {

	if rpcBalance == nil {
		return nil, fmt.Errorf("nil asset balance")
	}

	var genesis *tapsdk.IssuanceGenesis
	if rpcBalance.AssetGenesis != nil {
		var err error
		genesis, err = unmarshalIssuanceGenesis(rpcBalance.AssetGenesis)
		if err != nil {
			return nil, err
		}
	}

	balance := &tapsdk.Balance{
		Balance: rpcBalance.Balance,
	}

	if len(rpcBalance.GroupKey) != 0 &&
		(genesis == nil || genesis.Type != tapsdk.AssetTypeCollectible) {

		if len(rpcBalance.GroupKey) != 33 {
			return nil, fmt.Errorf("invalid group key length: %d",
				len(rpcBalance.GroupKey))
		}

		var groupKey tapsdk.PubKey
		copy(groupKey[:], rpcBalance.GroupKey)
		balance.AssetRef = tapsdk.AssetRefFromGroupKey(groupKey)
		return balance, nil
	}

	if rpcBalance.AssetGenesis == nil {
		return nil, fmt.Errorf("missing asset genesis")
	}

	balance.AssetRef = tapsdk.AssetRefFromAsset(genesis.IssuanceID, nil)

	return balance, nil
}

func marshalSendAssetRequest(
	req *tapsdk.SendAssetRequest) (*taprpc.SendAssetRequest, error) {

	if req == nil {
		return &taprpc.SendAssetRequest{}, nil
	}

	feeRate, err := feeRateSatPerKWeight(req.FeeRate)
	if err != nil {
		return nil, err
	}

	rpcReq := &taprpc.SendAssetRequest{
		FeeRate:                   feeRate,
		Label:                     req.Label,
		SkipProofCourierPingCheck: req.SkipProofCourierPingCheck,
	}

	if len(req.Recipients) == 0 {
		return rpcReq, nil
	}

	// tapd exposes two mutually exclusive wire paths: TapAddrs for addresses
	// that already encode their amount, and AddressesWithAmounts for the
	// explicit-amount path. Mixed inputs are a caller contract violation;
	// Wallet.SendMulti is responsible for normalising them before reaching
	// this layer.
	allEmbedded, anyEmbedded := true, false
	for _, r := range req.Recipients {
		_, hasAmount := r.Amount()
		if !hasAmount {
			anyEmbedded = true
		} else {
			allEmbedded = false
		}
	}
	if !allEmbedded && anyEmbedded {
		return nil, tapsdk.ErrMixedRecipientAmounts
	}

	if allEmbedded {
		rpcReq.TapAddrs = make([]string, len(req.Recipients))
		for i, r := range req.Recipients {
			rpcReq.TapAddrs[i] = r.Address
		}
		return rpcReq, nil
	}

	rpcReq.AddressesWithAmounts = make(
		[]*taprpc.AddressWithAmount, 0, len(req.Recipients),
	)
	for _, r := range req.Recipients {
		amount, _ := r.Amount()
		rpcReq.AddressesWithAmounts = append(
			rpcReq.AddressesWithAmounts,
			&taprpc.AddressWithAmount{
				TapAddr: r.Address,
				Amount:  amount,
			},
		)
	}

	return rpcReq, nil
}

func marshalNewAddrRequest(
	req *tapsdk.NewAddressRequest) (*taprpc.NewAddrRequest, error) {

	if req == nil {
		return &taprpc.NewAddrRequest{}, nil
	}

	rpcReq := &taprpc.NewAddrRequest{
		Amt:                       req.Amount,
		TapscriptSibling:          req.TapscriptSibling,
		ProofCourierAddr:          req.ProofCourierAddr,
		SkipProofCourierConnCheck: req.SkipProofCourierConnCheck,
	}

	if !req.AssetRef.IsZero() {
		if err := req.AssetRef.Validate(); err != nil {
			return nil, err
		}

		if assetID, ok := req.AssetRef.AssetID(); ok {
			rpcReq.AssetId = assetID[:]
		}

		if groupKey, ok := req.AssetRef.GroupKey(); ok {
			rpcReq.GroupKey = groupKey[:]
		}
	}

	if req.ScriptKey != nil {
		rpcReq.ScriptKey = &taprpc.ScriptKey{
			PubKey:   req.ScriptKey.PubKey[:],
			TapTweak: req.ScriptKey.TapTweak,
		}
		if req.ScriptKey.KeyDesc.RawKeyBytes != (tapsdk.PubKey{}) {
			rpcReq.ScriptKey.KeyDesc = &taprpc.KeyDescriptor{
				RawKeyBytes: req.ScriptKey.KeyDesc.RawKeyBytes[:],
				KeyLoc: &taprpc.KeyLocator{
					KeyFamily: int32(req.ScriptKey.KeyDesc.KeyLocator.Family),
					KeyIndex:  int32(req.ScriptKey.KeyDesc.KeyLocator.Index),
				},
			}
		}
	}

	if req.InternalKey != nil {
		rpcReq.InternalKey = &taprpc.KeyDescriptor{
			RawKeyBytes: req.InternalKey.RawKeyBytes[:],
			KeyLoc: &taprpc.KeyLocator{
				KeyFamily: int32(req.InternalKey.KeyLocator.Family),
				KeyIndex:  int32(req.InternalKey.KeyLocator.Index),
			},
		}
	}

	if req.AssetVersion != nil {
		rpcReq.AssetVersion = taprpc.AssetVersion(*req.AssetVersion)
	}

	if req.AddressVersion != nil {
		rpcReq.AddressVersion = taprpc.AddrVersion(*req.AddressVersion)
	}

	return rpcReq, nil
}

func unmarshalAddr(rpcAddr *taprpc.Addr) (*tapsdk.Address, error) {
	if rpcAddr == nil {
		return nil, fmt.Errorf("nil address")
	}

	assetType, err := unmarshalAssetType(rpcAddr.AssetType)
	if err != nil {
		return nil, err
	}

	addr := &tapsdk.Address{
		Encoded:          rpcAddr.Encoded,
		AssetType:        assetType,
		Amount:           rpcAddr.Amount,
		TapscriptSibling: rpcAddr.TapscriptSibling,
		ProofCourierAddr: rpcAddr.ProofCourierAddr,
		AssetVersion:     tapsdk.AssetVersion(rpcAddr.AssetVersion),
		AddressVersion:   tapsdk.AddressVersion(rpcAddr.AddressVersion),
	}

	var assetID *tapsdk.AssetID
	if len(rpcAddr.AssetId) == 32 {
		parsedAssetID, err := tapsdk.ParseAssetID(rpcAddr.AssetId)
		if err != nil {
			return nil, fmt.Errorf("invalid asset ID: %w", err)
		}

		assetID = &parsedAssetID
	}

	var groupKey *tapsdk.PubKey
	if len(rpcAddr.GroupKey) == 33 {
		parsedGroupKey, err := tapsdk.ParsePubKey(rpcAddr.GroupKey)
		if err != nil {
			return nil, fmt.Errorf("invalid group key: %w", err)
		}

		groupKey = &parsedGroupKey
	}

	switch {
	case assetID != nil && assetID.IsZero() && groupKey != nil:
		addr.AssetRef = tapsdk.AssetRefFromGroupKey(*groupKey)

	case assetID != nil && assetID.IsZero():
		return nil, fmt.Errorf("address asset ID is zero with no " +
			"group key")

	case assetID != nil:
		addr.AssetRef = tapsdk.AssetRefFromTypedAsset(
			*assetID, groupKey, addr.AssetType,
		)

	case groupKey != nil:
		addr.AssetRef = tapsdk.AssetRefFromGroupKey(*groupKey)

	default:
		return nil, fmt.Errorf("address missing asset ID or group key")
	}

	// Parse script key (required).
	if len(rpcAddr.ScriptKey) != 33 {
		return nil, fmt.Errorf("invalid script key length: %d",
			len(rpcAddr.ScriptKey))
	}
	copy(addr.ScriptKey[:], rpcAddr.ScriptKey)

	// Parse internal key (required).
	if len(rpcAddr.InternalKey) != 33 {
		return nil, fmt.Errorf("invalid internal key length: %d",
			len(rpcAddr.InternalKey))
	}
	copy(addr.InternalKey[:], rpcAddr.InternalKey)

	// Parse taproot output key (required).
	if len(rpcAddr.TaprootOutputKey) != 32 {
		return nil, fmt.Errorf("invalid taproot output key length: %d",
			len(rpcAddr.TaprootOutputKey))
	}
	copy(addr.TaprootOutputKey[:], rpcAddr.TaprootOutputKey)

	return addr, nil
}

func unmarshalAddrEvent(rpcEvent *taprpc.AddrEvent) (*tapsdk.AddressEvent,
	error) {

	if rpcEvent == nil {
		return nil, fmt.Errorf("nil address event")
	}

	event := &tapsdk.AddressEvent{
		CreationTime:       rpcEvent.CreationTimeUnixSeconds,
		Status:             tapsdk.AddressEventStatus(rpcEvent.Status),
		Outpoint:           rpcEvent.Outpoint,
		UTXOAmountSat:      rpcEvent.UtxoAmtSat,
		TaprootSibling:     rpcEvent.TaprootSibling,
		ConfirmationHeight: rpcEvent.ConfirmationHeight,
		HasProof:           rpcEvent.HasProof,
	}

	// Unmarshal the embedded address if present.
	if rpcEvent.Addr != nil {
		addr, err := unmarshalAddr(rpcEvent.Addr)
		if err != nil {
			return nil, fmt.Errorf("invalid address in event: %w", err)
		}
		event.Address = addr
	}

	return event, nil
}

// ListUtxos lists managed UTXOs with optional filtering.
func (s *walletClient) ListUtxos(ctx context.Context,
	req *tapsdk.ListUtxosRequest) (map[string]*tapsdk.ManagedUtxo,
	error) {

	rpcCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)

	rpcReq := &taprpc.ListUtxosRequest{}
	if req != nil {
		rpcReq.IncludeLeased = req.IncludeLeased
		scriptKeyType, err := marshalScriptKeyTypeQuery(
			req.ScriptKeyType,
		)
		if err != nil {
			return nil, err
		}
		rpcReq.ScriptKeyType = scriptKeyType
	}

	resp, err := s.client.ListUtxos(rpcCtx, rpcReq)
	if err != nil {
		return nil, err
	}

	result := make(
		map[string]*tapsdk.ManagedUtxo, len(resp.ManagedUtxos),
	)

	for key, rpcUtxo := range resp.ManagedUtxos {
		utxo, err := unmarshalManagedUtxo(rpcUtxo)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal "+
				"utxo %s: %w", key, err)
		}

		result[key] = utxo
	}

	return result, nil
}

// ListAssetGroups lists all known asset groups.
func (s *walletClient) ListAssetGroups(
	ctx context.Context) ([]tapsdk.AssetGroupRecord, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)

	resp, err := s.client.ListGroups(
		rpcCtx, &taprpc.ListGroupsRequest{},
	)
	if err != nil {
		return nil, err
	}

	result := make([]tapsdk.AssetGroupRecord, 0, len(resp.Groups))

	for key, rpcGroup := range resp.Groups {
		group, err := unmarshalAssetGroupRecord(key, rpcGroup)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal "+
				"group %s: %w", key, err)
		}

		result = append(result, *group)
	}

	return result, nil
}

// BurnAsset burns asset units. The confirmation text must be set to
// "assets will be destroyed" for the burn to succeed.
func (s *walletClient) BurnAsset(ctx context.Context,
	req *tapsdk.BurnAssetRequest) (*tapsdk.BurnAssetResponse,
	error) {

	rpcCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)

	rpcReq, err := marshalBurnAssetRequest(req)
	if err != nil {
		return nil, err
	}

	resp, err := s.client.BurnAsset(rpcCtx, rpcReq)
	if err != nil {
		return nil, err
	}

	result := &tapsdk.BurnAssetResponse{}

	if resp.BurnTransfer != nil {
		transfer, err := unmarshalAssetTransfer(resp.BurnTransfer)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal "+
				"burn transfer: %w", err)
		}

		result.BurnTransfer = transfer
	}

	// Retain the SDK singular-proof API for older tapd burn responses.
	if resp.BurnProof != nil { //nolint:staticcheck
		proof, err := unmarshalDecodedProof(
			resp.BurnProof, //nolint:staticcheck
		)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal "+
				"burn proof: %w", err)
		}

		result.BurnProof = proof
	}

	return result, nil
}

// ListBurns lists asset burns with optional filtering.
func (s *walletClient) ListBurns(ctx context.Context,
	req *tapsdk.ListBurnsRequest) ([]*tapsdk.BurnRecord, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)

	rpcReq := &taprpc.ListBurnsRequest{}
	if req != nil {
		if req.AssetRef != nil {
			if err := req.AssetRef.Validate(); err != nil {
				return nil, err
			}

			if assetID, ok := req.AssetRef.AssetID(); ok {
				rpcReq.AssetId = assetID[:]
			}

			if groupKey, ok := req.AssetRef.GroupKey(); ok {
				rpcReq.TweakedGroupKey = groupKey[:]
			}
		}
		if req.AnchorTxid != nil {
			rpcReq.AnchorTxid = req.AnchorTxid[:]
		}
	}

	resp, err := s.client.ListBurns(rpcCtx, rpcReq)
	if err != nil {
		return nil, err
	}

	burns := make([]*tapsdk.BurnRecord, 0, len(resp.Burns))
	for _, rpcBurn := range resp.Burns {
		burn, err := unmarshalBurnRecord(rpcBurn)
		if err != nil {
			return nil, err
		}

		burns = append(burns, burn)
	}

	return burns, nil
}

// FetchAssetMeta fetches the metadata for an asset by ID or meta hash.
func (s *walletClient) FetchAssetMeta(ctx context.Context,
	req *tapsdk.FetchAssetMetaRequest) (*tapsdk.AssetMeta, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)

	rpcReq, err := marshalFetchAssetMetaRequest(req)
	if err != nil {
		return nil, err
	}

	resp, err := s.client.FetchAssetMeta(rpcCtx, rpcReq)
	if err != nil {
		return nil, err
	}

	return unmarshalFetchAssetMetaResponse(resp)
}

// VerifyProof verifies a proof file and returns the decoded last proof
// if valid.
func (s *walletClient) VerifyProof(ctx context.Context,
	rawProofFile []byte) (*tapsdk.VerifyProofResponse, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	rpcCtx = s.adminMac.WithMacaroonAuth(rpcCtx)

	resp, err := s.client.VerifyProof(rpcCtx, &taprpc.ProofFile{
		RawProofFile: rawProofFile,
	})
	if err != nil {
		return nil, err
	}

	result := &tapsdk.VerifyProofResponse{
		Valid: resp.Valid,
	}

	if resp.DecodedProof != nil {
		decoded, err := unmarshalDecodedProof(resp.DecodedProof)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal "+
				"decoded proof: %w", err)
		}

		result.DecodedProof = decoded
	}

	return result, nil
}

// unmarshalManagedUtxo converts an RPC ManagedUtxo to an
// tapsdk.ManagedUtxo.
func unmarshalManagedUtxo(
	rpcUtxo *taprpc.ManagedUtxo) (*tapsdk.ManagedUtxo, error) {

	if rpcUtxo == nil {
		return nil, fmt.Errorf("nil managed utxo")
	}

	outPoint, err := tapsdk.NewOutpointFromStr(rpcUtxo.OutPoint)
	if err != nil {
		return nil, fmt.Errorf("invalid outpoint: %w", err)
	}

	internalKey, err := tapsdk.ParsePubKey(rpcUtxo.InternalKey)
	if err != nil {
		return nil, fmt.Errorf("invalid internal key: %w", err)
	}

	taprootAssetRoot, err := tapsdk.ParseHash(
		rpcUtxo.TaprootAssetRoot,
	)
	if err != nil {
		return nil, fmt.Errorf("invalid taproot asset root: %w",
			err)
	}

	merkleRoot, err := tapsdk.ParseHash(rpcUtxo.MerkleRoot)
	if err != nil {
		return nil, fmt.Errorf("invalid merkle root: %w", err)
	}

	assets := make([]*tapsdk.AssetRecord, 0, len(rpcUtxo.Assets))
	for _, rpcAsset := range rpcUtxo.Assets {
		asset, err := unmarshalAsset(rpcAsset)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal "+
				"asset: %w", err)
		}

		assets = append(assets, asset)
	}

	leaseExpiry := max(rpcUtxo.LeaseExpiryUnix, int64(0))

	return &tapsdk.ManagedUtxo{
		OutPoint:         outPoint,
		AmtSat:           rpcUtxo.AmtSat,
		InternalKey:      internalKey,
		TaprootAssetRoot: taprootAssetRoot,
		MerkleRoot:       merkleRoot,
		Assets:           assets,
		LeaseOwner:       rpcUtxo.LeaseOwner,
		LeaseExpiryUnix:  leaseExpiry,
	}, nil
}

// unmarshalAssetGroupRecord converts an RPC GroupedAssets to an
// tapsdk.AssetGroupRecord. The groupKeyHex is the map key tapd returns,
// which may be either 33-byte compressed or 32-byte x-only hex.
func unmarshalAssetGroupRecord(groupKeyHex string,
	rpcGroup *taprpc.GroupedAssets) (*tapsdk.AssetGroupRecord, error) {

	if rpcGroup == nil {
		return nil, fmt.Errorf("nil grouped assets")
	}

	groupKey, err := tapsdk.ParseGroupRefKey(groupKeyHex)
	if err != nil {
		return nil, fmt.Errorf("invalid group key: %w", err)
	}
	groupRef := tapsdk.AssetRefFromGroupKey(groupKey)

	members := make(
		[]*tapsdk.AssetGroupMember, 0, len(rpcGroup.Assets),
	)

	for _, rpcAsset := range rpcGroup.Assets {
		if rpcAsset == nil {
			return nil, fmt.Errorf("nil asset in group")
		}

		assetID, err := tapsdk.ParseAssetID(rpcAsset.Id)
		if err != nil {
			return nil, fmt.Errorf("invalid asset ID: %w",
				err)
		}

		metaHash, err := tapsdk.ParseHash(rpcAsset.MetaHash)
		if err != nil {
			return nil, fmt.Errorf("invalid meta hash: %w",
				err)
		}

		assetType, err := unmarshalAssetType(rpcAsset.Type)
		if err != nil {
			return nil, err
		}

		members = append(members, &tapsdk.AssetGroupMember{
			AssetRef:         groupRef,
			IssuanceID:       assetID,
			Amount:           rpcAsset.Amount,
			LockTime:         rpcAsset.LockTime,
			RelativeLockTime: rpcAsset.RelativeLockTime,
			Tag:              rpcAsset.Tag,
			MetaHash:         metaHash,
			Type:             assetType,
			Version:          uint8(rpcAsset.Version),
		})
	}

	return &tapsdk.AssetGroupRecord{
		AssetRef: groupRef,
		Members:  members,
	}, nil
}

// unmarshalBurnRecord converts an RPC AssetBurn to an tapsdk.BurnRecord.
func unmarshalBurnRecord(
	rpcBurn *taprpc.AssetBurn) (*tapsdk.BurnRecord, error) {

	if rpcBurn == nil {
		return nil, fmt.Errorf("nil asset burn")
	}

	assetID, err := tapsdk.ParseAssetID(rpcBurn.AssetId)
	if err != nil {
		return nil, fmt.Errorf("invalid asset ID: %w", err)
	}

	anchorTxid, err := tapsdk.ParseHash(rpcBurn.AnchorTxid)
	if err != nil {
		return nil, fmt.Errorf("invalid anchor txid: %w", err)
	}

	assetType, err := unmarshalBurnAssetType(rpcBurn.AssetType)
	if err != nil {
		return nil, err
	}
	if assetType == tapsdk.AssetTypeCollectible && rpcBurn.Amount != 1 {
		return nil, fmt.Errorf("invalid collectible burn amount: %d",
			rpcBurn.Amount)
	}

	var groupKey *tapsdk.PubKey
	if len(rpcBurn.TweakedGroupKey) > 0 {
		parsedGroupKey, err := tapsdk.ParsePubKey(
			rpcBurn.TweakedGroupKey,
		)
		if err != nil {
			return nil, fmt.Errorf("invalid tweaked group "+
				"key: %w", err)
		}

		groupKey = &parsedGroupKey
	}

	var collectionRef *tapsdk.AssetRef
	if assetType == tapsdk.AssetTypeCollectible && groupKey != nil {
		ref := tapsdk.AssetRefFromGroupKey(*groupKey)
		collectionRef = &ref
	}

	burn := &tapsdk.BurnRecord{
		Note:          rpcBurn.Note,
		AssetRef:      tapsdk.AssetRefFromTypedAsset(assetID, groupKey, assetType),
		CollectionRef: collectionRef,
		Type:          assetType,
		IssuanceID:    assetID,
		Amount:        rpcBurn.Amount,
		AnchorTxid:    anchorTxid,
	}

	return burn, nil
}

// unmarshalBurnAssetType converts the tapd burn asset type enum to the SDK
// type. Burn history rejects unknown values because the type determines how
// the row is keyed by AssetRef.
func unmarshalBurnAssetType(assetType taprpc.AssetType) (tapsdk.AssetType,
	error) {

	switch assetType {
	case taprpc.AssetType_NORMAL:
		return tapsdk.AssetTypeFungible, nil

	case taprpc.AssetType_COLLECTIBLE:
		return tapsdk.AssetTypeNFT, nil

	default:
		return 0, fmt.Errorf("unknown burn asset type: %v", assetType)
	}
}

// marshalBurnAssetRequest converts a BurnAssetRequest to an RPC request.
func marshalBurnAssetRequest(
	req *tapsdk.BurnAssetRequest) (*taprpc.BurnAssetRequest, error) {

	spec, err := marshalAssetSpecifier(req.AssetRef)
	if err != nil {
		return nil, err
	}

	return &taprpc.BurnAssetRequest{
		AmountToBurn:     req.AmountToBurn,
		ConfirmationText: req.ConfirmationText,
		Note:             req.Note,
		AssetSpecifier:   spec,
	}, nil
}

// marshalAssetSpecifier converts an AssetRef to a taprpc AssetSpecifier.
func marshalAssetSpecifier(
	ref tapsdk.AssetRef) (*taprpc.AssetSpecifier, error) {

	if ref.IsZero() {
		return nil, fmt.Errorf("asset ref is required")
	}

	if err := ref.Validate(); err != nil {
		return nil, err
	}

	if groupKey, ok := ref.GroupKey(); ok {
		return &taprpc.AssetSpecifier{
			Id: &taprpc.AssetSpecifier_GroupKey{
				GroupKey: groupKey[:],
			},
		}, nil
	}

	if assetID, ok := ref.AssetID(); ok {
		return &taprpc.AssetSpecifier{
			Id: &taprpc.AssetSpecifier_AssetId{
				AssetId: assetID[:],
			},
		}, nil
	}

	return nil, fmt.Errorf("asset ref must contain an " +
		"asset ID or group key")
}

// marshalFetchAssetMetaRequest converts a FetchAssetMetaRequest to an
// RPC request.
func marshalFetchAssetMetaRequest(
	req *tapsdk.FetchAssetMetaRequest) (*taprpc.FetchAssetMetaRequest,
	error) {

	rpcReq := &taprpc.FetchAssetMetaRequest{}

	if req == nil {
		return rpcReq, nil
	}

	if req.AssetRef != nil {
		if err := req.AssetRef.Validate(); err != nil {
			return nil, err
		}

		assetID, ok := req.AssetRef.AssetID()
		if !ok {
			return nil, fmt.Errorf("metadata lookup " +
				"requires an asset-ID ref; tapd does " +
				"not support group-key metadata lookup")
		}

		rpcReq.Asset = &taprpc.FetchAssetMetaRequest_AssetId{
			AssetId: assetID[:],
		}
	} else if req.MetaHash != nil {
		rpcReq.Asset = &taprpc.FetchAssetMetaRequest_MetaHash{
			MetaHash: req.MetaHash[:],
		}
	}

	return rpcReq, nil
}

// unmarshalFetchAssetMetaResponse converts an RPC FetchAssetMetaResponse
// to an tapsdk.AssetMeta.
func unmarshalFetchAssetMetaResponse(
	resp *taprpc.FetchAssetMetaResponse) (*tapsdk.AssetMeta, error) {

	if resp == nil {
		return nil, fmt.Errorf("nil asset meta response")
	}

	metaHash, err := tapsdk.ParseHash(resp.MetaHash)
	if err != nil {
		return nil, fmt.Errorf("invalid meta hash: %w", err)
	}

	unknownOddTypes := make(
		map[uint64][]byte, len(resp.UnknownOddTypes),
	)
	maps.Copy(unknownOddTypes, resp.UnknownOddTypes)

	return &tapsdk.AssetMeta{
		Data:                  resp.Data,
		Type:                  tapsdk.AssetMetaType(resp.Type),
		MetaHash:              metaHash,
		UnknownOddTypes:       unknownOddTypes,
		DecimalDisplay:        resp.DecimalDisplay,
		UniverseCommitments:   resp.UniverseCommitments,
		CanonicalUniverseURLs: resp.CanonicalUniverseUrls,
		DelegationKey:         resp.DelegationKey,
	}, nil
}

// unmarshalDecodedProof converts an RPC DecodedProof to an
// tapsdk.DecodedProof. This is extracted from proof_client.go for
// reuse in BurnAsset and VerifyProof.
func unmarshalDecodedProof(
	rpcProof *taprpc.DecodedProof) (*tapsdk.DecodedProof, error) {

	if rpcProof == nil {
		return nil, fmt.Errorf("nil decoded proof")
	}

	if rpcProof.Asset == nil {
		return nil, fmt.Errorf("nil proof asset")
	}

	asset := rpcProof.Asset
	if asset.AssetGenesis == nil {
		return nil, fmt.Errorf("nil proof asset genesis")
	}

	assetID, err := tapsdk.ParseAssetID(asset.AssetGenesis.AssetId)
	if err != nil {
		return nil, fmt.Errorf("invalid asset ID: %w", err)
	}
	assetType, err := unmarshalAssetType(asset.AssetGenesis.AssetType)
	if err != nil {
		return nil, fmt.Errorf("invalid asset type: %w", err)
	}

	scriptKey, err := tapsdk.ParsePubKey(asset.ScriptKey)
	if err != nil {
		return nil, fmt.Errorf("invalid script key: %w", err)
	}

	proof := &tapsdk.DecodedProof{
		ProofAtDepth:   rpcProof.ProofAtDepth,
		NumberOfProofs: rpcProof.NumberOfProofs,
		AssetRef:       tapsdk.AssetRefFromTypedAsset(assetID, nil, assetType),
		IssuanceID:     assetID,
		ScriptKey:      scriptKey,
		Amount:         asset.Amount,
		IsIssuance:     rpcProof.GenesisReveal != nil,
	}

	if asset.ChainAnchor != nil {
		op, err := tapsdk.NewOutpointFromStr(
			asset.ChainAnchor.AnchorOutpoint,
		)
		if err != nil {
			return nil, fmt.Errorf("invalid anchor "+
				"outpoint: %w", err)
		}

		proof.Outpoint = op
	}

	if asset.AssetGroup != nil &&
		len(asset.AssetGroup.TweakedGroupKey) > 0 {

		groupKey, err := tapsdk.ParsePubKey(
			asset.AssetGroup.TweakedGroupKey,
		)
		if err != nil {
			return nil, fmt.Errorf("invalid group key: %w",
				err)
		}

		proof.AssetRef = tapsdk.AssetRefFromTypedAsset(
			assetID, &groupKey, assetType,
		)
	}

	if len(rpcProof.AltLeaves) > 0 {
		altLeaves, err := codec.DecodeAltLeaves(rpcProof.AltLeaves)
		if err != nil {
			return nil, fmt.Errorf("decode alt leaves: %w", err)
		}

		proof.AltLeaves = altLeaves
	}

	if len(asset.PrevWitnesses) > 0 {
		proof.PrevIDs = make(
			[]tapsdk.PrevID, 0, len(asset.PrevWitnesses),
		)
	}
	for idx, witness := range asset.PrevWitnesses {
		if witness == nil || witness.PrevId == nil {
			return nil, fmt.Errorf("missing prev_id for witness %d", idx)
		}

		prev := witness.PrevId
		prevOutpoint, err := tapsdk.NewOutpointFromStr(prev.AnchorPoint)
		if err != nil {
			return nil, fmt.Errorf("invalid prev_id outpoint for "+
				"witness %d: %w", idx, err)
		}

		prevAssetID, err := tapsdk.ParseAssetID(prev.AssetId)
		if err != nil {
			return nil, fmt.Errorf("invalid prev_id asset ID for "+
				"witness %d: %w", idx, err)
		}
		if isZeroDecodedProofPrevID(
			prevOutpoint, prevAssetID, prev.ScriptKey,
		) {

			if rpcProof.GenesisReveal == nil &&
				witness.SplitCommitment == nil {

				return nil, fmt.Errorf("zero prev_id is only valid for "+
					"issuance or split witness %d", idx)
			}

			continue
		}

		prevScriptKey, err := tapsdk.ParsePubKey(prev.ScriptKey)
		if err != nil {
			return nil, fmt.Errorf("invalid prev_id script key for "+
				"witness %d: %w", idx, err)
		}

		proof.PrevIDs = append(proof.PrevIDs, tapsdk.PrevID{
			Outpoint:   prevOutpoint,
			IssuanceID: prevAssetID,
			ScriptKey:  prevScriptKey,
		})
	}

	return proof, nil
}

func isZeroDecodedProofPrevID(outpoint tapsdk.Outpoint,
	assetID tapsdk.AssetID, scriptKey []byte) bool {

	if outpoint != (tapsdk.Outpoint{}) || assetID != (tapsdk.AssetID{}) ||
		len(scriptKey) != len(tapsdk.PubKey{}) {

		return false
	}
	for _, value := range scriptKey {
		if value != 0 {
			return false
		}
	}

	return true
}
