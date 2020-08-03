package labels

import (
	"errors"
)

const (
	// MaxLength is the maximum length we allow for labels.
	MaxLength = 500

	// Reserved is used as a prefix to separate labels that are created by
	// loopd from those created by users.
	Reserved = "[reserved]"
)

var (
	// ErrLabelTooLong is returned when a label exceeds our length limit.
	ErrLabelTooLong = errors.New("label exceeds maximum length")

	// ErrReservedPrefix is returned when a label contains the prefix
	// which is reserved for internally produced labels.
	ErrReservedPrefix = errors.New("label contains reserved prefix")
)

// Validate checks that a label is of appropriate length and is not in our list
// of reserved labels.
func Validate(label string) error {
	if len(label) > MaxLength {
		return ErrLabelTooLong
	}

	// If the label is shorter than our reserved prefix, it cannot contain
	// it.
	if len(label) < len(Reserved) {
		return nil
	}

	// Check if our label begins with our reserved prefix. We don't mind if
	// it has our reserved prefix in another case, we just need to be able
	// to reserve a subset of labels with this prefix.
	if label[0:len(Reserved)] == Reserved {
		return ErrReservedPrefix
	}

	return nil
}
