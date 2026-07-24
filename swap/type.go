package swap

// Type indicates the type of swap.
type Type uint8

const (
	// TypeIn is a loop in swap.
	TypeIn Type = iota

	// TypeOut is a loop out swap.
	TypeOut

	// TypeStaticAddressLoopIn is a static-address loop-in swap.
	TypeStaticAddressLoopIn
)

// IsOut returns true only if the swap is TypeOut; TypeIn and
// TypeStaticAddressLoopIn both return false.
func (t Type) IsOut() bool {
	return t == TypeOut
}

func (t Type) String() string {
	switch t {
	case TypeIn:
		return "In"
	case TypeOut:
		return "Out"
	case TypeStaticAddressLoopIn:
		return "StaticAddressLoopIn"
	default:
		return "Unknown"
	}
}
