package lsat

import (
	"errors"
	"testing"
)

// TestServicesCaveatSerialization ensures that we can properly encode/decode
// valid services from a caveat and cannot do so for invalid ones.
func TestServicesCaveatSerialization(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		err   error
	}{
		{
			name:  "single service",
			value: "a:0",
			err:   nil,
		},
		{
			name:  "multiple services",
			value: "a:0,b:1,c:0",
			err:   nil,
		},
		{
			name:  "no services",
			value: "",
			err:   ErrNoServices,
		},
		{
			name:  "service missing name",
			value: ":0",
			err:   ErrInvalidService,
		},
		{
			name:  "service missing tier",
			value: "a",
			err:   ErrInvalidService,
		},
		{
			name:  "service empty tier",
			value: "a:",
			err:   ErrInvalidService,
		},
		{
			name:  "service non-numeric tier",
			value: "a:b",
			err:   ErrInvalidService,
		},
		{
			name:  "empty services",
			value: ",,",
			err:   ErrInvalidService,
		},
	}

	for _, test := range tests {
		test := test
		success := t.Run(test.name, func(t *testing.T) {
			services, err := decodeServicesCaveatValue(test.value)
			if !errors.Is(err, test.err) {
				t.Fatalf("expected err \"%v\", got \"%v\"",
					test.err, err)
			}

			if test.err != nil {
				return
			}

			value, _ := encodeServicesCaveatValue(services...)
			if value != test.value {
				t.Fatalf("expected encoded services \"%v\", "+
					"got \"%v\"", test.value, value)
			}
		})
		if !success {
			return
		}
	}
}
