package tapsdk

// BurnOptions configures optional parameters for high-level burn operations.
type BurnOptions struct {
	note string
}

// BurnOption configures optional parameters for Wallet.Burn.
type BurnOption func(*BurnOptions)

// WithBurnNote attaches user-defined metadata to a burn.
func WithBurnNote(note string) BurnOption {
	return func(o *BurnOptions) {
		o.note = note
	}
}

func applyBurnOptions(opts []BurnOption) *BurnOptions {
	o := &BurnOptions{}
	for _, opt := range opts {
		opt(o)
	}

	return o
}
