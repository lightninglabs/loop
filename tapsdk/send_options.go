package tapsdk

// SendOptions configures optional parameters for high-level send operations.
type SendOptions struct {
	// amount is the explicit send amount for V2 addresses that do not
	// embed one. Zero means "use the amount embedded in the address";
	// the validation in Wallet.Send still rejects an embedded-less V2
	// address with no caller-supplied amount.
	amount uint64

	// feeRate is the target fee rate for the anchor transaction. Zero uses
	// the daemon default.
	feeRate FeeRate

	// label is an optional short label for tracking the send.
	label string

	// skipProofCourierPingCheck skips the proof courier connectivity
	// check before sending. Useful when the courier is temporarily
	// unreachable but the send should proceed anyway.
	skipProofCourierPingCheck bool
}

// SendOption is a functional option for configuring Send and SendMulti.
type SendOption func(*SendOptions)

// WithAmount sets the explicit send amount for a V2 address that does not
// embed one. It is only meaningful for Wallet.Send: V0/V1 addresses and V2
// addresses with an embedded amount reject any value that does not match
// the embedded amount.
func WithAmount(amount uint64) SendOption {
	return func(o *SendOptions) {
		o.amount = amount
	}
}

// WithFeeRate sets the target fee rate for the anchor transaction.
func WithFeeRate(feeRate FeeRate) SendOption {
	return func(o *SendOptions) {
		o.feeRate = feeRate
	}
}

// WithLabel attaches a short label for tracking the send.
func WithLabel(label string) SendOption {
	return func(o *SendOptions) {
		o.label = label
	}
}

// WithSkipProofCourierPingCheck skips the proof courier connectivity check.
func WithSkipProofCourierPingCheck() SendOption {
	return func(o *SendOptions) {
		o.skipProofCourierPingCheck = true
	}
}

// applySendOptions merges functional options into a SendOptions struct.
func applySendOptions(opts []SendOption) *SendOptions {
	o := &SendOptions{}
	for _, opt := range opts {
		opt(o)
	}

	return o
}
