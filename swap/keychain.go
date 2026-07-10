package swap

var (
	// KeyFamily is the key family used to generate keys that allow
	// spending of the htlc.
	KeyFamily = int32(99)

	// StaticAddressKeyFamily is the legacy static-address key family. It is
	// used for the V0 single static-address key and for static-address HTLC
	// keys.
	StaticAddressKeyFamily = int32(42060)

	// StaticMultiAddressKeyFamily is the key family used to generate
	// externally visible multi-address static-address receive keys.
	StaticMultiAddressKeyFamily = int32(42061)

	// StaticAddressChangeKeyFamily is the key family used to generate
	// static-address change outputs.
	StaticAddressChangeKeyFamily = int32(42062)
)
