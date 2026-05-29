package loop

import (
	"testing"

	looptest "github.com/lightninglabs/loop/test"
	"github.com/stretchr/testify/require"
)

// TestParseServerPubKey ensures that parseServerPubKey accepts a valid
// compressed public key and rejects keys with an invalid length or contents.
func TestParseServerPubKey(t *testing.T) {
	t.Parallel()

	_, pubKey := looptest.CreateKey(1)
	pubKeyBytes := pubKey.SerializeCompressed()

	parsedKey, err := parseServerPubKey("test key", pubKeyBytes)
	require.NoError(t, err)
	require.Equal(t, pubKeyBytes, parsedKey[:])

	_, err = parseServerPubKey("test key", pubKeyBytes[:32])
	require.ErrorContains(t, err, "invalid test key length")

	invalidKey := make([]byte, 33)
	_, err = parseServerPubKey("test key", invalidKey)
	require.ErrorContains(t, err, "invalid test key")
}
