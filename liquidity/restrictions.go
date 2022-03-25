package liquidity

import (
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
)

// Restrictions indicates the restrictions placed on a swap.
type Restrictions struct {
	// Minimum is the minimum amount we can swap, inclusive.
	Minimum btcutil.Amount

	// Maximum is the maximum amount we can swap, inclusive.
	Maximum btcutil.Amount
}

// String returns a string representation of a set of restrictions.
func (r *Restrictions) String() string {
	return fmt.Sprintf("%v-%v", r.Minimum, r.Maximum)
}

// NewRestrictions creates a set of restrictions.
func NewRestrictions(minimum, maximum btcutil.Amount) *Restrictions {
	return &Restrictions{
		Minimum: minimum,
		Maximum: maximum,
	}
}
