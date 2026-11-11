package swapkit

import (
	"errors"
)

var (
	ErrInvalid = errors.New("invalid asset transaction")
	ErrPending = errors.New("asset transaction pending")
)

// CreateDepositRequest binds idempotent deposit funding to wallet key locators.
//
//nolint:tagliatelle // Preserve the existing recovery and wire format.
type CreateDepositRequest struct {
	RequestID [32]byte `json:"RequestID"`
	GroupKey  [33]byte `json:"GroupKey"`
	Amount    uint64   `json:"Amount"`
	// #nosec G117 -- ClientKey is a public Bitcoin key.
	ClientKey [33]byte `json:"ClientKey"`
	ServerKey [33]byte `json:"ServerKey"`
	CSV       uint32   `json:"CSV"`
	KeyFamily int32    `json:"KeyFamily"`
	KeyIndex  uint32   `json:"KeyIndex"`
}
