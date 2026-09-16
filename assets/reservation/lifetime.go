package reservation

import (
	"errors"
	"math"
)

// Lifetime contains absolute block heights derived from the first funding
// confirmation. Restart and extra confirmations never reset these heights.
type Lifetime struct {
	// TimeoutHeight is the first block that may contain a CSV timeout spend.
	TimeoutHeight uint32

	// ExecutionCutoff is exclusive: no new swap may start at this height.
	ExecutionCutoff uint32
}

// Lifetime derives the reservation's deadlines using block-based CSV rules.
// Heights must fit the signed block-height fields used by LND.
func (t Terms) Lifetime(confirmationHeight uint32) (Lifetime, error) {
	if err := t.Validate(); err != nil {
		return Lifetime{}, err
	}
	expiry := uint64(confirmationHeight) + uint64(t.CSVDelay)
	if confirmationHeight == 0 || expiry > math.MaxInt32 {
		return Lifetime{}, errors.New("invalid funding confirmation height")
	}

	return Lifetime{
		TimeoutHeight:   uint32(expiry),
		ExecutionCutoff: uint32(expiry) - t.ExecutionDelta,
	}, nil
}
