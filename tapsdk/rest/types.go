package rest

// types.go contains the JSON wire types returned by tapd's gRPC-gateway
// REST proxy. These are intermediate structures used only within the rest
// package to unmarshal HTTP responses. They are converted to
// tapsdk.* types before being returned to callers.
//
// Field names match the proto JSON encoding (snake_case).

// jsonGetInfoResponse is the JSON shape of taprpc.GetInfoResponse.
type jsonGetInfoResponse struct {
	Version           string `json:"version"`
	LndVersion        string `json:"lnd_version"`
	Network           string `json:"network"`
	LndIdentityPubkey string `json:"lnd_identity_pubkey"`
	NodeAlias         string `json:"node_alias"`
	BlockHeight       uint32 `json:"block_height"`
	BlockHash         string `json:"block_hash"`
	SyncToChain       bool   `json:"sync_to_chain"`
}

// jsonGenesisInfo is the JSON shape of taprpc.GenesisInfo.
type jsonGenesisInfo struct {
	GenesisPoint string `json:"genesis_point"`
	Name         string `json:"name"`
	MetaHash     string `json:"meta_hash"`
	AssetID      string `json:"asset_id"`
	AssetType    string `json:"asset_type"`
	OutputIndex  uint32 `json:"output_index"`
}

// jsonAssetGroup is the JSON shape of taprpc.AssetGroup.
type jsonAssetGroup struct {
	RawGroupKey     string `json:"raw_group_key"`
	TweakedGroupKey string `json:"tweaked_group_key"`
	TapscriptRoot   string `json:"tapscript_root"`
}

// jsonAsset is the JSON shape of taprpc.Asset.
type jsonAsset struct {
	// Version is serialised as the proto enum name (e.g.
	// "ASSET_VERSION_V0") by grpc-gateway.
	Version          string           `json:"version"`
	AssetGenesis     *jsonGenesisInfo `json:"asset_genesis"`
	Amount           string           `json:"amount"`
	LockTime         int32            `json:"lock_time"`
	RelativeLockTime int32            `json:"relative_lock_time"`
	ScriptVersion    int32            `json:"script_version"`
	ScriptKey        string           `json:"script_key"`
	AssetGroup       *jsonAssetGroup  `json:"asset_group"`
	ChainAnchor      *jsonChainAnchor `json:"chain_anchor"`
}

// jsonListAssetsResponse is the JSON shape of taprpc.ListAssetResponse.
type jsonListAssetsResponse struct {
	Assets []*jsonAsset `json:"assets"`
}

// jsonAssetBalance is the JSON shape of taprpc.AssetBalance.
type jsonAssetBalance struct {
	AssetGenesis *jsonGenesisInfo `json:"asset_genesis"`
	Balance      string           `json:"balance"`
	GroupKey     string           `json:"group_key"`
}

// jsonAssetGroupBalance is the JSON shape of taprpc.AssetGroupBalance.
type jsonAssetGroupBalance struct {
	GroupKey string `json:"group_key"`
	Balance  string `json:"balance"`
}

// jsonListBalancesResponse is the JSON shape of
// taprpc.ListBalancesResponse. grpc-gateway emits uint64 proto fields
// as strings, so UnconfirmedTransfers is a string and parsed by the
// unmarshal helper.
type jsonListBalancesResponse struct {
	AssetBalances        map[string]*jsonAssetBalance      `json:"asset_balances"`        //nolint:lll
	AssetGroupBalances   map[string]*jsonAssetGroupBalance `json:"asset_group_balances"`  //nolint:lll
	UnconfirmedTransfers string                            `json:"unconfirmed_transfers"` //nolint:lll
}

// jsonAnchorInfo is the JSON shape of taprpc.TransferOutputAnchor
// (nested inside TransferOutput). The corresponding proto fields are
// plain `outpoint` / `value` — not the `anchor_*` names that the
// top-level AnchorInfo message uses. Value is an int64 proto field
// which grpc-gateway serialises as a JSON string.
type jsonAnchorInfo struct {
	Outpoint string `json:"outpoint"`
	Value    string `json:"value"`
}

// jsonTransferOutput is the JSON shape of taprpc.TransferOutput.
type jsonTransferOutput struct {
	Amount       string          `json:"amount"`
	AssetID      string          `json:"asset_id"`
	AssetType    string          `json:"asset_type"`
	ScriptKey    string          `json:"script_key"`
	NewProofBlob string          `json:"new_proof_blob"`
	Anchor       *jsonAnchorInfo `json:"anchor"`
	GroupKey     string          `json:"group_key"`
}

// jsonTransferInput is the JSON shape of taprpc.TransferInput.
type jsonTransferInput struct {
	AnchorPoint string `json:"anchor_point"`
	AssetID     string `json:"asset_id"`
	AssetType   string `json:"asset_type"`
	ScriptKey   string `json:"script_key"`
	Amount      string `json:"amount"`
	GroupKey    string `json:"group_key"`
}

// jsonBlockHash is the JSON shape of taprpc.ProtoHash.
type jsonBlockHash struct {
	Hash    string `json:"hash"`
	HashStr string `json:"hash_str"`
}

// jsonAssetTransfer is the JSON shape of
// taprpc.AssetTransfer.
type jsonAssetTransfer struct {
	TransferTimestamp  string `json:"transfer_timestamp"`
	AnchorTxHash       string `json:"anchor_tx_hash"`
	AnchorTxHeightHint uint32 `json:"anchor_tx_height_hint"`
	AnchorTxChainFees  string `json:"anchor_tx_chain_fees"`

	Inputs  []*jsonTransferInput  `json:"inputs"`
	Outputs []*jsonTransferOutput `json:"outputs"`

	AnchorTxBlockHash   *jsonBlockHash `json:"anchor_tx_block_hash"`
	AnchorTxBlockHeight uint32         `json:"anchor_tx_block_height"`
	Label               string         `json:"label"`
	AnchorTx            string         `json:"anchor_tx"`
}

// jsonListTransfersResponse is the JSON shape of
// taprpc.ListTransfersResponse.
type jsonListTransfersResponse struct {
	Transfers []*jsonAssetTransfer `json:"transfers"`
}

// jsonSendAssetResponse is the JSON shape of taprpc.SendAssetResponse.
type jsonSendAssetResponse struct {
	Transfer *jsonAssetTransfer `json:"transfer"`
}

// jsonAddr is the JSON shape of taprpc.Addr.
type jsonAddr struct {
	Encoded          string `json:"encoded"`
	AssetID          string `json:"asset_id"`
	AssetType        string `json:"asset_type"`
	Amount           string `json:"amount"`
	GroupKey         string `json:"group_key"`
	ScriptKey        string `json:"script_key"`
	InternalKey      string `json:"internal_key"`
	TapscriptSibling string `json:"tapscript_sibling"`
	TaprootOutputKey string `json:"taproot_output_key"`
	ProofCourierAddr string `json:"proof_courier_addr"`
	AssetVersion     string `json:"asset_version"`
	AddressVersion   string `json:"address_version"`
}

// jsonQueryAddrsResponse is the JSON shape of taprpc.QueryAddrResponse.
type jsonQueryAddrsResponse struct {
	Addrs []*jsonAddr `json:"addrs"`
}

// jsonAddrEvent is the JSON shape of taprpc.AddrEvent.
type jsonAddrEvent struct {
	CreationTimeUnixSeconds string    `json:"creation_time_unix_seconds"`
	Addr                    *jsonAddr `json:"addr"`
	Status                  string    `json:"status"`
	Outpoint                string    `json:"outpoint"`
	UtxoAmtSat              string    `json:"utxo_amt_sat"`
	TaprootSibling          string    `json:"taproot_sibling"`
	ConfirmationHeight      uint32    `json:"confirmation_height"`
	HasProof                bool      `json:"has_proof"`
}

// jsonAddrReceivesResponse is the JSON shape of
// taprpc.AddrReceivesResponse.
type jsonAddrReceivesResponse struct {
	Events []*jsonAddrEvent `json:"events"`
}

// jsonProofFile is the JSON shape of taprpc.ProofFile.
type jsonProofFile struct {
	RawProofFile string `json:"raw_proof_file"`
	GenesisPoint string `json:"genesis_point"`
}

// jsonUnpackProofFileResponse is the JSON shape of
// taprpc.UnpackProofFileResponse.
type jsonUnpackProofFileResponse struct {
	RawProofs []string `json:"raw_proofs"`
}

// jsonChainAnchor is the JSON shape of taprpc.AnchorInfo in proof
// context.
type jsonChainAnchor struct {
	AnchorOutpoint string `json:"anchor_outpoint"`
}

// jsonPrevWitness is the JSON shape of taprpc.PrevWitness.
type jsonPrevWitness struct {
	PrevID          *jsonPrevID          `json:"prev_id"`
	SplitCommitment *jsonSplitCommitment `json:"split_commitment"`
}

// jsonSplitCommitment records whether a decoded witness is a synthetic split
// leaf. Its nested root asset isn't needed by the SDK's proof summary.
type jsonSplitCommitment struct{}

// jsonPrevID is the JSON shape of taprpc.PrevInputAsset.
type jsonPrevID struct {
	AnchorPoint string `json:"anchor_point"`
	AssetID     string `json:"asset_id"`
	ScriptKey   string `json:"script_key"`
}

// jsonGenesisReveal is the JSON shape of taprpc.GenesisReveal.
type jsonGenesisReveal struct {
	GenesisBaseReveal any `json:"genesis_base_reveal"`
}

// jsonDecodedAsset is the inner asset of a decoded proof.
type jsonDecodedAsset struct {
	AssetGenesis  *jsonGenesisInfo   `json:"asset_genesis"`
	Amount        string             `json:"amount"`
	ScriptKey     string             `json:"script_key"`
	ChainAnchor   *jsonChainAnchor   `json:"chain_anchor"`
	AssetGroup    *jsonAssetGroup    `json:"asset_group"`
	PrevWitnesses []*jsonPrevWitness `json:"prev_witnesses"`
}

// jsonDecodedProof is the JSON shape of taprpc.DecodedProof.
type jsonDecodedProof struct {
	ProofAtDepth   uint32             `json:"proof_at_depth"`
	NumberOfProofs uint32             `json:"number_of_proofs"`
	Asset          *jsonDecodedAsset  `json:"asset"`
	GenesisReveal  *jsonGenesisReveal `json:"genesis_reveal"`
	// AltLeaves is a proto `bytes` field — hex-encoded on the wire
	// thanks to UseHexForBytes.
	AltLeaves string `json:"alt_leaves"`
}

// jsonDecodeProofResponse is the JSON shape of
// taprpc.DecodeProofResponse.
type jsonDecodeProofResponse struct {
	DecodedProof *jsonDecodedProof `json:"decoded_proof"`
}

// jsonRegisteredAsset is the JSON shape of the inner asset in
// RegisterTransferResponse.
type jsonRegisteredAsset struct {
	AssetGenesis *jsonGenesisInfo `json:"asset_genesis"`
	Amount       string           `json:"amount"`
	ScriptKey    string           `json:"script_key"`
	ChainAnchor  *jsonChainAnchor `json:"chain_anchor"`
}

// jsonRegisterTransferResponse is the JSON shape of
// taprpc.RegisterTransferResponse.
type jsonRegisterTransferResponse struct {
	RegisteredAsset *jsonRegisteredAsset `json:"registered_asset"`
}

// jsonScriptKey is the JSON shape of taprpc.ScriptKey.
type jsonScriptKey struct {
	PubKey   string             `json:"pub_key"`
	KeyDesc  *jsonKeyDescriptor `json:"key_desc"`
	TapTweak string             `json:"tap_tweak"`
}

// jsonKeyDescriptor is the JSON shape of taprpc.KeyDescriptor.
type jsonKeyDescriptor struct {
	RawKeyBytes string          `json:"raw_key_bytes"`
	KeyLoc      *jsonKeyLocator `json:"key_loc"`
}

// jsonKeyLocator is the JSON shape of taprpc.KeyLocator.
type jsonKeyLocator struct {
	KeyFamily int32 `json:"key_family"`
	KeyIndex  int32 `json:"key_index"`
}

// jsonExternalKey is the JSON shape of taprpc.ExternalKey.
type jsonExternalKey struct {
	XPub              string `json:"xpub"`
	MasterFingerprint string `json:"master_fingerprint"`
	DerivationPath    string `json:"derivation_path"`
}

// jsonNextScriptKeyResponse is the JSON shape of
// assetwalletrpc.NextScriptKeyResponse.
type jsonNextScriptKeyResponse struct {
	ScriptKey *jsonScriptKey `json:"script_key"`
}

// jsonNextInternalKeyResponse is the JSON shape of
// assetwalletrpc.NextInternalKeyResponse.
type jsonNextInternalKeyResponse struct {
	InternalKey *jsonKeyDescriptor `json:"internal_key"`
}

// jsonFundVirtualPsbtResponse is the JSON shape of
// assetwalletrpc.FundVirtualPsbtResponse.
type jsonFundVirtualPsbtResponse struct {
	FundedPsbt        string   `json:"funded_psbt"`
	PassiveAssetPsbts []string `json:"passive_asset_psbts"`
}

// jsonSignVirtualPsbtResponse is the JSON shape of
// assetwalletrpc.SignVirtualPsbtResponse.
type jsonSignVirtualPsbtResponse struct {
	SignedPsbt string `json:"signed_psbt"`
}

// jsonCommitVirtualPsbtsResponse is the JSON shape of
// assetwalletrpc.CommitVirtualPsbtsResponse.
type jsonCommitVirtualPsbtsResponse struct {
	AnchorPsbt        string          `json:"anchor_psbt"`
	VirtualPsbts      []string        `json:"virtual_psbts"`
	PassiveAssetPsbts []string        `json:"passive_asset_psbts"`
	ChangeOutputIndex int32           `json:"change_output_index"`
	LndLockedUtxos    []*jsonOutpoint `json:"lnd_locked_utxos"`
}

// jsonPublishAndLogResponse is the JSON shape of
// assetwalletrpc.PublishAndLogResponse.
type jsonPublishAndLogResponse struct {
	Transfer *jsonPublishTransfer `json:"transfer"`
}

// jsonPublishTransfer is the anchor transfer from PublishAndLog.
type jsonPublishTransfer struct {
	AnchorTx string `json:"anchor_tx"`
}

// jsonInsertProofResponse is the JSON shape of
// universerpc.AssetProofResponse.
type jsonInsertProofResponse struct {
	// We don't need the response fields; just check for errors.
}

// jsonAssetMeta is the JSON shape of taprpc.AssetMeta.
type jsonAssetMeta struct {
	Data     string `json:"data"`
	Type     string `json:"type"`
	MetaHash string `json:"meta_hash"`
}

// jsonMintAsset is the JSON shape of mintrpc.MintAsset.
type jsonMintAsset struct {
	AssetVersion       string `json:"asset_version"`
	AssetType          string `json:"asset_type"`
	Name               string `json:"name"`
	Amount             string `json:"amount"`
	NewGroupedAsset    bool   `json:"new_grouped_asset"`
	GroupedAsset       bool   `json:"grouped_asset"`
	GroupKey           string `json:"group_key"`
	GroupAnchor        string `json:"group_anchor"`
	GroupTapscriptRoot string `json:"group_tapscript_root"`

	ScriptKey        *jsonScriptKey     `json:"script_key"`
	DecimalDisplay   uint32             `json:"decimal_display"`
	AssetMeta        *jsonAssetMeta     `json:"asset_meta"`
	GroupInternalKey *jsonKeyDescriptor `json:"group_internal_key"`
	ExternalGroupKey *jsonExternalKey   `json:"external_group_key"`

	EnableSupplyCommitments bool `json:"enable_supply_commitments"` //nolint:lll
}

// jsonPendingAsset is the JSON shape of mintrpc.PendingAsset.
type jsonPendingAsset struct {
	AssetVersion       string             `json:"asset_version"`
	AssetType          string             `json:"asset_type"`
	Name               string             `json:"name"`
	Amount             string             `json:"amount"`
	NewGroupedAsset    bool               `json:"new_grouped_asset"`
	GroupKey           string             `json:"group_key"`
	GroupAnchor        string             `json:"group_anchor"`
	GroupTapscriptRoot string             `json:"group_tapscript_root"`
	AssetMeta          *jsonAssetMeta     `json:"asset_meta"`
	ScriptKey          *jsonScriptKey     `json:"script_key"`
	GroupInternalKey   *jsonKeyDescriptor `json:"group_internal_key"`
}

// jsonTxOut is the JSON shape of wire.TxOut.
type jsonTxOut struct {
	Value    string `json:"value"`
	PkScript string `json:"pk_script"`
}

// jsonGroupKeyRequest is the JSON shape of taprpc.GroupKeyRequest.
type jsonGroupKeyRequest struct {
	RawKey        *jsonKeyDescriptor `json:"raw_key"`
	AnchorGenesis *jsonGenesisInfo   `json:"anchor_genesis"`
	TapscriptRoot string             `json:"tapscript_root"`
	NewAsset      string             `json:"new_asset"`
	ExternalKey   *jsonExternalKey   `json:"external_key"`
}

// jsonGroupVirtualTx is the JSON shape of taprpc.GroupVirtualTx.
type jsonGroupVirtualTx struct {
	Transaction string     `json:"transaction"`
	PrevOut     *jsonTxOut `json:"prev_out"`
	GenesisID   string     `json:"genesis_id"`
	TweakedKey  string     `json:"tweaked_key"`
}

// jsonUnsealedAsset is the JSON shape of mintrpc.UnsealedAsset.
type jsonUnsealedAsset struct {
	Asset            *jsonPendingAsset    `json:"asset"`
	GroupKeyRequest  *jsonGroupKeyRequest `json:"group_key_request"`
	GroupVirtualTx   *jsonGroupVirtualTx  `json:"group_virtual_tx"`
	GroupVirtualPSBT string               `json:"group_virtual_psbt"`
}

// jsonMintingBatch is the JSON shape of mintrpc.MintingBatch.
type jsonMintingBatch struct {
	BatchKey   string              `json:"batch_key"`
	BatchTxid  string              `json:"batch_txid"`
	State      string              `json:"state"`
	CreatedAt  string              `json:"created_at"`
	HeightHint uint32              `json:"height_hint"`
	BatchPsbt  string              `json:"batch_psbt"`
	Assets     []*jsonPendingAsset `json:"assets"`
}

// jsonMintAssetResponse is the JSON shape of
// mintrpc.MintAssetResponse.
type jsonMintAssetResponse struct {
	PendingBatch *jsonMintingBatch `json:"pending_batch"`
}

// jsonVerboseBatch is the JSON shape of mintrpc.VerboseBatch.
type jsonVerboseBatch struct {
	Batch          *jsonMintingBatch    `json:"batch"`
	UnsealedAssets []*jsonUnsealedAsset `json:"unsealed_assets"`
}

// jsonFundBatchResponse is the JSON shape of
// mintrpc.FundBatchResponse.
type jsonFundBatchResponse struct {
	Batch *jsonVerboseBatch `json:"batch"`
}

// jsonSealBatchResponse is the JSON shape of
// mintrpc.SealBatchResponse.
type jsonSealBatchResponse struct {
	Batch *jsonMintingBatch `json:"batch"`
}

// jsonFinalizeBatchResponse is the JSON shape of
// mintrpc.FinalizeBatchResponse.
type jsonFinalizeBatchResponse struct {
	Batch *jsonMintingBatch `json:"batch"`
}

// jsonCancelBatchResponse is the JSON shape of
// mintrpc.CancelBatchResponse.
type jsonCancelBatchResponse struct {
	BatchKey string `json:"batch_key"`
}

// jsonListBatchesResponse is the JSON shape of
// mintrpc.ListBatchResponse.
type jsonListBatchesResponse struct {
	Batches []*jsonVerboseBatch `json:"batches"`
}

// --- WalletClient stub types ---

// jsonManagedUtxo is the JSON shape of taprpc.ManagedUtxo.
type jsonManagedUtxo struct {
	Outpoint         string       `json:"out_point"`
	AmtSat           string       `json:"amt_sat"`
	InternalKey      string       `json:"internal_key"`
	TaprootAssetRoot string       `json:"taproot_asset_root"`
	MerkleRoot       string       `json:"merkle_root"`
	Assets           []*jsonAsset `json:"assets"`
	LeaseOwner       string       `json:"lease_owner"`
	LeaseExpiryUnix  string       `json:"lease_expiry_unix"`
}

// jsonListUtxosResponse is the JSON shape of
// taprpc.ListUtxosResponse.
type jsonListUtxosResponse struct {
	ManagedUtxos map[string]*jsonManagedUtxo `json:"managed_utxos"`
}

// jsonAssetHumanReadable is the JSON shape of taprpc.AssetHumanReadable, the
// simplified asset record returned by tapd's ListGroups RPC (distinct from the
// full jsonAsset used by ListAssets).
type jsonAssetHumanReadable struct {
	ID               string `json:"id"`
	Amount           string `json:"amount"`
	LockTime         int32  `json:"lock_time"`
	RelativeLockTime int32  `json:"relative_lock_time"`
	Tag              string `json:"tag"`
	MetaHash         string `json:"meta_hash"`
	Type             string `json:"type"`
	Version          string `json:"version"`
}

// jsonGroupedAssets is the JSON shape of taprpc.GroupedAssets.
type jsonGroupedAssets struct {
	Assets []*jsonAssetHumanReadable `json:"assets"`
}

// jsonListGroupsResponse is the JSON shape of
// taprpc.ListGroupsResponse.
type jsonListGroupsResponse struct {
	Groups map[string]*jsonGroupedAssets `json:"groups"`
}

// jsonBurnAssetResponse is the JSON shape of
// taprpc.BurnAssetResponse.
type jsonBurnAssetResponse struct {
	BurnTransfer *jsonAssetTransfer `json:"burn_transfer"`
	BurnProof    *jsonDecodedProof  `json:"burn_proof"`
}

// jsonAssetBurn is the JSON shape of taprpc.AssetBurn.
type jsonAssetBurn struct {
	Note            string `json:"note"`
	AssetID         string `json:"asset_id"`
	TweakedGroupKey string `json:"tweaked_group_key"`
	Amount          string `json:"amount"`
	AnchorTxid      string `json:"anchor_txid"`
	AssetType       string `json:"asset_type"`
}

// jsonListBurnsResponse is the JSON shape of
// taprpc.ListBurnsResponse.
type jsonListBurnsResponse struct {
	Burns []*jsonAssetBurn `json:"burns"`
}

// jsonFetchAssetMetaResponse is the JSON shape of
// taprpc.FetchAssetMetaResponse (same as taprpc.AssetMeta).
type jsonFetchAssetMetaResponse struct {
	Data     string `json:"data"`
	Type     string `json:"type"`
	MetaHash string `json:"meta_hash"`
}

// jsonVerifyProofResponse is the JSON shape of
// taprpc.VerifyProofResponse.
type jsonVerifyProofResponse struct {
	Valid        bool              `json:"valid"`
	DecodedProof *jsonDecodedProof `json:"decoded_proof"`
}

// --- WalletKitClient types ---

// jsonQueryInternalKeyResponse is the JSON shape of
// assetwalletrpc.QueryInternalKeyResponse.
type jsonQueryInternalKeyResponse struct {
	InternalKey *jsonKeyDescriptor `json:"internal_key"`
}

// jsonQueryScriptKeyResponse is the JSON shape of
// assetwalletrpc.QueryScriptKeyResponse.
type jsonQueryScriptKeyResponse struct {
	ScriptKey *jsonScriptKey `json:"script_key"`
}

// jsonOwnershipProof is the JSON shape of
// assetwalletrpc.ProveAssetOwnershipResponse.
type jsonOwnershipProof struct {
	ProofWithWitness string `json:"proof_with_witness"`
}

// jsonVerifyOwnershipResponse is the JSON shape of
// assetwalletrpc.VerifyAssetOwnershipResponse.
type jsonVerifyOwnershipResponse struct {
	ValidProof   bool          `json:"valid_proof"`
	Outpoint     *jsonOutpoint `json:"outpoint"`
	OutpointStr  string        `json:"outpoint_str"`
	BlockHash    string        `json:"block_hash"`
	BlockHashStr string        `json:"block_hash_str"`
	BlockHeight  uint32        `json:"block_height"`
}

// jsonDeclareScriptKeyResponse is the JSON shape of
// assetwalletrpc.DeclareScriptKeyResponse.
type jsonDeclareScriptKeyResponse struct {
	ScriptKey *jsonScriptKey `json:"script_key"`
}

// jsonExportAssetWalletBackupResponse is the JSON shape of
// assetwalletrpc.ExportAssetWalletBackupResponse.
type jsonExportAssetWalletBackupResponse struct {
	Backup string `json:"backup"`
}

// jsonImportAssetsFromBackupResponse is the JSON shape of
// assetwalletrpc.ImportAssetsFromBackupResponse.
type jsonImportAssetsFromBackupResponse struct {
	NumImported uint32 `json:"num_imported"`
}

// --- UniverseClient types ---

// jsonMerkleSumNode is the JSON shape of universerpc.MerkleSumNode.
type jsonMerkleSumNode struct {
	RootHash string `json:"root_hash"`
	RootSum  string `json:"root_sum"`
}

// jsonUniverseRoot is the JSON shape of universerpc.UniverseRoot.
type jsonUniverseRoot struct {
	ID               *jsonUniverseID    `json:"id"`
	MSSMTRoot        *jsonMerkleSumNode `json:"mssmt_root"`
	AssetName        string             `json:"asset_name"`
	AmountsByAssetID map[string]string  `json:"amounts_by_asset_id"` //nolint:lll
}

// jsonAssetRootsResponse is the JSON shape of
// universerpc.AssetRootResponse.
type jsonAssetRootsResponse struct {
	UniverseRoots map[string]*jsonUniverseRoot `json:"universe_roots"` //nolint:lll
}

// jsonQueryRootResponse is the JSON shape of
// universerpc.QueryRootResponse.
type jsonQueryRootResponse struct {
	IssuanceRoot *jsonUniverseRoot `json:"issuance_root"`
	TransferRoot *jsonUniverseRoot `json:"transfer_root"`
}

// jsonDeleteRootResponse is the JSON shape of
// universerpc.DeleteRootResponse (empty body).
type jsonDeleteRootResponse struct {
}

// jsonAssetLeafKeysResponse is the JSON shape of
// universerpc.AssetLeafKeysResponse.
type jsonAssetLeafKeysResponse struct {
	AssetKeys []*jsonAssetKey `json:"asset_keys"`
}

// jsonAssetKey is the JSON shape of universerpc.AssetKey.
type jsonAssetKey struct {
	Outpoint       *jsonOutpoint `json:"outpoint"`
	Op             *jsonOutpoint `json:"op"`
	OpStr          string        `json:"op_str"`
	ScriptKey      string        `json:"script_key"`
	ScriptKeyBytes string        `json:"script_key_bytes"`
	ScriptKeyStr   string        `json:"script_key_str"`
}

// jsonOutpoint is the JSON shape of taprpc.Outpoint.
type jsonOutpoint struct {
	Txid        string `json:"txid"`
	HashStr     string `json:"hash_str"`
	OutputIndex uint32 `json:"output_index"`
	Index       uint32 `json:"index"`
}

// jsonAssetLeavesResponse is the JSON shape of
// universerpc.AssetLeafResponse.
type jsonAssetLeavesResponse struct {
	Leaves []*jsonAssetLeafResp `json:"leaves"`
}

// jsonAssetLeafResp is the JSON shape of universerpc.AssetLeaf in
// responses.
type jsonAssetLeafResp struct {
	Asset *jsonAsset `json:"asset"`
	Proof string     `json:"proof"`
}

// jsonQueryProofResponse is the JSON shape of
// universerpc.AssetProofResponse.
type jsonQueryProofResponse struct {
	Req                      *jsonUniverseKey   `json:"req"`
	UniverseRoot             *jsonUniverseRoot  `json:"universe_root"`
	UniverseInclusionProof   string             `json:"universe_inclusion_proof"` //nolint:lll
	AssetLeaf                *jsonAssetLeafResp `json:"asset_leaf"`
	MultiverseRoot           *jsonMerkleSumNode `json:"multiverse_root"`
	MultiverseInclusionProof string             `json:"multiverse_inclusion_proof"` //nolint:lll
}

// jsonUniverseStatsResponse is the JSON shape of
// universerpc.UniverseAssetStats.
type jsonUniverseStatsResponse struct {
	NumTotalAssets string `json:"num_total_assets"`
	NumTotalGroups string `json:"num_total_groups"`
	NumTotalSyncs  string `json:"num_total_syncs"`
	NumTotalProofs string `json:"num_total_proofs"`
}

// jsonAssetStatsResponse is the JSON shape of
// universerpc.AssetStatsQueryResponse.
type jsonAssetStatsResponse struct {
	AssetStats []*jsonAssetStatsSnapshot `json:"asset_stats"`
}

// jsonAssetStatsSnapshot is the JSON shape of
// universerpc.AssetStatsSnapshot.
type jsonAssetStatsSnapshot struct {
	AssetID     string               `json:"asset_id"`
	GroupKey    string               `json:"group_key"`
	GroupSupply string               `json:"group_supply"`
	GroupAnchor *jsonAssetStatsAsset `json:"group_anchor"`
	Asset       *jsonAssetStatsAsset `json:"asset"`
	TotalSyncs  string               `json:"total_syncs"`
	TotalProofs string               `json:"total_proofs"`
}

// jsonAssetStatsAsset is the JSON shape of
// universerpc.AssetStatsAsset.
type jsonAssetStatsAsset struct {
	AssetID          string `json:"asset_id"`
	GenesisPoint     string `json:"genesis_point"`
	TotalSupply      string `json:"total_supply"`
	AssetName        string `json:"asset_name"`
	AssetType        string `json:"asset_type"`
	GenesisHeight    int32  `json:"genesis_height"`
	GenesisTimestamp string `json:"genesis_timestamp"`
	AnchorPoint      string `json:"anchor_point"`
	DecimalDisplay   uint32 `json:"decimal_display"`
}

// jsonQueryEventsResponse is the JSON shape of
// universerpc.QueryEventsResponse.
type jsonQueryEventsResponse struct {
	Events []*jsonGroupedUniverseEvents `json:"events"`
}

// jsonGroupedUniverseEvents is the JSON shape of
// universerpc.GroupedUniverseEvents.
type jsonGroupedUniverseEvents struct {
	Date           string `json:"date"`
	SyncEvents     string `json:"sync_events"`
	NewProofEvents string `json:"new_proof_events"`
}

// jsonListFederationServersResponse is the JSON shape of
// universerpc.ListFederationServersResponse.
type jsonListFederationServersResponse struct {
	Servers []*jsonUniverseFederationServer `json:"servers"`
}

// jsonUniverseFederationServer is the JSON shape of
// universerpc.UniverseFederationServer.
type jsonUniverseFederationServer struct {
	Host string `json:"host"`
	ID   int32  `json:"id"`
}

// jsonAddFederationServerRequest is the JSON body for
// AddFederationServer.
type jsonAddFederationServerRequest struct {
	Servers []*jsonUniverseFederationServer `json:"servers"`
}

// jsonAddFederationServerResponse is the JSON shape of
// universerpc.AddFederationServerResponse (empty body).
type jsonAddFederationServerResponse struct {
}

// jsonDeleteFederationServerRequest is the JSON body for
// DeleteFederationServer.
type jsonDeleteFederationServerRequest struct {
	Servers []*jsonUniverseFederationServer `json:"servers"`
}

// jsonDeleteFederationServerResponse is the JSON shape of
// universerpc.DeleteFederationServerResponse (empty body).
type jsonDeleteFederationServerResponse struct {
}

// jsonSetFederationSyncConfigRequest is the JSON body for
// SetFederationSyncConfig.
type jsonSetFederationSyncConfigRequest struct {
	GlobalSyncConfigs []*jsonGlobalFederationSyncConfig `json:"global_sync_configs"` //nolint:lll
	AssetSyncConfigs  []*jsonAssetFederationSyncConfig  `json:"asset_sync_configs"`  //nolint:lll
}

// jsonGlobalFederationSyncConfig is the JSON shape of
// universerpc.GlobalFederationSyncConfig.
type jsonGlobalFederationSyncConfig struct {
	ProofType       string `json:"proof_type"`
	AllowSyncInsert bool   `json:"allow_sync_insert"`
	AllowSyncExport bool   `json:"allow_sync_export"`
}

// jsonAssetFederationSyncConfig is the JSON shape of
// universerpc.AssetFederationSyncConfig.
type jsonAssetFederationSyncConfig struct {
	ID              *jsonUniverseID `json:"id"`
	AllowSyncInsert bool            `json:"allow_sync_insert"`
	AllowSyncExport bool            `json:"allow_sync_export"`
}

// jsonSetFederationSyncConfigResponse is the JSON shape of
// universerpc.SetFederationSyncConfigResponse (empty body).
type jsonSetFederationSyncConfigResponse struct {
}

// jsonQueryFederationSyncConfigResponse is the JSON shape of
// universerpc.QueryFederationSyncConfigResponse.
type jsonQueryFederationSyncConfigResponse struct {
	GlobalSyncConfigs []*jsonGlobalFederationSyncConfig `json:"global_sync_configs"` //nolint:lll
	AssetSyncConfigs  []*jsonAssetFederationSyncConfig  `json:"asset_sync_configs"`  //nolint:lll
}

// jsonInfoResponse is the JSON shape of
// universerpc.InfoResponse.
type jsonInfoResponse struct {
	RuntimeID string `json:"runtime_id"`
}

// jsonSyncUniverseRequest is the JSON body for SyncUniverse.
type jsonSyncUniverseRequest struct {
	UniverseHost string            `json:"universe_host"`
	SyncMode     string            `json:"sync_mode"`
	SyncTargets  []*jsonSyncTarget `json:"sync_targets"`
}

// jsonSyncTarget is the JSON shape of universerpc.SyncTarget.
type jsonSyncTarget struct {
	ID *jsonUniverseID `json:"id"`
}

// jsonSyncUniverseResponse is the JSON shape of
// universerpc.SyncResponse.
type jsonSyncUniverseResponse struct {
	SyncedUniverses []*jsonSyncedUniverse `json:"synced_universes"`
}

// jsonSyncedUniverse is the JSON shape of
// universerpc.SyncedUniverse.
type jsonSyncedUniverse struct {
	OldAssetRoot   *jsonUniverseRoot    `json:"old_asset_root"`
	NewAssetRoot   *jsonUniverseRoot    `json:"new_asset_root"`
	NewAssetLeaves []*jsonAssetLeafResp `json:"new_asset_leaves"`
}

// jsonSubscribeReceiveEventsRequest is the JSON body for
// TaprootAssets.SubscribeReceiveEvents.
type jsonSubscribeReceiveEventsRequest struct {
	FilterAddr     string `json:"filter_addr,omitempty"`
	StartTimestamp string `json:"start_timestamp,omitempty"`
}

// jsonSubscribeSendEventsRequest is the JSON body for
// TaprootAssets.SubscribeSendEvents.
type jsonSubscribeSendEventsRequest struct {
	FilterScriptKey string `json:"filter_script_key,omitempty"`
	FilterLabel     string `json:"filter_label,omitempty"`
	StartTimestamp  string `json:"start_timestamp,omitempty"`
}

// jsonSubscribeMintEventsRequest is the JSON body for
// Mint.SubscribeMintEvents.
type jsonSubscribeMintEventsRequest struct {
	ShortResponse bool `json:"short_response,omitempty"`
}

// jsonReceiveEvent is the JSON shape of taprpc.ReceiveEvent.
type jsonReceiveEvent struct {
	Timestamp          string    `json:"timestamp"`
	Address            *jsonAddr `json:"address"`
	Outpoint           string    `json:"outpoint"`
	Status             string    `json:"status"`
	ConfirmationHeight uint32    `json:"confirmation_height"`
	Error              string    `json:"error"`
}

// jsonSendEvent is the JSON shape of taprpc.SendEvent.
type jsonSendEvent struct {
	Timestamp             string                 `json:"timestamp"`
	SendState             string                 `json:"send_state"`
	ParcelType            string                 `json:"parcel_type"`
	Addresses             []*jsonAddr            `json:"addresses"`
	VirtualPackets        []string               `json:"virtual_packets"`
	PassiveVirtualPackets []string               `json:"passive_virtual_packets"` //nolint:lll
	AnchorTransaction     *jsonAnchorTransaction `json:"anchor_transaction"`
	Transfer              *jsonAssetTransfer     `json:"transfer"`
	Error                 string                 `json:"error"`
	TransferLabel         string                 `json:"transfer_label"`
	NextSendState         string                 `json:"next_send_state"`
}

// jsonAnchorTransaction is the JSON shape of taprpc.AnchorTransaction.
type jsonAnchorTransaction struct {
	AnchorPsbt         string          `json:"anchor_psbt"`
	ChangeOutputIndex  int32           `json:"change_output_index"`
	ChainFeesSats      string          `json:"chain_fees_sats"`
	TargetFeeRateSatKw int32           `json:"target_fee_rate_sat_kw"`
	LndLockedUtxos     []*jsonOutpoint `json:"lnd_locked_utxos"`
	FinalTx            string          `json:"final_tx"`
}

// jsonMintEvent is the JSON shape of mintrpc.MintEvent.
type jsonMintEvent struct {
	Timestamp  string            `json:"timestamp"`
	BatchState string            `json:"batch_state"`
	Batch      *jsonMintingBatch `json:"batch"`
	Error      string            `json:"error"`
}
