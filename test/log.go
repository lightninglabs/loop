package test

import (
	"os"

	"github.com/btcsuite/btclog"
)

// log is a logger that is initialized with no output filters.  This
// means the package will not perform any logging by default until the caller
// requests it.
var (
	backendLog = btclog.NewBackend(logWriter{})
	logger     = backendLog.Logger("TEST")
)

// logWriter implements an io.Writer that outputs to both standard output and
// the write-end pipe of an initialized log rotator.
type logWriter struct{}

func (logWriter) Write(p []byte) (int, error) {
	os.Stdout.Write(p)
	return len(p), nil
}
