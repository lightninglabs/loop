package loopin

import (
	"fmt"
	"math"

	"github.com/lightninglabs/loop/staticaddr/deposit"
)

// validateDepositHtlcExpiry checks the actual server HTLC expiry against each
// deposit's CSV expiry before releasing any signatures to the server.
func validateDepositHtlcExpiry(deposits []*deposit.Deposit,
	htlcExpiry int32, initiationHeight uint32) error {

	if htlcExpiry <= 0 {
		return fmt.Errorf("invalid htlc expiry height %d", htlcExpiry)
	}
	if len(deposits) == 0 {
		return fmt.Errorf("no deposits to validate")
	}

	requiredExpiry := int64(htlcExpiry) + DepositHtlcDelta
	for i, d := range deposits {
		if d == nil || d.AddressParams == nil {
			return fmt.Errorf("missing deposit or address parameters "+
				"at index %d", i)
		}

		confirmationHeight := d.GetConfirmationHeight()
		if confirmationHeight < 0 || confirmationHeight > math.MaxUint32 {
			return fmt.Errorf("invalid confirmation height %d for "+
				"deposit %s", confirmationHeight, d.OutPoint)
		}
		if confirmationHeight == 0 {
			// An unconfirmed deposit can confirm in the next block.
			// Later confirmation only increases its expiry height.
			confirmationHeight = int64(initiationHeight) + 1
		}

		expiry := confirmationHeight + int64(d.AddressParams.Expiry)
		if expiry < requiredExpiry {
			return fmt.Errorf("deposit %s expires at height %d, "+
				"before htlc expiry %d plus safety buffer %d",
				d.OutPoint, expiry, htlcExpiry, DepositHtlcDelta)
		}
	}

	return nil
}
