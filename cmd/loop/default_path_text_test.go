package main

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDefaultPathText verifies HOME-based path elision behavior used for help
// defaults without relying on the real environment.
func TestDefaultPathText(t *testing.T) {
	sep := string(filepath.Separator)
	home := filepath.Clean(sep + "home" + sep + "alice")

	homeDir := func() (string, error) { return home, nil }

	tests := []struct {
		name   string
		value  string
		homeFn func() (string, error)
		want   string
	}{
		{
			name:  "empty value",
			value: "",
			homeFn: func() (string, error) {
				return home, nil
			},
			want: "",
		},
		{
			name:   "nil homedir func",
			value:  home + sep + "data",
			homeFn: nil,
			want:   home + sep + "data",
		},
		{
			name:  "homedir error",
			value: home + sep + "data",
			homeFn: func() (string, error) {
				return "", errors.New("homedir error")
			},
			want: home + sep + "data",
		},
		{
			name:   "exact home",
			value:  home,
			homeFn: homeDir,
			want:   "~",
		},
		{
			name:   "home prefix",
			value:  home + sep + "dir" + sep + "file",
			homeFn: homeDir,
			want:   "~" + sep + "dir" + sep + "file",
		},
		{
			name:   "non-home path",
			value:  filepath.Clean(sep + "var" + sep + "tmp"),
			homeFn: homeDir,
			want:   filepath.Clean(sep + "var" + sep + "tmp"),
		},
		{
			name:   "prefix but not path segment",
			value:  home + "x" + sep + "dir",
			homeFn: homeDir,
			want:   home + "x" + sep + "dir",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := defaultPathText(test.value, test.homeFn)
			require.Equalf(
				t, test.want, got,
				"defaultPathText(%q)", test.value,
			)
		})
	}
}
