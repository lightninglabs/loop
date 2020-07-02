package loopdb

import (
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/coreos/bbolt"
)

// migrateVersionNativeSegwit is a no-op migration used to bump the database
// version after the addition of support for native segwit loop in swaps. This
// version bump is added to prevent clients from downgrading after creating
// native segwit swaps, because the loopd daemon will no longer recognize its
// own native segwit swaps if the client downgrades with a native segwit swap
// in progress.
func migrateVersionNativeSegwit(_ *bbolt.Tx, _ *chaincfg.Params) error {
	return nil
}
