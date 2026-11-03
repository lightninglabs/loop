package tapsdk

import (
	"errors"
)

// ErrSigningRequest means an SDK signing request failed its fixed binding.
var ErrSigningRequest = errors.New("invalid asset signing request")
