package loopin

import (
	"errors"
	"testing"

	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/version"
	"github.com/stretchr/testify/require"
)

// TestNewFSMRecoveryUsesPersistedProtocolVersion verifies that recovering a
// loop-in selects its state machine from the version stored with the swap. It
// must not depend on legacy/root static-address parameters, which might not be
// available for a valid multi-address swap.
func TestNewFSMRecoveryUsesPersistedProtocolVersion(t *testing.T) {
	addressMgr := &mockAddressManager{
		getParamsErr: errors.New("legacy address parameters unavailable"),
	}
	loopIn := &StaticAddressLoopIn{
		ProtocolVersion: version.ProtocolVersion_V0,
	}
	loopIn.SetState(SignHtlcTx)

	recoveredFSM, err := NewFSM(
		t.Context(), loopIn, &Config{AddressManager: addressMgr}, true,
	)
	require.NoError(t, err)
	require.NotNil(t, recoveredFSM)
	require.Zero(t, addressMgr.getParamsCalls.Load())
}

// TestNewFSMRejectsPersistedUnsupportedProtocolVersion verifies that the
// persisted swap version, rather than a legacy address row, controls protocol
// validation during FSM construction.
func TestNewFSMRejectsPersistedUnsupportedProtocolVersion(t *testing.T) {
	addressMgr := &mockAddressManager{}
	loopIn := &StaticAddressLoopIn{
		ProtocolVersion: version.ProtocolVersion_V0 + 1,
	}

	loopInFSM, err := NewFSM(
		t.Context(), loopIn, &Config{AddressManager: addressMgr}, true,
	)
	require.ErrorIs(t, err, deposit.ErrProtocolVersionNotSupported)
	require.Nil(t, loopInFSM)
	require.Zero(t, addressMgr.getParamsCalls.Load())
}
