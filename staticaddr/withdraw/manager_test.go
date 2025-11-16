package withdraw

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNewManagerHeightValidation ensures the constructor rejects zero heights.
func TestNewManagerHeightValidation(t *testing.T) {
	t.Parallel()

	cfg := &ManagerConfig{}

	_, err := NewManager(cfg, 0)
	require.ErrorContains(t, err, "invalid current height 0")

	manager, err := NewManager(cfg, 1)
	require.NoError(t, err)
	require.NotNil(t, manager)
}
