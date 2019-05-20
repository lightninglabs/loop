package loopdb

import (
	"os"

	"github.com/btcsuite/btclog"
)

var (
	backendLog = btclog.NewBackend(logWriter{})
	log        = backendLog.Logger("STORE")
)

// logWriter implements an io.Writer that outputs to both standard output and
// the write-end pipe of an initialized log rotator.
type logWriter struct{}

func (logWriter) Write(p []byte) (n int, err error) {
	os.Stdout.Write(p)
	return len(p), nil
}
