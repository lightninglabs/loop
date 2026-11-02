package tapsdk

// ListBalancesRequest specifies filters for querying wallet balances using the
// SDK's semantic asset model.
type ListBalancesRequest struct {
	// AssetRef restricts the result to a single asset.
	AssetRef *AssetRef

	// IncludeLeased includes assets that are currently leased by a pending
	// transfer and therefore unavailable for coin selection.
	IncludeLeased bool

	// ScriptKeyType optionally restricts balances to a specific script key
	// type or all types.
	ScriptKeyType *ScriptKeyTypeQuery
}

// Balance is the wallet's confirmed amount for a semantic asset.
type Balance struct {
	// AssetRef is the SDK identifier for the asset.
	AssetRef AssetRef

	// Balance is the confirmed amount owned by the wallet.
	Balance uint64
}

// ListBalancesResponse contains wallet balances keyed by AssetRef.
type ListBalancesResponse struct {
	// Balances is keyed by AssetRef.String().
	Balances map[string]*Balance

	// UnconfirmedTransfers counts outgoing transfers that are not yet
	// confirmed on-chain and therefore excluded from the reported balances.
	UnconfirmedTransfers uint64
}
