package swap

// Type indicates the type of swap.
type Type uint8

const (
	// TypeIn is a loop in swap.
	TypeIn Type = iota

	// TypeOut is a loop out swap.
	TypeOut
)

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
