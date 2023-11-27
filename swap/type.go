package swap

// Type indicates the type of swap.
type Type uint8

const (
	// TypeIn is a loop in swap.
	TypeIn Type = iota

	// TypeOut is a loop out swap.
	TypeOut
)

// IsOut returns true if the swap is a loop out swap, false if it is a loop in
// swap.
func (t Type) IsOut() bool {
	return t == TypeOut
}

func (t Type) String() string {
	switch t {
	case TypeIn:
		return "In"
	case TypeOut:
		return "Out"
	default:
		return "Unknown"
	}
}
