package reservation

import (
	"errors"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/chaincfg"
)

const (
	// MainnetMinCSVDelay is the shortest reservation CSV delay a mainnet
	// client accepts, about one week of blocks.
	MainnetMinCSVDelay uint32 = 1008

	// MainnetMinRequiredConfirmations is the shallowest funding depth a
	// mainnet client accepts before treating a reservation as Ready.
	MainnetMinRequiredConfirmations uint32 = 3

	// MainnetMinExecutionDelta is the smallest margin a mainnet client
	// accepts between the execution cutoff and CSV maturity, leaving a
	// later swap about half a day to complete.
	MainnetMinExecutionDelta uint32 = 72

	// MainnetMinUsableBlocks is the smallest number of blocks a mainnet
	// client accepts between reaching the required depth and the
	// execution cutoff, about one day.
	MainnetMinUsableBlocks uint32 = 144
)

// LifetimeFloor bounds the lifetime a client accepts in a server quote. Terms
// validation only checks that the quoted values are consistent with each
// other, so without a floor a server could quote a reservation that is usable
// for a single block. Bounding each value alone is not enough: a long CSV
// delay can be consumed by a large confirmation depth or execution delta, so
// the floor also bounds the usable window between them. The zero value
// accepts any consistent terms.
type LifetimeFloor struct {
	// MinCSVDelay is the smallest accepted CSV delay.
	MinCSVDelay uint32

	// MinRequiredConfirmations is the smallest accepted funding depth.
	MinRequiredConfirmations uint32

	// MinExecutionDelta is the smallest accepted margin between the
	// execution cutoff and CSV maturity.
	MinExecutionDelta uint32

	// MinUsableBlocks is the smallest accepted number of blocks between
	// reaching the required funding depth and the execution cutoff.
	MinUsableBlocks uint32
}

// LifetimeFloorForChain returns the floor loopd applies on the given network.
// Only mainnet is bounded. Test networks commonly quote short lifetimes, where
// waiting for mainnet-sized delays is impractical.
func LifetimeFloorForChain(params *chaincfg.Params) LifetimeFloor {
	if params == nil || params.Net != chaincfg.MainNetParams.Net {
		return LifetimeFloor{}
	}
	return LifetimeFloor{
		MinCSVDelay:              MainnetMinCSVDelay,
		MinRequiredConfirmations: MainnetMinRequiredConfirmations,
		MinExecutionDelta:        MainnetMinExecutionDelta,
		MinUsableBlocks:          MainnetMinUsableBlocks,
	}
}

// Check rejects quoted terms whose lifetime falls below the floor. The terms
// must already be structurally valid.
func (f LifetimeFloor) Check(t Terms) error {
	if err := t.Validate(); err != nil {
		return err
	}
	switch {
	case t.CSVDelay < f.MinCSVDelay:
		return fmt.Errorf("quoted csv delay %d is below the minimum %d",
			t.CSVDelay, f.MinCSVDelay)

	case t.RequiredConfirmations < f.MinRequiredConfirmations:
		return fmt.Errorf("quoted confirmation depth %d is below the "+
			"minimum %d", t.RequiredConfirmations,
			f.MinRequiredConfirmations)

	case t.ExecutionDelta < f.MinExecutionDelta:
		return fmt.Errorf("quoted execution delta %d is below the "+
			"minimum %d", t.ExecutionDelta, f.MinExecutionDelta)
	}

	// Validate guarantees that the confirmation depth and execution delta
	// fit within the CSV delay, so this cannot underflow.
	usable := t.CSVDelay - t.RequiredConfirmations - t.ExecutionDelta
	if usable < f.MinUsableBlocks {
		return fmt.Errorf("quoted lifetime leaves %d usable blocks, "+
			"below the minimum %d", usable, f.MinUsableBlocks)
	}
	return nil
}

// Lifetime contains absolute block heights derived from the first funding
// confirmation. Restart and extra confirmations never reset these heights.
type Lifetime struct {
	// TimeoutHeight is the first block that may contain a CSV timeout spend.
	TimeoutHeight uint32

	// ExecutionCutoff is exclusive: no new swap may start at this height.
	ExecutionCutoff uint32
}

// Lifetime derives the reservation's deadlines using block-based CSV rules.
// Heights must fit the signed block-height fields used by LND.
func (t Terms) Lifetime(confirmationHeight uint32) (Lifetime, error) {
	if err := t.Validate(); err != nil {
		return Lifetime{}, err
	}
	expiry := uint64(confirmationHeight) + uint64(t.CSVDelay)
	if confirmationHeight == 0 || expiry > math.MaxInt32 {
		return Lifetime{}, errors.New("invalid funding confirmation height")
	}

	return Lifetime{
		TimeoutHeight:   uint32(expiry),
		ExecutionCutoff: uint32(expiry) - t.ExecutionDelta,
	}, nil
}
