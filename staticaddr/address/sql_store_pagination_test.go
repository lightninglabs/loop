package address

import (
	"testing"

	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/test"
	"github.com/stretchr/testify/require"
)

// TestListStaticAddressesPages checks cursor boundaries and gaps against the
// selected SQL backend, including an exact page boundary and an empty tail.
func TestListStaticAddressesPages(t *testing.T) {
	db := loopdb.NewTestDB(t)
	store := NewSqlStore(db.BaseDB)
	_, client := test.CreateKey(1)
	_, server := test.CreateKey(2)
	var ids []int32
	for i := range 5 {
		params := &AddressParameters{
			ClientPubkey: client,
			ServerPubkey: server,
			PkScript:     []byte{byte(i)},
			Expiry:       144,
		}
		require.NoError(t, store.CreateStaticAddress(t.Context(), params))
		id, err := store.GetStaticAddressID(t.Context(), params.PkScript)
		require.NoError(t, err)
		ids = append(ids, id)
	}
	_, err := db.ExecContext(t.Context(),
		"DELETE FROM static_addresses WHERE id = $1", ids[1])
	require.NoError(t, err)

	var actual []int32
	var afterID int32
	for range 3 {
		page, err := store.ListStaticAddresses(t.Context(), afterID, 2)
		require.NoError(t, err)
		require.LessOrEqual(t, len(page), 2)
		for _, params := range page {
			require.Greater(t, params.ID, afterID)
			actual = append(actual, params.ID)
			afterID = params.ID
		}
	}
	require.Equal(t, []int32{ids[0], ids[2], ids[3], ids[4]}, actual)
	page, err := store.ListStaticAddresses(t.Context(), afterID, 2)
	require.NoError(t, err)
	require.Empty(t, page)
	_, err = store.ListStaticAddresses(t.Context(), 0, 0)
	require.Error(t, err)
	_, err = store.ListStaticAddresses(t.Context(), -1, 2)
	require.Error(t, err)
}
