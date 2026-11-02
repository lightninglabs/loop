package macaroon

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSourceFromPath verifies that FromPath loads a single macaroon
// file and maps it to every service entry in the pouch.
func TestSourceFromPath(t *testing.T) {
	t.Parallel()

	payload := []byte("pretend-this-is-a-macaroon")
	path := filepath.Join(t.TempDir(), "admin.macaroon")
	require.NoError(t, os.WriteFile(path, payload, 0o600))

	pouch, err := FromPath(path).LoadPouch()
	require.NoError(t, err)

	expected := SerializedMacaroon(hex.EncodeToString(payload))
	for _, svc := range macaroonServices {
		require.Equal(t, expected, pouch[svc],
			"service %q should share the single-path mac", svc)
	}
}

// TestSourceFromPath_Missing verifies that FromPath surfaces OS
// errors when the file is absent.
func TestSourceFromPath_Missing(t *testing.T) {
	t.Parallel()

	_, err := FromPath("/nonexistent/tapd.macaroon").LoadPouch()
	require.Error(t, err)
}

// TestSourceFromDir verifies that FromDir loads one macaroon per
// service name from the given directory.
func TestSourceFromDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	contents := map[TaprpcServiceMac][]byte{}
	for _, svc := range macaroonServices {
		payload := []byte("body-" + string(svc))
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, string(svc)), payload, 0o600,
		))
		contents[svc] = payload
	}

	pouch, err := FromDir(dir).LoadPouch()
	require.NoError(t, err)

	for svc, payload := range contents {
		require.Equal(t,
			SerializedMacaroon(hex.EncodeToString(payload)),
			pouch[svc],
		)
	}
}

// TestSourceFromDir_MissingFile verifies that FromDir fails as soon
// as any expected service macaroon is absent.
func TestSourceFromDir_MissingFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Intentionally only drop one of the expected files; LoadPouch
	// must fail rather than silently returning a partial pouch.
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, string(AdminServiceMac)),
		[]byte("partial"), 0o600,
	))

	_, err := FromDir(dir).LoadPouch()
	require.Error(t, err)
}

// TestSourceFromHex verifies that FromHex treats its input as the
// single credential for every service.
func TestSourceFromHex(t *testing.T) {
	t.Parallel()

	const rawHex = "deadbeef"
	pouch, err := FromHex(rawHex).LoadPouch()
	require.NoError(t, err)

	for _, svc := range macaroonServices {
		require.Equal(t, SerializedMacaroon(rawHex), pouch[svc])
	}
}

func TestSourceFromHexInvalid(t *testing.T) {
	t.Parallel()

	for _, hex := range []string{"", "not-hex"} {
		_, err := FromHex(hex).LoadPouch()
		require.Error(t, err)
	}
}
