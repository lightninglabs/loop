package main

import (
	"fmt"
	"os"

	"github.com/lightninglabs/loop/loopd"
)

func main() {
	cfg := loopd.RPCConfig{}
	err := loopd.Run(cfg)
	if err != nil {
		fmt.Printf("loopd exited with an error: %v\n", err)
		os.Exit(1)
	}
}
