package labels

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestValidate tests validation of labels.
func TestValidate(t *testing.T) {
	tests := []struct {
		name  string
		label string
		err   error
	}{
		{
			name:  "label ok",
			label: "label",
			err:   nil,
		},
		{
			name:  "exceeds limit",
			label: strings.Repeat(" ", MaxLength+1),
			err:   ErrLabelTooLong,
		},
		{
			name:  "exactly reserved prefix",
			label: Reserved,
			err:   ErrReservedPrefix,
		},
		{
			name:  "starts with reserved prefix",
			label: fmt.Sprintf("%v test", Reserved),
			err:   ErrReservedPrefix,
		},
		{
			name:  "ends with reserved prefix",
			label: fmt.Sprintf("test %v", Reserved),
			err:   nil,
		},
	}

	for _, test := range tests {
		test := test

		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, test.err, Validate(test.label))
		})
	}
}
