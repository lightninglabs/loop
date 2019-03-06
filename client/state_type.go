package client

// SwapStateType defines the types of swap states that exist. Every swap state
// defined as type SwapState above, falls into one of these SwapStateType
// categories.
type SwapStateType uint8

const (
	// StateTypePending indicates that the swap is still pending.
	StateTypePending SwapStateType = iota

	// StateTypeSuccess indicates that the swap has completed successfully.
	StateTypeSuccess

	// StateTypeFail indicates that the swap has failed.
	StateTypeFail
)
