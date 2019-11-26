package lsat

import (
	"bytes"
	"errors"
	"testing"

	"github.com/lightningnetwork/lnd/lntypes"
)

var (
	testPaymentHash lntypes.Hash
	testTokenID     [TokenIDSize]byte
)

// TestIdentifierSerialization ensures proper serialization of known identifier
// versions and failures for unknown versions.
func TestIdentifierSerialization(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		id   Identifier
		err  error
	}{
		{
			name: "valid identifier",
			id: Identifier{
				Version:     LatestVersion,
				PaymentHash: testPaymentHash,
				TokenID:     testTokenID,
			},
			err: nil,
		},
		{
			name: "unknown version",
			id: Identifier{
				Version:     LatestVersion + 1,
				PaymentHash: testPaymentHash,
				TokenID:     testTokenID,
			},
			err: ErrUnknownVersion,
		},
	}

	for _, test := range tests {
		test := test
		success := t.Run(test.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := EncodeIdentifier(&buf, &test.id)
			if !errors.Is(err, test.err) {
				t.Fatalf("expected err \"%v\", got \"%v\"",
					test.err, err)
			}
			if test.err != nil {
				return
			}
			id, err := DecodeIdentifier(&buf)
			if err != nil {
				t.Fatalf("unable to decode identifier: %v", err)
			}
			if *id != test.id {
				t.Fatalf("expected id %v, got %v", test.id, *id)
			}
		})
		if !success {
			return
		}
	}
}
