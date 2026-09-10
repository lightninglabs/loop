package reservation

import (
	"crypto/sha256"
	"errors"
	"math/big"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/swapserverrpc"
	"google.golang.org/protobuf/proto"
)

// ProbeOutcome describes reachability, not a promise of payment success.
type ProbeOutcome uint8

const (
	ProbeNotRun ProbeOutcome = iota
	ProbeSucceeded
	ProbeFailed
	ProbeTimedOut
	ProbeUnsupported
)

// ProbeResults retains the main probe and fee estimates shown before approval.
type ProbeResults struct {
	// Main reports whether the estimated swap payment reached its destination.
	Main ProbeOutcome

	// PrepayFeeMsat scales MainFeeMsat by the BTC payment amounts, rounded up.
	// It ignores fixed hop fees; availability requires success and FeeKnown.
	PrepayFeeMsat uint64

	// FeeKnown distinguishes a captured route fee from missing history.
	FeeKnown bool

	// MainFeeMsat is the most recently observed in-flight route's fee.
	// It is an estimate; LND may retry with a different route.
	MainFeeMsat uint64

	// CheckedAt records when the main probe completed.
	CheckedAt time.Time
}

// QuoteHash identifies the exact quote the user approved.
func QuoteHash(q *swapserverrpc.AssetReservationQuote) ([32]byte, error) {
	if q == nil {
		return [32]byte{}, errors.New("missing reservation quote")
	}
	data, err := (proto.MarshalOptions{
		Deterministic: true,
	}).Marshal(q)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(data), nil
}

// TermsFromRPC checks a server offer without imposing client pricing policy.
func TermsFromRPC(t *swapserverrpc.AssetReservationTerms) (Terms, error) {
	if t == nil || len(t.AssetId) != 32 {
		return Terms{}, errors.New("invalid reservation terms")
	}
	terms := Terms{
		AssetID:               [32]byte(t.AssetId),
		Amount:                t.Amount,
		Fee:                   t.Fee,
		CSVDelay:              t.CsvDelay,
		RequiredConfirmations: t.RequiredConfirmations,
		ExecutionDelta:        t.ExecutionDelta,
	}
	return terms, terms.Validate()
}

// ParseOutpoint accepts exactly the canonical txid:vout representation.
func ParseOutpoint(value string) (*wire.OutPoint, error) {
	outpoint, err := wire.NewOutPointFromString(value)
	if err != nil || outpoint.String() != value {
		return nil, errors.New("invalid reservation outpoint")
	}
	return outpoint, nil
}

// estimatePrepayRoutingFee scales the main fee by the BTC payment amounts.
// It rounds up to a millisatoshi and deliberately ignores fixed hop fees.
func estimatePrepayRoutingFee(mainFee, prepayAmount, mainAmount uint64) (
	uint64, error) {

	if prepayAmount == 0 || mainAmount == 0 {
		return 0, errors.New("missing amount for prepay fee estimate")
	}
	// Multiply before dividing, using a wide integer so large valid quotes
	// cannot overflow or lose precision through floating-point arithmetic.
	fee := new(big.Int).SetUint64(mainFee)
	fee.Mul(fee, new(big.Int).SetUint64(prepayAmount))
	fee.Add(fee, new(big.Int).SetUint64(mainAmount-1))
	fee.Quo(fee, new(big.Int).SetUint64(mainAmount))
	if !fee.IsInt64() {
		return 0, errors.New("prepay routing fee estimate is out of range")
	}
	return fee.Uint64(), nil
}
