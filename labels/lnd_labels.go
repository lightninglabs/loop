package labels

import "fmt"

const (
	// loopdLabelPattern is the pattern that loop uses to label on-chain
	// transactions in the lnd backend.
	loopdLabelPattern = "loopd -- %s(swap=%s)"

	// loopOutSweepSuccess is the label used for loop out swaps to sweep
	// the HTLC in the success case.
	loopOutSweepSuccess = "OutSweepSuccess"

	// loopInHtlc is the label used for loop in swaps to publish an HTLC.
	loopInHtlc = "InHtlc"

	// loopInTimeout is the label used for loop in swaps to sweep an HTLC
	// that has timed out.
	loopInSweepTimeout = "InSweepTimeout"
)

// LoopOutSweepSuccess returns the label used for loop out swaps to sweep the
// HTLC in the success case.
func LoopOutSweepSuccess(swapHash string) string {
	return fmt.Sprintf(loopdLabelPattern, loopOutSweepSuccess, swapHash)
}

// LoopInHtlcLabel returns the label used for loop in swaps to publish an HTLC.
func LoopInHtlcLabel(swapHash string) string {
	return fmt.Sprintf(loopdLabelPattern, loopInHtlc, swapHash)
}

// LoopInSweepTimeout returns the label used for loop in swaps to sweep an HTLC
// that has timed out.
func LoopInSweepTimeout(swapHash string) string {
	return fmt.Sprintf(loopdLabelPattern, loopInSweepTimeout, swapHash)
}
