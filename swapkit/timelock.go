package swapkit

import (
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/txscript"
)

// ErrTimelockOrder reports heights that leave a party without a safe exit.
var ErrTimelockOrder = errors.New("unsafe timelock ordering")

// MinExecutionDelta is the smallest window in which a fallback HTLC can
// confirm and its success leaf can then satisfy the one-block CSV.
const MinExecutionDelta = 2

// LoopInTimelocks contains the heights that decide whether the server of an
// asset Loop In can still claim the fallback HTLC after paying the invoice. It
// is a point-in-time check: build it from the current tip immediately before
// each release of authority, and never reuse an earlier result.
type LoopInTimelocks struct {
	// Tip is the current best block height.
	Tip uint32

	// DepositHeight is the confirmation height of the deposit output.
	DepositHeight uint32

	// DepositCSV is the relative delay of the client's deposit refund.
	DepositCSV uint32

	// HTLCExpiry is the absolute height of the client's HTLC refund.
	HTLCExpiry uint32

	// RecoveryDelta is the number of blocks the deposit refund must stay
	// locked after HTLCExpiry, so the client has no competing spend of the
	// deposit while the HTLC is still claimable.
	RecoveryDelta uint32

	// ExecutionDelta is the number of blocks the server needs to confirm
	// the fallback and its preimage claim.
	ExecutionDelta uint32
}

// Validate checks that the deposit refund opens only after the HTLC refund
// and that the server can still confirm the fallback and claim it from the
// current tip. Call ValidatePayment as well before paying the invoice.
func (t LoopInTimelocks) Validate() error {
	if err := validateSwapCSV(t.DepositCSV); err != nil {
		return err
	}
	if err := validateHeight("HTLC expiry", t.HTLCExpiry); err != nil {
		return err
	}
	if t.DepositHeight == 0 || t.Tip < t.DepositHeight {
		return fmt.Errorf("%w: deposit is not confirmed at the tip",
			ErrTimelockOrder)
	}
	if t.ExecutionDelta < MinExecutionDelta {
		return fmt.Errorf("%w: execution delta must be at least %d",
			ErrTimelockOrder, MinExecutionDelta)
	}

	// The client's deposit refund must stay locked until the HTLC refund
	// has been open for the whole recovery window.
	maturity := uint64(t.DepositHeight) + uint64(t.DepositCSV)
	if maturity <= uint64(t.HTLCExpiry)+uint64(t.RecoveryDelta) {
		return fmt.Errorf("%w: deposit refund at %d opens within %d "+
			"blocks of HTLC expiry %d", ErrTimelockOrder, maturity,
			t.RecoveryDelta, t.HTLCExpiry)
	}

	// The server must be able to confirm the fallback and its claim before
	// the client's HTLC refund opens.
	if uint64(t.Tip)+uint64(t.ExecutionDelta) > uint64(t.HTLCExpiry) {
		return fmt.Errorf("%w: tip %d leaves fewer than %d blocks "+
			"before HTLC expiry %d", ErrTimelockOrder, t.Tip,
			t.ExecutionDelta, t.HTLCExpiry)
	}

	return nil
}

// ValidatePayment additionally bounds the server's outgoing payment. The
// payment expiry is the highest absolute CLTV of any HTLC the server offers
// for the swap invoice, which is the last height at which it can still learn
// the preimage. The server needs the execution window after that height.
func (t LoopInTimelocks) ValidatePayment(paymentExpiry uint32) error {
	if err := t.Validate(); err != nil {
		return err
	}
	err := validateHeight("payment expiry", paymentExpiry)
	if err != nil {
		return err
	}
	if paymentExpiry < t.Tip {
		return fmt.Errorf("%w: payment expiry %d is below tip %d",
			ErrTimelockOrder, paymentExpiry, t.Tip)
	}
	if uint64(paymentExpiry)+uint64(t.ExecutionDelta) >
		uint64(t.HTLCExpiry) {

		return fmt.Errorf("%w: payment expiry %d leaves fewer than %d "+
			"blocks before HTLC expiry %d", ErrTimelockOrder,
			paymentExpiry, t.ExecutionDelta, t.HTLCExpiry)
	}

	return nil
}

// LoopOutTimelocks contains the heights that decide whether the client of an
// asset Loop Out can still confirm its fallback HTLC and claim it, and whether
// the server can still settle its incoming payment afterwards. It is a
// point-in-time check: build it from the current tip immediately before each
// release of authority, and never reuse an earlier result.
type LoopOutTimelocks struct {
	// Tip is the current best block height.
	Tip uint32

	// ReservationExpiry is the absolute height of the server's reservation
	// refund.
	ReservationExpiry uint32

	// HTLCExpiry is the absolute height of the server's HTLC refund.
	HTLCExpiry uint32

	// ConfirmationDelta is the number of blocks the client allows for the
	// fallback, which spends the reservation, to confirm.
	ConfirmationDelta uint32

	// ClaimDelta is the number of blocks the client needs to confirm its
	// preimage claim once the fallback has confirmed.
	ClaimDelta uint32

	// SettleDelta is the number of blocks the server needs after HTLCExpiry
	// to learn a preimage revealed on chain and settle its invoice.
	SettleDelta uint32
}

// Validate checks that the fallback can confirm before the server's
// reservation refund opens, and that the client can then claim the HTLC before
// the server's HTLC refund opens. The client calls it before paying the swap
// invoice, before signing, and before revealing the preimage to the server.
func (t LoopOutTimelocks) Validate() error {
	err := validateHeight("reservation expiry", t.ReservationExpiry)
	if err != nil {
		return err
	}
	if err := validateHeight("HTLC expiry", t.HTLCExpiry); err != nil {
		return err
	}
	if t.Tip == 0 || t.ConfirmationDelta == 0 || t.ClaimDelta == 0 {
		return fmt.Errorf("%w: tip, confirmation delta and claim "+
			"delta are required", ErrTimelockOrder)
	}

	// The fallback spends the reservation, so it must confirm before the
	// server's reservation refund can compete with it.
	confirmed := uint64(t.Tip) + uint64(t.ConfirmationDelta)
	if confirmed > uint64(t.ReservationExpiry) {
		return fmt.Errorf("%w: tip %d leaves fewer than %d blocks "+
			"before reservation expiry %d", ErrTimelockOrder, t.Tip,
			t.ConfirmationDelta, t.ReservationExpiry)
	}

	// The preimage claim must confirm before the server's HTLC refund can
	// compete with it.
	if confirmed+uint64(t.ClaimDelta) > uint64(t.HTLCExpiry) {
		return fmt.Errorf("%w: tip %d leaves fewer than %d blocks "+
			"before HTLC expiry %d", ErrTimelockOrder, t.Tip,
			uint64(t.ConfirmationDelta)+uint64(t.ClaimDelta),
			t.HTLCExpiry)
	}

	return nil
}

// ValidatePayment bounds the server's incoming payment. The payment expiry is
// the lowest absolute CLTV of any HTLC accepted for the swap invoice. The
// client can reveal the preimage on chain until the HTLC expires, so the
// server must be able to settle for the whole settle window after that height.
// The server calls it before it signs the fallback.
func (t LoopOutTimelocks) ValidatePayment(paymentExpiry uint32) error {
	if err := validateHeight("HTLC expiry", t.HTLCExpiry); err != nil {
		return err
	}
	err := validateHeight("payment expiry", paymentExpiry)
	if err != nil {
		return err
	}
	if t.SettleDelta == 0 {
		return fmt.Errorf("%w: settle delta is required",
			ErrTimelockOrder)
	}
	if uint64(t.HTLCExpiry)+uint64(t.SettleDelta) > uint64(paymentExpiry) {
		return fmt.Errorf("%w: payment expiry %d is within %d blocks "+
			"of HTLC expiry %d", ErrTimelockOrder, paymentExpiry,
			t.SettleDelta, t.HTLCExpiry)
	}

	return nil
}

func validateHeight(name string, height uint32) error {
	if height == 0 || height >= txscript.LockTimeThreshold {
		return fmt.Errorf("%w: %s must be a block height",
			ErrTimelockOrder, name)
	}
	return nil
}
