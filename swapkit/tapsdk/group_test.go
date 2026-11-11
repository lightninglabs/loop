package tapsdk

import (
	"bytes"
	"testing"

	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/stretchr/testify/require"
)

func TestSameGroupKeys(t *testing.T) {
	group := bindingPub(11)
	opposite := group
	opposite[0] ^= 1
	other := bindingPub(12)
	invalidPrefix := group
	invalidPrefix[0] = 4
	for _, test := range []struct {
		name string
		key  []byte
		want bool
	}{
		{name: "same", key: group[:], want: true},
		{name: "opposite parity", key: opposite[:], want: true},
		{name: "different", key: other[:]},
		{name: "missing", key: nil},
		{name: "x-only", key: group[1:]},
		{name: "invalid prefix", key: invalidPrefix[:]},
		{name: "invalid point", key: make([]byte, 33)},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := bytes.Clone(test.key)
			require.Equal(t, test.want, sameGroupKeys(group[:], test.key))
			require.Equal(t, test.want, sameGroupKeys(test.key, group[:]))
			require.Equal(t, before, test.key)
		})
	}
}

func TestInputBindingAcceptsGroupParity(t *testing.T) {
	p := bindingProof(t, bindingGenesis(), bindingKey(2).PubKey(),
		bindingKey(3).PubKey(), nil, bindingGenesis().FirstPrevOut,
		100, false)
	group := sdk.PubKey(bindingPub(11))
	group[0] ^= 1
	binding := InputBinding{
		ID: "grouped input", IssuanceID: sdk.AssetID(p.Asset.ID()),
		GroupKey: &group, Amount: p.Asset.Amount, ScriptKey: bindingPub(2),
		AnchorOutpoint: sdk.Outpoint{
			Txid: sdk.Hash(p.OutPoint().Hash), Index: p.OutPoint().Index,
		},
	}
	require.NoError(t, binding.validate(p))
	other := sdk.PubKey(bindingPub(12))
	binding.GroupKey = &other
	require.ErrorIs(t, binding.validate(p), swapkit.ErrInvalid)
}

func TestVerifiedSummaryAcceptsGroupParity(t *testing.T) {
	p := bindingProof(t, bindingGenesis(), bindingKey(2).PubKey(),
		bindingKey(3).PubKey(), nil, bindingGenesis().FirstPrevOut,
		100, false)
	response, err := fixtureVerified(bindingRaw(t, p), true)
	require.NoError(t, err)
	group := bindingPub(11)
	group[0] ^= 1
	response.DecodedProof.AssetRef = sdk.AssetRefFromGroupKey(group)
	require.NoError(t, checkVerifiedSummary(response, p))
	response.DecodedProof.AssetRef = sdk.AssetRefFromGroupKey(bindingPub(12))
	require.ErrorIs(t, checkVerifiedSummary(response, p), swapkit.ErrInvalid)
}

func TestDepositAndOfferAcceptGroupParity(t *testing.T) {
	_, s, _, _ := bindingOutcome(t, false)
	d := s.Deposit
	p := d.Proof
	contract := d.Contract
	group, ok := contract.Output.AssetRef.GroupKey()
	require.True(t, ok)
	group[0] ^= 1
	contract.Output.AssetRef = sdk.AssetRefFromGroupKey(group)
	require.NoError(t, checkDepositContract(d.Deposit, contract, p))
	s.Offer.Terms.GroupKey[0] ^= 1
	_, err := NewSwap(s.Offer, d, nil)
	require.NoError(t, err)

	for _, relative := range []bool{false, true} {
		changed := *p
		changed.Asset = *p.Asset.Copy()
		if relative {
			changed.Asset.RelativeLockTime = 1
		} else {
			changed.Asset.LockTime = 1
		}
		require.ErrorIs(t,
			checkDepositContract(d.Deposit, contract, &changed),
			swapkit.ErrInvalid,
		)
	}
}
