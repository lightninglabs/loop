package grpc

import (
	"context"
	"fmt"
	"time"

	tapsdk "github.com/lightninglabs/loop/tapsdk"
	"github.com/lightninglabs/loop/tapsdk/internal/anchor"
	"github.com/lightninglabs/loop/tapsdk/macaroon"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightninglabs/taproot-assets/taprpc/assetwalletrpc"
	"google.golang.org/grpc"
)

// walletKitClient is a wrapper around the assetwalletrpc.AssetWalletClient.
type walletKitClient struct {
	client       assetwalletrpc.AssetWalletClient
	timeout      time.Duration
	walletKitMac macaroon.SerializedMacaroon
}

// NewWalletKitClient creates a new WalletKit client.
func NewWalletKitClient(conn grpc.ClientConnInterface, timeout time.Duration,
	walletKitMac macaroon.SerializedMacaroon) *walletKitClient {

	return &walletKitClient{
		client:       assetwalletrpc.NewAssetWalletClient(conn),
		timeout:      timeout,
		walletKitMac: walletKitMac,
	}
}

func (m *walletKitClient) rawClientWithMacAuth(
	parentCtx context.Context) (context.Context,
	context.CancelFunc, assetwalletrpc.AssetWalletClient) {

	rpcCtx, cancel := context.WithTimeout(parentCtx, m.timeout)
	return m.walletKitMac.WithMacaroonAuth(rpcCtx), cancel, m.client
}

// CustomAnchorCapabilities returns the SDK-version-pinned tapd capability
// assumptions. tapd does not expose runtime capability discovery over RPC.
func (m *walletKitClient) CustomAnchorCapabilities(
	ctx context.Context) (*tapsdk.CustomAnchorCapabilities, error) {

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	caps := tapsdk.DefaultTapdCustomAnchorCapabilities()
	return &caps, nil
}

// FundTransfer funds a virtual transaction.
func (m *walletKitClient) FundTransfer(ctx context.Context,
	recipients []tapsdk.Recipient, inputs []tapsdk.PrevID) (
	*tapsdk.FundedTransfer, error) {

	// Map inputs to RPC inputs
	rpcInputs := make([]*assetwalletrpc.PrevId, len(inputs))
	for i, input := range inputs {
		rpcInputs[i] = &assetwalletrpc.PrevId{
			Outpoint: &taprpc.OutPoint{
				Txid:        input.Outpoint.Txid[:],
				OutputIndex: input.Outpoint.Index,
			},
			Id:        input.IssuanceID[:],
			ScriptKey: input.ScriptKey[:],
		}
	}

	// FundVirtualPsbt only exposes AddressesWithAmounts, so every
	// Recipient must carry an explicit amount; there is no
	// embedded-amount wire path at this level.
	rpcRecipients := make([]*taprpc.AddressWithAmount, len(recipients))
	for i, r := range recipients {
		amount, ok := r.Amount()
		if !ok || amount == 0 {
			return nil, fmt.Errorf("FundTransfer recipient "+
				"%q requires an explicit amount", r.Address)
		}
		rpcRecipients[i] = &taprpc.AddressWithAmount{
			TapAddr: r.Address,
			Amount:  amount,
		}
	}

	// Create the raw template.
	rawTemplate := &assetwalletrpc.TxTemplate{
		Recipients:           nil, // Deprecated
		Inputs:               rpcInputs,
		AddressesWithAmounts: rpcRecipients,
	}

	req := &assetwalletrpc.FundVirtualPsbtRequest{
		Template: &assetwalletrpc.FundVirtualPsbtRequest_Raw{
			Raw: rawTemplate,
		},
	}

	authCtx, cancel, client := m.rawClientWithMacAuth(ctx)
	defer cancel()
	resp, err := client.FundVirtualPsbt(authCtx, req)
	if err != nil {
		return nil, err
	}

	return &tapsdk.FundedTransfer{
		FundedPsbt:        resp.FundedPsbt,
		PassiveAssetPsbts: resp.PassiveAssetPsbts,
	}, nil
}

// SignVirtualPsbt signs a virtual transaction.
func (m *walletKitClient) SignVirtualPsbt(ctx context.Context,
	fundedPsbt []byte) ([]byte, error) {

	req := &assetwalletrpc.SignVirtualPsbtRequest{
		FundedPsbt: fundedPsbt,
	}

	authCtx, cancel, client := m.rawClientWithMacAuth(ctx)
	defer cancel()
	resp, err := client.SignVirtualPsbt(authCtx, req)
	if err != nil {
		return nil, err
	}

	return resp.SignedPsbt, nil
}

// CommitVirtualPsbts commits virtual transactions using the legacy
// wallet-kit surface.
func (m *walletKitClient) CommitVirtualPsbts(ctx context.Context,
	virtualPsbts [][]byte, passivePsbts [][]byte,
	feeRate tapsdk.FeeRate) (*tapsdk.CommittedTransfer, error) {

	anchorPsbt, err := anchor.PreparePsbt(virtualPsbts, passivePsbts)
	if err != nil {
		return nil, fmt.Errorf("prepare anchor PSBT: %w", err)
	}

	resp, err := m.CommitVirtualPsbtsWithRequest(
		ctx, &tapsdk.CommitVirtualPsbtsRequest{
			AnchorPsbt:        anchorPsbt,
			VirtualPsbts:      virtualPsbts,
			PassiveAssetPsbts: passivePsbts,
			Funding: tapsdk.AnchorFundingPlan{
				ChangeOutput: tapsdk.AnchorChangeOutput{
					Mode: tapsdk.AnchorChangeOutputAdd,
				},
				Fee: tapsdk.AnchorFee{
					Mode:    tapsdk.AnchorFeeSatPerVByte,
					FeeRate: feeRate,
				},
			},
		},
	)
	if err != nil {
		return nil, err
	}

	return &tapsdk.CommittedTransfer{
		AnchorPsbt:        resp.AnchorPsbt,
		VirtualPsbts:      resp.VirtualPsbts,
		PassiveAssetPsbts: resp.PassiveAssetPsbts,
	}, nil
}

// CommitVirtualPsbtsWithRequest commits virtual transactions using the full
// advanced request DTO.
func (m *walletKitClient) CommitVirtualPsbtsWithRequest(ctx context.Context,
	req *tapsdk.CommitVirtualPsbtsRequest) (
	*tapsdk.CommitVirtualPsbtsResponse, error) {

	rpcReq, err := marshalCommitVirtualPsbtsRequest(req)
	if err != nil {
		return nil, err
	}

	authCtx, cancel, client := m.rawClientWithMacAuth(ctx)
	defer cancel()
	resp, err := client.CommitVirtualPsbts(authCtx, rpcReq)
	if err != nil {
		return nil, err
	}

	return unmarshalCommitVirtualPsbtsResponse(resp)
}

// PublishAndLogTransfer publishes the anchor transaction using the legacy
// wallet-kit surface and logs the transfer.
func (m *walletKitClient) PublishAndLogTransfer(ctx context.Context,
	anchorPsbt []byte, virtualPsbts [][]byte, passivePsbts [][]byte,
	skipAnchorTxBroadcast bool) (*tapsdk.AssetPacket, error) {

	return m.PublishAndLogTransferWithRequest(
		ctx, &tapsdk.PublishAndLogTransferRequest{
			AnchorPsbt:            anchorPsbt,
			VirtualPsbts:          virtualPsbts,
			PassiveAssetPsbts:     passivePsbts,
			ChangeOutputIndex:     0,
			SkipAnchorTxBroadcast: skipAnchorTxBroadcast,
		},
	)
}

// PublishAndLogTransferWithRequest publishes the anchor transaction and logs
// the transfer using the full advanced request DTO.
func (m *walletKitClient) PublishAndLogTransferWithRequest(ctx context.Context,
	req *tapsdk.PublishAndLogTransferRequest) (
	*tapsdk.AssetPacket, error) {

	rpcReq, err := marshalPublishAndLogTransferRequest(req)
	if err != nil {
		return nil, err
	}

	authCtx, cancel, client := m.rawClientWithMacAuth(ctx)
	defer cancel()
	resp, err := client.PublishAndLogTransfer(authCtx, rpcReq)
	if err != nil {
		return nil, err
	}
	if resp == nil || resp.Transfer == nil {
		return nil, fmt.Errorf("invalid transfer response")
	}

	return &tapsdk.AssetPacket{
		AnchorTransaction:        resp.Transfer.AnchorTx,
		VirtualTransactions:      req.VirtualPsbts,
		PassiveAssetTransactions: req.PassiveAssetPsbts,
	}, nil
}

func marshalCommitVirtualPsbtsRequest(
	req *tapsdk.CommitVirtualPsbtsRequest) (
	*assetwalletrpc.CommitVirtualPsbtsRequest, error) {

	if err := req.Validate(); err != nil {
		return nil, err
	}

	rpcReq := &assetwalletrpc.CommitVirtualPsbtsRequest{
		VirtualPsbts:          req.VirtualPsbts,
		PassiveAssetPsbts:     req.PassiveAssetPsbts,
		AnchorPsbt:            req.AnchorPsbt,
		CustomLockId:          req.Funding.CustomLockID,
		LockExpirationSeconds: req.Funding.LockExpirationSeconds,
		SkipFunding:           req.Funding.SkipFunding,
		TransitionProofVersion: assetwalletrpc.
			TransitionProofVersion_TRANSITION_PROOF_VERSION_V1,
	}

	if req.Funding.SkipFunding {
		return rpcReq, nil
	}

	switch change := req.Funding.ChangeOutput; change.Mode {
	case tapsdk.AnchorChangeOutputAdd:
		rpcReq.AnchorChangeOutput =
			&assetwalletrpc.CommitVirtualPsbtsRequest_Add{
				Add: true,
			}

	case tapsdk.AnchorChangeOutputNoNew:
		rpcReq.AnchorChangeOutput =
			&assetwalletrpc.CommitVirtualPsbtsRequest_Add{
				Add: false,
			}

	case tapsdk.AnchorChangeOutputExisting:
		rpcReq.AnchorChangeOutput =
			&assetwalletrpc.CommitVirtualPsbtsRequest_ExistingOutputIndex{
				ExistingOutputIndex: change.ExistingOutputIndex,
			}
	}

	switch fee := req.Funding.Fee; fee.Mode {
	case tapsdk.AnchorFeeSatPerVByte:
		rpcReq.Fees =
			&assetwalletrpc.CommitVirtualPsbtsRequest_SatPerVbyte{
				SatPerVbyte: fee.FeeRate.SatPerVByteCeil(),
			}

	case tapsdk.AnchorFeeTargetConf:
		rpcReq.Fees =
			&assetwalletrpc.CommitVirtualPsbtsRequest_TargetConf{
				TargetConf: fee.TargetConf,
			}
	}

	return rpcReq, nil
}

func unmarshalCommitVirtualPsbtsResponse(
	resp *assetwalletrpc.CommitVirtualPsbtsResponse) (
	*tapsdk.CommitVirtualPsbtsResponse, error) {

	if resp == nil {
		return nil, fmt.Errorf("nil commit virtual PSBTs response")
	}

	lockedUTXOs := make([]tapsdk.Outpoint, 0, len(resp.LndLockedUtxos))
	for _, rpcOutpoint := range resp.LndLockedUtxos {
		outpoint, err := unmarshalOutPoint(rpcOutpoint)
		if err != nil {
			return nil, fmt.Errorf("invalid locked utxo: %w", err)
		}

		lockedUTXOs = append(lockedUTXOs, outpoint)
	}

	return &tapsdk.CommitVirtualPsbtsResponse{
		AnchorPsbt:        resp.AnchorPsbt,
		VirtualPsbts:      resp.VirtualPsbts,
		PassiveAssetPsbts: resp.PassiveAssetPsbts,
		ChangeOutputIndex: resp.ChangeOutputIndex,
		LockedUTXOs:       lockedUTXOs,
	}, nil
}

func marshalPublishAndLogTransferRequest(
	req *tapsdk.PublishAndLogTransferRequest) (
	*assetwalletrpc.PublishAndLogRequest, error) {

	if err := req.Validate(); err != nil {
		return nil, err
	}

	rpcLockedUTXOs := make([]*taprpc.OutPoint, 0, len(req.LockedUTXOs))
	for _, outpoint := range req.LockedUTXOs {
		rpcLockedUTXOs = append(rpcLockedUTXOs, &taprpc.OutPoint{
			Txid:        outpoint.Txid[:],
			OutputIndex: outpoint.Index,
		})
	}

	return &assetwalletrpc.PublishAndLogRequest{
		AnchorPsbt:            req.AnchorPsbt,
		VirtualPsbts:          req.VirtualPsbts,
		PassiveAssetPsbts:     req.PassiveAssetPsbts,
		ChangeOutputIndex:     req.ChangeOutputIndex,
		LndLockedUtxos:        rpcLockedUTXOs,
		SkipAnchorTxBroadcast: req.SkipAnchorTxBroadcast,
		Label:                 req.Label,
	}, nil
}

// DeriveScriptKey derives a new script key for receiving assets.
// The script key includes both the internal key and the tweaked Taproot
// output key.
func (m *walletKitClient) DeriveScriptKey(ctx context.Context) (
	*tapsdk.ScriptKey, error) {

	req := &assetwalletrpc.NextScriptKeyRequest{
		KeyFamily: tapsdk.TaprootAssetsKeyFamily,
	}

	authCtx, cancel, client := m.rawClientWithMacAuth(ctx)
	defer cancel()
	resp, err := client.NextScriptKey(authCtx, req)
	if err != nil {
		return nil, err
	}

	if resp.ScriptKey == nil {
		return nil, fmt.Errorf("invalid script key response")
	}

	scriptKey, err := unmarshalScriptKey(resp.ScriptKey)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal script key: %w", err)
	}

	return scriptKey, nil
}

// DeriveInternalKey derives a new internal key for anchor outputs.
func (m *walletKitClient) DeriveInternalKey(ctx context.Context) (
	*tapsdk.InternalKey, error) {

	req := &assetwalletrpc.NextInternalKeyRequest{
		KeyFamily: tapsdk.TaprootAssetsKeyFamily,
	}

	authCtx, cancel, client := m.rawClientWithMacAuth(ctx)
	defer cancel()
	resp, err := client.NextInternalKey(authCtx, req)
	if err != nil {
		return nil, err
	}

	if resp.InternalKey == nil {
		return nil, fmt.Errorf("invalid internal key response")
	}

	internalKey, err := unmarshalKeyDescriptor(resp.InternalKey)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal internal key: %w", err)
	}

	return &tapsdk.InternalKey{
		PubKey:     internalKey.RawKeyBytes,
		KeyLocator: internalKey.KeyLocator,
	}, nil
}

// FundInteractivePsbt funds a virtual PSBT for interactive sends.
// The psbt parameter should be a serialized vPacket created for the
// interactive transfer.
func (m *walletKitClient) FundInteractivePsbt(ctx context.Context,
	psbt []byte) (*tapsdk.FundedTransfer, error) {

	req := &assetwalletrpc.FundVirtualPsbtRequest{
		Template: &assetwalletrpc.FundVirtualPsbtRequest_Psbt{
			Psbt: psbt,
		},
	}

	authCtx, cancel, client := m.rawClientWithMacAuth(ctx)
	defer cancel()
	resp, err := client.FundVirtualPsbt(authCtx, req)
	if err != nil {
		return nil, err
	}

	return &tapsdk.FundedTransfer{
		FundedPsbt:        resp.FundedPsbt,
		PassiveAssetPsbts: resp.PassiveAssetPsbts,
	}, nil
}

// AnchorVirtualPsbts anchors signed virtual PSBTs in a single call.
// This combines signing, committing, and publishing into one operation.
// It returns the completed transfer result with transaction details and proofs.
func (m *walletKitClient) AnchorVirtualPsbts(ctx context.Context,
	signedPsbts [][]byte) (*tapsdk.AssetTransfer, error) {

	req := &assetwalletrpc.AnchorVirtualPsbtsRequest{
		VirtualPsbts: signedPsbts,
	}

	authCtx, cancel, client := m.rawClientWithMacAuth(ctx)
	defer cancel()
	resp, err := client.AnchorVirtualPsbts(authCtx, req)
	if err != nil {
		return nil, err
	}

	if resp.Transfer == nil {
		return nil, fmt.Errorf("invalid transfer response")
	}

	return unmarshalAssetTransfer(resp.Transfer)
}

// unmarshalScriptKey converts an RPC ScriptKey to an tapsdk.ScriptKey.
func unmarshalScriptKey(rpcKey *taprpc.ScriptKey) (*tapsdk.ScriptKey, error) {
	pubKey, err := tapsdk.ParseTaprootPubKey(rpcKey.PubKey)
	if err != nil {
		return nil, fmt.Errorf("invalid script key public key: %w", err)
	}

	scriptKey := &tapsdk.ScriptKey{
		PubKey:   pubKey,
		TapTweak: rpcKey.TapTweak,
	}

	if rpcKey.KeyDesc != nil {
		keyDesc, err := unmarshalKeyDescriptor(rpcKey.KeyDesc)
		if err != nil {
			return nil, err
		}

		scriptKey.KeyDesc = *keyDesc
	}

	return scriptKey, nil
}

// unmarshalKeyDescriptor converts an RPC KeyDescriptor to an
// tapsdk.KeyDescriptor.
func unmarshalKeyDescriptor(rpcKey *taprpc.KeyDescriptor) (
	*tapsdk.KeyDescriptor, error) {

	rawKeyBytes, err := tapsdk.ParsePubKey(rpcKey.RawKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("invalid raw key bytes: %w", err)
	}

	keyDesc := &tapsdk.KeyDescriptor{
		RawKeyBytes: rawKeyBytes,
	}

	if rpcKey.KeyLoc != nil {
		keyDesc.KeyLocator = tapsdk.KeyLocator{
			Family: uint32(rpcKey.KeyLoc.KeyFamily),
			Index:  uint32(rpcKey.KeyLoc.KeyIndex),
		}
	}

	return keyDesc, nil
}

// QueryInternalKey looks up an internal key by its raw public key bytes.
// The input can be 32-byte x-only or 33-byte compressed.
func (m *walletKitClient) QueryInternalKey(ctx context.Context,
	internalKey []byte) (*tapsdk.KeyDescriptor, error) {

	authCtx, cancel, client := m.rawClientWithMacAuth(ctx)
	defer cancel()

	resp, err := client.QueryInternalKey(
		authCtx, &assetwalletrpc.QueryInternalKeyRequest{
			InternalKey: internalKey,
		},
	)
	if err != nil {
		return nil, err
	}

	if resp.InternalKey == nil {
		return nil, fmt.Errorf("nil internal key in response")
	}

	return unmarshalKeyDescriptor(resp.InternalKey)
}

// QueryScriptKey looks up a script key by its tweaked public key bytes.
// The input can be 32-byte x-only or 33-byte compressed.
func (m *walletKitClient) QueryScriptKey(ctx context.Context,
	tweakedScriptKey []byte) (*tapsdk.ScriptKey, error) {

	authCtx, cancel, client := m.rawClientWithMacAuth(ctx)
	defer cancel()

	resp, err := client.QueryScriptKey(
		authCtx, &assetwalletrpc.QueryScriptKeyRequest{
			TweakedScriptKey: tweakedScriptKey,
		},
	)
	if err != nil {
		return nil, err
	}

	if resp.ScriptKey == nil {
		return nil, fmt.Errorf("nil script key in response")
	}

	return unmarshalScriptKey(resp.ScriptKey)
}

// ProveAssetOwnership generates a proof of ownership for an asset.
func (m *walletKitClient) ProveAssetOwnership(ctx context.Context,
	req *tapsdk.ProveOwnershipRequest) (*tapsdk.OwnershipProof,
	error) {

	if err := req.AssetRef.Validate(); err != nil {
		return nil, err
	}

	assetID, ok := req.AssetRef.AssetID()
	if !ok {
		return nil, fmt.Errorf("prove ownership requires an " +
			"asset-ID ref; group-key refs span multiple " +
			"tranches (see issue #85)")
	}
	if err := tapsdk.ValidateOwnershipChallenge(req.Challenge); err != nil {
		return nil, err
	}

	authCtx, cancel, client := m.rawClientWithMacAuth(ctx)
	defer cancel()

	rpcReq := &assetwalletrpc.ProveAssetOwnershipRequest{
		AssetId:   assetID[:],
		ScriptKey: req.ScriptKey[:],
		Outpoint: &taprpc.OutPoint{
			Txid:        req.Outpoint.Txid[:],
			OutputIndex: req.Outpoint.Index,
		},
	}

	if len(req.Challenge) > 0 {
		rpcReq.Challenge = req.Challenge
	}

	resp, err := client.ProveAssetOwnership(authCtx, rpcReq)
	if err != nil {
		return nil, err
	}

	return &tapsdk.OwnershipProof{
		AssetRef:         req.AssetRef,
		IssuanceID:       assetID,
		ScriptKey:        req.ScriptKey,
		Outpoint:         req.Outpoint,
		ProofWithWitness: resp.ProofWithWitness,
	}, nil
}

// VerifyAssetOwnership verifies an asset ownership proof.
func (m *walletKitClient) VerifyAssetOwnership(ctx context.Context,
	req *tapsdk.VerifyOwnershipRequest) (
	*tapsdk.VerifyOwnershipResponse, error) {

	if err := tapsdk.ValidateOwnershipChallenge(req.Challenge); err != nil {
		return nil, err
	}

	authCtx, cancel, client := m.rawClientWithMacAuth(ctx)
	defer cancel()

	rpcReq := &assetwalletrpc.VerifyAssetOwnershipRequest{
		ProofWithWitness: req.ProofWithWitness,
	}

	if len(req.Challenge) > 0 {
		rpcReq.Challenge = req.Challenge
	}

	resp, err := client.VerifyAssetOwnership(authCtx, rpcReq)
	if err != nil {
		return nil, err
	}

	return unmarshalVerifyOwnershipResponse(resp)
}

func unmarshalVerifyOwnershipResponse(
	resp *assetwalletrpc.VerifyAssetOwnershipResponse) (
	*tapsdk.VerifyOwnershipResponse, error) {

	if resp == nil {
		return nil, fmt.Errorf("nil verify ownership response")
	}

	result := &tapsdk.VerifyOwnershipResponse{
		Valid:       resp.ValidProof,
		BlockHeight: resp.BlockHeight,
	}

	if resp.Outpoint != nil {
		if len(resp.Outpoint.Txid) != len(result.Outpoint.Txid) {
			return nil, fmt.Errorf(
				"invalid outpoint txid length: got %d bytes, want %d",
				len(resp.Outpoint.Txid), len(result.Outpoint.Txid),
			)
		}

		result.Outpoint = tapsdk.Outpoint{
			Index: resp.Outpoint.OutputIndex,
		}
		copy(result.Outpoint.Txid[:], resp.Outpoint.Txid)
	}

	switch len(resp.BlockHash) {
	case 0:

	case len(result.BlockHash):
		copy(result.BlockHash[:], resp.BlockHash)

	default:
		return nil, fmt.Errorf(
			"invalid block hash length: got %d bytes, want %d",
			len(resp.BlockHash), len(result.BlockHash),
		)
	}

	if result.Valid && result.Outpoint == (tapsdk.Outpoint{}) {
		return nil, fmt.Errorf(
			"valid ownership proof response missing outpoint",
		)
	}

	return result, nil
}

// RemoveUTXOLease removes a lease on a UTXO.
func (m *walletKitClient) RemoveUTXOLease(ctx context.Context,
	outpoint tapsdk.Outpoint) error {

	authCtx, cancel, client := m.rawClientWithMacAuth(ctx)
	defer cancel()

	_, err := client.RemoveUTXOLease(
		authCtx, &assetwalletrpc.RemoveUTXOLeaseRequest{
			Outpoint: &taprpc.OutPoint{
				Txid:        outpoint.Txid[:],
				OutputIndex: outpoint.Index,
			},
		},
	)

	return err
}

// DeclareScriptKey informs the wallet about an externally derived
// script key.
func (m *walletKitClient) DeclareScriptKey(ctx context.Context,
	req *tapsdk.DeclareScriptKeyRequest) (*tapsdk.ScriptKey,
	error) {

	authCtx, cancel, client := m.rawClientWithMacAuth(ctx)
	defer cancel()

	// The script key travels as a 32-byte x-only key on the RPC, while
	// the SDK type holds the compressed form.
	rpcKey := &taprpc.ScriptKey{
		PubKey:   req.ScriptKey.PubKey[1:],
		TapTweak: req.ScriptKey.TapTweak,
	}

	if req.ScriptKey.KeyDesc.RawKeyBytes != (tapsdk.PubKey{}) {
		rpcKey.KeyDesc = &taprpc.KeyDescriptor{
			RawKeyBytes: req.ScriptKey.KeyDesc.RawKeyBytes[:],
			KeyLoc: &taprpc.KeyLocator{
				KeyFamily: int32(
					req.ScriptKey.KeyDesc.KeyLocator.Family,
				),
				KeyIndex: int32(
					req.ScriptKey.KeyDesc.KeyLocator.Index,
				),
			},
		}
	}

	resp, err := client.DeclareScriptKey(
		authCtx, &assetwalletrpc.DeclareScriptKeyRequest{
			ScriptKey: rpcKey,
		},
	)
	if err != nil {
		return nil, err
	}

	if resp.ScriptKey == nil {
		return nil, fmt.Errorf("nil script key in response")
	}

	return unmarshalScriptKey(resp.ScriptKey)
}

// ExportBackup exports an asset wallet backup blob.
func (m *walletKitClient) ExportBackup(ctx context.Context,
	mode tapsdk.BackupMode) ([]byte, error) {

	authCtx, cancel, client := m.rawClientWithMacAuth(ctx)
	defer cancel()

	resp, err := client.ExportAssetWalletBackup(
		authCtx, &assetwalletrpc.ExportAssetWalletBackupRequest{
			Mode: marshalBackupMode(mode),
		},
	)
	if err != nil {
		return nil, err
	}

	return resp.Backup, nil
}

// ImportBackup imports assets from a previously exported wallet backup blob.
func (m *walletKitClient) ImportBackup(ctx context.Context,
	backup []byte) (uint32, error) {

	authCtx, cancel, client := m.rawClientWithMacAuth(ctx)
	defer cancel()

	resp, err := client.ImportAssetsFromBackup(
		authCtx, &assetwalletrpc.ImportAssetsFromBackupRequest{
			Backup: backup,
		},
	)
	if err != nil {
		return 0, err
	}

	return resp.NumImported, nil
}
