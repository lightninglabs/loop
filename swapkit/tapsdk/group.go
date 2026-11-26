package tapsdk

import sdk "github.com/lightninglabs/tap-sdk"

// SameGroupKey compares valid compressed group keys by their x-only identity.
// It preserves the caller's serialized key metadata and rejects malformed keys.
func SameGroupKey(first, second []byte) bool {
	a, err := sdk.ParsePubKey(first)
	if err != nil {
		return false
	}
	b, err := sdk.ParsePubKey(second)
	if err != nil {
		return false
	}
	return sdk.AssetRefFromGroupKey(a).Equivalent(sdk.AssetRefFromGroupKey(b))
}

func sameGroupKeys(first, second []byte) bool {
	return SameGroupKey(first, second)
}
