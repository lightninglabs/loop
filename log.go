package loop

import (
	"fmt"
	"os"

	"github.com/btcsuite/btclog"
	"github.com/lightningnetwork/lnd/lntypes"
)

// log is a logger that is initialized with no output filters.  This
// means the package will not perform any logging by default until the caller
// requests it.
var (
	backendLog     = btclog.NewBackend(logWriter{})
	logger         = backendLog.Logger("CLIENT")
	servicesLogger = backendLog.Logger("SERVICES")
)

// logWriter implements an io.Writer that outputs to both standard output and
// the write-end pipe of an initialized log rotator.
type logWriter struct{}

func (logWriter) Write(p []byte) (n int, err error) {
	os.Stdout.Write(p)
	return len(p), nil
}

// SwapLog logs with a short swap hash prefix.
type SwapLog struct {
	// Logger is the underlying based logger.
	Logger btclog.Logger

	// Hash is the hash the identifies the target swap.
	Hash lntypes.Hash
}

// Infof formats message according to format specifier and writes to
// log with LevelInfo.
func (s *SwapLog) Infof(format string, params ...interface{}) {
	s.Logger.Infof(
		fmt.Sprintf("%v %s", ShortHash(&s.Hash), format),
		params...,
	)
}

// Warnf formats message according to format specifier and writes to
// to log with LevelError.
func (s *SwapLog) Warnf(format string, params ...interface{}) {
	s.Logger.Warnf(
		fmt.Sprintf("%v %s", ShortHash(&s.Hash), format),
		params...,
	)
}

// Errorf formats message according to format specifier and writes to
// to log with LevelError.
func (s *SwapLog) Errorf(format string, params ...interface{}) {
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
