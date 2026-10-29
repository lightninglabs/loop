package withdraw

import (
	"testing"

	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/require"
)

// TestCreateFinalizedWithdrawalTxRejectsEmptyInputs rejects an empty selection
// before touching the signer, wallet, address manager, or server.
func TestCreateFinalizedWithdrawalTxRejectsEmptyInputs(t *testing.T) {
	t.Parallel()

	for _, deposits := range [][]*deposit.Deposit{nil, {}} {
		manager := &Manager{}
		tx, packet, err := manager.CreateFinalizedWithdrawalTx(
			t.Context(), deposits, nil, -250_000, 100_000,
			lnrpc.CommitmentType_ANCHORS,
		)
		require.ErrorContains(t, err, "no deposits selected")
		require.Nil(t, tx)
		require.Nil(t, packet)
	}
}
