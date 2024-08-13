#!/usr/bin/env bash
go run ./fsm/stateparser/stateparser.go --out ./fsm/example_fsm.md --fsm example
go run ./fsm/stateparser/stateparser.go --out ./instantout/reservation/reservation_fsm.md --fsm reservation
go run ./fsm/stateparser/stateparser.go --out ./instantout/fsm.md --fsm instantout
go run ./fsm/stateparser/stateparser.go --out ./hyperloop/fsm.md --fsm hyperloop