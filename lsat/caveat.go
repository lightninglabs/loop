package lsat

import (
	"errors"
	"fmt"
	"strings"

	"gopkg.in/macaroon.v2"
)

const (
	// PreimageKey is the key used for a payment preimage caveat.
	PreimageKey = "preimage"
)

var (
	// ErrInvalidCaveat is an error returned when we attempt to decode a
	// caveat with an invalid format.
	ErrInvalidCaveat = errors.New("caveat must be of the form " +
		"\"condition=value\"")
)

// Caveat is a predicate that can be applied to an LSAT in order to restrict its
// use in some form. Caveats are evaluated during LSAT verification after the
// LSAT's signature is verified. The predicate of each caveat must hold true in
// order to successfully validate an LSAT.
type Caveat struct {
	// Condition serves as a way to identify a caveat and how to satisfy it.
	Condition string

	// Value is what will be used to satisfy a caveat. This can be as
	// flexible as needed, as long as it can be encoded into a string.
	Value string
}

// NewCaveat construct a new caveat with the given condition and value.
func NewCaveat(condition string, value string) Caveat {
	return Caveat{Condition: condition, Value: value}
}

// String returns a user-friendly view of a caveat.
func (c Caveat) String() string {
	return EncodeCaveat(c)
}

// EncodeCaveat encodes a caveat into its string representation.
func EncodeCaveat(c Caveat) string {
	return fmt.Sprintf("%v=%v", c.Condition, c.Value)
}

// DecodeCaveat decodes a caveat from its string representation.
func DecodeCaveat(s string) (Caveat, error) {
	parts := strings.SplitN(s, "=", 2)
	if len(parts) != 2 {
		return Caveat{}, ErrInvalidCaveat
	}
	return Caveat{Condition: parts[0], Value: parts[1]}, nil
}

// AddFirstPartyCaveats adds a set of caveats as first-party caveats to a
// macaroon.
func AddFirstPartyCaveats(m *macaroon.Macaroon, caveats ...Caveat) error {
	for _, c := range caveats {
		rawCaveat := []byte(EncodeCaveat(c))
		if err := m.AddFirstPartyCaveat(rawCaveat); err != nil {
			return err
		}
	}

	return nil
}

// HasCaveat checks whether the given macaroon has a caveat with the given
// condition, and if so, returns its value. If multiple caveats with the same
// condition exist, then the value of the last one is returned.
func HasCaveat(m *macaroon.Macaroon, cond string) (string, bool) {
	var value *string
	for _, rawCaveat := range m.Caveats() {
		caveat, err := DecodeCaveat(string(rawCaveat.Id))
		if err != nil {
			// Ignore any unknown caveats as we can't decode them.
			continue
		}
		if caveat.Condition == cond {
			value = &caveat.Value
		}
	}

	if value == nil {
		return "", false
	}
	return *value, true
}

// VerifyCaveats determines whether every relevant caveat of an LSAT holds true.
// A caveat is considered relevant if a satisfier is provided for it, which is
// what we'll use as their evaluation.
//
// NOTE: The caveats provided should be in the same order as in the LSAT to
// ensure the correctness of each satisfier's SatisfyPrevious.
func VerifyCaveats(caveats []Caveat, satisfiers ...Satisfier) error {
	// Construct a set of our satisfiers to determine which caveats we know
	// how to satisfy.
	caveatSatisfiers := make(map[string]Satisfier, len(satisfiers))
	for _, satisfier := range satisfiers {
		caveatSatisfiers[satisfier.Condition] = satisfier
	}
	relevantCaveats := make(map[string][]Caveat)
	for _, caveat := range caveats {
		if _, ok := caveatSatisfiers[caveat.Condition]; !ok {
			continue
		}
		relevantCaveats[caveat.Condition] = append(
			relevantCaveats[caveat.Condition], caveat,
		)
	}

	for condition, caveats := range relevantCaveats {
		satisfier := caveatSatisfiers[condition]

		// Since it's possible for a chain of caveat to exist for the
		// same condition as a way to demote privileges, we'll ensure
		// each one satisfies its previous.
		for i, j := 0, 1; j < len(caveats); i, j = i+1, j+1 {
			prevCaveat := caveats[i]
			curCaveat := caveats[j]
			err := satisfier.SatisfyPrevious(prevCaveat, curCaveat)
			if err != nil {
				return err
			}
		}

		// Once we verify the previous ones, if any, we can proceed to
		// verify the final one, which is the decision maker.
		err := satisfier.SatisfyFinal(caveats[len(caveats)-1])
		if err != nil {
			return err
		}
	}

	return nil
}
