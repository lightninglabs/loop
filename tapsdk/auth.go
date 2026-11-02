package tapsdk

import "github.com/lightninglabs/loop/tapsdk/macaroon"

// MacaroonSource describes where the SDK should load tapd authentication
// macaroons from.
type MacaroonSource = macaroon.Source

// MacaroonFromPath loads a single macaroon file and reuses it for every tapd
// service.
func MacaroonFromPath(path string) MacaroonSource {
	return macaroon.FromPath(path)
}

// MacaroonFromDir loads one macaroon per tapd service from a directory.
func MacaroonFromDir(dir string) MacaroonSource {
	return macaroon.FromDir(dir)
}

// MacaroonFromHex uses one hex-encoded macaroon for every tapd service.
func MacaroonFromHex(hex string) MacaroonSource {
	return macaroon.FromHex(hex)
}
