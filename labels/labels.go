package labels

import (
	"errors"
	"fmt"
	"strings"

	"github.com/lightninglabs/loop/swap"
)

const (
	// MaxLength is the maximum length we allow for labels.
	MaxLength = 500

	// Reserved is used as a prefix to separate labels that are created by
	// loopd from those created by users.
	Reserved = "[reserved]"

	// autoOut is the label used for loop out swaps that are automatically
	// dispatched.
	autoOut = "autoloop-out"

	// autoIn is the label used for loop in swaps that are automatically
	// dispatched.
	autoIn = "autoloop-in"

	// easyAutoOut is the label used for easy loop out swaps that are
	// automatically dispatched.
	easyAutoOut = "easy-autoloop-out"

	// easyAutoIn is the label used for easy loop in swaps that are
	// automatically dispatched.
	easyAutoIn = "easy-autoloop-in"
)

var (
	// ErrLabelTooLong is returned when a label exceeds our length limit.
	ErrLabelTooLong = errors.New("label exceeds maximum length")

	// ErrReservedPrefix is returned when a label contains the prefix
	// which is reserved for internally produced labels.
	ErrReservedPrefix = errors.New("label contains reserved prefix")
)

// AutoloopLabel returns a label with the reserved prefix that identifies
// automatically dispatched swaps depending on the type of swap being executed.
func AutoloopLabel(swapType swap.Type) string {
	if swapType == swap.TypeOut {
		return fmt.Sprintf("%v: %v", Reserved, autoOut)
	}

	return fmt.Sprintf("%v: %v", Reserved, autoIn)
}

// EasyAutoloopLabel returns a label with the reserved prefix that identifies
// automatically dispatched swaps depending on the type of swap being executed.
func EasyAutoloopLabel(swapType swap.Type) string {
	if swapType == swap.TypeOut {
		return fmt.Sprintf("%v: %v", Reserved, easyAutoOut)
	}

	return fmt.Sprintf("%v: %v", Reserved, easyAutoIn)
}

// Validate checks that a label is of appropriate length and is not in our list
// of reserved labels.
func Validate(label string) error {
	if len(label) > MaxLength {
		return ErrLabelTooLong
	}

	// Check if our label begins with our reserved prefix. We don't mind if
	// it has our reserved prefix in another case, we just need to be able
	// to reserve a subset of labels with this prefix.
	if strings.HasPrefix(label, Reserved) {
		return ErrReservedPrefix
	}

	return nil
}
