#!/usr/bin/env bash
set -euo pipefail

go run ./fsm/stateparser/stateparser.go --out ./fsm/example_fsm.md --fsm example
go run ./fsm/stateparser/stateparser.go --out ./instantout/reservation/reservation_fsm.md --fsm reservation
go run ./fsm/stateparser/stateparser.go --out ./instantout/fsm.md --fsm instantout
go run ./fsm/stateparser/stateparser.go --out ./staticaddr/deposit/fsm.md --fsm staticaddr-deposit
go run ./fsm/stateparser/stateparser.go --out ./staticaddr/loopin/fsm.md --fsm staticaddr-loopin
