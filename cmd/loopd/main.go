package main

import (
	"fmt"

	"github.com/lightninglabs/loop/loopd"
)

func main() {
	err := loopd.Start()
	if err != nil {
		fmt.Println(err)
	}
}
