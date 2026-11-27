package tapsdk

import (
	"crypto/sha256"
	"testing"

	"github.com/lightninglabs/loop/swapkit"
	"github.com/stretchr/testify/require"
)

// cooperativeTerms returns the funder's terms for a cooperative spend of the
// Loop Out fixture's reservation.
func cooperativeTerms(f *loopOutFixture) CooperativeSpendTerms {
	return CooperativeSpendTerms{
		Funding:        f.terms.Funding,
		Input:          f.terms.Input,
		InputProof:     f.terms.InputProof,
		SessionContext: f.terms.SessionContext,
	}
}

// TestValidateCooperativeSpend checks the funder's acceptance of a cooperative
// spend of its funded output. The package under test moves the reservation
// into an HTLC, which is one such spend: the check does not constrain where
// the asset goes, only what the funder's signature can be used for.
func TestValidateCooperativeSpend(t *testing.T) {
	f := newLoopOutFixture(t)
	raw := f.commit(t)
	terms := cooperativeTerms(f)

	pkg, err := ValidateCooperativeSpend(terms, raw)
	require.NoError(t, err)
	require.Len(t, pkg.Inputs, 1)

	// The accepted package signs into a transaction that is valid for the
	// authenticated funding output.
	final := finishFallback(t, pkg)
	require.NoError(t, executeAgainst(t, final, f.prevout))

	deposit, err := swapkit.NewDepositKit(swapkit.DepositKitParams{
		ClientKey: bindingPub(3), ServerKey: bindingPub(2),
		CSVExpiry: 100,
	})
	require.NoError(t, err)
	otherContext := sha256.Sum256([]byte{1})
	corrupted := append([]byte(nil), terms.InputProof...)
	corrupted[len(corrupted)-1] ^= 1

	tests := []struct {
		name   string
		mutate func(*CooperativeSpendTerms)
	}{{
		name: "session context",
		mutate: func(terms *CooperativeSpendTerms) {
			terms.SessionContext = otherContext[:]
		},
	}, {
		name: "funding timelock",
		mutate: func(terms *CooperativeSpendTerms) {
			terms.Funding = loopOutReservation(
				t, loopOutReservationExpiry+1,
			)
		},
	}, {
		name: "funding contract",
		mutate: func(terms *CooperativeSpendTerms) {
			terms.Funding = deposit
		},
	}, {
		name: "asset amount",
		mutate: func(terms *CooperativeSpendTerms) {
			terms.Input.Amount++
		},
	}, {
		name: "anchor value",
		mutate: func(terms *CooperativeSpendTerms) {
			terms.Input.AnchorValueSat++
		},
	}, {
		name: "funding outpoint",
		mutate: func(terms *CooperativeSpendTerms) {
			terms.Input.AnchorOutpoint.Index++
		},
	}, {
		name: "asset script key",
		mutate: func(terms *CooperativeSpendTerms) {
			terms.Input.ScriptKey[32] ^= 1
		},
	}, {
		name: "missing funding",
		mutate: func(terms *CooperativeSpendTerms) {
			terms.Funding = nil
		},
	}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutated := terms
			test.mutate(&mutated)
			_, err := ValidateCooperativeSpend(mutated, raw)
			require.ErrorIs(t, err, swapkit.ErrInvalid)
		})
	}

	// A package built for another proof of the funding is not the one the
	// funder verified.
	mutated := terms
	mutated.InputProof = corrupted
	_, err = ValidateCooperativeSpend(mutated, raw)
	require.Error(t, err)

	// Bytes that are not a sealed package are rejected.
	_, err = ValidateCooperativeSpend(terms, raw[:len(raw)-1])
	require.Error(t, err)
}
