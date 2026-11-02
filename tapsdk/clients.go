package tapsdk

import (
	"context"
)

// Client combines all sub-clients into a single interface.
type Client interface {
	WalletClient
	ProofClient
	WalletKitClient
	UniverseClient
	MintClient
	EventClient

	Close() error
}

// WalletClient exposes the TaprootAssets service gRPC client.
type WalletClient interface {
	GetInfo(ctx context.Context) (*Info, error)

	// ListAssetRecords lists tapd wallet asset records with optional filtering.
	// This is the low-level per-output/per-issuance surface. Use the
	// high-level Wallet.ListAssets, Wallet.ListCollections, and
	// Wallet.ListIssuances methods for SDK business entities. Amount filters
	// in ListAssetsRequest are forwarded to tapd as per-record filters here.
	ListAssetRecords(ctx context.Context,
		req *ListAssetsRequest) ([]*AssetRecord, error)

	// ListBalances lists wallet balances keyed by AssetRef.
	ListBalances(ctx context.Context,
		req *ListBalancesRequest) (*ListBalancesResponse,
		error)

	// ListTransfers lists outgoing transfers with optional filtering.
	ListTransfers(ctx context.Context,
		req *ListTransfersRequest) ([]*AssetTransfer, error)

	// SendAsset performs a low-level one-shot address-based send using the
	// TaprootAssets service. For reusable V2 addresses, specify amounts
	// through SendAssetRequest.Recipients.
	//
	// This method intentionally exposes the raw daemon capability for advanced
	// callers. Higher-level send APIs should remain opinionated about semantic
	// asset identity and default address handling.
	SendAsset(ctx context.Context,
		req *SendAssetRequest) (*AssetTransfer, error)

	// NewAddr creates a new Taproot Asset address for receiving assets.
	// The address is stored in tapd and can be queried later.
	//
	// For V0/V1 addresses, AssetRef must resolve to a concrete issuance asset
	// ID. For V2 addresses, AssetRef may resolve to either an issuance asset ID
	// or a group key.
	//
	// If ScriptKey and InternalKey are not provided, tapd derives them
	// from its internal wallet.
	NewAddr(ctx context.Context,
		req *NewAddressRequest) (*Address, error)

	// DecodeAddr decodes a bech32m Taproot Asset address string into its
	// components. This does not store the address or require it to be
	// previously known.
	DecodeAddr(ctx context.Context, addr string) (*Address, error)

	// QueryAddrs returns addresses that were previously created by this
	// tapd instance, filtered by the query parameters.
	QueryAddrs(ctx context.Context,
		query *AddressQuery) ([]*Address, error)

	// AddrReceives returns incoming transfer events for addresses created
	// by this tapd instance. Use this to track the status of expected
	// incoming transfers.
	AddrReceives(ctx context.Context,
		query *AddressReceivesQuery) ([]*AddressEvent,
		error)

	// ListUtxos lists managed UTXOs with optional filtering.
	ListUtxos(ctx context.Context,
		req *ListUtxosRequest) (
		map[string]*ManagedUtxo, error)

	// ListAssetGroups lists all known protocol-level asset groups. This is a
	// low-level group inspection surface; most wallet callers should use
	// Wallet.ListAssets, Wallet.ListCollections, or Wallet.ListIssuances
	// instead.
	ListAssetGroups(ctx context.Context) ([]AssetGroupRecord, error)

	// BurnAsset burns asset units. The confirmation text must be set
	// to "assets will be destroyed" for the burn to succeed.
	BurnAsset(ctx context.Context,
		req *BurnAssetRequest) (
		*BurnAssetResponse, error)

	// ListBurns lists asset burns with optional filtering.
	ListBurns(ctx context.Context,
		req *ListBurnsRequest) ([]*BurnRecord, error)

	// FetchAssetMeta fetches the metadata for an asset by AssetRef or meta
	// hash.
	FetchAssetMeta(ctx context.Context,
		req *FetchAssetMetaRequest) (
		*AssetMeta, error)

	// VerifyProof verifies a proof file and returns the decoded last
	// proof if valid.
	VerifyProof(ctx context.Context,
		rawProofFile []byte) (
		*VerifyProofResponse, error)
}

// ProofClient exposes proof-related operations from the TaprootAssets service.
type ProofClient interface {
	// ExportProof exports a proof file for a specific asset output.
	// If outpoint is nil, then the latest proof for the given asset/script key
	// is exported. AssetRef must resolve to an asset ID; group-key refs are
	// rejected because a single proof commits to exactly one tranche.
	ExportProof(ctx context.Context, ref AssetRef,
		scriptKey PubKey,
		outpoint *Outpoint) (*ProofFile, error)

	// UnpackProofFile unpacks a proof file into individual proofs.
	UnpackProofFile(ctx context.Context, rawProofFile []byte) ([][]byte, error)

	// DecodeProof decodes a raw proof and returns details about it.
	DecodeProof(ctx context.Context,
		rawProof []byte) (*DecodedProof, error)

	// RegisterTransfer registers an inbound transfer for an interactive send.
	// The proof must already be in the local universe before calling this.
	RegisterTransfer(ctx context.Context, assetRef AssetRef,
		scriptKey PubKey,
		outpoint Outpoint) (*RegisteredAsset, error)
}

// WalletKitClient exposes the AssetWalletClient service gRPC client.
type WalletKitClient interface {
	// DeriveScriptKey derives a new script key for receiving assets.
	// The script key includes both the internal key and the tweaked
	// Taproot output key.
	DeriveScriptKey(ctx context.Context) (*ScriptKey, error)

	// DeriveInternalKey derives a new internal key for anchor outputs.
	DeriveInternalKey(ctx context.Context) (*InternalKey, error)

	// FundTransfer funds a virtual transaction using addresses.
	FundTransfer(ctx context.Context, recipients []Recipient,
		inputs []PrevID) (*FundedTransfer, error)

	// FundInteractivePsbt funds a virtual PSBT for interactive sends.
	FundInteractivePsbt(ctx context.Context, psbt []byte) (
		*FundedTransfer, error)

	// SignVirtualPsbt signs a virtual transaction.
	SignVirtualPsbt(ctx context.Context, fundedPsbt []byte) ([]byte, error)

	// CommitVirtualPsbts commits virtual transactions using an anchor fee
	// rate. Advanced callers that need to preserve a caller-supplied anchor
	// template should use CustomAnchorWalletKitClient when the concrete
	// client implements it.
	CommitVirtualPsbts(ctx context.Context, virtualPsbts [][]byte,
		passivePsbts [][]byte, feeRate FeeRate) (*CommittedTransfer,
		error)

	// AnchorVirtualPsbts anchors signed virtual PSBTs in a single call.
	// This combines committing and publishing into one operation.
	AnchorVirtualPsbts(ctx context.Context, signedPsbts [][]byte) (
		*AssetTransfer, error)

	// PublishAndLogTransfer publishes the anchor transaction and logs the
	// transfer.
	PublishAndLogTransfer(ctx context.Context, anchorPsbt []byte,
		virtualPsbts [][]byte, passivePsbts [][]byte,
		skipAnchorTxBroadcast bool) (*AssetPacket, error)

	// QueryInternalKey looks up an internal key by its raw public key
	// bytes. The input can be 32-byte x-only or 33-byte compressed.
	QueryInternalKey(ctx context.Context,
		internalKey []byte) (*KeyDescriptor, error)

	// QueryScriptKey looks up a script key by its tweaked public key
	// bytes. The input can be 32-byte x-only or 33-byte compressed.
	QueryScriptKey(ctx context.Context,
		tweakedScriptKey []byte) (*ScriptKey, error)

	// ProveAssetOwnership generates a proof of ownership for an
	// asset.
	ProveAssetOwnership(ctx context.Context,
		req *ProveOwnershipRequest) (
		*OwnershipProof, error)

	// VerifyAssetOwnership verifies an asset ownership proof.
	VerifyAssetOwnership(ctx context.Context,
		req *VerifyOwnershipRequest) (
		*VerifyOwnershipResponse, error)

	// RemoveUTXOLease removes a lease on a UTXO.
	RemoveUTXOLease(ctx context.Context,
		outpoint Outpoint) error

	// DeclareScriptKey informs the wallet about an externally derived
	// script key.
	DeclareScriptKey(ctx context.Context,
		req *DeclareScriptKeyRequest) (
		*ScriptKey, error)

	// ExportBackup exports an asset wallet backup blob using the requested
	// backup mode.
	ExportBackup(ctx context.Context,
		mode BackupMode) ([]byte, error)

	// ImportBackup imports assets from a previously exported wallet backup
	// blob and returns the number of imported assets.
	ImportBackup(ctx context.Context, backup []byte) (uint32, error)
}

// CustomAnchorWalletKitClient is an optional WalletKitClient extension for
// advanced anchor construction. It accepts the full request DTOs required to
// preserve caller-owned Bitcoin transaction topology and durable funding
// metadata. Keeping this separate from WalletKitClient preserves source
// compatibility for existing client implementations and test doubles.
type CustomAnchorWalletKitClient interface {
	// CommitVirtualPsbtsWithRequest commits virtual transactions into the
	// caller-supplied anchor PSBT according to the exact funding plan.
	CommitVirtualPsbtsWithRequest(ctx context.Context,
		req *CommitVirtualPsbtsRequest) (*CommitVirtualPsbtsResponse,
		error)

	// PublishAndLogTransferWithRequest publishes or logs a finalized custom
	// anchor transfer together with the commit response metadata.
	PublishAndLogTransferWithRequest(ctx context.Context,
		req *PublishAndLogTransferRequest) (*AssetPacket, error)
}

// CustomAnchorCapabilityProvider is an optional extension implemented by
// clients that can provide a custom-anchor capability profile. It is kept
// separate from WalletKitClient so existing clients and test doubles aren't
// forced to implement an SDK-specific discovery method that tapd does not
// expose over RPC.
//
// Implementations must distinguish an explicitly unsupported feature from an
// unknown one. Callers fail closed for both states.
type CustomAnchorCapabilityProvider interface {
	CustomAnchorCapabilities(ctx context.Context) (
		*CustomAnchorCapabilities, error)
}

// UniverseClient exposes the Universe service gRPC client.
type UniverseClient interface {
	// InsertProof inserts a proof into the local universe.
	InsertProof(ctx context.Context, rawProof []byte,
		decoded *DecodedProof) error

	// AssetRoots returns the known universe roots for all assets.
	AssetRoots(ctx context.Context,
		req *AssetRootRequest) (map[string]*UniverseRoot,
		error)

	// QueryAssetRoots queries the issuance and transfer roots for a
	// specific asset identified by its UniverseID.
	QueryAssetRoots(ctx context.Context,
		id *UniverseID) (*QueryRootResponse, error)

	// DeleteAssetRoot deletes a universe root and all associated data
	// for a given asset.
	DeleteAssetRoot(ctx context.Context,
		id *UniverseID) error

	// AssetLeafKeys returns the set of leaf keys for a universe.
	AssetLeafKeys(ctx context.Context,
		req *AssetLeafKeysRequest) ([]AssetLeafKey,
		error)

	// AssetLeaves returns the set of asset leaves for a universe.
	AssetLeaves(ctx context.Context,
		id *UniverseID) ([]AssetLeaf, error)

	// QueryProof queries a specific proof from the universe by its
	// full key (universe ID + leaf key).
	QueryProof(ctx context.Context,
		key *UniverseKey) (*AssetProofResponse,
		error)

	// UniverseStats returns aggregate statistics for the universe.
	UniverseStats(ctx context.Context) (*UniverseStats, error)

	// QueryAssetStats returns per-asset statistics filtered and sorted
	// by the given query parameters.
	QueryAssetStats(ctx context.Context,
		req *AssetStatsQuery) ([]AssetStatsSnapshot,
		error)

	// QueryEvents returns daily sync and proof event counts within a
	// time range.
	QueryEvents(ctx context.Context,
		req *QueryEventsRequest) ([]GroupedUniverseEvents,
		error)

	// ListFederationServers lists the universe federation peers.
	ListFederationServers(
		ctx context.Context) ([]FederationServer, error)

	// AddFederationServer adds servers to the federation.
	AddFederationServer(ctx context.Context,
		servers []FederationServer) error

	// DeleteFederationServer removes servers from the federation.
	DeleteFederationServer(ctx context.Context,
		servers []FederationServer) error

	// SetFederationSyncConfig sets the federation sync configuration.
	SetFederationSyncConfig(ctx context.Context,
		global []GlobalFederationSyncConfig,
		asset []AssetFederationSyncConfig) error

	// QueryFederationSyncConfig queries the federation sync
	// configuration.
	QueryFederationSyncConfig(ctx context.Context,
		ids []UniverseID) (*FederationSyncConfig,
		error)

	// Info returns basic universe server information.
	Info(ctx context.Context) (*UniverseInfo, error)

	// SyncUniverse synchronizes with a remote universe server.
	SyncUniverse(ctx context.Context,
		req *SyncRequest) ([]SyncedUniverse, error)
}

// MintClient exposes low-level minting operations from the Mint service gRPC
// client.
//
// These methods are batch building blocks for advanced callers. Application
// code that wants business entities should use Issuer instead.
type MintClient interface {
	// MintAsset adds a brand-new asset to the pending mint batch.
	MintAsset(ctx context.Context,
		req *MintAssetRequest) (*MintingBatch, error)

	// MintIssuance adds an additional issuance/tranche for an existing
	// asset in the pending mint batch.
	MintIssuance(ctx context.Context,
		req *MintIssuanceRequest) (*MintingBatch, error)

	// FundBatch funds the current pending mint batch.
	FundBatch(ctx context.Context,
		req *FundBatchRequest) (*VerboseMintingBatch,
		error)

	// SealBatch seals a funded batch before finalization.
	SealBatch(ctx context.Context,
		req *SealBatchRequest) (*MintingBatch, error)

	// FinalizeBatch finalizes the current pending mint batch.
	FinalizeBatch(ctx context.Context,
		req *FinalizeBatchRequest) (*MintingBatch, error)

	// CancelBatch cancels the current mint batch.
	CancelBatch(ctx context.Context) (*CancelBatchResponse, error)

	// ListBatches lists mint batches known to the daemon.
	ListBatches(ctx context.Context,
		req *ListBatchesRequest) ([]*VerboseMintingBatch,
		error)
}

// EventClient exposes server-streaming RPCs for real-time event
// notifications. All subscribe methods return a channel that delivers
// events until the context is cancelled or the stream encounters an
// error. The error channel receives at most one value: the terminal
// stream error (nil on clean shutdown).
type EventClient interface {
	// SubscribeReceiveEvents streams incoming asset transfer events.
	// Filter by address or start time via the request. The returned
	// channels are closed when the context is cancelled.
	SubscribeReceiveEvents(ctx context.Context,
		req *SubscribeReceiveEventsRequest) (
		<-chan *ReceiveEventRecord, <-chan error, error)

	// SubscribeSendEvents streams outgoing asset transfer events.
	// Filter by script key, label, or start time via the request.
	SubscribeSendEvents(ctx context.Context,
		req *SubscribeSendEventsRequest) (
		<-chan *SendEventRecord, <-chan error, error)

	// SubscribeMintEvents streams minting batch lifecycle events.
	SubscribeMintEvents(ctx context.Context,
		req *SubscribeMintEventsRequest) (
		<-chan *MintEvent, <-chan error, error)
}
