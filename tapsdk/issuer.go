package tapsdk

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	defaultMintResolveTimeout  = 10 * time.Second
	defaultMintResolveInterval = 200 * time.Millisecond
	defaultMintCancelTimeout   = 5 * time.Second
)

var errMintResultNotReady = errors.New("mint result is not visible yet")

// Issuer is the high-level minting surface for SDK business assets.
//
// It creates grouped fungible assets, standalone NFTs, NFT collections, and
// additional supply or collection items while hiding tapd batch mechanics from
// application code. MintClient remains available through Client for callers
// that need direct batch control.
//
// Issuer serializes its own mint calls because tapd has one active mint batch
// per daemon. Issuers returned by the same Wallet share that lock.
type Issuer struct {
	client Client
	mu     *sync.Mutex
}

// NewIssuer creates a high-level issuer backed by the given client.
// Concurrent calls on the returned Issuer are serialized.
func NewIssuer(client Client) *Issuer {
	return &Issuer{
		client: client,
		mu:     &sync.Mutex{},
	}
}

// NewIssuer returns a high-level issuer backed by the wallet's client.
// Issuers returned by the same Wallet share mint-call serialization.
func (s *Wallet) NewIssuer() *Issuer {
	return &Issuer{
		client: s.client,
		mu:     &s.issuerMu,
	}
}

// MintOption configures high-level issuer calls.
type MintOption func(*MintOptions)

// MintOptions contains optional parameters for high-level issuer calls.
type MintOptions struct {
	// feeRate is the target fee rate for the genesis transaction. Zero uses
	// the daemon default.
	feeRate FeeRate

	// resolveTimeout is how long high-level issuer calls wait for tapd's
	// wallet projection to expose the accepted mint result.
	resolveTimeout time.Duration

	// externalIssuanceKey is the externally managed key descriptor used for
	// issuance authorization.
	externalIssuanceKey *ExternalKey

	// externalIssuanceSigner signs issuance authorization payloads.
	externalIssuanceSigner ExternalIssuanceSigner
}

// WithMintFeeRate sets the target fee rate for the genesis transaction. A
// zero value uses tapd's daemon default.
func WithMintFeeRate(feeRate FeeRate) MintOption {
	return func(o *MintOptions) {
		o.feeRate = feeRate
	}
}

// WithMintResolveTimeout sets how long high-level issuer calls wait for tapd's
// wallet projection to expose the accepted mint result. If this timeout is hit,
// the call returns ErrMintResolveTimeout and the mint may still have been
// accepted by tapd.
func WithMintResolveTimeout(timeout time.Duration) MintOption {
	return func(o *MintOptions) {
		o.resolveTimeout = timeout
	}
}

// WithExternalIssuanceKey configures an externally managed issuance key for
// high-level issuer calls. It maps to tapd's external group key at the
// transport boundary while keeping application code on the SDK Asset,
// Collection, and Issuance model.
func WithExternalIssuanceKey(key ExternalKey) MintOption {
	return func(o *MintOptions) {
		keyCopy := key
		o.externalIssuanceKey = &keyCopy
	}
}

// WithExternalIssuanceSigner configures the signer used when the issuer needs
// an external issuance authorization signature.
func WithExternalIssuanceSigner(signer ExternalIssuanceSigner) MintOption {
	return func(o *MintOptions) {
		o.externalIssuanceSigner = signer
	}
}

// CreateFungible creates a grouped fungible asset and returns the SDK business
// Asset keyed by its group AssetRef.
//
// If the returned error wraps ErrMintResolveTimeout, tapd may already have
// accepted or finalized the mint. Do not blindly retry; inspect wallet assets,
// issuances, or mint batches first to avoid duplicate issuance.
func (i *Issuer) CreateFungible(ctx context.Context,
	spec FungibleAssetSpec,
	opts ...MintOption) (*Asset, error) {

	unlock := i.lock()
	defer unlock()

	asset, err := i.createFungible(ctx, spec, opts...)
	if err != nil {
		return nil, wrapErr("CreateFungible", err)
	}

	return asset, nil
}

// IssueFungible creates an additional issuance for an existing grouped
// fungible asset.
//
// If the returned error wraps ErrMintResolveTimeout, tapd may already have
// accepted or finalized the mint. Do not blindly retry; inspect wallet assets,
// issuances, or mint batches first to avoid duplicate issuance.
func (i *Issuer) IssueFungible(ctx context.Context,
	ref AssetRef, amount uint64,
	opts ...MintOption) (*Issuance, error) {

	unlock := i.lock()
	defer unlock()

	issuance, err := i.issueFungible(ctx, ref, amount, opts...)
	if err != nil {
		return nil, wrapErr("IssueFungible", err)
	}

	return issuance, nil
}

// CreateNFT creates a standalone NFT and returns it as an Asset keyed by its
// asset-ID AssetRef.
//
// If the returned error wraps ErrMintResolveTimeout, tapd may already have
// accepted or finalized the mint. Do not blindly retry; inspect wallet assets
// or mint batches first to avoid duplicate NFTs.
func (i *Issuer) CreateNFT(ctx context.Context, spec NFTSpec,
	opts ...MintOption) (*Asset, error) {

	unlock := i.lock()
	defer unlock()

	asset, err := i.createNFT(ctx, spec, opts...)
	if err != nil {
		return nil, wrapErr("CreateNFT", err)
	}

	return asset, nil
}

// CreateCollection creates a new NFT collection by minting the first item in a
// grouped collectible asset. It returns the collection and first item together
// in a CollectionMintResult.
//
// If the returned error wraps ErrMintResolveTimeout, tapd may already have
// accepted or finalized the mint. Do not blindly retry; inspect wallet assets,
// collections, or mint batches first to avoid duplicate NFTs.
func (i *Issuer) CreateCollection(ctx context.Context,
	firstItem NFTSpec,
	opts ...MintOption) (*CollectionMintResult, error) {

	unlock := i.lock()
	defer unlock()

	result, err := i.createCollection(ctx, firstItem, opts...)
	if err != nil {
		return nil, wrapErr("CreateCollection", err)
	}

	return result, nil
}

// MintCollectionItem mints a new NFT item into an existing collection.
//
// If the returned error wraps ErrMintResolveTimeout, tapd may already have
// accepted or finalized the mint. Do not blindly retry; inspect wallet assets,
// collections, or mint batches first to avoid duplicate NFTs.
func (i *Issuer) MintCollectionItem(ctx context.Context,
	collectionRef AssetRef, item NFTSpec,
	opts ...MintOption) (*Asset, error) {

	unlock := i.lock()
	defer unlock()

	asset, err := i.mintCollectionItem(ctx, collectionRef, item, opts...)
	if err != nil {
		return nil, wrapErr("MintCollectionItem", err)
	}

	return asset, nil
}

func (i *Issuer) createFungible(ctx context.Context,
	spec FungibleAssetSpec,
	opts ...MintOption) (*Asset, error) {

	if err := validateFungibleSpec(spec); err != nil {
		return nil, err
	}

	stage := &MintAsset{
		AssetVersion:            spec.AssetVersion,
		AssetType:               AssetTypeFungible,
		Name:                    spec.Name,
		AssetMeta:               spec.AssetMeta,
		InitialSupply:           spec.Amount,
		AllowIssuance:           true,
		DecimalDisplay:          spec.DecimalDisplay,
		ScriptKey:               spec.ScriptKey,
		EnableSupplyCommitments: spec.EnableSupplyCommitments,
	}

	record, err := i.mintAssetAndResolve(ctx, stage, nil, func(
		record *AssetRecord) bool {

		return isRecord(record, AssetTypeFungible, spec.Name,
			spec.Amount) && record.AssetRef.IsGroupRef()
	}, opts)
	if err != nil {
		return nil, err
	}

	assets := assetsFromRecords([]*AssetRecord{record})
	if len(assets) != 1 {
		return nil, fmt.Errorf("%w: %s", ErrMintResultNotFound,
			spec.Name)
	}

	return assets[0], nil
}

func (i *Issuer) issueFungible(ctx context.Context,
	ref AssetRef, amount uint64,
	opts ...MintOption) (*Issuance, error) {

	if err := validateGroupedRef(ref); err != nil {
		return nil, err
	}
	if amount == 0 {
		return nil, ErrZeroAmount
	}

	mintOpts := applyMintOptions(opts)
	if err := i.ensureReady(ctx); err != nil {
		return nil, err
	}

	base, err := i.resolveFungibleAsset(ctx, ref)
	if err != nil {
		return nil, err
	}

	before, err := i.listMintRecords(ctx, &base.AssetRef)
	if err != nil {
		return nil, err
	}

	stage := &MintIssuance{
		AssetRef:  base.AssetRef,
		Name:      base.Genesis.Tag,
		AssetType: AssetTypeFungible,
		Amount:    amount,
	}
	applyExternalIssuanceKeyToIssuance(stage, mintOpts)

	err = i.mintAndFinalize(ctx, func() (*MintingBatch,
		error) {

		return i.client.MintIssuance(ctx,
			&MintIssuanceRequest{
				Issuance:      stage,
				ShortResponse: true,
			},
		)
	}, mintOpts, issuanceSigningContext{
		operation: IssuanceOperationIssueAsset,
		assetRef:  base.AssetRef,
	})
	if err != nil {
		return nil, err
	}

	record, err := i.findNewRecord(ctx, &base.AssetRef, recordIDs(before),
		func(record *AssetRecord) bool {
			return isRecord(
				record, AssetTypeFungible,
				base.Genesis.Tag, amount,
			)
		},
		mintOpts.resolveTimeoutOrDefault(),
	)
	if err != nil {
		return nil, err
	}

	issuances := issuancesFromRecords([]*AssetRecord{record})
	if len(issuances) != 1 {
		return nil, fmt.Errorf("%w: %s", ErrMintResultNotFound, ref)
	}

	return issuances[0], nil
}

func (i *Issuer) createNFT(ctx context.Context, spec NFTSpec,
	opts ...MintOption) (*Asset, error) {

	if err := validateNFTSpec(spec); err != nil {
		return nil, err
	}

	stage := mintNFTAsset(spec, false)
	record, err := i.mintAssetAndResolve(ctx, stage, nil, func(
		record *AssetRecord) bool {

		return isRecord(record, AssetTypeNFT, spec.Name, 1) &&
			record.AssetRef.IsAssetIDRef()
	}, opts)
	if err != nil {
		return nil, err
	}

	assets := assetsFromRecords([]*AssetRecord{record})
	if len(assets) != 1 {
		return nil, fmt.Errorf("%w: %s", ErrMintResultNotFound,
			spec.Name)
	}

	return assets[0], nil
}

func (i *Issuer) createCollection(ctx context.Context,
	firstItem NFTSpec,
	opts ...MintOption) (*CollectionMintResult, error) {

	if err := validateNFTSpec(firstItem); err != nil {
		return nil, err
	}

	stage := mintNFTAsset(firstItem, true)
	record, err := i.mintAssetAndResolve(ctx, stage, nil, func(
		record *AssetRecord) bool {

		return isRecord(
			record, AssetTypeNFT, firstItem.Name, 1,
		) && record.AssetRef.IsGroupRef()
	}, opts)
	if err != nil {
		return nil, err
	}

	collections := collectionsFromRecords([]*AssetRecord{record})
	if len(collections) != 1 {
		return nil, fmt.Errorf("%w: %s", ErrMintResultNotFound,
			firstItem.Name)
	}

	assets := assetsFromRecords([]*AssetRecord{record})
	if len(assets) != 1 {
		return nil, fmt.Errorf("%w: %s", ErrMintResultNotFound,
			firstItem.Name)
	}

	return &CollectionMintResult{
		Collection: collections[0],
		FirstItem:  assets[0],
	}, nil
}

func (i *Issuer) mintCollectionItem(ctx context.Context,
	collectionRef AssetRef, item NFTSpec,
	opts ...MintOption) (*Asset, error) {

	if err := validateGroupedRef(collectionRef); err != nil {
		return nil, err
	}
	if err := validateNFTSpec(item); err != nil {
		return nil, err
	}

	mintOpts := applyMintOptions(opts)
	if err := i.ensureReady(ctx); err != nil {
		return nil, err
	}

	collection, err := i.resolveCollection(ctx, collectionRef)
	if err != nil {
		return nil, err
	}

	before, err := i.listMintRecords(ctx, &collection.AssetRef)
	if err != nil {
		return nil, err
	}

	stage := &MintIssuance{
		AssetRef:     collection.AssetRef,
		Name:         item.Name,
		AssetType:    AssetTypeNFT,
		AssetMeta:    item.AssetMeta,
		Amount:       1,
		AssetVersion: item.AssetVersion,
		ScriptKey:    item.ScriptKey,
	}
	applyExternalIssuanceKeyToIssuance(stage, mintOpts)

	err = i.mintAndFinalize(ctx, func() (*MintingBatch,
		error) {

		return i.client.MintIssuance(ctx,
			&MintIssuanceRequest{
				Issuance:      stage,
				ShortResponse: true,
			},
		)
	}, mintOpts, issuanceSigningContext{
		operation: IssuanceOperationMintCollectionItem,
		assetRef:  collection.AssetRef,
	})
	if err != nil {
		return nil, err
	}

	record, err := i.findNewRecord(ctx, &collection.AssetRef,
		recordIDs(before), func(record *AssetRecord) bool {
			return isRecord(
				record, AssetTypeNFT, item.Name, 1,
			) && record.AssetRef.Equivalent(collection.AssetRef)
		},
		mintOpts.resolveTimeoutOrDefault(),
	)
	if err != nil {
		return nil, err
	}

	assets := assetsFromRecords([]*AssetRecord{record})
	if len(assets) != 1 {
		return nil, fmt.Errorf("%w: %s", ErrMintResultNotFound,
			item.Name)
	}

	return assets[0], nil
}

func validateFungibleSpec(spec FungibleAssetSpec) error {
	if spec.Name == "" {
		return ErrAssetNameRequired
	}
	if spec.Amount == 0 {
		return ErrZeroAmount
	}

	return nil
}

func validateNFTSpec(spec NFTSpec) error {
	if spec.Name == "" {
		return ErrAssetNameRequired
	}

	return nil
}

func validateGroupedRef(ref AssetRef) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if !ref.IsGroupRef() {
		return ErrAssetNotIssuable
	}

	return nil
}

func mintNFTAsset(spec NFTSpec, collection bool) *MintAsset {
	return &MintAsset{
		AssetVersion:  spec.AssetVersion,
		AssetType:     AssetTypeNFT,
		Name:          spec.Name,
		AssetMeta:     spec.AssetMeta,
		InitialSupply: 1,
		AllowIssuance: collection,
		ScriptKey:     spec.ScriptKey,
	}
}

func (i *Issuer) lock() func() {
	if i == nil || i.mu == nil {
		return func() {}
	}

	i.mu.Lock()
	return i.mu.Unlock
}

type recordMatcher func(*AssetRecord) bool

func (i *Issuer) mintAssetAndResolve(ctx context.Context,
	stage *MintAsset, ref *AssetRef,
	matches recordMatcher,
	opts []MintOption) (*AssetRecord, error) {

	mintOpts := applyMintOptions(opts)
	signingOperation := signingOperationForMintAsset(stage)
	if mintOpts.externalIssuanceSigner != nil &&
		signingOperation == IssuanceOperationUnknown {

		return nil, ErrAssetNotIssuable
	}

	if err := i.ensureReady(ctx); err != nil {
		return nil, err
	}

	before, err := i.listMintRecords(ctx, ref)
	if err != nil {
		return nil, err
	}

	applyExternalIssuanceKeyToAsset(stage, mintOpts)

	err = i.mintAndFinalize(ctx, func() (*MintingBatch,
		error) {

		return i.client.MintAsset(ctx, &MintAssetRequest{
			Asset:         stage,
			ShortResponse: true,
		})
	}, mintOpts, issuanceSigningContext{
		operation: signingOperation,
	})
	if err != nil {
		return nil, err
	}

	return i.findNewRecord(
		ctx, ref, recordIDs(before), matches,
		mintOpts.resolveTimeoutOrDefault(),
	)
}

func (i *Issuer) mintAndFinalize(ctx context.Context,
	stage func() (*MintingBatch, error),
	opts *MintOptions, signingContext issuanceSigningContext) error {

	if i == nil || i.client == nil {
		return fmt.Errorf("issuer client is nil")
	}
	if opts == nil {
		opts = &MintOptions{}
	}

	if opts.externalIssuanceKey != nil &&
		opts.externalIssuanceSigner == nil {

		return ErrExternalIssuanceSignerRequired
	}
	if opts.externalIssuanceSigner != nil &&
		opts.externalIssuanceKey == nil {

		return ErrExternalIssuanceKeyRequired
	}

	if _, err := stage(); err != nil {
		return err
	}

	if opts.externalIssuanceSigner != nil {
		err := i.signAndFinalizeExternalIssuance(
			ctx, opts, signingContext,
		)
		if err != nil {
			i.cancelBatchBestEffort()
		}

		return err
	}

	_, err := i.client.FinalizeBatch(ctx,
		&FinalizeBatchRequest{
			ShortResponse: true,
			FeeRate:       opts.feeRate,
		},
	)
	if err == nil {
		return nil
	}

	i.cancelBatchBestEffort()

	return err
}

type issuanceSigningContext struct {
	operation IssuanceOperation
	assetRef  AssetRef
}

func (i *Issuer) signAndFinalizeExternalIssuance(ctx context.Context,
	opts *MintOptions, signingContext issuanceSigningContext) error {

	if opts.externalIssuanceKey == nil {
		return ErrExternalIssuanceKeyRequired
	}

	funded, err := i.client.FundBatch(ctx, &FundBatchRequest{
		FeeRate: opts.feeRate,
	})
	if err != nil {
		return err
	}

	requests, err := issuanceSigningRequests(
		funded, opts.externalIssuanceKey, signingContext,
	)
	if err != nil {
		return err
	}

	signedPsbts := make([]string, 0, len(requests))
	for _, req := range requests {
		signed, err := opts.externalIssuanceSigner.SignIssuance(
			ctx, req,
		)
		if err != nil {
			return err
		}
		if signed.VirtualPSBT == "" {
			return ErrExternalIssuanceSignatureRequired
		}

		signedPsbts = append(signedPsbts, signed.VirtualPSBT)
	}

	_, err = i.client.SealBatch(ctx, &SealBatchRequest{
		ShortResponse:           true,
		SignedGroupVirtualPSBTs: signedPsbts,
	})
	if err != nil {
		return err
	}

	_, err = i.client.FinalizeBatch(ctx, &FinalizeBatchRequest{
		ShortResponse: true,
	})

	return err
}

func issuanceSigningRequests(batch *VerboseMintingBatch,
	externalKey *ExternalKey,
	signingContext issuanceSigningContext) ([]IssuanceSigningRequest, error) {

	if batch == nil {
		return nil, fmt.Errorf("nil funded minting batch")
	}
	if externalKey == nil {
		return nil, ErrExternalIssuanceKeyRequired
	}

	requests := make([]IssuanceSigningRequest, 0, len(batch.UnsealedAssets))
	for _, unsealed := range batch.UnsealedAssets {
		if unsealed.GroupVirtualPSBT == "" {
			continue
		}

		req := IssuanceSigningRequest{
			Operation:   signingContext.operation,
			AssetRef:    signingContext.assetRef,
			ExternalKey: *externalKey,
			VirtualPSBT: unsealed.GroupVirtualPSBT,
			VirtualTx:   unsealed.GroupVirtualTx,
		}

		if unsealed.Asset != nil {
			req.Name = unsealed.Asset.Name
			req.AssetType = unsealed.Asset.AssetType
			req.Amount = unsealed.Asset.Amount
			req.ScriptKey = unsealed.Asset.ScriptKey
		}
		if unsealed.GroupKeyRequest != nil {
			req.AnchorGenesis = unsealed.GroupKeyRequest.AnchorGenesis
			if unsealed.GroupKeyRequest.ExternalKey != nil {
				req.ExternalKey = *unsealed.GroupKeyRequest.ExternalKey
			}
		}
		if req.AssetRef.IsZero() && unsealed.GroupVirtualTx != nil &&
			unsealed.GroupVirtualTx.TweakedKey != nil {

			req.AssetRef = AssetRefFromGroupKey(
				*unsealed.GroupVirtualTx.TweakedKey,
			)
		}

		requests = append(requests, req)
	}

	if len(requests) == 0 {
		return nil, ErrExternalIssuanceRequestNotFound
	}

	return requests, nil
}

func applyExternalIssuanceKeyToAsset(stage *MintAsset, opts *MintOptions) {
	if stage == nil || opts == nil || opts.externalIssuanceKey == nil {
		return
	}
	if !stage.AllowIssuance {
		return
	}

	stage.ExternalGroupKey = opts.externalIssuanceKey
}

func applyExternalIssuanceKeyToIssuance(stage *MintIssuance,
	opts *MintOptions) {

	if stage == nil || opts == nil {
		return
	}

	stage.ExternalGroupKey = opts.externalIssuanceKey
}

func signingOperationForMintAsset(stage *MintAsset) IssuanceOperation {
	if stage == nil || !stage.AllowIssuance {
		return IssuanceOperationUnknown
	}

	switch stage.AssetType {
	case AssetTypeFungible:
		return IssuanceOperationCreateAsset
	case AssetTypeNFT:
		return IssuanceOperationCreateCollection
	default:
		return IssuanceOperationUnknown
	}
}

func (i *Issuer) cancelBatchBestEffort() {
	ctx, cancel := context.WithTimeout(
		context.Background(), defaultMintCancelTimeout,
	)
	defer cancel()

	_, _ = i.client.CancelBatch(ctx)
}

func (i *Issuer) ensureReady(ctx context.Context) error {
	if i == nil || i.client == nil {
		return fmt.Errorf("issuer client is nil")
	}

	batches, err := i.client.ListBatches(ctx, &ListBatchesRequest{})
	if err != nil {
		return err
	}

	for _, batch := range batches {
		if batch == nil {
			continue
		}
		if isActiveMintBatch(batch.Batch.State) {
			return fmt.Errorf("%w: %s is in state %s",
				ErrMintBatchActive, batch.Batch.BatchKey,
				batch.Batch.State)
		}
	}

	return nil
}

func isActiveMintBatch(state BatchState) bool {
	switch state {
	case BatchStatePending,
		BatchStateFrozen,
		BatchStateCommitted,
		BatchStateBroadcast:

		return true

	default:
		return false
	}
}

func (i *Issuer) resolveFungibleAsset(ctx context.Context,
	ref AssetRef) (*AssetRecord, error) {

	records, err := i.listMintRecords(ctx, &ref)
	if err != nil {
		return nil, err
	}

	for _, record := range records {
		if record == nil {
			continue
		}

		if record.Genesis.Type != AssetTypeFungible {
			return nil, fmt.Errorf("%w: expected fungible asset",
				ErrWrongAssetType)
		}
		if !record.AssetRef.IsGroupRef() {
			return nil, ErrAssetNotIssuable
		}

		return record, nil
	}

	return nil, fmt.Errorf("%w: %s", ErrAssetUnknown, ref)
}

func (i *Issuer) resolveCollection(ctx context.Context,
	ref AssetRef) (*Collection, error) {

	records, err := i.listMintRecords(ctx, &ref)
	if err != nil {
		return nil, err
	}

	for _, record := range records {
		if record == nil {
			continue
		}

		if record.Genesis.Type != AssetTypeNFT {
			return nil, fmt.Errorf("%w: expected NFT collection",
				ErrWrongAssetType)
		}
		if !record.AssetRef.IsGroupRef() {
			return nil, fmt.Errorf("%w: expected collection ref",
				ErrWrongAssetType)
		}

		collections := collectionsFromRecords(
			[]*AssetRecord{record},
		)
		if len(collections) != 1 {
			return nil, fmt.Errorf("%w: %s", ErrMintResultNotFound,
				ref)
		}

		return collections[0], nil
	}

	return nil, fmt.Errorf("%w: %s", ErrAssetUnknown, ref)
}

func (i *Issuer) listMintRecords(ctx context.Context,
	ref *AssetRef) ([]*AssetRecord, error) {

	if i == nil || i.client == nil {
		return nil, fmt.Errorf("issuer client is nil")
	}

	req := &ListAssetsRequest{
		IncludeUnconfirmedMints: true,
		AssetRef:                ref,
	}

	return i.client.ListAssetRecords(ctx, req)
}

func (i *Issuer) findNewRecord(ctx context.Context,
	ref *AssetRef, before map[AssetID]struct{},
	matches recordMatcher, timeout time.Duration) (*AssetRecord,
	error) {

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	ticker := time.NewTicker(defaultMintResolveInterval)
	defer ticker.Stop()

	for {
		record, err := i.findNewRecordOnce(ctx, ref, before, matches)
		if err == nil {
			return record, nil
		}
		if !errors.Is(err, errMintResultNotReady) {
			return nil, err
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()

		case <-deadline.C:
			return nil, fmt.Errorf("%w: %v",
				ErrMintResolveTimeout, err)

		case <-ticker.C:
		}
	}
}

func (i *Issuer) findNewRecordOnce(ctx context.Context,
	ref *AssetRef, before map[AssetID]struct{},
	matches recordMatcher) (*AssetRecord, error) {

	records, err := i.listMintRecords(ctx, ref)
	if err != nil {
		return nil, err
	}

	for _, record := range records {
		if record == nil {
			continue
		}

		if _, ok := before[record.Genesis.IssuanceID]; ok {
			continue
		}
		if matches == nil || matches(record) {
			return record, nil
		}
	}

	return nil, errMintResultNotReady
}

func recordIDs(records []*AssetRecord) map[AssetID]struct{} {
	ids := make(map[AssetID]struct{}, len(records))
	for _, record := range records {
		if record == nil {
			continue
		}

		ids[record.Genesis.IssuanceID] = struct{}{}
	}

	return ids
}

func isRecord(record *AssetRecord, assetType AssetType,
	name string, amount uint64) bool {

	if record == nil {
		return false
	}

	return record.Genesis.Type == assetType &&
		record.Genesis.Tag == name &&
		record.Amount == amount
}

func applyMintOptions(opts []MintOption) *MintOptions {
	o := &MintOptions{}
	for _, opt := range opts {
		if opt == nil {
			continue
		}

		opt(o)
	}

	return o
}

func (o *MintOptions) resolveTimeoutOrDefault() time.Duration {
	if o == nil || o.resolveTimeout <= 0 {
		return defaultMintResolveTimeout
	}

	return o.resolveTimeout
}
