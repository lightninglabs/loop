package grpc

import (
	"context"
	"fmt"
	"time"

	tapsdk "github.com/lightninglabs/loop/tapsdk"
	"github.com/lightninglabs/loop/tapsdk/macaroon"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"google.golang.org/grpc"
)

// proofClient is a wrapper around the taprpc.TaprootAssetsClient for
// proof-related operations.
type proofClient struct {
	client   taprpc.TaprootAssetsClient
	timeout  time.Duration
	proofMac macaroon.SerializedMacaroon
}

// NewProofClient creates a new Proof client.
func NewProofClient(conn grpc.ClientConnInterface, timeout time.Duration,
	proofMac macaroon.SerializedMacaroon) *proofClient {

	return &proofClient{
		client:   taprpc.NewTaprootAssetsClient(conn),
		timeout:  timeout,
		proofMac: proofMac,
	}
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

	rpcCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	rpcCtx = p.proofMac.WithMacaroonAuth(rpcCtx)
	req := &taprpc.ExportProofRequest{
		AssetId:   assetID[:],
		ScriptKey: scriptKey[:],
	}
	if outpoint != nil {
		req.Outpoint = &taprpc.OutPoint{
			Txid:        outpoint.Txid[:],
			OutputIndex: outpoint.Index,
		}
	}

	resp, err := p.client.ExportProof(rpcCtx, req)
	if err != nil {
		return nil, err
	}

	proofFile := &tapsdk.ProofFile{
		RawProofFile: resp.RawProofFile,
	}

	// GenesisPoint is optional - only parse if provided.
	if resp.GenesisPoint != "" {
		genesisPoint, err := tapsdk.NewOutpointFromStr(resp.GenesisPoint)
		if err != nil {
			return nil, fmt.Errorf("invalid genesis point: %v", err)
		}
		proofFile.GenesisPoint = genesisPoint
	}

	return proofFile, nil
}

// UnpackProofFile unpacks a proof file into individual proofs.
func (p *proofClient) UnpackProofFile(ctx context.Context,
	rawProofFile []byte) ([][]byte, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	rpcCtx = p.proofMac.WithMacaroonAuth(rpcCtx)
	resp, err := p.client.UnpackProofFile(rpcCtx, &taprpc.UnpackProofFileRequest{
		RawProofFile: rawProofFile,
	})
	if err != nil {
		return nil, err
	}

	return resp.RawProofs, nil
}

// DecodeProof decodes a raw proof and returns details about it.
func (p *proofClient) DecodeProof(ctx context.Context,
	rawProof []byte) (*tapsdk.DecodedProof, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	rpcCtx = p.proofMac.WithMacaroonAuth(rpcCtx)
	resp, err := p.client.DecodeProof(rpcCtx, &taprpc.DecodeProofRequest{
		RawProof:          rawProof,
		WithMetaReveal:    true,
		WithPrevWitnesses: true,
	})
	if err != nil {
		return nil, err
	}

	if resp.DecodedProof == nil || resp.DecodedProof.Asset == nil {
		return nil, fmt.Errorf("invalid decoded proof response")
	}

	return unmarshalDecodedProof(resp.DecodedProof)
}

// RegisterTransfer registers an inbound transfer for an interactive send.
// The proof must already be in the local universe before calling this.
func (p *proofClient) RegisterTransfer(ctx context.Context,
	assetRef tapsdk.AssetRef, scriptKey tapsdk.PubKey,
	outpoint tapsdk.Outpoint) (
	*tapsdk.RegisteredAsset, error) {

	return p.registerTransfer(
		ctx, assetRef, tapsdk.AssetID{}, scriptKey, outpoint,
	)
}

// RegisterTransferWithIssuance registers an inbound transfer when the user
// facing asset ref is a group key and tapd still needs the concrete issuance
// ID from the imported proof.
func (p *proofClient) RegisterTransferWithIssuance(ctx context.Context,
	assetRef tapsdk.AssetRef, issuanceID tapsdk.AssetID,
	scriptKey tapsdk.PubKey, outpoint tapsdk.Outpoint) (
	*tapsdk.RegisteredAsset, error) {

	return p.registerTransfer(ctx, assetRef, issuanceID, scriptKey, outpoint)
}

func (p *proofClient) registerTransfer(ctx context.Context,
	assetRef tapsdk.AssetRef, issuanceID tapsdk.AssetID,
	scriptKey tapsdk.PubKey, outpoint tapsdk.Outpoint) (
	*tapsdk.RegisteredAsset, error) {

	rpcCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	rpcCtx = p.proofMac.WithMacaroonAuth(rpcCtx)
	if err := assetRef.Validate(); err != nil {
		return nil, err
	}

	rpcReq := &taprpc.RegisterTransferRequest{
		ScriptKey: scriptKey[:],
		Outpoint: &taprpc.OutPoint{
			Txid:        outpoint.Txid[:],
			OutputIndex: outpoint.Index,
		},
	}

	if assetID, ok := assetRef.AssetID(); ok {
		rpcReq.AssetId = assetID[:]
	}

	if assetRef.IsGroupRef() && issuanceID != (tapsdk.AssetID{}) {
		rpcReq.AssetId = issuanceID[:]
	}

	if groupKey, ok := assetRef.GroupKey(); ok {
		rpcReq.GroupKey = groupKey[:]
	}

	resp, err := p.client.RegisterTransfer(rpcCtx, rpcReq)
	if err != nil {
		return nil, err
	}

	if resp.RegisteredAsset == nil {
		return nil, fmt.Errorf("invalid registered asset response")
	}

	asset := resp.RegisteredAsset
	result := &tapsdk.RegisteredAsset{
		Amount:   asset.Amount,
		AssetRef: assetRef,
	}

	// Copy asset ID.
	if asset.AssetGenesis != nil && len(asset.AssetGenesis.AssetId) == 32 {
		copy(result.IssuanceID[:], asset.AssetGenesis.AssetId)
	}

	// Copy script key.
	if len(asset.ScriptKey) == 33 {
		copy(result.ScriptKey[:], asset.ScriptKey)
	}

	// Get outpoint from chain anchor.
	if asset.ChainAnchor != nil {
		op, err := tapsdk.NewOutpointFromStr(asset.ChainAnchor.AnchorOutpoint)
		if err != nil {
			return nil, fmt.Errorf("invalid anchor outpoint: %v", err)
		}
		result.Outpoint = op
	}

	return result, nil
}
