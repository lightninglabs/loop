package tapsdk

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/stretchr/testify/require"
)

func TestDecodePackage(t *testing.T) {
	approved, final := nativeSignedFixture(t)
	for _, pkg := range []*sdk.CustomAnchorTransferPackage{approved, final} {
		raw, err := pkg.MarshalBinary()
		require.NoError(t, err)
		decoded, err := DecodePackage(raw)
		require.NoError(t, err)
		require.Equal(t, pkg.PackageDigest, decoded.PackageDigest)
		require.Equal(t, pkg.CommittedPackageDigest,
			decoded.CommittedPackageDigest)
	}

	changed := approved.Clone()
	changed.Outputs[0].Amount++
	raw, err := approved.MarshalBinary()
	require.NoError(t, err)
	// Encode an inconsistent DTO without the SDK's validating marshaler.
	type packageJSON sdk.CustomAnchorTransferPackage
	payload, err := json.Marshal((*packageJSON)(changed))
	require.NoError(t, err)
	headerSize := bytes.IndexByte(raw, '{')
	require.Positive(t, headerSize)
	raw = append(raw[:headerSize:headerSize], payload...)
	binary.BigEndian.PutUint32(raw[headerSize-4:headerSize],
		uint32(len(payload)))
	decoded, err := DecodePackage(raw)
	require.Error(t, err)
	require.Nil(t, decoded)

	_, err = DecodePackage(raw[:len(raw)-1])
	require.Error(t, err)
	_, err = DecodePackage(nil)
	require.Error(t, err)
}

func TestDecodePackageSizeLimit(t *testing.T) {
	_, err := DecodePackage(make([]byte, MaxPackageBytes+1))
	require.ErrorIs(t, err, swapkit.ErrInvalid)
}
