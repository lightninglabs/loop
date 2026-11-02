package tapsdk

import (
	"context"
	"fmt"
	"math"
	"math/bits"
	"strings"
	"sync"

	"github.com/lightninglabs/loop/tapsdk/internal/vpsbt"
)

// Wallet constitutes the high level service giving access to
// Taproot Assets features.
type Wallet struct {
	client Client

	networkHRP              string
	coinType                uint32
	defaultProofCourierAddr string
	issuerMu                sync.Mutex
}

// WalletOption configures optional Wallet behavior.
type WalletOption func(*Wallet)

const authMailboxUniverseCourierScheme = "authmailbox+universerpc://"
const burnConfirmationText = "assets will be destroyed"

type transferRegistrarWithIssuance interface {
	RegisterTransferWithIssuance(ctx context.Context,
		assetRef AssetRef, issuanceID AssetID,
		scriptKey PubKey, outpoint Outpoint) (
		*RegisteredAsset, error)
}

// WithDefaultProofCourierAddr sets the default proof courier address used by
// high-level V2 receive address helpers.
func WithDefaultProofCourierAddr(addr string) WalletOption {
	return func(w *Wallet) {
		w.defaultProofCourierAddr = addr
	}
}

// WithAuthMailboxCourier configures the default proof courier for high-level
// V2 receive address helpers using the auth mailbox transport. The host should
// include the port, for example "tapd.example:10029".
func WithAuthMailboxCourier(host string) WalletOption {
	return WithDefaultProofCourierAddr(
		authMailboxUniverseCourierScheme + host,
	)
}

// NewWallet creates a new Wallet instance.
func NewWallet(client Client, network Network,
	opts ...WalletOption) *Wallet {

	networkHRP, coinType, err := getNetworkParams(network)
	if err != nil {
		panic(err)
	}

	wallet := &Wallet{
		client:     client,
		networkHRP: networkHRP,
		coinType:   coinType,
	}

	for _, opt := range opts {
		opt(wallet)
	}

	return wallet
}

// NewTxBuilder returns a new transaction builder for address-based transfers.
func (s *Wallet) NewTxBuilder() *TxBuilder {
	return newTxBuilder(s.client)
}

// NewInteractiveTxBuilder returns a new builder for interactive transfers
// where the receiver provides their keys directly.
func (s *Wallet) NewInteractiveTxBuilder() *InteractiveTxBuilder {
	return newInteractiveTxBuilder(s.client, s.networkHRP, s.coinType)
}

// Client returns the low-level composite client wrapped by the wallet.
//
// Most application code should prefer the Wallet methods. Use this accessor
// for advanced RPC-shaped operations that intentionally are not promoted onto
// the high-level wallet surface.
func (s *Wallet) Client() Client {
	return s.client
}

// CustomAnchorCapabilities reports the optional capability profile exposed by
// the configured client. tapd does not currently expose a capability RPC, so a
// client that doesn't implement CustomAnchorCapabilityProvider returns an
// all-unknown profile. Unknown capabilities fail closed when Require is used.
func (s *Wallet) CustomAnchorCapabilities(
	ctx context.Context) (*CustomAnchorCapabilities, error) {

	provider, ok := s.client.(CustomAnchorCapabilityProvider)
	if !ok {
		return &CustomAnchorCapabilities{}, nil
	}

	caps, err := provider.CustomAnchorCapabilities(ctx)
	if err != nil {
		return nil, wrapErr("CustomAnchorCapabilities", err)
	}
	if caps == nil {
		return &CustomAnchorCapabilities{}, nil
	}

	return caps, nil
}

// Close releases resources held by the underlying client.
func (s *Wallet) Close() error {
	return s.client.Close()
}

// NewReceiveAddress creates a V2 address for receiving the given
// asset. The sender chooses the specific units and amount to send.
// Collectible/NFT addresses always receive exactly one unit; the SDK
// automatically supplies that amount when tapd rejects the default
// sender-chosen amount shape.
//
// For more control (custom keys, V0/V1 addresses, explicit amounts),
// use the lower-level NewAddr method on the client directly. If your
// tapd does not expose a suitable default proof courier for V2
// addresses, configure the wallet with WithAuthMailboxCourier or
// WithDefaultProofCourierAddr.
func (s *Wallet) NewReceiveAddress(ctx context.Context,
	ref AssetRef) (*Address, error) {

	v2 := AddressVersionV2
	req := &NewAddressRequest{
		AssetRef:         ref,
		ProofCourierAddr: s.defaultProofCourierAddr,
		AddressVersion:   &v2,
	}

	addr, err := s.client.NewAddr(ctx, req)
	if err != nil {
		if shouldRetryCollectibleAmount(ref, err) {
			req.Amount = 1

			addr, retryErr := s.client.NewAddr(ctx, req)
			if retryErr == nil {
				return addr, nil
			}

			err = retryErr
		}

		if shouldRetryExactGroupRef(ref, err) {
			exactRef := s.resolveExactGroupRef(ctx, ref)
			if exactRef != ref {
				req.AssetRef = exactRef

				addr, retryErr := s.client.NewAddr(ctx, req)
				if retryErr == nil {
					return addr, nil
				}
			}
		}

		return nil, wrapErr("NewReceiveAddress", err)
	}

	return addr, nil
}

func shouldRetryCollectibleAmount(ref AssetRef, err error) bool {
	if ref.IsZero() || err == nil {
		return false
	}

	return strings.Contains(err.Error(), "collectible asset amount not one")
}

func shouldRetryExactGroupRef(ref AssetRef, err error) bool {
	if !ref.IsGroupRef() || err == nil {
		return false
	}

	errMsg := err.Error()
	return strings.Contains(errMsg, "unable to find asset or group") ||
		strings.Contains(errMsg, "asset lookup failed")
}

func (s *Wallet) resolveExactGroupRef(ctx context.Context,
	ref AssetRef) AssetRef {

	if !ref.IsGroupRef() {
		return ref
	}

	groups, err := s.client.ListAssetGroups(ctx)
	if err != nil {
		return ref
	}

	for _, group := range groups {
		if group.AssetRef.Equivalent(ref) {
			return group.AssetRef
		}
	}

	return ref
}

// GetBalance returns the confirmed balance for the given asset. If the
// wallet holds units across multiple issuances, the total is aggregated
// into a single sum.
//
// If the wallet has no record of the asset ref, GetBalance returns an
// error wrapping ErrAssetUnknown; detect it with errors.Is. "Known"
// means the local universe has an issuance or transfer root for the
// ref, which tapd populates for assets the wallet minted, received,
// or bootstrapped via SyncUniverse. A known asset with zero confirmed
// units returns (0, nil).
//
// The universe probe fires only on the cold path where ListBalances
// returns an empty map, so the common "have I been paid?" poll still
// costs a single RPC.
func (s *Wallet) GetBalance(ctx context.Context,
	ref AssetRef) (uint64, error) {

	resp, err := s.ListBalances(ctx, &ListBalancesRequest{
		AssetRef: &ref,
	})
	if err != nil {
		return 0, wrapErr("GetBalance", err)
	}

	if balance, ok := resp.Balances[ref.String()]; ok {
		return balance.Balance, nil
	}

	return 0, wrapErr("GetBalance", fmt.Errorf(
		"%w: %s", ErrAssetUnknown, ref,
	))
}

// ListBalances returns wallet balances keyed by user-facing AssetRef.
//
// If the request filters by AssetRef and the wallet has no confirmed units,
// ListBalances checks the local universe to distinguish a known zero balance
// from an unknown asset ref. Known zero balances are returned as an entry with
// Balance set to zero. Unknown refs return an error wrapping ErrAssetUnknown.
func (s *Wallet) ListBalances(ctx context.Context,
	req *ListBalancesRequest) (*ListBalancesResponse, error) {

	resp, err := s.client.ListBalances(ctx, req)
	if err != nil {
		return nil, wrapErr("ListBalances", err)
	}

	if resp == nil {
		resp = &ListBalancesResponse{}
	}
	if resp.Balances == nil {
		resp.Balances = make(map[string]*Balance)
	}

	if req == nil || req.AssetRef == nil {
		return resp, nil
	}

	ref := *req.AssetRef
	if _, ok := resp.Balances[ref.String()]; ok {
		return resp, nil
	}

	known, err := s.assetKnown(ctx, ref)
	if err != nil {
		return nil, wrapErr("ListBalances", err)
	}
	if known {
		resp.Balances[ref.String()] = &Balance{
			AssetRef: ref,
		}

		return resp, nil
	}

	return nil, wrapErr("ListBalances", fmt.Errorf(
		"%w: %s", ErrAssetUnknown, ref,
	))
}

func (s *Wallet) assetKnown(ctx context.Context,
	ref AssetRef) (bool, error) {

	roots, err := s.client.QueryAssetRoots(ctx, &UniverseID{
		AssetRef:  ref,
		ProofType: ProofTypeIssuance,
	})
	if err != nil {
		return false, err
	}
	if roots != nil &&
		(roots.IssuanceRoot != nil || roots.TransferRoot != nil) {

		return true, nil
	}

	return false, nil
}

// DeriveKeys derives a new script key and internal key for receiving assets.
// The receiver calls this method and shares the result with the sender for
// interactive transfers.
//
// This is a convenience method that combines DeriveScriptKey and
// DeriveInternalKey into a single call.
func (s *Wallet) DeriveKeys(ctx context.Context) (*DerivedKeys,
	error) {

	scriptKey, err := s.client.DeriveScriptKey(ctx)
	if err != nil {
		return nil, wrapErr("DeriveKeys", err)
	}

	internalKey, err := s.client.DeriveInternalKey(ctx)
	if err != nil {
		return nil, wrapErr("DeriveKeys", err)
	}

	return &DerivedKeys{
		ScriptKey:   *scriptKey,
		InternalKey: *internalKey,
	}, nil
}

// ListAssets returns wallet assets keyed by user-facing AssetRef.
//
// Grouped fungible issuances are aggregated under one group-key AssetRef. A
// single NFT is returned as one asset-ID AssetRef. NFT collection items are
// returned as NFT assets with their item asset-ID AssetRef and optional
// CollectionRef; use ListCollections for collection-level rows. Amount filters
// are applied after this high-level aggregation.
func (s *Wallet) ListAssets(ctx context.Context,
	req *ListAssetsRequest) ([]*Asset, error) {

	records, err := s.listAssetRecords(ctx, listAssetsToRecordsReq(req))
	if err != nil {
		return nil, wrapErr("ListAssets", err)
	}

	return filterAssetsByAmount(assetsFromRecords(records), req), nil
}

// ListCollections returns wallet-known NFT collections.
//
// A collection is a group of NFT assets. It is not returned by ListAssets as an
// Asset because it is not itself transferable or spendable.
func (s *Wallet) ListCollections(ctx context.Context,
	req *ListCollectionsRequest) ([]*Collection, error) {

	records, err := s.listAssetRecords(ctx, listCollectionsToRecordsReq(req))
	if err != nil {
		return nil, wrapErr("ListCollections", err)
	}

	return collectionsFromRecords(records), nil
}

// ListIssuances returns wallet-known fungible issuance/tranche records.
//
// This is the issuer/admin/debug companion to ListAssets. It intentionally
// excludes NFTs and collections.
func (s *Wallet) ListIssuances(ctx context.Context,
	req *ListIssuancesRequest) ([]*Issuance, error) {

	records, err := s.listAssetRecords(ctx, listIssuancesToRecordsReq(req))
	if err != nil {
		return nil, wrapErr("ListIssuances", err)
	}

	return issuancesFromRecords(records), nil
}

// ListCollectionItems returns wallet-known NFT assets that belong to a
// collection.
//
// Each returned Asset uses the concrete NFT asset-ID AssetRef and has
// CollectionRef set to the collection AssetRef.
func (s *Wallet) ListCollectionItems(ctx context.Context,
	req *ListCollectionItemsRequest) ([]*Asset, error) {

	records, err := s.listAssetRecords(ctx, listCollectionItemsToRecordsReq(req))
	if err != nil {
		return nil, wrapErr("ListCollectionItems", err)
	}

	items := make([]*Asset, 0, len(records))
	for _, asset := range assetsFromRecords(records) {
		if asset.Type != AssetTypeCollectible ||
			asset.CollectionRef == nil {

			continue
		}

		items = append(items, asset)
	}

	return items, nil
}

// ListTransfers returns high-level wallet transfers keyed by AssetRef.
func (s *Wallet) ListTransfers(ctx context.Context,
	req *ListTransfersRequest) ([]*Transfer, error) {

	rawTransfers, err := s.client.ListTransfers(ctx, req)
	if err != nil {
		return nil, wrapErr("ListTransfers", err)
	}

	transfers := make([]*Transfer, 0, len(rawTransfers))
	for _, raw := range rawTransfers {
		if raw == nil {
			continue
		}

		transfers = append(transfers, NewTransfer(raw))
	}

	return transfers, nil
}

func (s *Wallet) listAssetRecords(ctx context.Context,
	req *ListAssetsRequest) ([]*AssetRecord, error) {

	return s.client.ListAssetRecords(ctx, req)
}

func listAssetsToRecordsReq(
	req *ListAssetsRequest) *ListAssetsRequest {

	if req == nil {
		return nil
	}

	recordReq := *req
	recordReq.MinAmount = 0
	recordReq.MaxAmount = 0

	return &recordReq
}

func listIssuancesToRecordsReq(
	req *ListIssuancesRequest) *ListAssetsRequest {

	if req == nil {
		return nil
	}

	return &ListAssetsRequest{
		AssetRef: req.AssetRef,
	}
}

func listCollectionsToRecordsReq(
	req *ListCollectionsRequest) *ListAssetsRequest {

	if req == nil {
		return nil
	}

	return &ListAssetsRequest{
		AssetRef: req.AssetRef,
	}
}

func listCollectionItemsToRecordsReq(
	req *ListCollectionItemsRequest) *ListAssetsRequest {

	if req == nil {
		return nil
	}

	recordReq := &ListAssetsRequest{}
	switch {
	case req.CollectionRef != nil:
		recordReq.AssetRef = req.CollectionRef
	case req.AssetRef != nil:
		recordReq.AssetRef = req.AssetRef
	}

	return recordReq
}

type assetAccumulator struct {
	asset      *Asset
	seenNFTIDs map[AssetID]struct{}
}

func assetsFromRecords(records []*AssetRecord) []*Asset {
	accs := make(map[string]*assetAccumulator)
	order := make([]string, 0, len(records))

	for _, record := range records {
		if record == nil {
			continue
		}

		asset := assetFromRecord(record)
		key := asset.AssetRef.String()

		acc, ok := accs[key]
		if !ok {
			acc = &assetAccumulator{
				asset: asset,
			}
			accs[key] = acc
			order = append(order, key)
		}

		if record.Genesis.Type == AssetTypeCollectible {
			if acc.seenNFTIDs == nil {
				acc.seenNFTIDs = make(map[AssetID]struct{})
			}

			if _, ok := acc.seenNFTIDs[record.Genesis.IssuanceID]; ok {
				continue
			}

			acc.seenNFTIDs[record.Genesis.IssuanceID] = struct{}{}
		}

		acc.asset.Amount = addSaturatingUint64(
			acc.asset.Amount, record.Amount,
		)
	}

	assets := make([]*Asset, 0, len(order))
	for _, key := range order {
		assets = append(assets, accs[key].asset)
	}

	return assets
}

func filterAssetsByAmount(assets []*Asset,
	req *ListAssetsRequest) []*Asset {

	if req == nil || (req.MinAmount == 0 && req.MaxAmount == 0) {
		return assets
	}

	filtered := make([]*Asset, 0, len(assets))
	for _, asset := range assets {
		if asset == nil {
			continue
		}

		if req.MinAmount != 0 && asset.Amount < req.MinAmount {
			continue
		}
		if req.MaxAmount != 0 && asset.Amount > req.MaxAmount {
			continue
		}

		filtered = append(filtered, asset)
	}

	return filtered
}

func assetFromRecord(record *AssetRecord) *Asset {
	ref := record.AssetRef
	var collectionRef *AssetRef
	if record.Genesis.Type == AssetTypeCollectible && ref.IsGroupRef() {
		collRef := ref
		collectionRef = &collRef
		ref = AssetRefFromAssetID(record.Genesis.IssuanceID)
	}

	return &Asset{
		AssetRef:      ref,
		Type:          record.Genesis.Type,
		Name:          record.Genesis.Tag,
		MetaHash:      record.Genesis.MetaHash,
		CollectionRef: collectionRef,
	}
}

type collectionAccumulator struct {
	collection *Collection
	itemIDs    map[AssetID]struct{}
}

func collectionsFromRecords(
	records []*AssetRecord) []*Collection {

	accs := make(map[string]*collectionAccumulator)
	order := make([]string, 0, len(records))

	for _, record := range records {
		if record == nil ||
			record.Genesis.Type != AssetTypeCollectible ||
			!record.AssetRef.IsGroupRef() {

			continue
		}

		key := record.AssetRef.String()
		acc, ok := accs[key]
		if !ok {
			acc = &collectionAccumulator{
				collection: &Collection{
					AssetRef: record.AssetRef,
				},
				itemIDs: make(map[AssetID]struct{}),
			}
			accs[key] = acc
			order = append(order, key)
		}

		acc.itemIDs[record.Genesis.IssuanceID] = struct{}{}
		acc.collection.ItemCount = uint64(len(acc.itemIDs))
	}

	collections := make([]*Collection, 0, len(order))
	for _, key := range order {
		collections = append(collections, accs[key].collection)
	}

	return collections
}

type issuanceAccumulator struct {
	issuance *Issuance
}

func issuancesFromRecords(
	records []*AssetRecord) []*Issuance {

	accs := make(map[string]*issuanceAccumulator)
	order := make([]string, 0, len(records))

	for _, record := range records {
		if record == nil ||
			record.Genesis.Type != AssetTypeNormal {

			continue
		}

		key := record.AssetRef.String() + "/" +
			record.Genesis.IssuanceID.String()
		acc, ok := accs[key]
		if !ok {
			acc = &issuanceAccumulator{
				issuance: &Issuance{
					AssetRef:   record.AssetRef,
					IssuanceID: record.Genesis.IssuanceID,
					Name:       record.Genesis.Tag,
					MetaHash:   record.Genesis.MetaHash,
				},
			}
			accs[key] = acc
			order = append(order, key)
		}

		acc.issuance.Amount = addSaturatingUint64(
			acc.issuance.Amount, record.Amount,
		)
	}

	issuances := make([]*Issuance, 0, len(order))
	for _, key := range order {
		issuances = append(issuances, accs[key].issuance)
	}

	return issuances
}

func addSaturatingUint64(a, b uint64) uint64 {
	sum, carry := bits.Add64(a, b, 0)
	if carry != 0 {
		return math.MaxUint64
	}

	return sum
}

// ExportProof exports all wallet-known proof files for the given user-facing
// AssetRef.
//
// For a grouped fungible asset this enumerates each wallet-known
// issuance/tranche and exports one proof entry per asset output. For a single
// NFT/collectible or ungrouped asset-ID ref this normally returns one entry.
func (s *Wallet) ExportProof(ctx context.Context,
	ref AssetRef) (*ProofBundle, error) {

	if err := ref.Validate(); err != nil {
		return nil, wrapErr("ExportProof", err)
	}

	assets, err := s.listAssetRecords(ctx, &ListAssetsRequest{
		AssetRef: &ref,
	})
	if err != nil {
		return nil, wrapErr("ExportProof", err)
	}

	if len(assets) == 0 {
		return nil, wrapErr("ExportProof", fmt.Errorf(
			"%w: %s", ErrAssetUnknown, ref,
		))
	}

	bundle := &ProofBundle{
		AssetRef: ref,
		Entries:  make([]ProofEntry, 0, len(assets)),
	}

	for _, asset := range assets {
		if asset == nil {
			continue
		}

		issuanceID := asset.Genesis.IssuanceID
		proofRef := AssetRefFromAssetID(issuanceID)
		proof, err := s.exportProofFile(
			ctx, proofRef, asset.ScriptKey.PubKey, nil,
			"ExportProof",
		)
		if err != nil {
			return nil, err
		}
		if proof == nil || len(proof.RawProofFile) == 0 {
			return nil, wrapErr("ExportProof", fmt.Errorf(
				"%w: empty proof for issuance %s",
				ErrNoProofs, issuanceID,
			))
		}

		bundle.Entries = append(bundle.Entries, ProofEntry{
			AssetRef:   proofBundleEntryRef(ref, asset),
			IssuanceID: issuanceID,
			ScriptKey:  asset.ScriptKey.PubKey,
			Amount:     asset.Amount,
			ProofFile:  proof.RawProofFile,
		})
	}

	if len(bundle.Entries) == 0 {
		return nil, wrapErr("ExportProof", fmt.Errorf(
			"%w: %s", ErrNoProofs, ref,
		))
	}

	return bundle, nil
}

// ExportProofFile exports a raw proof file for a specific asset output.
//
// This is the advanced/legacy escape hatch that maps closely to tapd's proof
// RPC. Most application code should use ExportProof with an AssetRef instead.
func (s *Wallet) ExportProofFile(ctx context.Context,
	ref AssetRef, scriptKey PubKey,
	outpoint *Outpoint) (*ProofFile, error) {

	return s.exportProofFile(
		ctx, ref, scriptKey, outpoint, "ExportProofFile",
	)
}

func (s *Wallet) exportProofFile(ctx context.Context,
	ref AssetRef, scriptKey PubKey,
	outpoint *Outpoint, op string) (*ProofFile, error) {

	proof, err := s.client.ExportProof(ctx, ref, scriptKey, outpoint)
	if err != nil {
		return nil, wrapErr(op, err)
	}

	return proof, nil
}

// ImportProof imports every proof file in a ProofBundle and registers the
// resulting wallet transfers. The caller only supplies the bundle; concrete
// issuance IDs needed by tapd are decoded and registered internally.
func (s *Wallet) ImportProof(ctx context.Context,
	bundle *ProofBundle) ([]*RegisteredAsset, error) {

	if bundle == nil || len(bundle.Entries) == 0 {
		return nil, wrapErr("ImportProof", ErrIncompleteProofBundle)
	}

	for idx := range bundle.Entries {
		entry := bundle.Entries[idx]
		if len(entry.ProofFile) == 0 {
			return nil, wrapErr("ImportProof", fmt.Errorf(
				"%w: entry %d has empty proof file",
				ErrIncompleteProofBundle, idx,
			))
		}
	}

	registered := make([]*RegisteredAsset, 0, len(bundle.Entries))
	for idx := range bundle.Entries {
		entry := bundle.Entries[idx]
		reg, err := s.importProofFile(ctx, &ProofFile{
			RawProofFile: entry.ProofFile,
		}, "ImportProof")
		if err != nil {
			return nil, err
		}

		registered = append(registered, reg)
	}

	return registered, nil
}

// ImportProofFile imports a raw proof file received from a sender during an
// interactive transfer. This method handles the full import flow:
// 1. Unpacks the proof file into individual proofs
// 2. Inserts each proof into the local universe
// 3. Registers the transfer so the wallet recognizes the new asset
//
// Returns the registered asset details.
func (s *Wallet) ImportProofFile(ctx context.Context,
	proofFile *ProofFile) (*RegisteredAsset, error) {

	return s.importProofFile(ctx, proofFile, "ImportProofFile")
}

func (s *Wallet) importProofFile(ctx context.Context,
	proofFile *ProofFile, op string) (*RegisteredAsset,
	error) {

	if proofFile == nil || len(proofFile.RawProofFile) == 0 {
		return nil, wrapErr(op, ErrNoProofs)
	}

	// Step 1: Unpack the proof file into individual proofs.
	rawProofs, err := s.client.UnpackProofFile(ctx, proofFile.RawProofFile)
	if err != nil {
		return nil, wrapErr(op, err)
	}

	if len(rawProofs) == 0 {
		return nil, wrapErr(op,
			fmt.Errorf("proof file contains no proofs"))
	}

	// Step 2: Decode and insert each proof into the universe.
	var lastDecoded *DecodedProof
	for _, rawProof := range rawProofs {
		// TODO: Decode the proof locally without using the RPC client.
		decoded, err := s.client.DecodeProof(ctx, rawProof)
		if err != nil {
			return nil, wrapErr(op, err)
		}

		err = s.client.InsertProof(ctx, rawProof, decoded)
		if err != nil {
			return nil, wrapErr(op, err)
		}

		lastDecoded = decoded
	}

	// Step 3: Register the transfer using the last proof's details.
	registrar, ok := s.client.(transferRegistrarWithIssuance)
	if ok {
		registered, err := registrar.RegisterTransferWithIssuance(
			ctx, lastDecoded.AssetRef, lastDecoded.IssuanceID,
			lastDecoded.ScriptKey, lastDecoded.Outpoint,
		)
		if err != nil {
			return nil, wrapErr(op, err)
		}

		return registered, nil
	}

	registered, err := s.client.RegisterTransfer(
		ctx, lastDecoded.AssetRef, lastDecoded.ScriptKey,
		lastDecoded.Outpoint,
	)
	if err != nil {
		return nil, wrapErr(op, err)
	}

	return registered, nil
}

func proofBundleEntryRef(ref AssetRef,
	asset *AssetRecord) AssetRef {

	if asset == nil {
		return ref
	}

	if ref.IsGroupRef() && asset.Genesis.Type == AssetTypeCollectible {
		return AssetRefFromAssetID(asset.Genesis.IssuanceID)
	}

	return ref
}

// Burn destroys units of the asset identified by AssetRef.
//
// The SDK supplies tapd's confirmation text internally so normal callers do
// not need to carry the daemon's safety phrase through application code.
func (s *Wallet) Burn(ctx context.Context, ref AssetRef,
	amount uint64, opts ...BurnOption) (*Burn, error) {

	if amount == 0 {
		return nil, wrapErr("Burn", ErrZeroAmount)
	}
	if err := ref.Validate(); err != nil {
		return nil, wrapErr("Burn", err)
	}

	o := applyBurnOptions(opts)
	resp, err := s.client.BurnAsset(ctx, &BurnAssetRequest{
		AssetRef:         ref,
		AmountToBurn:     amount,
		ConfirmationText: burnConfirmationText,
		Note:             o.note,
	})
	if err != nil {
		return nil, wrapErr("Burn", err)
	}
	if resp == nil {
		resp = &BurnAssetResponse{}
	}

	return &Burn{
		AssetRef: ref,
		Amount:   amount,
		Note:     o.note,
		Transfer: resp.BurnTransfer,
		Proof:    resp.BurnProof,
	}, nil
}

// ListBurns returns wallet burn history filtered by AssetRef and/or anchor
// txid.
//
// This is the high-level companion to Wallet.Burn. It calls the same daemon
// RPC as Client.ListBurns but wraps errors with operation context so callers
// can use errors.Is against the SDK sentinels (ErrAssetUnknown, etc.). The
// returned BurnRecord rows are already keyed by AssetRef, so application
// code stays on the same logical handle it uses everywhere else.
func (s *Wallet) ListBurns(ctx context.Context,
	req *ListBurnsRequest) ([]*BurnRecord, error) {

	burns, err := s.client.ListBurns(ctx, req)
	if err != nil {
		return nil, wrapErr("ListBurns", err)
	}

	return burns, nil
}

// Send performs a simple one-shot address-based asset transfer.
//
// The addr must be a valid bech32m-encoded Taproot Asset address. The
// SDK decodes it up-front to decide how to frame the request:
//
//   - If the address embeds an amount (all V0/V1 addresses and V2
//     addresses that bake one in), omit WithAmount or pass the exact
//     matching value. Any other value returns ErrAmountMismatch.
//   - If the address embeds no amount (only possible for V2
//     addresses), the caller MUST pass WithAmount with a non-zero
//     value. Otherwise Send returns ErrAmountRequired.
//
// For multi-recipient sends in a single anchor transaction, use
// SendMulti. For fine-grained control over the Fund → Sign → Commit →
// Publish pipeline, use NewTxBuilder.
func (s *Wallet) Send(ctx context.Context, addr string,
	opts ...SendOption) (*AssetTransfer, error) {

	// Decode locally: the SDK can read a bech32m Tap address from the
	// string alone, so Send does not spend an RPC round-trip just to
	// learn the embedded amount or address version.
	decoded, err := DecodeAddress(addr)
	if err != nil {
		return nil, wrapErr("Send", err)
	}

	o := applySendOptions(opts)

	if err := validateSendAmount(decoded, o.amount); err != nil {
		return nil, wrapErr("Send", err)
	}

	// WithAmount set: route through the explicit-amount path to
	// preserve caller intent on the wire. Otherwise rely on the
	// amount embedded in the address.
	recipient := RecipientWithEmbeddedAmount(addr)
	if o.amount != 0 {
		recipient = RecipientWithAmount(addr, o.amount)
	}

	req := &SendAssetRequest{
		Recipients:                []Recipient{recipient},
		FeeRate:                   o.feeRate,
		Label:                     o.label,
		SkipProofCourierPingCheck: o.skipProofCourierPingCheck,
	}

	transfer, err := s.client.SendAsset(ctx, req)
	if err != nil {
		return nil, wrapErr("Send", err)
	}

	return transfer, nil
}

// validateSendAmount enforces the amount vs. address-embedded-amount
// invariant used by Send. The caller passes the decoded destination
// address and the amount argument they intend to send.
func validateSendAmount(addr *Address, amount uint64) error {
	switch {
	case addr.Amount == 0 && amount == 0:
		return ErrAmountRequired

	case addr.Amount > 0 && amount > 0 && amount != addr.Amount:
		return fmt.Errorf(
			"%w: address embeds %d, caller passed %d",
			ErrAmountMismatch, addr.Amount, amount,
		)
	}

	return nil
}

// SendMulti sends one logical asset to multiple recipients in a single anchor
// transaction. Every recipient address must resolve to the same AssetRef.
// Mixed-asset payout batches must be split by the caller.
//
// Use RecipientWithAmount for explicit sender-chosen amounts and
// RecipientWithEmbeddedAmount for addresses that encode their amount.
// Mixing the two recipient forms is supported: SendMulti decodes each address
// and echoes embedded values into the wire request so tapd sees a uniform
// shape.
//
// For single-recipient sends, prefer Send for simplicity.
func (s *Wallet) SendMulti(ctx context.Context,
	recipients []Recipient,
	opts ...SendOption) (*AssetTransfer, error) {

	if len(recipients) == 0 {
		return nil, wrapErr("SendMulti", ErrNoRecipients)
	}

	normalised, err := normaliseSendRecipients(recipients, false)
	if err != nil {
		return nil, wrapErr("SendMulti", err)
	}

	o := applySendOptions(opts)
	req := &SendAssetRequest{
		Recipients:                normalised,
		FeeRate:                   o.feeRate,
		Label:                     o.label,
		SkipProofCourierPingCheck: o.skipProofCourierPingCheck,
	}

	transfer, err := s.client.SendAsset(ctx, req)
	if err != nil {
		return nil, wrapErr("SendMulti", err)
	}

	return transfer, nil
}

func validateSingleAssetSendBatch(addrs []*Address) error {
	var batchRef AssetRef
	for idx, addr := range addrs {
		if addr == nil || addr.AssetRef.IsZero() {
			return fmt.Errorf("%w: recipient %d has no asset ref",
				ErrInvalidAssetRef, idx)
		}

		if idx == 0 {
			batchRef = addr.AssetRef
			continue
		}

		if batchRef.Equivalent(addr.AssetRef) {
			continue
		}

		return fmt.Errorf(
			"%w: recipient 0 uses %s, recipient %d uses %s",
			ErrMixedAssetBatchUnsupported, batchRef, idx,
			addr.AssetRef,
		)
	}

	return nil
}

// getNetworkParams returns the HRP and coin type for a given network.
func getNetworkParams(network Network) (string, uint32, error) {
	switch network {
	case NetworkMainnet:
		return vpsbt.MainnetHRP, 0, nil
	case NetworkTestnet:
		return vpsbt.TestnetHRP, 1, nil
	case NetworkTestnet4:
		return vpsbt.Testnet4HRP, 1, nil
	case NetworkSignet:
		return vpsbt.SigNetHRP, 1, nil
	case NetworkSimnet:
		return vpsbt.SimNetHRP, 1, nil
	case NetworkRegtest:
		return vpsbt.RegTestHRP, 1, nil
	default:
		return "", 0, fmt.Errorf("unsupported network: %s", network)
	}
}
