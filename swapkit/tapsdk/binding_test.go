package tapsdk

import (
	sdk "github.com/lightninglabs/tap-sdk"
)

func fixtureNodeInfo() *sdk.Info { return &sdk.Info{Version: "0.8.8", Network: "regtest"} }
