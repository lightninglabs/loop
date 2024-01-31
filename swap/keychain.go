package swap

var (
	// KeyFamily is the key family used to generate keys that allow
	// spending of the htlc.
	KeyFamily = int32(99)

	// StaticAddressKeyFamily is the key family used to generate static
	// address keys.
	StaticAddressKeyFamily = int32(42060)
)
