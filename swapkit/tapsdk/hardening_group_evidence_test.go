package tapsdk

import (
	"testing"

	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/stretchr/testify/require"
)

// The nested TLV path of the merkle proofs inside a proof's inclusion proof:

// TestAuditBGroupEvidenceMissingScriptKey shows that group evidence whose
// asset omits its script key record is decoded without validation and then
// crashes when VerifyGroupMembership encodes it again for tapd.
func TestGroupEvidenceRejectsInvalidAsset(t *testing.T) {
	anchor := groupEvidence(t, "anchor", true)
	key := anchor.Asset.GroupKey.GroupPubKey.SerializeCompressed()
	// Asset leaf record 16 is the script key.
	single := auditStripProofRecord(t, anchor, []uint64{10}, 16)

	var verifyErr error
	require.NotPanics(t, func() {
		verifyErr = VerifyGroupMembership(t.Context(), &evidenceNode{},
			&address.RegressionNetTap, key,
			[]asset.ID{anchor.Asset.ID()}, [][]byte{single}, 1<<20)
	})
	require.Error(t, verifyErr)
}
