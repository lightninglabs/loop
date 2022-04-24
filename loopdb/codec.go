package loopdb

import (
	"bytes"
	"fmt"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/keychain"
)

// itob returns an 8-byte big endian representation of v.
func itob(v uint64) []byte {
	b := make([]byte, 8)
	byteOrder.PutUint64(b, v)
	return b
}

// UnmarshalProtocolVersion attempts to unmarshal a byte slice to a
// ProtocolVersion value. If the unmarshal fails, ProtocolVersionUnrecorded is
// returned along with an error.
func UnmarshalProtocolVersion(b []byte) (ProtocolVersion, error) {
	if b == nil {
		return ProtocolVersionUnrecorded, nil
	}

	if len(b) != 4 {
		return ProtocolVersionUnrecorded,
			fmt.Errorf("invalid size: %v", len(b))
	}

	version := ProtocolVersion(byteOrder.Uint32(b))
	if !version.Valid() {
		return ProtocolVersionUnrecorded,
			fmt.Errorf("invalid protocol version: %v", version)
	}

	return version, nil
}

// MarshalProtocolVersion marshals a ProtocolVersion value to a byte slice.
func MarshalProtocolVersion(version ProtocolVersion) []byte {
	var versionBytes [4]byte
	byteOrder.PutUint32(versionBytes[:], uint32(version))

	return versionBytes[:]
}

// MarshalKeyLocator marshals a keychain.KeyLocator to a byte slice.
func MarshalKeyLocator(keyLocator keychain.KeyLocator) ([]byte, error) {
	var (
		scratch [8]byte
		buf     bytes.Buffer
	)

	err := channeldb.EKeyLocator(&buf, &keyLocator, &scratch)
	if err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// UnmarshalKeyLocator unmarshals a keychain.KeyLocator from a byte slice.
func UnmarshalKeyLocator(data []byte) (keychain.KeyLocator, error) {
	if data == nil {
		return keychain.KeyLocator{}, nil
	}

	var (
		scratch    [8]byte
		keyLocator keychain.KeyLocator
	)

	err := channeldb.DKeyLocator(
		bytes.NewReader(data), &keyLocator, &scratch, 8,
	)
	if err != nil {
		return keychain.KeyLocator{}, err
	}

	return keyLocator, nil
}
