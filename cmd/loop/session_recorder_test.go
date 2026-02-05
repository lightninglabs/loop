package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDeriveSessionSlug verifies slug derivation from CLI arguments.
func TestDeriveSessionSlug(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "empty_args",
			want: "",
		},
		{
			name: "binary_only",
			args: []string{"/usr/local/bin/loop"},
			want: "loop",
		},
		{
			name: "with_subcommand",
			args: []string{"loop", "out"},
			want: "loop-out",
		},
		{
			name: "stops_at_flag",
			args: []string{"loop", "out", "--network", "regtest"},
			want: "loop-out",
		},
		{
			name: "skips_empty_args",
			args: []string{"loop", "", "quote", "out"},
			want: "loop-quote-out",
		},
		{
			name: "sanitizes_tokens",
			args: []string{"loop", "Quote", "Out"},
			want: "loop-quote-out",
		},
		{
			name: "sanitizes_path_base",
			args: []string{"/tmp/loop-cli", "out"},
			want: "loop-cli-out",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			slug := deriveSessionSlug(test.args)
			require.Equal(t, test.want, slug)
		})
	}
}

// TestSanitizeSlug verifies slug normalization behavior.
func TestSanitizeSlug(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "already_clean",
			input: "loop-out",
			want:  "loop-out",
		},
		{
			name:  "upper_and_spaces",
			input: "Loop Out",
			want:  "loop-out",
		},
		{
			name:  "symbols_collapsed",
			input: "loop@@@out",
			want:  "loop-out",
		},
		{
			name:  "trims_dashes",
			input: "--loop-out--",
			want:  "loop-out",
		},
		{
			name:  "empty_to_default",
			input: "!!!",
			want:  "session",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			slug := sanitizeSlug(test.input)
			require.Equal(t, test.want, slug)
		})
	}
}

// TestParseSessionCounter verifies session counter parsing.
func TestParseSessionCounter(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int
		ok    bool
	}{
		{
			name:  "with_suffix",
			input: "01_loop-out.json",
			want:  1,
			ok:    true,
		},
		{
			name:  "with_extra_underscores",
			input: "12_loop_in.json",
			want:  12,
			ok:    true,
		},
		{
			name:  "no_suffix",
			input: "99",
			want:  99,
			ok:    true,
		},
		{
			name:  "no_extension",
			input: "7_loop-out",
			want:  7,
			ok:    true,
		},
		{
			name:  "empty_name",
			input: "",
			ok:    false,
		},
		{
			name:  "missing_prefix",
			input: "_loop.json",
			ok:    false,
		},
		{
			name:  "non_numeric_prefix",
			input: "loop.json",
			ok:    false,
		},
		{
			name:  "mixed_prefix",
			input: "1a_loop.json",
			ok:    false,
		},
		{
			name:  "negative_prefix",
			input: "-1_loop.json",
			ok:    false,
		},
		{
			name:  "plus_prefix",
			input: "+1_loop.json",
			ok:    false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value, ok := parseSessionCounter(test.input)
			require.Equal(t, test.ok, ok)
			if test.ok {
				require.Equal(t, test.want, value)
			}
		})
	}
}
