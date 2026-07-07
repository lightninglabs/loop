package deposit

import (
	"fmt"

	"github.com/btcsuite/btcd/wire"
)

// CheckDuplicates returns an error if the outpoint list contains duplicates.
func CheckDuplicates(outpoints []wire.OutPoint) error {
	seen := make(map[wire.OutPoint]struct{}, len(outpoints))
	for _, outpoint := range outpoints {
		if _, ok := seen[outpoint]; ok {
			return fmt.Errorf("duplicate outpoint %v", outpoint)
		}

		seen[outpoint] = struct{}{}
	}

	return nil
}
