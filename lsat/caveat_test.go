package lsat

import (
	"errors"
	"testing"

	"gopkg.in/macaroon.v2"
)

var (
	testMacaroon, _ = macaroon.New(nil, nil, "", macaroon.LatestVersion)
)

// TestCaveatSerialization ensures that we can properly encode/decode valid
// caveats and cannot do so for invalid ones.
func TestCaveatSerialization(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		caveatStr string
		err       error
	}{
		{
			name:      "valid caveat",
			caveatStr: "expiration=1337",
			err:       nil,
		},
		{
			name:      "valid caveat with separator in value",
			caveatStr: "expiration=1337=",
			err:       nil,
		},
		{
			name:      "invalid caveat",
			caveatStr: "expiration:1337",
			err:       ErrInvalidCaveat,
		},
	}

	for _, test := range tests {
		test := test
		success := t.Run(test.name, func(t *testing.T) {
			caveat, err := DecodeCaveat(test.caveatStr)
			if !errors.Is(err, test.err) {
				t.Fatalf("expected err \"%v\", got \"%v\"",
					test.err, err)
			}

			if test.err != nil {
				return
			}

			caveatStr := EncodeCaveat(caveat)
			if caveatStr != test.caveatStr {
				t.Fatalf("expected encoded caveat \"%v\", "+
					"got \"%v\"", test.caveatStr, caveatStr)
			}
		})
		if !success {
			return
		}
	}
}

// TestHasCaveat ensures we can determine whether a macaroon contains a caveat
// with a specific condition.
func TestHasCaveat(t *testing.T) {
	t.Parallel()

	const (
		cond  = "cond"
		value = "value"
	)
	m := testMacaroon.Clone()

	// The macaroon doesn't have any caveats, so we shouldn't find any.
	if _, ok := HasCaveat(m, cond); ok {
		t.Fatal("found unexpected caveat with unknown condition")
	}

	// Add two caveats, one in a valid LSAT format and another invalid.
	// We'll test that we're still able to determine the macaroon contains
	// the valid caveat even though there is one that is invalid.
	invalidCaveat := []byte("invalid")
	if err := m.AddFirstPartyCaveat(invalidCaveat); err != nil {
		t.Fatalf("unable to add macaroon caveat: %v", err)
	}
	validCaveat1 := Caveat{Condition: cond, Value: value}
	if err := AddFirstPartyCaveats(m, validCaveat1); err != nil {
		t.Fatalf("unable to add macaroon caveat: %v", err)
	}

	caveatValue, ok := HasCaveat(m, cond)
	if !ok {
		t.Fatal("expected macaroon to contain caveat")
	}
	if caveatValue != validCaveat1.Value {
		t.Fatalf("expected caveat value \"%v\", got \"%v\"",
			validCaveat1.Value, caveatValue)
	}

	// If we add another caveat with the same condition, the value of the
	// most recently added caveat should be returned instead.
	validCaveat2 := validCaveat1
	validCaveat2.Value += value
	if err := AddFirstPartyCaveats(m, validCaveat2); err != nil {
		t.Fatalf("unable to add macaroon caveat: %v", err)
	}

	caveatValue, ok = HasCaveat(m, cond)
	if !ok {
		t.Fatal("expected macaroon to contain caveat")
	}
	if caveatValue != validCaveat2.Value {
		t.Fatalf("expected caveat value \"%v\", got \"%v\"",
			validCaveat2.Value, caveatValue)
	}
}

// TestVerifyCaveats ensures caveat verification only holds true for known
// caveats.
func TestVerifyCaveats(t *testing.T) {
	t.Parallel()

	caveat1 := Caveat{Condition: "1", Value: "test"}
	caveat2 := Caveat{Condition: "2", Value: "test"}
	satisfier := Satisfier{
		Condition: caveat1.Condition,
		SatisfyPrevious: func(c Caveat, prev Caveat) error {
			return nil
		},
		SatisfyFinal: func(c Caveat) error {
			return nil
		},
	}
	invalidSatisfyPrevious := func(c Caveat, prev Caveat) error {
		return errors.New("no")
	}
	invalidSatisfyFinal := func(c Caveat) error {
		return errors.New("no")
	}

	tests := []struct {
		name       string
		caveats    []Caveat
		satisfiers []Satisfier
		shouldFail bool
	}{
		{
			name:       "simple verification",
			caveats:    []Caveat{caveat1},
			satisfiers: []Satisfier{satisfier},
			shouldFail: false,
		},
		{
			name:       "unknown caveat",
			caveats:    []Caveat{caveat1, caveat2},
			satisfiers: []Satisfier{satisfier},
			shouldFail: false,
		},
		{
			name:    "one invalid",
			caveats: []Caveat{caveat1, caveat2},
			satisfiers: []Satisfier{
				satisfier,
				{
					Condition:    caveat2.Condition,
					SatisfyFinal: invalidSatisfyFinal,
				},
			},
			shouldFail: true,
		},
		{
			name:    "prev invalid",
			caveats: []Caveat{caveat1, caveat1},
			satisfiers: []Satisfier{
				{
					Condition:       caveat1.Condition,
					SatisfyPrevious: invalidSatisfyPrevious,
				},
			},
			shouldFail: true,
		},
	}

	for _, test := range tests {
		test := test
		success := t.Run(test.name, func(t *testing.T) {
			err := VerifyCaveats(test.caveats, test.satisfiers...)
			if test.shouldFail && err == nil {
				t.Fatal("expected caveat verification to fail")
			}
			if !test.shouldFail && err != nil {
				t.Fatal("unexpected caveat verification failure")
			}
		})
		if !success {
			return
		}
	}
}
