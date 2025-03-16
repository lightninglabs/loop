package assets

import "github.com/btcsuite/btcd/btcutil"

const (
	fixedPrice = 100
)

// FixedExchangeRateProvider is a fixed exchange rate provider.
type FixedExchangeRateProvider struct {
}

// NewFixedExchangeRateProvider creates a new fixed exchange rate provider.
func NewFixedExchangeRateProvider() *FixedExchangeRateProvider {
	return &FixedExchangeRateProvider{}
}

// GetSatsPerAssetUnit returns the fixed price in sats per asset unit.
func (e *FixedExchangeRateProvider) GetSatsPerAssetUnit(assetId []byte) (
	btcutil.Amount, error) {

	return btcutil.Amount(fixedPrice), nil
}
