// +build !dev

package loopd

import "gopkg.in/macaroon-bakery.v2/bakery"

var (
	debugRequiredPermissions = map[string][]bakery.Op{}
	debugPermissions         []bakery.Op
)

// registerDebugServer is our default debug server registration function, which
// excludes debug functionality.
func (d *Daemon) registerDebugServer() {}
