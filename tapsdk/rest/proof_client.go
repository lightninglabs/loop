package rest

import (
	"context"
	"encoding/hex"
	"fmt"

	tapsdk "github.com/lightninglabs/loop/tapsdk"
	"github.com/lightninglabs/loop/tapsdk/internal/codec"
	"github.com/lightninglabs/loop/tapsdk/macaroon"
)

// proofClient implements tapsdk.ProofClient over REST.
type proofClient struct {
	transport *transport
}

func newProofClient(tp *transport) *proofClient {
	return &proofClient{transport: tp}
}

// jsonExportProofRequest is the JSON body for ExportProof.
type jsonExportProofRequest struct {
	AssetID   string           `json:"asset_id"`
	ScriptKey string           `json:"script_key"`
	Outpoint  *jsonOutpointReq `json:"outpoint,omitempty"`
}

// jsonOutpointReq is the JSON shape of taprpc.OutPoint.
type jsonOutpointReq struct {
	Txid        string `json:"txid"`
	OutputIndex uint32 `json:"output_index"`
}

// ExportProof exports a proof file for a specific asset output.
func (p *proofClient) ExportProof(ctx context.Context,
	ref tapsdk.AssetRef, scriptKey tapsdk.PubKey,
	outpoint *tapsdk.Outpoint) (*tapsdk.ProofFile, error) {

	if err := ref.Validate(); err != nil {
		return nil, err
	}

	assetID, ok := ref.AssetID()
	if !ok {
		return nil, fmt.Errorf("export proof requires an " +
			"asset-ID ref; group-key refs commit to " +
			"multiple tranches")
	}

	body := &jsonExportProofRequest{
		AssetID: hex.EncodeToString(
			assetID[:],
		),
		ScriptKey: hex.EncodeToString(
			scriptKey[:],
		),
	}

	if outpoint != nil {
		body.Outpoint = &jsonOutpointReq{
			Txid: hex.EncodeToString(
				outpoint.Txid[:],
			),
			OutputIndex: outpoint.Index,
		}
	}

	var resp jsonProofFile
	err := p.transport.doPost(
		ctx, "/v1/taproot-assets/proofs/export",
		macaroon.ProofServiceMac, body, &resp,
	)
	if err != nil {
		return nil, err
	}

	rawProof, err := parseHexBytes(resp.RawProofFile)
	if err != nil {
		return nil, fmt.Errorf("invalid raw_proof_file: %w", err)
	}

	proofFile := &tapsdk.ProofFile{RawProofFile: rawProof}
	if resp.GenesisPoint != "" {
		genesisPoint, err := tapsdk.NewOutpointFromStr(
			resp.GenesisPoint,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"invalid genesis point: %w", err,
			)
		}

		proofFile.GenesisPoint = genesisPoint
	}

	return proofFile, nil
}

// jsonUnpackProofFileRequest is the JSON body for
// UnpackProofFile.
type jsonUnpackProofFileRequest struct {
	RawProofFile string `json:"raw_proof_file"`
}

// UnpackProofFile unpacks a proof file into individual proofs.
func (p *proofClient) UnpackProofFile(ctx context.Context,
	rawProofFile []byte) ([][]byte, error) {

	body := &jsonUnpackProofFileRequest{
		RawProofFile: hex.EncodeToString(
			rawProofFile,
		),
	}

	var resp jsonUnpackProofFileResponse
	err := p.transport.doPost(
		ctx, "/v1/taproot-assets/proofs/unpack-file",
		macaroon.ProofServiceMac, body, &resp,
	)
	if err != nil {
		return nil, err
	}

	proofs := make([][]byte, 0, len(resp.RawProofs))
	for _, rawProof := range resp.RawProofs {
		proofBytes, err := parseHexBytes(rawProof)
		if err != nil {
			return nil, fmt.Errorf(
				"invalid raw proof: %w", err,
			)
		}

		proofs = append(proofs, proofBytes)
	}

	return proofs, nil
}

// jsonDecodeProofRequest is the JSON body for DecodeProof.
type jsonDecodeProofRequest struct {
	RawProof          string `json:"raw_proof"`
	WithMetaReveal    bool   `json:"with_meta_reveal"`
	WithPrevWitnesses bool   `json:"with_prev_witnesses"`
}

// DecodeProof decodes a raw proof and returns details about it.
func (p *proofClient) DecodeProof(ctx context.Context,
	rawProof []byte) (*tapsdk.DecodedProof, error) {

	body := &jsonDecodeProofRequest{
		RawProof: hex.EncodeToString(
			rawProof,
		),
		WithMetaReveal:    true,
		WithPrevWitnesses: true,
	}

	var resp jsonDecodeProofResponse
	err := p.transport.doPost(
		ctx, "/v1/taproot-assets/proofs/decode",
		macaroon.ProofServiceMac, body, &resp,
	)
	if err != nil {
		return nil, err
	}

	if resp.DecodedProof == nil || resp.DecodedProof.Asset == nil {
		return nil, fmt.Errorf("invalid decoded proof response")
	}

	return unmarshalDecodedProof(resp.DecodedProof)
}

// jsonRegisterTransferRequest is the JSON body for
// RegisterTransfer.
type jsonRegisterTransferRequest struct {
	AssetID   string           `json:"asset_id,omitempty"`
	GroupKey  string           `json:"group_key,omitempty"`
	ScriptKey string           `json:"script_key"`
	Outpoint  *jsonOutpointReq `json:"outpoint"`
}

// RegisterTransfer registers an inbound transfer for an
// interactive send.
func (p *proofClient) RegisterTransfer(ctx context.Context,
	assetRef tapsdk.AssetRef, scriptKey tapsdk.PubKey,
	outpoint tapsdk.Outpoint) (*tapsdk.RegisteredAsset, error) {

	return p.registerTransfer(
		ctx, assetRef, tapsdk.AssetID{}, scriptKey, outpoint,
	)
}

// RegisterTransferWithIssuance registers an inbound transfer when the user
// facing asset ref is a group key and tapd still needs the concrete issuance
// ID from the imported proof.
func (p *proofClient) RegisterTransferWithIssuance(ctx context.Context,
	assetRef tapsdk.AssetRef, issuanceID tapsdk.AssetID,
	scriptKey tapsdk.PubKey,
	outpoint tapsdk.Outpoint) (*tapsdk.RegisteredAsset, error) {

	return p.registerTransfer(ctx, assetRef, issuanceID, scriptKey, outpoint)
}

func (p *proofClient) registerTransfer(ctx context.Context,
	assetRef tapsdk.AssetRef, issuanceID tapsdk.AssetID,
	scriptKey tapsdk.PubKey,
	outpoint tapsdk.Outpoint) (*tapsdk.RegisteredAsset, error) {

	if err := assetRef.Validate(); err != nil {
		return nil, err
	}

	body := &jsonRegisterTransferRequest{
		ScriptKey: hex.EncodeToString(
			scriptKey[:],
		),
		Outpoint: &jsonOutpointReq{
			Txid: hex.EncodeToString(
				outpoint.Txid[:],
			),
			OutputIndex: outpoint.Index,
		},
	}

	if assetID, ok := assetRef.AssetID(); ok {
		body.AssetID = hex.EncodeToString(assetID[:])
	}

	if assetRef.IsGroupRef() && issuanceID != (tapsdk.AssetID{}) {
		body.AssetID = hex.EncodeToString(issuanceID[:])
	}

	if groupKey, ok := assetRef.GroupKey(); ok {
		body.GroupKey = hex.EncodeToString(groupKey[:])
	}

	var resp jsonRegisterTransferResponse
	err := p.transport.doPost(
		ctx, "/v1/taproot-assets/assets/transfers/register",
		macaroon.ProofServiceMac, body, &resp,
	)
	if err != nil {
		return nil, err
	}

	if resp.RegisteredAsset == nil {
		return nil, fmt.Errorf("invalid registered asset response")
	}

	registered, err := unmarshalRegisteredAsset(resp.RegisteredAsset)
	if err != nil {
		return nil, err
	}

	if registered.AssetRef.IsZero() {
		registered.AssetRef = assetRef
	}

	return registered, nil
}

// unmarshalDecodedProof converts a JSON decoded proof to the entity
// type.
func unmarshalDecodedProof(
	d *jsonDecodedProof) (*tapsdk.DecodedProof, error) {

	if d == nil {
		return nil, fmt.Errorf("nil decoded proof")
	}

	result := &tapsdk.DecodedProof{
		ProofAtDepth:   d.ProofAtDepth,
		NumberOfProofs: d.NumberOfProofs,
		IsIssuance:     d.GenesisReveal != nil,
	}

	asset := d.Asset
	if asset == nil {
		return nil, fmt.Errorf("nil decoded asset")
	}

	if asset.AssetGenesis == nil {
		return nil, fmt.Errorf("nil proof asset genesis")
	}

	assetIDBytes, err := parseHexBytes(asset.AssetGenesis.AssetID)
	if err != nil {
		return nil, fmt.Errorf("invalid asset ID: %w", err)
	}

	assetID, err := tapsdk.ParseAssetID(assetIDBytes)
	if err != nil {
		return nil, fmt.Errorf("invalid asset ID: %w", err)
	}
	result.IssuanceID = assetID
	assetType := tapsdk.AssetTypeFungible
	if asset.AssetGenesis.AssetType != "" {
		assetType, err = parseAssetType(asset.AssetGenesis.AssetType)
		if err != nil {
			return nil, fmt.Errorf("invalid asset type: %w", err)
		}
	}

	scriptKeyBytes, err := parseHexBytes(asset.ScriptKey)
	if err != nil {
		return nil, fmt.Errorf(
			"invalid script key: %w", err,
		)
	}

	scriptKey, err := tapsdk.ParsePubKey(scriptKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("invalid script key: %w", err)
	}
	result.ScriptKey = scriptKey

	amount, err := parseUint64(asset.Amount)
	if err != nil {
		return nil, fmt.Errorf("invalid amount: %w", err)
	}

	result.Amount = amount

	if asset.ChainAnchor != nil {
		op, err := tapsdk.NewOutpointFromStr(
			asset.ChainAnchor.AnchorOutpoint,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"invalid anchor outpoint: %w", err,
			)
		}

		result.Outpoint = op
	}

	if asset.AssetGroup != nil &&
		asset.AssetGroup.TweakedGroupKey != "" {

		groupKeyBytes, err := parseHexBytes(
			asset.AssetGroup.TweakedGroupKey,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"invalid group key: %w", err,
			)
		}

		groupKey, err := tapsdk.ParsePubKey(groupKeyBytes)
		if err != nil {
			return nil, fmt.Errorf(
				"invalid group key: %w", err,
			)
		}

		result.AssetRef = tapsdk.AssetRefFromTypedAsset(
			result.IssuanceID, &groupKey, assetType,
		)
	}

	if result.AssetRef.IsZero() {
		result.AssetRef = tapsdk.AssetRefFromTypedAsset(
			result.IssuanceID, nil, assetType,
		)
	}

	if d.AltLeaves != "" {
		encodedAltLeaves, err := parseHexBytes(d.AltLeaves)
		if err != nil {
			return nil, fmt.Errorf("invalid alt leaves: %w", err)
		}

		altLeaves, err := codec.DecodeAltLeaves(encodedAltLeaves)
		if err != nil {
			return nil, fmt.Errorf("decode alt leaves: %w", err)
		}

		result.AltLeaves = altLeaves
	}

	// Populate prev IDs.
	if len(asset.PrevWitnesses) > 0 {
		prevIDs := make(
			[]tapsdk.PrevID, 0,
			len(asset.PrevWitnesses),
		)
		for idx, witness := range asset.PrevWitnesses {
			if witness == nil || witness.PrevID == nil {
				return nil, fmt.Errorf(
					"missing prev_id for witness %d",
					idx,
				)
			}

			prev := witness.PrevID
			prevOutpoint, err := tapsdk.NewOutpointFromStr(
				prev.AnchorPoint,
			)
			if err != nil {
				return nil, fmt.Errorf(
					"invalid prev_id outpoint for "+
						"witness %d: %w",
					idx, err,
				)
			}

			assetIDBytes, err := parseHexBytes(prev.AssetID)
			if err != nil {
				return nil, fmt.Errorf(
					"invalid prev_id asset_id for "+
						"witness %d: %w",
					idx, err,
				)
			}

			prevAssetID, err := tapsdk.ParseAssetID(assetIDBytes)
			if err != nil {
				return nil, fmt.Errorf(
					"invalid prev_id asset_id for "+
						"witness %d: %w", idx, err,
				)
			}

			scriptKeyBytes, err := parseHexBytes(
				prev.ScriptKey,
			)
			if err != nil {
				return nil, fmt.Errorf(
					"invalid prev_id script_key "+
						"for witness %d: %w",
					idx, err,
				)
			}
			if isZeroDecodedProofPrevID(
				prevOutpoint, prevAssetID, scriptKeyBytes,
			) {

				if d.GenesisReveal == nil &&
					witness.SplitCommitment == nil {

					return nil, fmt.Errorf("zero prev_id is only "+
						"valid for issuance or split witness %d", idx)
				}

				continue
			}

			prevScriptKey, err := tapsdk.ParsePubKey(scriptKeyBytes)
			if err != nil {
				return nil, fmt.Errorf(
					"invalid prev_id script_key for "+
						"witness %d: %w", idx, err,
				)
			}

			prevIDs = append(prevIDs, tapsdk.PrevID{
				Outpoint:   prevOutpoint,
				IssuanceID: prevAssetID,
				ScriptKey:  prevScriptKey,
			})
		}

		result.PrevIDs = prevIDs
	}

	return result, nil
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

// unmarshalRegisteredAsset converts a JSON registered asset to the
// entity type.
func unmarshalRegisteredAsset(
	a *jsonRegisteredAsset) (*tapsdk.RegisteredAsset, error) {

	if a == nil {
		return nil, fmt.Errorf("nil registered asset")
	}

	amount, err := parseUint64(a.Amount)
	if err != nil {
		return nil, fmt.Errorf("invalid amount: %w", err)
	}

	result := &tapsdk.RegisteredAsset{Amount: amount}

	if a.AssetGenesis != nil {
		assetIDBytes, err := parseHexBytes(
			a.AssetGenesis.AssetID,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"invalid asset ID: %w", err,
			)
		}

		if len(assetIDBytes) == 32 {
			copy(result.IssuanceID[:], assetIDBytes)
			result.AssetRef = tapsdk.AssetRefFromAssetID(
				result.IssuanceID,
			)
		}
	}

	scriptKeyBytes, err := parseHexBytes(a.ScriptKey)
	if err != nil {
		return nil, fmt.Errorf(
			"invalid script key: %w", err,
		)
	}

	if len(scriptKeyBytes) == 33 {
		copy(result.ScriptKey[:], scriptKeyBytes)
	}

	if a.ChainAnchor != nil {
		op, err := tapsdk.NewOutpointFromStr(
			a.ChainAnchor.AnchorOutpoint,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"invalid anchor outpoint: %w", err,
			)
		}

		result.Outpoint = op
	}

	return result, nil
}
