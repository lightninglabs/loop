package main

import (
	"fmt"

	"github.com/lightninglabs/loop/loopd"
)

func main() {
	cfg := loopd.RPCConfig{}
	err := loopd.Start(cfg)
	if err != nil {
		fmt.Printf("loopd exited with an error: %v\n", err)
	}
}
