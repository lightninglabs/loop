package outpoint

import "github.com/btcsuite/btcd/wire"

// FirstDuplicate returns the first repeated outpoint in the given list.
func FirstDuplicate(outpoints []wire.OutPoint) (wire.OutPoint, bool) {
	seen := make(map[wire.OutPoint]struct{}, len(outpoints))
	for _, outpoint := range outpoints {
		if _, ok := seen[outpoint]; ok {
			return outpoint, true
		}

		seen[outpoint] = struct{}{}
	}

	return wire.OutPoint{}, false
}

// HasDuplicates returns true if the given list contains a repeated outpoint.
func HasDuplicates(outpoints []wire.OutPoint) bool {
	_, ok := FirstDuplicate(outpoints)

	return ok
}
