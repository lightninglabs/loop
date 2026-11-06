package loopin

import (
	"fmt"

	"github.com/btcsuite/btcd/wire"
)

// signedOutpoints returns the deposits the swap signed its htlc tx for: its
// outpoint snapshot, or its deposits if it has none.
func (l *StaticAddressLoopIn) signedOutpoints() ([]wire.OutPoint, error) {
	if len(l.DepositOutpoints) > 0 {
		return parseOutpoints(l.DepositOutpoints)
	}

	outpoints := make([]wire.OutPoint, 0, len(l.Deposits))
	for _, d := range l.Deposits {
		if d == nil {
			return nil, fmt.Errorf("nil deposit in swap %v",
				l.SwapHash)
		}

		outpoints = append(outpoints, d.OutPoint)
	}

	return outpoints, nil
}

// parseOutpoints parses the given outpoint strings.
func parseOutpoints(outpointStrings []string) ([]wire.OutPoint, error) {
	outpoints := make([]wire.OutPoint, 0, len(outpointStrings))
	for _, outpointString := range outpointStrings {
		outpoint, err := wire.NewOutPointFromString(outpointString)
		if err != nil {
			return nil, fmt.Errorf("unable to parse outpoint %q: %w",
				outpointString, err)
		}

		outpoints = append(outpoints, *outpoint)
	}

	return outpoints, nil
}

// spendsExactly returns true if the tx spends exactly the given outpoints.
func spendsExactly(tx *wire.MsgTx, outpoints []wire.OutPoint) bool {
	if tx == nil || len(outpoints) == 0 || len(tx.TxIn) != len(outpoints) {
		return false
	}

	inputs := make(map[wire.OutPoint]struct{}, len(tx.TxIn))
	for _, txIn := range tx.TxIn {
		inputs[txIn.PreviousOutPoint] = struct{}{}
	}

	signed := make(map[wire.OutPoint]struct{}, len(outpoints))
	for _, outpoint := range outpoints {
		if _, ok := inputs[outpoint]; !ok {
			return false
		}

		signed[outpoint] = struct{}{}
	}

	// A duplicate outpoint would let through an input that wasn't signed.
	return len(signed) == len(outpoints) && len(inputs) == len(outpoints)
}
