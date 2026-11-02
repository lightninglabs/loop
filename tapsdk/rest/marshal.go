package rest

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	tapsdk "github.com/lightninglabs/loop/tapsdk"
)

const (
	assetTypeNormalJSON      = "NORMAL"
	assetTypeCollectibleJSON = "COLLECTIBLE"
)

// parseHexBytes decodes a hex string into a byte slice, returning nil
// for empty input. tapd's REST gateway uses UseHexForBytes, so all
// proto `bytes` fields in JSON bodies travel as hex.
func parseHexBytes(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}

	return hex.DecodeString(s)
}

// parseBase64Bytes decodes a base64 string into a byte slice. Only a
// few endpoints (WebSocket streaming, grpc-gateway path params) still
// ship bytes as base64; proto3 JSON allows any of the four variants so
// we try each in turn.
func parseBase64Bytes(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}

	encodings := []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	}

	var lastErr error
	for _, enc := range encodings {
		out, err := enc.DecodeString(s)
		if err == nil {
			return out, nil
		}
		lastErr = err
	}

	return nil, lastErr
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}

	return ""
}

// parseUint64 parses a JSON string-encoded uint64. The gRPC-gateway
// encodes uint64 values as strings in JSON.
func parseUint64(s string) (uint64, error) {
	if s == "" {
		return 0, nil
	}

	return strconv.ParseUint(s, 10, 64)
}

// parseInt64 parses a JSON string-encoded int64.
func parseInt64(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}

	return strconv.ParseInt(s, 10, 64)
}

// parseAssetType converts a proto enum string to tapsdk.AssetType.
func parseAssetType(s string) (tapsdk.AssetType, error) {
	switch s {
	case assetTypeNormalJSON:
		return tapsdk.AssetTypeFungible, nil

	case assetTypeCollectibleJSON:
		return tapsdk.AssetTypeNFT, nil

	default:
		return 0, fmt.Errorf("unknown asset_type: %s", s)
	}
}

// parseBurnAssetType converts a burn asset_type enum string to
// tapsdk.AssetType. Burn history rejects unknown values because the type
// determines how the row is keyed by AssetRef.
func parseBurnAssetType(s string) (tapsdk.AssetType, error) {
	switch s {
	case assetTypeNormalJSON:
		return tapsdk.AssetTypeFungible, nil

	case assetTypeCollectibleJSON:
		return tapsdk.AssetTypeNFT, nil

	default:
		return 0, fmt.Errorf("unknown burn asset_type: %s", s)
	}
}

// parseAssetVersion converts a proto enum string to
// tapsdk.AssetVersion.
func parseAssetVersion(s string) tapsdk.AssetVersion {
	switch s {
	case "ASSET_VERSION_V1":
		return tapsdk.AssetVersionV1
	default:
		return tapsdk.AssetVersionV0
	}
}

// parseAddressVersion converts a proto enum string to
// tapsdk.AddressVersion.
func parseAddressVersion(s string) tapsdk.AddressVersion {
	switch s {
	case "ADDR_VERSION_V1":
		return tapsdk.AddressVersionV1
	case "ADDR_VERSION_V2":
		return tapsdk.AddressVersionV2
	default:
		return tapsdk.AddressVersionV0
	}
}

// parseBackupMode converts a proto enum string to tapsdk.BackupMode.
func parseBackupMode(s string) tapsdk.BackupMode {
	switch s {
	case "COMPACT":
		return tapsdk.BackupModeCompact
	case "OPTIMISTIC":
		return tapsdk.BackupModeOptimistic
	default:
		return tapsdk.BackupModeRaw
	}
}

// parseAddressEventStatus converts a proto enum string to
// tapsdk.AddressEventStatus.
func parseAddressEventStatus(s string) tapsdk.AddressEventStatus {
	switch s {
	case "ADDR_EVENT_STATUS_TRANSACTION_DETECTED":
		return tapsdk.AddressEventStatusTransactionDetected
	case "ADDR_EVENT_STATUS_TRANSACTION_CONFIRMED":
		return tapsdk.AddressEventStatusTransactionConfirmed
	case "ADDR_EVENT_STATUS_PROOF_RECEIVED":
		return tapsdk.AddressEventStatusProofReceived
	case "ADDR_EVENT_STATUS_COMPLETED":
		return tapsdk.AddressEventStatusCompleted
	default:
		return tapsdk.AddressEventStatusUnknown
	}
}

// parseBatchState converts a proto enum string to tapsdk.BatchState.
func parseBatchState(s string) tapsdk.BatchState {
	switch s {
	case "BATCH_STATE_PENDING":
		return tapsdk.BatchStatePending
	case "BATCH_STATE_FROZEN":
		return tapsdk.BatchStateFrozen
	case "BATCH_STATE_COMMITTED":
		return tapsdk.BatchStateCommitted
	case "BATCH_STATE_BROADCAST":
		return tapsdk.BatchStateBroadcast
	case "BATCH_STATE_CONFIRMED":
		return tapsdk.BatchStateConfirmed
	case "BATCH_STATE_FINALIZED":
		return tapsdk.BatchStateFinalized
	case "BATCH_STATE_SEEDLING_CANCELLED":
		return tapsdk.BatchStateSeedlingCancelled
	case "BATCH_STATE_SPROUT_CANCELLED":
		return tapsdk.BatchStateSproutCancelled
	default:
		return tapsdk.BatchStatePending
	}
}

// parseAssetMetaType converts a proto enum string to
// tapsdk.AssetMetaType.
func parseAssetMetaType(s string) tapsdk.AssetMetaType {
	switch s {
	case "META_TYPE_JSON":
		return tapsdk.AssetMetaTypeJSON
	default:
		return tapsdk.AssetMetaTypeOpaque
	}
}

// unmarshalInfo converts a JSON info response to tapsdk.Info.
func unmarshalInfo(resp *jsonGetInfoResponse) (*tapsdk.Info, error) {
	pubKeyBytes, err := parseHexBytes(resp.LndIdentityPubkey)
	if err != nil {
		return nil, fmt.Errorf("invalid lnd_identity_pubkey: %w", err)
	}

	var pubKey [33]byte
	copy(pubKey[:], pubKeyBytes)

	return &tapsdk.Info{
		Version:           resp.Version,
		LndVersion:        resp.LndVersion,
		Network:           resp.Network,
		LndIdentityPubkey: pubKey,
		NodeAlias:         resp.NodeAlias,
		BlockHeight:       resp.BlockHeight,
		SyncedToChain:     resp.SyncToChain,
	}, nil
}

// unmarshalIssuanceGenesis converts a JSON asset_genesis wire field into the
// SDK's concrete issuance genesis type.
func unmarshalIssuanceGenesis(
	g *jsonGenesisInfo) (*tapsdk.IssuanceGenesis, error) {

	if g == nil {
		return nil, fmt.Errorf("nil asset genesis")
	}

	if g.GenesisPoint == "" {
		return nil, fmt.Errorf("missing genesis point")
	}

	firstPrevOut, err := tapsdk.NewOutpointFromStr(g.GenesisPoint)
	if err != nil {
		return nil, fmt.Errorf("invalid genesis point: %w", err)
	}

	assetIDBytes, err := parseHexBytes(g.AssetID)
	if err != nil {
		return nil, fmt.Errorf("invalid asset ID: %w", err)
	}

	if len(assetIDBytes) != 32 {
		return nil, fmt.Errorf("invalid asset ID length: %d",
			len(assetIDBytes))
	}

	var assetID [32]byte
	copy(assetID[:], assetIDBytes)

	var metaHash [32]byte
	metaHashBytes, err := parseHexBytes(g.MetaHash)
	if err != nil {
		return nil, fmt.Errorf("invalid meta hash: %w", err)
	}

	if len(metaHashBytes) == 32 {
		copy(metaHash[:], metaHashBytes)
	}

	assetType, err := parseAssetType(g.AssetType)
	if err != nil {
		return nil, err
	}

	return &tapsdk.IssuanceGenesis{
		FirstPrevOut: firstPrevOut,
		Tag:          g.Name,
		MetaHash:     metaHash,
		IssuanceID:   assetID,
		OutputIndex:  g.OutputIndex,
		Type:         assetType,
	}, nil
}

// unmarshalAsset converts a JSON asset to tapsdk.AssetRecord.
func unmarshalAsset(a *jsonAsset) (*tapsdk.AssetRecord, error) {
	if a == nil {
		return nil, fmt.Errorf("nil asset")
	}

	genesis, err := unmarshalIssuanceGenesis(a.AssetGenesis)
	if err != nil {
		return nil, err
	}

	scriptKeyBytes, err := parseHexBytes(a.ScriptKey)
	if err != nil {
		return nil, fmt.Errorf("invalid script key: %w", err)
	}

	if len(scriptKeyBytes) != 33 {
		return nil, fmt.Errorf("invalid script key length: %d",
			len(scriptKeyBytes))
	}

	var scriptKeyPub [33]byte
	copy(scriptKeyPub[:], scriptKeyBytes)

	amount, err := parseUint64(a.Amount)
	if err != nil {
		return nil, fmt.Errorf("invalid amount: %w", err)
	}

	asset := &tapsdk.AssetRecord{
		Version:          uint8(parseAssetVersion(a.Version)),
		Genesis:          *genesis,
		Amount:           amount,
		LockTime:         uint64(a.LockTime),
		RelativeLockTime: uint64(a.RelativeLockTime),
		ScriptVersion:    uint16(a.ScriptVersion),
		ScriptKey: tapsdk.ScriptKey{
			PubKey: scriptKeyPub,
		},
	}
	if a.ChainAnchor != nil && a.ChainAnchor.AnchorOutpoint != "" {
		raw := a.ChainAnchor.AnchorOutpoint
		point, err := tapsdk.NewOutpointFromStr(raw)
		if err != nil || point.String() != raw {
			return nil, fmt.Errorf(
				"invalid canonical anchor outpoint",
			)
		}
		asset.AnchorOutpoint = &point
	}

	// tapd keys every queryable map by the tweaked group key, so
	// that's what AssetRef must encode.
	var tweakedGroupKey *tapsdk.PubKey
	if a.AssetGroup != nil && a.AssetGroup.TweakedGroupKey != "" {
		tweaked, err := parseHexBytes(a.AssetGroup.TweakedGroupKey)
		if err != nil {
			return nil, fmt.Errorf(
				"invalid tweaked group key: %w", err,
			)
		}
		if len(tweaked) == 33 {
			var gk tapsdk.PubKey
			copy(gk[:], tweaked)
			tweakedGroupKey = &gk
		}
	}

	asset.AssetRef = tapsdk.AssetRefFromAsset(
		asset.Genesis.IssuanceID, tweakedGroupKey,
	)

	return asset, nil
}

// unmarshalAddr converts a JSON addr to tapsdk.Address.
func unmarshalAddr(a *jsonAddr) (*tapsdk.Address, error) {
	if a == nil {
		return nil, fmt.Errorf("nil address")
	}

	scriptKeyBytes, err := parseHexBytes(a.ScriptKey)
	if err != nil {
		return nil, fmt.Errorf("invalid script key: %w", err)
	}

	if len(scriptKeyBytes) != 33 {
		return nil, fmt.Errorf("invalid script key length: %d",
			len(scriptKeyBytes))
	}

	internalKeyBytes, err := parseHexBytes(a.InternalKey)
	if err != nil {
		return nil, fmt.Errorf("invalid internal key: %w", err)
	}

	if len(internalKeyBytes) != 33 {
		return nil, fmt.Errorf("invalid internal key length: %d",
			len(internalKeyBytes))
	}

	taprootOutputKeyBytes, err := parseHexBytes(a.TaprootOutputKey)
	if err != nil {
		return nil, fmt.Errorf("invalid taproot output key: %w", err)
	}

	if len(taprootOutputKeyBytes) != 32 {
		return nil, fmt.Errorf(
			"invalid taproot output key length: %d",
			len(taprootOutputKeyBytes),
		)
	}

	assetType, err := parseAssetType(a.AssetType)
	if err != nil {
		return nil, err
	}

	addr := &tapsdk.Address{
		Encoded:          a.Encoded,
		AssetType:        assetType,
		ProofCourierAddr: a.ProofCourierAddr,
		AssetVersion:     parseAssetVersion(a.AssetVersion),
		AddressVersion:   parseAddressVersion(a.AddressVersion),
	}

	amount, err := parseUint64(a.Amount)
	if err != nil {
		return nil, fmt.Errorf("invalid amount: %w", err)
	}

	addr.Amount = amount

	copy(addr.ScriptKey[:], scriptKeyBytes)
	copy(addr.InternalKey[:], internalKeyBytes)
	copy(addr.TaprootOutputKey[:], taprootOutputKeyBytes)

	var assetID *tapsdk.AssetID
	assetIDBytes, err := parseHexBytes(a.AssetID)
	if err != nil {
		return nil, fmt.Errorf("invalid asset ID: %w", err)
	}

	if len(assetIDBytes) == 32 {
		var parsedAssetID tapsdk.AssetID
		copy(parsedAssetID[:], assetIDBytes)
		assetID = &parsedAssetID
	}

	var groupKey *tapsdk.PubKey
	groupKeyBytes, err := parseHexBytes(a.GroupKey)
	if err != nil {
		return nil, fmt.Errorf("invalid group key: %w", err)
	}

	if len(groupKeyBytes) == 33 {
		var parsedGroupKey tapsdk.PubKey
		copy(parsedGroupKey[:], groupKeyBytes)
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

	tapscriptSibling, err := parseHexBytes(a.TapscriptSibling)
	if err != nil {
		return nil, fmt.Errorf("invalid tapscript sibling: %w", err)
	}

	addr.TapscriptSibling = tapscriptSibling

	return addr, nil
}

// unmarshalAssetTransfer converts a JSON transfer to
// tapsdk.AssetTransfer.
func unmarshalAssetTransfer(
	t *jsonAssetTransfer) (*tapsdk.AssetTransfer, error) {

	if t == nil {
		return nil, fmt.Errorf("nil transfer")
	}

	var transferTxid [32]byte
	var anchorTxid string

	txHashBytes, err := parseHexBytes(t.AnchorTxHash)
	if err != nil {
		return nil, fmt.Errorf("invalid anchor_tx_hash: %w", err)
	}

	if len(txHashBytes) == 32 {
		copy(transferTxid[:], txHashBytes)

		var h chainhash.Hash
		copy(h[:], txHashBytes)
		anchorTxid = h.String()
	}

	outputs := make([]tapsdk.TransferOutput, 0, len(t.Outputs))
	for _, out := range t.Outputs {
		amount, err := parseUint64(out.Amount)
		if err != nil {
			return nil, fmt.Errorf("invalid output amount: %w",
				err)
		}

		proofBlob, err := parseHexBytes(out.NewProofBlob)
		if err != nil {
			return nil, fmt.Errorf("invalid proof blob: %w", err)
		}

		assetType, err := parseAssetType(out.AssetType)
		if err != nil {
			return nil, err
		}

		output := tapsdk.TransferOutput{
			Amount:    amount,
			AssetType: assetType,
			ProofBlob: proofBlob,
		}

		if out.AssetID != "" {
			assetIDBytes, err := parseHexBytes(out.AssetID)
			if err != nil {
				return nil, fmt.Errorf(
					"invalid output asset ID: %w", err,
				)
			}

			if len(assetIDBytes) != 32 {
				return nil, fmt.Errorf(
					"invalid output asset ID length: %d",
					len(assetIDBytes),
				)
			}
			copy(output.IssuanceID[:], assetIDBytes)
		}

		scriptKeyBytes, err := parseHexBytes(out.ScriptKey)
		if err != nil {
			return nil, fmt.Errorf("invalid output script key: "+
				"%w", err)
		}

		if len(scriptKeyBytes) == 33 {
			copy(output.ScriptKey[:], scriptKeyBytes)
		}

		if out.Anchor != nil {
			op, err := tapsdk.NewOutpointFromStr(
				out.Anchor.Outpoint,
			)
			if err != nil {
				return nil, fmt.Errorf("invalid anchor "+
					"outpoint: %w", err)
			}

			value, err := parseInt64(out.Anchor.Value)
			if err != nil {
				return nil, fmt.Errorf(
					"invalid anchor value: %w", err,
				)
			}

			output.AnchorOutpoint = op
			output.AnchorValue = value
		}

		// Group key is empty for ungrouped assets and on responses
		// from older daemons; the SDK treats either as "no group" and
		// falls back to the asset-id ref.
		if out.GroupKey != "" {
			groupKeyBytes, err := parseHexBytes(out.GroupKey)
			if err != nil {
				return nil, fmt.Errorf(
					"invalid output group key: %w", err,
				)
			}
			if len(groupKeyBytes) > 0 {
				groupKey, err := tapsdk.ParsePubKey(
					groupKeyBytes,
				)
				if err != nil {
					return nil, fmt.Errorf(
						"invalid output group key: %w",
						err,
					)
				}
				output.GroupKey = &groupKey
			}
		}

		outputs = append(outputs, output)
	}

	inputs := make([]tapsdk.TransferInput, 0, len(t.Inputs))
	for _, in := range t.Inputs {
		if in == nil {
			return nil, fmt.Errorf("nil transfer input")
		}

		assetIDBytes, err := parseHexBytes(in.AssetID)
		if err != nil {
			return nil, fmt.Errorf("invalid input asset ID: %w",
				err)
		}

		if len(assetIDBytes) != 32 {
			return nil, fmt.Errorf(
				"invalid input asset ID length: %d",
				len(assetIDBytes),
			)
		}

		scriptKeyBytes, err := parseHexBytes(in.ScriptKey)
		if err != nil {
			return nil, fmt.Errorf("invalid input script key: "+
				"%w", err)
		}

		if len(scriptKeyBytes) != 33 {
			return nil, fmt.Errorf(
				"invalid input script key length: %d",
				len(scriptKeyBytes),
			)
		}

		amount, err := parseUint64(in.Amount)
		if err != nil {
			return nil, fmt.Errorf("invalid input amount: %w",
				err)
		}

		anchorPoint, err := tapsdk.NewOutpointFromStr(
			in.AnchorPoint,
		)
		if err != nil {
			return nil, fmt.Errorf("invalid anchor point: %w",
				err)
		}

		var assetID [32]byte
		copy(assetID[:], assetIDBytes)

		var scriptKey [33]byte
		copy(scriptKey[:], scriptKeyBytes)

		assetType, err := parseAssetType(in.AssetType)
		if err != nil {
			return nil, err
		}

		input := tapsdk.TransferInput{
			AnchorPoint: anchorPoint,
			IssuanceID:  assetID,
			AssetType:   assetType,
			ScriptKey:   scriptKey,
			Amount:      amount,
		}

		// Group key is empty for ungrouped assets and on responses
		// from older daemons; the SDK treats either as "no group" and
		// falls back to the asset-id ref.
		if in.GroupKey != "" {
			groupKeyBytes, err := parseHexBytes(in.GroupKey)
			if err != nil {
				return nil, fmt.Errorf(
					"invalid input group key: %w", err,
				)
			}
			if len(groupKeyBytes) > 0 {
				groupKey, err := tapsdk.ParsePubKey(
					groupKeyBytes,
				)
				if err != nil {
					return nil, fmt.Errorf(
						"invalid input group key: %w",
						err,
					)
				}
				input.GroupKey = &groupKey
			}
		}

		inputs = append(inputs, input)
	}

	var blockHash [32]byte
	var blockHashStr string

	if t.AnchorTxBlockHash != nil {
		blockHashStr = t.AnchorTxBlockHash.HashStr

		blockHashBytes, err := parseHexBytes(
			t.AnchorTxBlockHash.Hash,
		)
		if err != nil {
			return nil, fmt.Errorf("invalid block hash: %w", err)
		}

		if len(blockHashBytes) == 32 {
			copy(blockHash[:], blockHashBytes)
		}
	}

	anchorTxBytes, err := parseHexBytes(t.AnchorTx)
	if err != nil {
		return nil, fmt.Errorf("invalid anchor_tx: %w", err)
	}

	transferTimestamp, err := parseInt64(t.TransferTimestamp)
	if err != nil {
		return nil, fmt.Errorf("invalid transfer_timestamp: %w", err)
	}

	anchorChainFees, err := parseInt64(t.AnchorTxChainFees)
	if err != nil {
		return nil, fmt.Errorf(
			"invalid anchor_tx_chain_fees: %w", err,
		)
	}

	return &tapsdk.AssetTransfer{
		TransferTimestamp:    transferTimestamp,
		TransferTxid:         transferTxid,
		AnchorTxid:           anchorTxid,
		AnchorTxHeightHint:   t.AnchorTxHeightHint,
		AnchorTxChainFees:    anchorChainFees,
		Inputs:               inputs,
		Outputs:              outputs,
		AnchorTxBlockHash:    blockHash,
		AnchorTxBlockHashStr: blockHashStr,
		AnchorTxBlockHeight:  t.AnchorTxBlockHeight,
		Label:                t.Label,
		AnchorTx:             anchorTxBytes,
	}, nil
}

// unmarshalAddrEvent converts a JSON addr event to
// tapsdk.AddressEvent.
func unmarshalAddrEvent(
	e *jsonAddrEvent) (*tapsdk.AddressEvent, error) {

	if e == nil {
		return nil, fmt.Errorf("nil address event")
	}

	creationTime, err := parseUint64(e.CreationTimeUnixSeconds)
	if err != nil {
		return nil, fmt.Errorf("invalid creation time: %w", err)
	}

	utxoAmtSat, err := parseUint64(e.UtxoAmtSat)
	if err != nil {
		return nil, fmt.Errorf("invalid utxo amount: %w", err)
	}

	event := &tapsdk.AddressEvent{
		CreationTime:       creationTime,
		Status:             parseAddressEventStatus(e.Status),
		Outpoint:           e.Outpoint,
		UTXOAmountSat:      utxoAmtSat,
		ConfirmationHeight: e.ConfirmationHeight,
		HasProof:           e.HasProof,
	}

	taprootSibling, err := parseHexBytes(e.TaprootSibling)
	if err != nil {
		return nil, fmt.Errorf("invalid taproot sibling: %w", err)
	}

	event.TaprootSibling = taprootSibling

	if e.Addr != nil {
		addr, err := unmarshalAddr(e.Addr)
		if err != nil {
			return nil, fmt.Errorf(
				"invalid address in event: %w", err,
			)
		}

		event.Address = addr
	}

	return event, nil
}

// unmarshalScriptKey converts a JSON script key to
// tapsdk.ScriptKey.
func unmarshalScriptKey(k *jsonScriptKey) (*tapsdk.ScriptKey, error) {
	if k == nil {
		return nil, fmt.Errorf("nil script key")
	}

	pubKeyBytes, err := parseHexBytes(k.PubKey)
	if err != nil {
		return nil, fmt.Errorf("invalid script key pub_key: %w", err)
	}

	pubKey, err := tapsdk.ParseTaprootPubKey(pubKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("invalid script key public key: %w",
			err)
	}

	tapTweak, err := parseHexBytes(k.TapTweak)
	if err != nil {
		return nil, fmt.Errorf("invalid tap_tweak: %w", err)
	}

	scriptKey := &tapsdk.ScriptKey{
		PubKey:   pubKey,
		TapTweak: tapTweak,
	}

	if k.KeyDesc != nil {
		keyDesc, err := unmarshalKeyDescriptor(k.KeyDesc)
		if err != nil {
			return nil, err
		}

		scriptKey.KeyDesc = *keyDesc
	}

	return scriptKey, nil
}

// unmarshalKeyDescriptor converts a JSON key descriptor to
// tapsdk.KeyDescriptor.
func unmarshalKeyDescriptor(
	d *jsonKeyDescriptor) (*tapsdk.KeyDescriptor, error) {

	if d == nil {
		return nil, fmt.Errorf("nil key descriptor")
	}

	rawKeyBytes, err := parseHexBytes(d.RawKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("invalid raw_key_bytes: %w", err)
	}

	parsedKey, err := tapsdk.ParsePubKey(rawKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("invalid raw key bytes: %w", err)
	}

	keyDesc := &tapsdk.KeyDescriptor{RawKeyBytes: parsedKey}

	if d.KeyLoc != nil {
		keyDesc.KeyLocator = tapsdk.KeyLocator{
			Family: uint32(d.KeyLoc.KeyFamily),
			Index:  uint32(d.KeyLoc.KeyIndex),
		}
	}

	return keyDesc, nil
}

// unmarshalMintingBatch converts a JSON minting batch to
// tapsdk.MintingBatch.
func unmarshalMintingBatch(
	b *jsonMintingBatch) (*tapsdk.MintingBatch, error) {

	if b == nil {
		return nil, fmt.Errorf("nil minting batch")
	}

	createdAt, err := parseInt64(b.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("invalid created_at: %w", err)
	}

	batchPsbt, err := parseHexBytes(b.BatchPsbt)
	if err != nil {
		return nil, fmt.Errorf("invalid batch_psbt: %w", err)
	}

	batch := &tapsdk.MintingBatch{
		BatchTxid:  b.BatchTxid,
		State:      parseBatchState(b.State),
		CreatedAt:  createdAt,
		HeightHint: b.HeightHint,
		BatchPSBT:  batchPsbt,
	}

	if b.BatchKey != "" {
		batchKeyBytes, err := parseHexBytes(b.BatchKey)
		if err != nil {
			return nil, fmt.Errorf("invalid batch_key: %w", err)
		}

		batchKey, err := tapsdk.ParsePubKey(batchKeyBytes)
		if err != nil {
			return nil, fmt.Errorf("invalid batch key: %w", err)
		}

		batch.BatchKey = batchKey
	}

	batch.Assets = make(
		[]tapsdk.PendingMintAsset, 0, len(b.Assets),
	)
	for _, rpcAsset := range b.Assets {
		asset, err := unmarshalPendingMintAsset(rpcAsset)
		if err != nil {
			return nil, err
		}

		batch.Assets = append(batch.Assets, *asset)
	}

	return batch, nil
}

// unmarshalPendingMintAsset converts a JSON pending asset to
// tapsdk.PendingMintAsset.
func unmarshalPendingMintAsset(
	a *jsonPendingAsset) (*tapsdk.PendingMintAsset, error) {

	if a == nil {
		return nil, fmt.Errorf("nil pending mint asset")
	}

	amount, err := parseUint64(a.Amount)
	if err != nil {
		return nil, fmt.Errorf("invalid amount: %w", err)
	}

	tapscriptRoot, err := parseHexBytes(a.GroupTapscriptRoot)
	if err != nil {
		return nil, fmt.Errorf("invalid group tapscript root: %w",
			err)
	}

	assetType, err := parseAssetType(a.AssetType)
	if err != nil {
		return nil, err
	}

	asset := &tapsdk.PendingMintAsset{
		AssetVersion:       parseAssetVersion(a.AssetVersion),
		AssetType:          assetType,
		Name:               a.Name,
		Amount:             amount,
		NewGroupedAsset:    a.NewGroupedAsset,
		GroupAnchor:        a.GroupAnchor,
		GroupTapscriptRoot: tapscriptRoot,
	}

	if a.GroupKey != "" {
		groupKeyBytes, err := parseHexBytes(a.GroupKey)
		if err != nil {
			return nil, fmt.Errorf("invalid group key: %w", err)
		}

		groupKey, err := tapsdk.ParsePubKey(groupKeyBytes)
		if err != nil {
			return nil, fmt.Errorf("invalid group key: %w", err)
		}

		asset.GroupKey = &groupKey
	}

	if a.GroupInternalKey != nil {
		groupInternalKey, err := unmarshalKeyDescriptor(
			a.GroupInternalKey,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"invalid group internal key: %w", err,
			)
		}

		asset.GroupInternalKey = groupInternalKey
	}

	if a.ScriptKey != nil {
		scriptKey, err := unmarshalScriptKey(a.ScriptKey)
		if err != nil {
			return nil, fmt.Errorf("invalid script key: %w", err)
		}

		asset.ScriptKey = scriptKey
	}

	if a.AssetMeta != nil {
		meta, err := unmarshalAssetMeta(a.AssetMeta)
		if err != nil {
			return nil, err
		}

		asset.AssetMeta = meta
	}

	return asset, nil
}

// unmarshalUnsealedMintAsset converts a JSON unsealed asset to
// tapsdk.UnsealedMintAsset.
func unmarshalUnsealedMintAsset(
	a *jsonUnsealedAsset) (*tapsdk.UnsealedMintAsset, error) {

	if a == nil {
		return nil, fmt.Errorf("nil unsealed mint asset")
	}

	asset := &tapsdk.UnsealedMintAsset{
		GroupVirtualPSBT: a.GroupVirtualPSBT,
	}

	if a.Asset != nil {
		pendingAsset, err := unmarshalPendingMintAsset(a.Asset)
		if err != nil {
			return nil, err
		}

		asset.Asset = pendingAsset
	}

	if a.GroupKeyRequest != nil {
		groupKeyRequest, err := unmarshalGroupKeyRequest(
			a.GroupKeyRequest,
		)
		if err != nil {
			return nil, err
		}

		asset.GroupKeyRequest = groupKeyRequest
	}

	if a.GroupVirtualTx != nil {
		groupVirtualTx, err := unmarshalGroupVirtualTx(
			a.GroupVirtualTx,
		)
		if err != nil {
			return nil, err
		}

		asset.GroupVirtualTx = groupVirtualTx
	}

	return asset, nil
}

func unmarshalGroupKeyRequest(
	r *jsonGroupKeyRequest) (*tapsdk.GroupKeyRequest, error) {

	if r == nil {
		return nil, fmt.Errorf("nil group key request")
	}

	tapscriptRoot, err := parseHexBytes(r.TapscriptRoot)
	if err != nil {
		return nil, fmt.Errorf("invalid tapscript root: %w", err)
	}

	newAsset, err := parseHexBytes(r.NewAsset)
	if err != nil {
		return nil, fmt.Errorf("invalid new asset: %w", err)
	}

	request := &tapsdk.GroupKeyRequest{
		TapscriptRoot: tapscriptRoot,
		NewAsset:      newAsset,
	}

	if r.RawKey != nil {
		rawKey, err := unmarshalKeyDescriptor(r.RawKey)
		if err != nil {
			return nil, fmt.Errorf("invalid raw key: %w", err)
		}

		request.RawKey = rawKey
	}

	if r.AnchorGenesis != nil {
		anchorGenesis, err := unmarshalGenesisInfo(r.AnchorGenesis)
		if err != nil {
			return nil, err
		}

		request.AnchorGenesis = anchorGenesis
	}

	if r.ExternalKey != nil {
		externalKey, err := unmarshalExternalKey(r.ExternalKey)
		if err != nil {
			return nil, err
		}

		request.ExternalKey = externalKey
	}

	return request, nil
}

func unmarshalExternalKey(k *jsonExternalKey) (*tapsdk.ExternalKey, error) {
	if k == nil {
		return nil, fmt.Errorf("nil external key")
	}

	fingerprint, err := parseHexBytes(k.MasterFingerprint)
	if err != nil {
		return nil, fmt.Errorf("invalid master fingerprint: %w", err)
	}
	if len(fingerprint) != 4 {
		return nil, fmt.Errorf("invalid master fingerprint length: %d",
			len(fingerprint))
	}

	key := &tapsdk.ExternalKey{
		XPub:           k.XPub,
		DerivationPath: k.DerivationPath,
	}
	copy(key.MasterFingerprint[:], fingerprint)

	return key, nil
}

func unmarshalGenesisInfo(
	g *jsonGenesisInfo) (*tapsdk.GenesisInfo, error) {

	if g == nil {
		return nil, fmt.Errorf("nil genesis info")
	}

	metaHash, err := tapsdk.ParseHashHex(g.MetaHash)
	if err != nil {
		return nil, fmt.Errorf("invalid genesis meta hash: %w", err)
	}

	issuanceID, err := tapsdk.ParseAssetIDHex(g.AssetID)
	if err != nil {
		return nil, fmt.Errorf("invalid genesis issuance ID: %w", err)
	}

	assetType, err := parseAssetType(g.AssetType)
	if err != nil {
		return nil, err
	}

	return &tapsdk.GenesisInfo{
		GenesisPoint: g.GenesisPoint,
		Name:         g.Name,
		MetaHash:     metaHash,
		IssuanceID:   issuanceID,
		AssetType:    assetType,
		OutputIndex:  g.OutputIndex,
	}, nil
}

func unmarshalGroupVirtualTx(
	tx *jsonGroupVirtualTx) (*tapsdk.GroupVirtualTx, error) {

	if tx == nil {
		return nil, fmt.Errorf("nil group virtual tx")
	}

	transaction, err := parseHexBytes(tx.Transaction)
	if err != nil {
		return nil, fmt.Errorf("invalid group virtual tx: %w", err)
	}

	groupTx := &tapsdk.GroupVirtualTx{
		Transaction: transaction,
	}

	if tx.PrevOut != nil {
		value, err := parseInt64(tx.PrevOut.Value)
		if err != nil {
			return nil, fmt.Errorf("invalid prev out value: %w", err)
		}

		pkScript, err := parseHexBytes(tx.PrevOut.PkScript)
		if err != nil {
			return nil, fmt.Errorf("invalid prev out script: %w", err)
		}

		groupTx.PrevOut = &tapsdk.TxOut{
			Value:    value,
			PkScript: pkScript,
		}
	}

	if tx.GenesisID != "" {
		genesisID, err := tapsdk.ParseAssetIDHex(tx.GenesisID)
		if err != nil {
			return nil, fmt.Errorf("invalid group tx genesis ID: %w",
				err)
		}

		groupTx.GenesisID = genesisID
	}

	if tx.TweakedKey != "" {
		tweakedKeyBytes, err := parseHexBytes(tx.TweakedKey)
		if err != nil {
			return nil, fmt.Errorf("invalid tweaked key: %w", err)
		}

		tweakedKey, err := tapsdk.ParsePubKey(tweakedKeyBytes)
		if err != nil {
			return nil, fmt.Errorf("invalid tweaked key: %w", err)
		}

		groupTx.TweakedKey = &tweakedKey
	}

	return groupTx, nil
}

// unmarshalAssetMeta converts a JSON asset meta to
// tapsdk.AssetMeta.
func unmarshalAssetMeta(
	m *jsonAssetMeta) (*tapsdk.AssetMeta, error) {

	if m == nil {
		return nil, fmt.Errorf("nil asset meta")
	}

	// tapd's REST gateway is configured with UseHexForBytes, so
	// proto `bytes` fields are hex-encoded on the wire.
	data, err := parseHexBytes(m.Data)
	if err != nil {
		return nil, fmt.Errorf("invalid meta data: %w", err)
	}

	metaHashBytes, err := parseHexBytes(m.MetaHash)
	if err != nil {
		return nil, fmt.Errorf("invalid meta hash: %w", err)
	}

	metaHash, err := tapsdk.ParseHash(metaHashBytes)
	if err != nil {
		return nil, fmt.Errorf("invalid asset meta hash: %w", err)
	}

	return &tapsdk.AssetMeta{
		Data:     data,
		Type:     parseAssetMetaType(m.Type),
		MetaHash: metaHash,
	}, nil
}

// unmarshalManagedUtxo converts a JSON managed UTXO to an entity.
func unmarshalManagedUtxo(
	u *jsonManagedUtxo) (*tapsdk.ManagedUtxo, error) {

	if u == nil {
		return nil, fmt.Errorf("nil managed utxo")
	}

	amtSat, err := parseUint64(u.AmtSat)
	if err != nil {
		return nil, fmt.Errorf("invalid amt_sat: %w", err)
	}

	internalKey, err := parseHexBytes(u.InternalKey)
	if err != nil {
		return nil, fmt.Errorf("invalid internal_key: %w",
			err)
	}

	assets := make([]*tapsdk.AssetRecord, 0, len(u.Assets))
	for _, a := range u.Assets {
		asset, err := unmarshalAsset(a)
		if err != nil {
			return nil, fmt.Errorf("unmarshal utxo asset: %w",
				err)
		}
		assets = append(assets, asset)
	}

	taprootRoot, err := parseHexBytes(u.TaprootAssetRoot)
	if err != nil {
		return nil, fmt.Errorf("invalid taproot_root: %w", err)
	}

	merkleRoot, err := parseHexBytes(u.MerkleRoot)
	if err != nil {
		return nil, fmt.Errorf("invalid merkle_root: %w", err)
	}

	outpoint, err := tapsdk.NewOutpointFromStr(u.Outpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid outpoint: %w", err)
	}

	taprootRootHash, _ := tapsdk.ParseHash(taprootRoot)
	merkleRootHash, _ := tapsdk.ParseHash(merkleRoot)

	var pubKey tapsdk.PubKey
	copy(pubKey[:], internalKey)

	var leaseOwner []byte
	if u.LeaseOwner != "" {
		leaseOwner, err = parseHexBytes(u.LeaseOwner)
		if err != nil {
			return nil, fmt.Errorf("invalid lease_owner: %w", err)
		}
	}

	var leaseExpiry int64
	if u.LeaseExpiryUnix != "" {
		leaseExpiry, err = parseInt64(u.LeaseExpiryUnix)
		if err != nil {
			return nil, fmt.Errorf("invalid lease_expiry_unix: %w",
				err)
		}
		if leaseExpiry < 0 {
			leaseExpiry = 0
		}
	}

	return &tapsdk.ManagedUtxo{
		OutPoint:         outpoint,
		AmtSat:           int64(amtSat),
		InternalKey:      pubKey,
		TaprootAssetRoot: taprootRootHash,
		MerkleRoot:       merkleRootHash,
		Assets:           assets,
		LeaseOwner:       leaseOwner,
		LeaseExpiryUnix:  leaseExpiry,
	}, nil
}

// unmarshalAssetGroupRecord converts a JSON grouped assets to an entity. The
// groupKeyHex is the map key from the ListAssetGroups response, which tapd
// returns as either 33-byte compressed or 32-byte x-only hex.
func unmarshalAssetGroupRecord(groupKeyHex string,
	g *jsonGroupedAssets) (*tapsdk.AssetGroupRecord, error) {

	if g == nil {
		return nil, fmt.Errorf("nil grouped assets")
	}

	groupKey, err := tapsdk.ParseGroupRefKey(groupKeyHex)
	if err != nil {
		return nil, fmt.Errorf("invalid group key: %w", err)
	}
	groupRef := tapsdk.AssetRefFromGroupKey(groupKey)

	members := make(
		[]*tapsdk.AssetGroupMember, 0, len(g.Assets),
	)
	for _, a := range g.Assets {
		asset, err := unmarshalAssetGroupMember(a)
		if err != nil {
			return nil, fmt.Errorf(
				"unmarshal grouped asset: %w", err)
		}
		asset.AssetRef = groupRef
		members = append(members, asset)
	}

	return &tapsdk.AssetGroupRecord{
		AssetRef: groupRef,
		Members:  members,
	}, nil
}

// unmarshalAssetGroupMember converts tapd's simplified AssetHumanReadable JSON
// row into the SDK group member entity.
func unmarshalAssetGroupMember(
	a *jsonAssetHumanReadable) (*tapsdk.AssetGroupMember, error) {

	if a == nil {
		return nil, fmt.Errorf("nil asset")
	}

	amount, err := parseUint64(a.Amount)
	if err != nil {
		return nil, fmt.Errorf("invalid amount: %w", err)
	}

	var assetID tapsdk.AssetID
	if a.ID != "" {
		idBytes, err := parseHexBytes(a.ID)
		if err != nil {
			return nil, fmt.Errorf("invalid asset id: %w", err)
		}
		copy(assetID[:], idBytes)
	}

	var metaHash tapsdk.Hash
	if a.MetaHash != "" {
		hashBytes, err := parseHexBytes(a.MetaHash)
		if err != nil {
			return nil, fmt.Errorf("invalid meta hash: %w", err)
		}
		metaHash, _ = tapsdk.ParseHash(hashBytes)
	}

	assetType, err := parseAssetType(a.Type)
	if err != nil {
		return nil, err
	}

	return &tapsdk.AssetGroupMember{
		AssetRef:         tapsdk.AssetRefFromAssetID(assetID),
		IssuanceID:       assetID,
		Amount:           amount,
		LockTime:         a.LockTime,
		RelativeLockTime: a.RelativeLockTime,
		Tag:              a.Tag,
		MetaHash:         metaHash,
		Type:             assetType,
		Version:          uint8(parseAssetVersion(a.Version)),
	}, nil
}

// unmarshalBurnAssetResponse converts a JSON burn response to an
// entity.
func unmarshalBurnAssetResponse(
	r *jsonBurnAssetResponse) (*tapsdk.BurnAssetResponse,
	error) {

	if r == nil {
		return nil, fmt.Errorf("nil burn response")
	}

	var transfer *tapsdk.AssetTransfer
	if r.BurnTransfer != nil {
		var err error
		transfer, err = unmarshalAssetTransfer(r.BurnTransfer)
		if err != nil {
			return nil, fmt.Errorf(
				"unmarshal burn transfer: %w", err)
		}
	}

	return &tapsdk.BurnAssetResponse{
		BurnTransfer: transfer,
	}, nil
}

// unmarshalBurnRecord converts a JSON asset burn to an entity.
func unmarshalBurnRecord(
	b *jsonAssetBurn) (*tapsdk.BurnRecord, error) {

	if b == nil {
		return nil, fmt.Errorf("nil asset burn")
	}

	amount, err := parseUint64(b.Amount)
	if err != nil {
		return nil, fmt.Errorf("invalid burn amount: %w", err)
	}

	idBytes, err := parseHexBytes(b.AssetID)
	if err != nil {
		return nil, fmt.Errorf("invalid burn asset_id: %w",
			err)
	}
	assetID, err := tapsdk.ParseAssetID(idBytes)
	if err != nil {
		return nil, fmt.Errorf("invalid burn asset_id: %w", err)
	}

	groupKeyBytes, err := parseHexBytes(b.TweakedGroupKey)
	if err != nil {
		return nil, fmt.Errorf("invalid burn tweaked_group_key: %w",
			err)
	}

	var groupKey *tapsdk.PubKey
	if len(groupKeyBytes) > 0 {
		pk, err := tapsdk.ParsePubKey(groupKeyBytes)
		if err != nil {
			return nil, fmt.Errorf("invalid burn tweaked_group_key: "+
				"%w", err)
		}

		groupKey = &pk
	}

	assetType, err := parseBurnAssetType(b.AssetType)
	if err != nil {
		return nil, err
	}
	if assetType == tapsdk.AssetTypeCollectible && amount != 1 {
		return nil, fmt.Errorf("invalid collectible burn amount: %d",
			amount)
	}

	txidBytes, err := parseHexBytes(b.AnchorTxid)
	if err != nil {
		return nil, fmt.Errorf("invalid burn txid: %w", err)
	}
	anchorTxid, err := tapsdk.ParseHash(txidBytes)
	if err != nil {
		return nil, fmt.Errorf("invalid burn txid: %w", err)
	}

	var collectionRef *tapsdk.AssetRef
	if assetType == tapsdk.AssetTypeCollectible && groupKey != nil {
		ref := tapsdk.AssetRefFromGroupKey(*groupKey)
		collectionRef = &ref
	}

	return &tapsdk.BurnRecord{
		Note:          b.Note,
		AssetRef:      tapsdk.AssetRefFromTypedAsset(assetID, groupKey, assetType),
		CollectionRef: collectionRef,
		Type:          assetType,
		IssuanceID:    assetID,
		Amount:        amount,
		AnchorTxid:    anchorTxid,
	}, nil
}

func unmarshalVerifyOwnershipResponse(
	resp *jsonVerifyOwnershipResponse) (*tapsdk.VerifyOwnershipResponse,
	error) {

	if resp == nil {
		return nil, fmt.Errorf("nil verify ownership response")
	}

	result := &tapsdk.VerifyOwnershipResponse{
		Valid:       resp.ValidProof,
		BlockHeight: resp.BlockHeight,
	}

	switch {
	case resp.Outpoint != nil:
		outpoint, err := unmarshalJSONOutpoint(resp.Outpoint)
		if err != nil {
			return nil, fmt.Errorf("invalid outpoint: %w", err)
		}
		result.Outpoint = outpoint

	case resp.OutpointStr != "":
		outpoint, err := tapsdk.NewOutpointFromStr(resp.OutpointStr)
		if err != nil {
			return nil, fmt.Errorf("invalid outpoint: %w", err)
		}
		result.Outpoint = outpoint
	}

	blockHashBytes, err := parseOwnershipBlockHash(resp)
	if err != nil {
		return nil, err
	}
	switch len(blockHashBytes) {
	case 0:

	case len(result.BlockHash):
		copy(result.BlockHash[:], blockHashBytes)

	default:
		return nil, fmt.Errorf(
			"invalid block hash length: got %d bytes, want %d",
			len(blockHashBytes), len(result.BlockHash),
		)
	}
	if result.Valid && result.Outpoint == (tapsdk.Outpoint{}) {
		return nil, fmt.Errorf(
			"valid ownership proof response missing outpoint",
		)
	}

	return result, nil
}

func parseOwnershipBlockHash(
	resp *jsonVerifyOwnershipResponse) ([]byte, error) {

	if resp.BlockHash != "" {
		blockHash, err := parseHexBytes(resp.BlockHash)
		if err == nil {
			return blockHash, nil
		}

		blockHash, b64Err := parseBase64Bytes(resp.BlockHash)
		if b64Err != nil {
			return nil, fmt.Errorf(
				"invalid block_hash: hex=%v base64=%w",
				err, b64Err,
			)
		}

		return blockHash, nil
	}

	if resp.BlockHashStr == "" {
		return nil, nil
	}

	blockHash, err := chainhash.NewHashFromStr(resp.BlockHashStr)
	if err != nil {
		return nil, fmt.Errorf("invalid block_hash_str: %w", err)
	}

	return blockHash.CloneBytes(), nil
}

// unmarshalFetchAssetMetaResponse converts a JSON fetch meta response
// to an entity.
func unmarshalFetchAssetMetaResponse(
	r *jsonFetchAssetMetaResponse) (*tapsdk.AssetMeta, error) {

	return unmarshalAssetMeta(&jsonAssetMeta{
		Data:     r.Data,
		Type:     r.Type,
		MetaHash: r.MetaHash,
	})
}

// unmarshalVerifyProofResponse converts a JSON verify proof response
// to an entity.
func unmarshalVerifyProofResponse(
	r *jsonVerifyProofResponse) (
	*tapsdk.VerifyProofResponse, error) {

	if r == nil {
		return nil, fmt.Errorf("nil verify proof response")
	}
	var decoded *tapsdk.DecodedProof
	if r.DecodedProof != nil {
		var err error
		decoded, err = unmarshalDecodedProof(r.DecodedProof)
		if err != nil {
			return nil, fmt.Errorf("decoded proof: %w", err)
		}
	}

	return &tapsdk.VerifyProofResponse{
		Valid:        r.Valid,
		DecodedProof: decoded,
	}, nil
}

// --- Universe unmarshal helpers ---

// parseProofType converts a proto enum string to tapsdk.ProofType.
func parseProofType(s string) tapsdk.ProofType {
	switch s {
	case proofTypeIssuance:
		return tapsdk.ProofTypeIssuance
	case proofTypeTransfer:
		return tapsdk.ProofTypeTransfer
	default:
		return tapsdk.ProofTypeUnspecified
	}
}

// unmarshalMerkleSumNode converts a JSON merkle sum node to an
// entity.
func unmarshalMerkleSumNode(
	n *jsonMerkleSumNode) (*tapsdk.MerkleSumNode, error) {

	if n == nil {
		return nil, nil
	}

	rootHashBytes, err := parseHexBytes(n.RootHash)
	if err != nil {
		return nil, fmt.Errorf("invalid root_hash: %w", err)
	}

	rootHash, err := tapsdk.ParseHash(rootHashBytes)
	if err != nil {
		return nil, fmt.Errorf("invalid root hash: %w", err)
	}

	rootSum, err := parseInt64(n.RootSum)
	if err != nil {
		return nil, fmt.Errorf("invalid root_sum: %w", err)
	}

	return &tapsdk.MerkleSumNode{
		RootHash: rootHash,
		RootSum:  rootSum,
	}, nil
}

// unmarshalUniverseID converts a JSON universe ID to an entity.
func unmarshalUniverseID(
	id *jsonUniverseID) (*tapsdk.UniverseID, error) {

	if id == nil {
		return nil, fmt.Errorf("nil universe ID")
	}

	uniID := &tapsdk.UniverseID{ProofType: parseProofType(id.ProofType)}

	var assetID *tapsdk.AssetID
	var groupKey *tapsdk.PubKey

	if id.AssetID != "" {
		assetIDBytes, err := parseHexBytes(id.AssetID)
		if err != nil {
			return nil, fmt.Errorf("invalid asset_id: %w",
				err)
		}

		if len(assetIDBytes) == 32 {
			var parsedAssetID tapsdk.AssetID
			copy(parsedAssetID[:], assetIDBytes)
			assetID = &parsedAssetID
		}
	}

	if id.GroupKey != "" {
		groupKeyBytes, err := parseHexBytes(id.GroupKey)
		if err != nil {
			return nil, fmt.Errorf("invalid group_key: %w",
				err)
		}

		// Universe responses carry group keys as 32-byte
		// x-only (schnorr) while callers submit 33-byte
		// compressed keys, so accept either form.
		if len(groupKeyBytes) == 32 ||
			len(groupKeyBytes) == 33 {

			parsedGroupKey, err := tapsdk.ParseTaprootPubKey(
				groupKeyBytes,
			)
			if err != nil {
				return nil, fmt.Errorf(
					"invalid group_key: %w", err,
				)
			}
			groupKey = &parsedGroupKey
		}
	}

	if assetID != nil || groupKey != nil {
		assetRef, err := tapsdk.AssetRefFromSpecifier(
			assetID, groupKey,
		)
		if err != nil {
			return nil, err
		}
		uniID.AssetRef = assetRef
	}

	return uniID, nil
}

// unmarshalUniverseRoot converts a JSON universe root to an entity.
func unmarshalUniverseRoot(
	r *jsonUniverseRoot) (*tapsdk.UniverseRoot, error) {

	if r == nil {
		return nil, nil
	}

	// tapd returns a zero-valued UniverseRoot (nil ID) as a
	// tombstone when the queried asset has no matching root — e.g.
	// QueryAssetRoots on a ref the universe has never heard of.
	// Treat it as "no root present" so callers can distinguish
	// absent-from-universe from malformed responses.
	if r.ID == nil {
		return nil, nil
	}

	id, err := unmarshalUniverseID(r.ID)
	if err != nil {
		return nil, fmt.Errorf("invalid universe ID: %w", err)
	}

	mssmtRoot, err := unmarshalMerkleSumNode(r.MSSMTRoot)
	if err != nil {
		return nil, fmt.Errorf("invalid mssmt_root: %w", err)
	}

	root := &tapsdk.UniverseRoot{
		ID:        *id,
		MSSMTRoot: mssmtRoot,
		AssetName: r.AssetName,
	}

	if len(r.AmountsByAssetID) > 0 {
		root.AmountsByIssuanceID = make(map[string]uint64)
		for k, v := range r.AmountsByAssetID {
			amount, err := parseUint64(v)
			if err != nil {
				return nil, fmt.Errorf(
					"invalid amount for %s: %w", k, err,
				)
			}
			root.AmountsByIssuanceID[k] = amount
		}
	}

	return root, nil
}

// unmarshalAssetLeafKey converts a JSON asset key to an entity.
func unmarshalAssetLeafKey(
	k *jsonAssetKey) (*tapsdk.AssetLeafKey, error) {

	if k == nil {
		return nil, fmt.Errorf("nil asset key")
	}

	outpoint, err := unmarshalAssetKeyOutpoint(k)
	if err != nil {
		return nil, err
	}

	scriptKeyBytes, err := parseHexBytes(assetKeyScriptKey(k))
	if err != nil {
		return nil, fmt.Errorf("invalid script_key: %w", err)
	}

	scriptKey, err := tapsdk.ParseScriptKey(scriptKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("invalid script_key: %w", err)
	}

	return &tapsdk.AssetLeafKey{
		Outpoint:  outpoint,
		ScriptKey: scriptKey,
	}, nil
}

func unmarshalAssetKeyOutpoint(k *jsonAssetKey) (tapsdk.Outpoint, error) {
	opStr := k.OpStr
	if opStr != "" {
		outpoint, err := tapsdk.NewOutpointFromStr(opStr)
		if err != nil {
			return tapsdk.Outpoint{}, fmt.Errorf(
				"invalid outpoint: %w", err,
			)
		}

		return outpoint, nil
	}

	op := k.Outpoint
	if op == nil {
		op = k.Op
	}
	if op == nil {
		return tapsdk.Outpoint{}, fmt.Errorf("nil outpoint")
	}

	return unmarshalJSONOutpoint(op)
}

func unmarshalJSONOutpoint(op *jsonOutpoint) (tapsdk.Outpoint, error) {
	opStr := firstNonEmpty(op.Txid, op.HashStr)
	if opStr == "" {
		return tapsdk.Outpoint{}, fmt.Errorf("empty outpoint")
	}

	if strings.Contains(opStr, ":") {
		return tapsdk.NewOutpointFromStr(opStr)
	}

	txidBytes, err := parseHexBytes(opStr)
	if err != nil {
		return tapsdk.Outpoint{}, fmt.Errorf("invalid txid: %w", err)
	}
	if len(txidBytes) != 32 {
		return tapsdk.Outpoint{}, fmt.Errorf(
			"invalid txid length: %d", len(txidBytes),
		)
	}

	index := op.OutputIndex
	if index == 0 {
		index = op.Index
	}

	outpoint := tapsdk.Outpoint{
		Index: index,
	}
	copy(outpoint.Txid[:], txidBytes)

	return outpoint, nil
}

func assetKeyScriptKey(k *jsonAssetKey) string {
	return firstNonEmpty(
		k.ScriptKey, k.ScriptKeyBytes, k.ScriptKeyStr,
	)
}

// unmarshalAssetLeaf converts a JSON asset leaf to an entity.
func unmarshalAssetLeaf(
	l *jsonAssetLeafResp) (*tapsdk.AssetLeaf, error) {

	if l == nil {
		return nil, fmt.Errorf("nil asset leaf")
	}

	leaf := &tapsdk.AssetLeaf{}

	if l.Asset != nil {
		asset, err := unmarshalAsset(l.Asset)
		if err != nil {
			return nil, fmt.Errorf("invalid asset: %w", err)
		}
		leaf.Asset = asset
	}

	if l.Proof != "" {
		proofBytes, err := parseHexBytes(l.Proof)
		if err != nil {
			return nil, fmt.Errorf("invalid proof: %w", err)
		}
		leaf.Proof = proofBytes
	}

	return leaf, nil
}

// unmarshalAssetProofResponse converts a JSON query proof response to
// an entity.
func unmarshalAssetProofResponse(
	r *jsonQueryProofResponse) (*tapsdk.AssetProofResponse,
	error) {

	if r == nil {
		return nil, fmt.Errorf("nil proof response")
	}

	resp := &tapsdk.AssetProofResponse{}

	if r.Req != nil {
		id, err := unmarshalUniverseID(r.Req.ID)
		if err != nil {
			return nil, fmt.Errorf("invalid req ID: %w",
				err)
		}

		key := tapsdk.UniverseKey{ID: *id}

		if r.Req.LeafKey != nil {
			leafKey, err := unmarshalAssetLeafKey(r.Req.LeafKey)
			if err != nil {
				return nil, fmt.Errorf(
					"invalid leaf_key: %w", err,
				)
			}
			key.LeafKey = *leafKey
		}

		resp.Key = key
	}

	if r.UniverseRoot != nil {
		root, err := unmarshalUniverseRoot(r.UniverseRoot)
		if err != nil {
			return nil, fmt.Errorf(
				"invalid universe_root: %w", err,
			)
		}
		resp.UniverseRoot = root
	}

	if r.UniverseInclusionProof != "" {
		proof, err := parseHexBytes(r.UniverseInclusionProof)
		if err != nil {
			return nil, fmt.Errorf(
				"invalid universe_inclusion_proof: %w", err,
			)
		}
		resp.UniverseInclusionProof = proof
	}

	if r.AssetLeaf != nil {
		leaf, err := unmarshalAssetLeaf(r.AssetLeaf)
		if err != nil {
			return nil, fmt.Errorf("invalid asset_leaf: %w",
				err)
		}
		resp.AssetLeaf = leaf
	}

	if r.MultiverseRoot != nil {
		root, err := unmarshalMerkleSumNode(r.MultiverseRoot)
		if err != nil {
			return nil, fmt.Errorf(
				"invalid multiverse_root: %w", err,
			)
		}
		resp.MultiverseRoot = root
	}

	if r.MultiverseInclusionProof != "" {
		proof, err := parseHexBytes(r.MultiverseInclusionProof)
		if err != nil {
			return nil, fmt.Errorf(
				"invalid multiverse_inclusion_proof: %w",
				err,
			)
		}
		resp.MultiverseInclusionProof = proof
	}

	return resp, nil
}

// unmarshalUniverseStats converts a JSON universe stats response to an
// entity.
func unmarshalUniverseStats(
	r *jsonUniverseStatsResponse) (*tapsdk.UniverseStats,
	error) {

	if r == nil {
		return nil, fmt.Errorf("nil universe stats")
	}

	numTotalAssets, err := parseInt64(r.NumTotalAssets)
	if err != nil {
		return nil, fmt.Errorf("invalid num_total_assets: %w",
			err)
	}

	numTotalGroups, err := parseInt64(r.NumTotalGroups)
	if err != nil {
		return nil, fmt.Errorf("invalid num_total_groups: %w",
			err)
	}

	numTotalSyncs, err := parseInt64(r.NumTotalSyncs)
	if err != nil {
		return nil, fmt.Errorf("invalid num_total_syncs: %w",
			err)
	}

	numTotalProofs, err := parseInt64(r.NumTotalProofs)
	if err != nil {
		return nil, fmt.Errorf("invalid num_total_proofs: %w",
			err)
	}

	return &tapsdk.UniverseStats{
		NumTotalAssets: numTotalAssets,
		NumTotalGroups: numTotalGroups,
		NumTotalSyncs:  numTotalSyncs,
		NumTotalProofs: numTotalProofs,
	}, nil
}

// unmarshalAssetStatsAsset converts a JSON asset stats asset to an
// entity.
func unmarshalAssetStatsAsset(
	a *jsonAssetStatsAsset) (*tapsdk.AssetStatsAsset, error) {

	if a == nil {
		return nil, nil
	}

	assetIDBytes, err := parseHexBytes(a.AssetID)
	if err != nil {
		return nil, fmt.Errorf("invalid asset_id: %w", err)
	}

	var assetID tapsdk.AssetID
	if len(assetIDBytes) == 32 {
		copy(assetID[:], assetIDBytes)
	}

	totalSupply, err := parseInt64(a.TotalSupply)
	if err != nil {
		return nil, fmt.Errorf("invalid total_supply: %w", err)
	}

	genesisTimestamp, err := parseInt64(a.GenesisTimestamp)
	if err != nil {
		return nil, fmt.Errorf(
			"invalid genesis_timestamp: %w", err,
		)
	}

	assetType, err := parseAssetType(a.AssetType)
	if err != nil {
		return nil, err
	}

	return &tapsdk.AssetStatsAsset{
		AssetRef:         tapsdk.AssetRefFromAssetID(assetID),
		IssuanceID:       assetID,
		GenesisPoint:     a.GenesisPoint,
		TotalSupply:      totalSupply,
		AssetName:        a.AssetName,
		AssetType:        assetType,
		GenesisHeight:    a.GenesisHeight,
		GenesisTimestamp: genesisTimestamp,
		AnchorPoint:      a.AnchorPoint,
		DecimalDisplay:   a.DecimalDisplay,
	}, nil
}

// unmarshalAssetStatsSnapshot converts a JSON asset stats snapshot to
// an entity.
func unmarshalAssetStatsSnapshot(
	s *jsonAssetStatsSnapshot) (*tapsdk.AssetStatsSnapshot,
	error) {

	if s == nil {
		return nil, fmt.Errorf("nil asset stats snapshot")
	}

	snapshot := &tapsdk.AssetStatsSnapshot{}

	if s.GroupKey != "" {
		groupKey, err := tapsdk.ParseGroupRefKey(s.GroupKey)
		if err != nil {
			return nil, fmt.Errorf("invalid group_key: %w",
				err)
		}

		snapshot.GroupKey = &groupKey
		snapshot.AssetRef = tapsdk.AssetRefFromGroupKey(groupKey)
	}

	groupSupply, err := parseInt64(s.GroupSupply)
	if err != nil {
		return nil, fmt.Errorf("invalid group_supply: %w", err)
	}
	snapshot.GroupSupply = groupSupply

	if s.GroupAnchor != nil {
		anchor, err := unmarshalAssetStatsAsset(s.GroupAnchor)
		if err != nil {
			return nil, fmt.Errorf("invalid group_anchor: %w",
				err)
		}
		snapshot.GroupAnchor = anchor
	}

	if s.Asset != nil {
		asset, err := unmarshalAssetStatsAsset(s.Asset)
		if err != nil {
			return nil, fmt.Errorf("invalid asset: %w", err)
		}
		snapshot.Asset = asset
		if snapshot.AssetRef.IsZero() {
			snapshot.AssetRef = asset.AssetRef
		}
	}

	totalSyncs, err := parseInt64(s.TotalSyncs)
	if err != nil {
		return nil, fmt.Errorf("invalid total_syncs: %w", err)
	}
	snapshot.TotalSyncs = totalSyncs

	totalProofs, err := parseInt64(s.TotalProofs)
	if err != nil {
		return nil, fmt.Errorf("invalid total_proofs: %w", err)
	}
	snapshot.TotalProofs = totalProofs

	return snapshot, nil
}

// unmarshalGroupedUniverseEvents converts a JSON grouped universe
// events to an entity.
func unmarshalGroupedUniverseEvents(
	e *jsonGroupedUniverseEvents) (
	*tapsdk.GroupedUniverseEvents, error) {

	if e == nil {
		return nil, fmt.Errorf("nil grouped universe events")
	}

	syncEvents, err := parseUint64(e.SyncEvents)
	if err != nil {
		return nil, fmt.Errorf("invalid sync_events: %w", err)
	}

	newProofEvents, err := parseUint64(e.NewProofEvents)
	if err != nil {
		return nil, fmt.Errorf(
			"invalid new_proof_events: %w", err,
		)
	}

	return &tapsdk.GroupedUniverseEvents{
		Date:           e.Date,
		SyncEvents:     syncEvents,
		NewProofEvents: newProofEvents,
	}, nil
}

// unmarshalFederationServer converts a JSON federation server to an
// entity.
func unmarshalFederationServer(
	s *jsonUniverseFederationServer) (
	*tapsdk.FederationServer, error) {

	if s == nil {
		return nil, fmt.Errorf("nil federation server")
	}

	return &tapsdk.FederationServer{
		Host: s.Host,
		ID:   s.ID,
	}, nil
}

// unmarshalGlobalFederationSyncConfig converts a JSON global
// federation sync config to an entity.
func unmarshalGlobalFederationSyncConfig(
	c *jsonGlobalFederationSyncConfig) (
	*tapsdk.GlobalFederationSyncConfig, error) {

	if c == nil {
		return nil, fmt.Errorf("nil global federation sync config")
	}

	return &tapsdk.GlobalFederationSyncConfig{
		ProofType:       parseProofType(c.ProofType),
		AllowSyncInsert: c.AllowSyncInsert,
		AllowSyncExport: c.AllowSyncExport,
	}, nil
}

// unmarshalAssetFederationSyncConfig converts a JSON asset federation
// sync config to an entity.
func unmarshalAssetFederationSyncConfig(
	c *jsonAssetFederationSyncConfig) (
	*tapsdk.AssetFederationSyncConfig, error) {

	if c == nil {
		return nil, fmt.Errorf("nil asset federation sync config")
	}

	id, err := unmarshalUniverseID(c.ID)
	if err != nil {
		return nil, fmt.Errorf("invalid ID: %w", err)
	}

	return &tapsdk.AssetFederationSyncConfig{
		ID:              *id,
		AllowSyncInsert: c.AllowSyncInsert,
		AllowSyncExport: c.AllowSyncExport,
	}, nil
}

// unmarshalSyncedUniverse converts a JSON synced universe to an
// entity.
func unmarshalSyncedUniverse(
	s *jsonSyncedUniverse) (*tapsdk.SyncedUniverse, error) {

	if s == nil {
		return nil, fmt.Errorf("nil synced universe")
	}

	synced := &tapsdk.SyncedUniverse{}

	if s.OldAssetRoot != nil {
		oldRoot, err := unmarshalUniverseRoot(s.OldAssetRoot)
		if err != nil {
			return nil, fmt.Errorf(
				"invalid old_asset_root: %w", err,
			)
		}
		synced.OldAssetRoot = oldRoot
	}

	if s.NewAssetRoot != nil {
		newRoot, err := unmarshalUniverseRoot(s.NewAssetRoot)
		if err != nil {
			return nil, fmt.Errorf(
				"invalid new_asset_root: %w", err,
			)
		}
		synced.NewAssetRoot = newRoot
	}

	if len(s.NewAssetLeaves) > 0 {
		leaves := make(
			[]tapsdk.AssetLeaf, 0, len(s.NewAssetLeaves),
		)
		for _, l := range s.NewAssetLeaves {
			leaf, err := unmarshalAssetLeaf(l)
			if err != nil {
				return nil, fmt.Errorf(
					"invalid asset leaf: %w", err,
				)
			}
			leaves = append(leaves, *leaf)
		}
		synced.NewAssetLeaves = leaves
	}

	return synced, nil
}
