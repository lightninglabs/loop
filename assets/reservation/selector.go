package reservation

import (
	"bytes"
	"errors"

	"github.com/btcsuite/btcd/btcec/v2"
)

// AssetSelector is the immutable client choice. A group permits a fungible
// member to be chosen independently for the funding and Lightning payments.
type AssetSelector struct {
	AssetID  [32]byte
	GroupKey [33]byte
}

// ParseAssetSelector requires exclusively an exact issuance or a canonical
// compressed, tweaked group key (the key returned by tapd).
func ParseAssetSelector(id, group []byte) (AssetSelector, error) {
	switch {
	case len(id) == 32 && len(group) == 0 && [32]byte(id) != ([32]byte{}):
		return AssetSelector{AssetID: [32]byte(id)}, nil
	case len(id) == 0 && len(group) == 33:
		key, err := btcec.ParsePubKey(group)
		if err == nil && bytes.Equal(key.SerializeCompressed(), group) {
			return AssetSelector{GroupKey: [33]byte(group)}, nil
		}
	}
	return AssetSelector{}, errors.New("specify one asset ID or compressed group key")
}

// ProtocolVersion preserves the original wire protocol for exact issuances.
func (s AssetSelector) ProtocolVersion() uint32 {
	if s.GroupKey != ([33]byte{}) {
		return 1
	}
	return 0
}

// Bytes returns the mutually exclusive wire representation of the selector.
func (s AssetSelector) Bytes() ([]byte, []byte) {
	var id, group []byte
	if s.AssetID != ([32]byte{}) {
		id = s.AssetID[:]
	}
	if s.GroupKey != ([33]byte{}) {
		group = s.GroupKey[:]
	}
	return id, group
}

func (r *Reservation) selector() AssetSelector {
	if r.GroupKey != ([33]byte{}) {
		return AssetSelector{GroupKey: r.GroupKey}
	}
	return AssetSelector{AssetID: r.AssetID}
}

// SameGroupKey compares canonical compressed keys using the x-only group
// identity used by tapd's universe and Schnorr verification.
func SameGroupKey(a, b []byte) bool {
	if _, err := ParseAssetSelector(nil, a); err != nil {
		return false
	}
	if _, err := ParseAssetSelector(nil, b); err != nil {
		return false
	}
	return bytes.Equal(a[1:], b[1:])
}
