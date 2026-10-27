package reservation

import "github.com/lightninglabs/loop/assets/sweep"

// Transfer keeps the existing tapd virtual packet validation while the
// reservation's Bitcoin contract uses the shared swap kit.
type Transfer = sweep.Transfer
