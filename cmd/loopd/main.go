package main

import (
	"fmt"

	"github.com/lightninglabs/loop/loopd"
)

func main() {
	cfg := loopd.RPCConfig{}
	err := loopd.Start(cfg)
	if err != nil {
		fmt.Println(err)
	}
}
