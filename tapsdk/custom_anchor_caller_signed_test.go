package tapsdk

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/stretchr/testify/require"
)

// TestCallerSignedPlanClassification proves a caller-signed plan classifies
// its input, counts as exactly one variant, and derives no signing request.
func TestCallerSignedPlanClassification(t *testing.T) {
	t.Parallel()

	plans := []CustomAnchorInputSigningPlan{{
		InputIndex:   0,
		CallerSigned: &CustomAnchorCallerSignedPlan{},
	}}

	// One input, fully classified.
	require.NoError(t, validateCustomAnchorSigningPlans(plans, nil, 1))

	// A second input stays unclassified.
	err := validateCustomAnchorSigningPlans(plans, nil, 2)
	require.ErrorContains(t, err, "must cover all 2 anchor inputs")

	// Combining variants on one plan is ambiguous.
	signerKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	signer, err := ParseXOnlyPubKey(
		schnorr.SerializePubKey(signerKey.PubKey()),
	)
	require.NoError(t, err)
	ambiguous := []CustomAnchorInputSigningPlan{{
		InputIndex:   0,
		CallerSigned: &CustomAnchorCallerSignedPlan{},
		KeyPath: &CustomAnchorKeyPathSigningPlan{
			Signer: signer,
		},
	}}
	err = validateCustomAnchorSigningPlans(ambiguous, nil, 1)
	require.ErrorContains(t, err, "exactly one spending path")

	// Cloning preserves the variant. Zero-size values share one
	// address in Go, so pointer inequality is not asserted.
	clone := cloneCustomAnchorSigningPlans(plans)
	require.NotNil(t, clone[0].CallerSigned)
}
