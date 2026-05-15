package loopdb

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jessevdk/go-flags"
	"github.com/stretchr/testify/require"
)

// TestSecretUnmarshalFlag tests the Secret type's UnmarshalFlag method.
func TestSecretUnmarshalFlag(t *testing.T) {
	t.Parallel()

	t.Run("direct value", func(t *testing.T) {
		t.Parallel()

		var s Secret
		err := s.UnmarshalFlag("mypassword")
		require.NoError(t, err)
		require.Equal(t, Secret("mypassword"), s)
	})

	t.Run("empty value", func(t *testing.T) {
		t.Parallel()

		var s Secret
		err := s.UnmarshalFlag("")
		require.NoError(t, err)
		require.Equal(t, Secret(""), s)
	})

	t.Run("file reference", func(t *testing.T) {
		t.Parallel()

		// Create a temp file with a password.
		tmpDir := t.TempDir()
		passFile := filepath.Join(tmpDir, "password.txt")
		err := os.WriteFile(passFile, []byte("secretpassword"), 0600)
		require.NoError(t, err)

		var s Secret
		err = s.UnmarshalFlag("@" + passFile)
		require.NoError(t, err)
		require.Equal(t, Secret("secretpassword"), s)
	})

	t.Run("file with trailing newline", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		passFile := filepath.Join(tmpDir, "password.txt")
		err := os.WriteFile(passFile, []byte("secretpassword\n"), 0600)
		require.NoError(t, err)

		var s Secret
		err = s.UnmarshalFlag("@" + passFile)
		require.NoError(t, err)
		require.Equal(t, Secret("secretpassword"), s)
	})

	t.Run("file with CRLF", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		passFile := filepath.Join(tmpDir, "password.txt")
		err := os.WriteFile(passFile, []byte("secretpassword\r\n"), 0600)
		require.NoError(t, err)

		var s Secret
		err = s.UnmarshalFlag("@" + passFile)
		require.NoError(t, err)
		require.Equal(t, Secret("secretpassword"), s)
	})

	t.Run("file with trailing whitespace", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		passFile := filepath.Join(tmpDir, "password.txt")
		err := os.WriteFile(passFile, []byte("secretpassword \t\n"), 0600)
		require.NoError(t, err)

		var s Secret
		err = s.UnmarshalFlag("@" + passFile)
		require.NoError(t, err)
		require.Equal(t, Secret("secretpassword"), s)
	})

	t.Run("empty file", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		passFile := filepath.Join(tmpDir, "password.txt")
		err := os.WriteFile(passFile, []byte(""), 0600)
		require.NoError(t, err)

		var s Secret
		err = s.UnmarshalFlag("@" + passFile)
		require.NoError(t, err)
		require.Equal(t, Secret(""), s)
	})

	t.Run("file with only newlines", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		passFile := filepath.Join(tmpDir, "password.txt")
		err := os.WriteFile(passFile, []byte("\n\n\n"), 0600)
		require.NoError(t, err)

		var s Secret
		err = s.UnmarshalFlag("@" + passFile)
		require.NoError(t, err)
		require.Equal(t, Secret(""), s)
	})

	t.Run("file with newline in middle", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		passFile := filepath.Join(tmpDir, "password.txt")
		err := os.WriteFile(passFile, []byte("pass\nword\n"), 0600)
		require.NoError(t, err)

		var s Secret
		err = s.UnmarshalFlag("@" + passFile)
		require.NoError(t, err)
		require.Equal(t, Secret("pass\nword"), s)
	})

	t.Run("file not found", func(t *testing.T) {
		t.Parallel()

		var s Secret
		err := s.UnmarshalFlag("@/nonexistent/path/to/file")
		require.Error(t, err)
		require.Contains(t, err.Error(), "secret file not found")
		require.Contains(t, err.Error(), "/nonexistent/path/to/file")
	})

	t.Run("at symbol only", func(t *testing.T) {
		t.Parallel()

		// Just "@" means read from empty path, which should fail.
		var s Secret
		err := s.UnmarshalFlag("@")
		require.Error(t, err)
	})

	t.Run("value starting with at but not file ref", func(t *testing.T) {
		t.Parallel()

		// A value like "@myemail" would try to read file "myemail".
		// This should fail because that file doesn't exist.
		var s Secret
		err := s.UnmarshalFlag("@myemail")
		require.Error(t, err)
		require.Contains(t, err.Error(), "secret file not found")
	})
}

// TestSecretGoFlagsIntegration tests that Secret works correctly with the
// go-flags parser.
func TestSecretGoFlagsIntegration(t *testing.T) {
	t.Parallel()

	type Config struct {
		Password Secret `long:"password"`
	}

	t.Run("direct value via flags", func(t *testing.T) {
		t.Parallel()

		var cfg Config
		parser := flags.NewParser(&cfg, flags.Default)
		_, err := parser.ParseArgs([]string{"--password=directpass"})
		require.NoError(t, err)
		require.Equal(t, Secret("directpass"), cfg.Password)
	})

	t.Run("file reference via flags", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		passFile := filepath.Join(tmpDir, "password.txt")
		err := os.WriteFile(passFile, []byte("filepass\n"), 0600)
		require.NoError(t, err)

		var cfg Config
		parser := flags.NewParser(&cfg, flags.Default)
		_, err = parser.ParseArgs([]string{"--password=@" + passFile})
		require.NoError(t, err)
		require.Equal(t, Secret("filepass"), cfg.Password)
	})
}
