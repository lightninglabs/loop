package codec

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWriteVarIntLarge(t *testing.T) {
	var (
		buf     bytes.Buffer
		scratch [9]byte
	)

	// value = 0x100000000 (4294967296)
	// Encoded should be: ff 00 00 00 01 00 00 00 00 (big endian)
	// 0xff discriminator
	// 0x0000000100000000 value
	val := uint64(0x100000000)

	err := WriteVarInt(&buf, val, scratch[:])
	require.NoError(t, err)

	// Expected: 0xff + 8 bytes of val
	expected := []byte{0xff, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00}
	require.Equal(t, expected, buf.Bytes(), "encoded bytes mismatch")

	// Check length
	require.Equal(t, 9, buf.Len())
}
