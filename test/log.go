package test

import (
	"github.com/lightningnetwork/lnd/build"
)

// log is a logger that is initialized with no output filters.  This
// means the package will not perform any logging by default until the caller
// requests it.
var (
	logger = build.NewSubLogger("TEST", nil)
)
