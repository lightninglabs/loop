package swap

import (
	"fmt"

	"github.com/btcsuite/btclog"
	"github.com/lightningnetwork/lnd/lntypes"
)

// PrefixLog logs with a short swap hash prefix.
type PrefixLog struct {
	// Logger is the underlying based logger.
	Logger btclog.Logger

	// Hash is the hash the identifies the target swap.
	Hash lntypes.Hash
}

// Infof formats message according to format specifier and writes to
// log with LevelInfo.
func (s *PrefixLog) Infof(format string, params ...interface{}) {
	s.Logger.Infof(
		fmt.Sprintf("%v %s", ShortHash(&s.Hash), format),
		params...,
	)
}

// Warnf formats message according to format specifier and writes to log with
// LevelError.
func (s *PrefixLog) Warnf(format string, params ...interface{}) {
	s.Logger.Warnf(
		fmt.Sprintf("%v %s", ShortHash(&s.Hash), format),
		params...,
	)
}

// Errorf formats message according to format specifier and writes to log with
// LevelError.
func (s *PrefixLog) Errorf(format string, params ...interface{}) {
	s.Logger.Errorf(
		fmt.Sprintf("%v %s", ShortHash(&s.Hash), format),
		params...,
	)
}

// ShortHash returns a shortened version of the hash suitable for use in
// logging.
func ShortHash(hash *lntypes.Hash) string {
	return hash.String()[:6]
}
