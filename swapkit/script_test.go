package swapkit

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestQuoteLeafEncodings pins the unfunded templates used by existing
// callers, including a zero payment hash and signed script numbers.
func TestQuoteLeafEncodings(t *testing.T) {
	var key [32]byte
	prefix := "20" + strings.Repeat("00", 32) + "ad"

	success, err := HTLCSuccessScript(key, Hash{})
	require.NoError(t, err)
	const zeroPaymentHash160 = "d1a70126ff7a149ca6f9b638db084480440ff842"
	require.Equal(t, prefix+"82012088a914"+zeroPaymentHash160+"8851b2",
		hex.EncodeToString(success))

	csv, err := CSVTimeoutScript(key, -1)
	require.NoError(t, err)
	require.Equal(t, prefix+"4fb2", hex.EncodeToString(csv))

	cltv, err := CLTVTimeoutScript(key, 0)
	require.NoError(t, err)
	require.Equal(t, prefix+"00b1", hex.EncodeToString(cltv))
}
