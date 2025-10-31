package deposit

import (
	"database/sql"
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	loop_test "github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

func TestToDeposit(t *testing.T) {
	depositID, err := GetRandomDepositID()
	require.NoError(t, err)

	swapHash, err := lntypes.MakeHash(dummyHashBytes())
	require.NoError(t, err)

	tx := wire.NewMsgTx(2)
	txHash := tx.TxHash()

	_, client := loop_test.CreateKey(1)
	_, server := loop_test.CreateKey(2)

	tests := []struct {
		name       string
		row        sqlc.AllDepositsRow
		lastUpdate sqlc.DepositUpdate
		expectErr  bool
	}{
		{
			name: "fully valid data",
			row: sqlc.AllDepositsRow{
				DepositID:          depositID[:],
				TxHash:             txHash[:],
				Amount:             100000000,
				ConfirmationHeight: 123456,
				SwapHash:           swapHash[:],
				ClientPubkey:       client.SerializeCompressed(),
				ServerPubkey:       server.SerializeCompressed(),
				Expiry:             sql.NullInt32{Int32: 1000, Valid: true},
			},
			lastUpdate: sqlc.DepositUpdate{
				UpdateState: "completed",
			},
			expectErr: false,
		},
		{
			name: "fully valid data",
			row: sqlc.AllDepositsRow{
				DepositID:          depositID[:],
				TxHash:             txHash[:],
				Amount:             100000000,
				ConfirmationHeight: 123456,
				ClientPubkey:       client.SerializeCompressed(),
				ServerPubkey:       server.SerializeCompressed(),
				Expiry:             sql.NullInt32{Int32: 1000, Valid: true},
			},
			lastUpdate: sqlc.DepositUpdate{
				UpdateState: "completed",
			},
			expectErr: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := ToDeposit(test.row, test.lastUpdate)
			if test.expectErr {
				require.Error(t, err)
				require.Nil(t, result)
			} else {
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, fsm.StateType(test.lastUpdate.UpdateState), result.state)
			}
		})
	}
}

func dummyHashBytes() []byte {
	return []byte{0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b,
		0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12, 0x13, 0x14, 0x15,
		0x16, 0x17, 0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f,
		0x20, 0x21, 0x22, 0x23}
}
