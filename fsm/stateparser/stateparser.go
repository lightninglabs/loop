package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/instantout"
	"github.com/lightninglabs/loop/instantout/reservation"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/loopin"
)

var errInvalidFSMSelector = errors.New("missing or unknown fsm selector")

func main() {
	if err := run(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func run() error {
	out := flag.String("out", "", "outfile")
	stateMachine := flag.String("fsm", "", "the swap state machine to parse")
	flag.Parse()

	if filepath.Ext(*out) != ".md" {
		return errors.New("wrong argument: out must be a .md file")
	}

	fp, err := filepath.Abs(*out)
	if err != nil {
		return err
	}

	states, err := getStates(*stateMachine)
	if err != nil {
		return err
	}

	return writeMermaidFile(fp, states)
}

func getStates(stateMachine string) (fsm.States, error) {
	switch stateMachine {
	case "example":
		exampleFSM := &fsm.ExampleFSM{}
		return exampleFSM.GetStates(), nil

	case "reservation":
		reservationFSM := &reservation.FSM{}
		return reservationFSM.GetServerInitiatedReservationStates(), nil

	case "instantout":
		instantOutFSM := &instantout.FSM{}
		return instantOutFSM.GetV1ReservationStates(), nil

	case "staticaddr-deposit":
		depositFSM := &deposit.FSM{}
		return depositFSM.DepositStatesV0(), nil

	case "staticaddr-loopin":
		loopInFSM := &loopin.FSM{}
		return loopInFSM.LoopInStatesV0(), nil

	default:
		return nil, fmt.Errorf(
			"%w %q; supported selectors: example, instantout, "+
				"reservation, staticaddr-deposit, staticaddr-loopin",
			errInvalidFSMSelector, stateMachine,
		)
	}
}

func writeMermaidFile(filename string, states fsm.States) error {
	f, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer f.Close()

	var b bytes.Buffer
	fmt.Fprint(&b, "```mermaid\nstateDiagram-v2\n")

	sortedStates := sortedKeys(states)
	for _, state := range sortedStates {
		edges := states[fsm.StateType(state)]
		// write state name
		if len(state) > 0 {
			fmt.Fprintf(&b, "%s\n", state)
		} else {
			state = "[*]"
		}
		// write transitions
		for _, edge := range sortedTransitionKeys(edges.Transitions) {
			fmt.Fprintf(
				&b, "%s --> %s: %s\n", state,
				edges.Transitions[fsm.EventType(edge)], edge,
			)
		}
	}

	fmt.Fprint(&b, "```")
	_, err = f.Write(b.Bytes())
	if err != nil {
		return err
	}

	return nil
}

func sortedKeys(m fsm.States) []string {
	keys := make([]string, len(m))
	i := 0
	for k := range m {
		keys[i] = string(k)
		i++
	}
	sort.Strings(keys)
	return keys
}

func sortedTransitionKeys(m fsm.Transitions) []string {
	keys := make([]string, len(m))
	i := 0
	for k := range m {
		keys[i] = string(k)
		i++
	}
	sort.Strings(keys)
	return keys
}
