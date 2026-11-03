package swapkit

import (
	"errors"
)

var (
	ErrInvalid = errors.New("invalid asset transaction")
	ErrPending = errors.New("asset transaction pending")
)
