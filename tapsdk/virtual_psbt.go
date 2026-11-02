package tapsdk

import (
	"fmt"
	"math"
	"time"
)

const maxCustomAnchorLockExpirationSeconds = uint64(
	math.MaxInt64 / int64(time.Second),
)

// AnchorChangeOutputMode identifies how anchor change should be handled while
// funding an anchor PSBT.
type AnchorChangeOutputMode uint8

const (
	// AnchorChangeOutputUnspecified leaves change-output selection unset.
	AnchorChangeOutputUnspecified AnchorChangeOutputMode = iota

	// AnchorChangeOutputAdd asks the backend to add a new change output when
	// needed.
	AnchorChangeOutputAdd

	// AnchorChangeOutputExisting asks the backend to use an existing output as
	// the change output.
	AnchorChangeOutputExisting

	// AnchorChangeOutputNoNew asks the backend not to add a new change output
	// when possible.
	AnchorChangeOutputNoNew
)

// AnchorChangeOutput describes how anchor change should be handled.
type AnchorChangeOutput struct {
	// Mode selects the change-output behavior.
	Mode AnchorChangeOutputMode

	// ExistingOutputIndex is the output index to use when Mode is
	// AnchorChangeOutputExisting.
	ExistingOutputIndex int32
}

// AnchorFeeMode identifies how anchor transaction fees should be selected.
type AnchorFeeMode uint8

const (
	// AnchorFeeUnspecified leaves fee selection unset.
	AnchorFeeUnspecified AnchorFeeMode = iota

	// AnchorFeeSatPerVByte uses an explicit sat/vB fee rate.
	AnchorFeeSatPerVByte

	// AnchorFeeTargetConf lets the backend estimate fees for a confirmation
	// target.
	AnchorFeeTargetConf
)

// AnchorFee describes how anchor transaction fees should be selected.
type AnchorFee struct {
	// Mode selects the fee behavior.
	Mode AnchorFeeMode

	// FeeRate is used when Mode is AnchorFeeSatPerVByte.
	FeeRate FeeRate

	// TargetConf is used when Mode is AnchorFeeTargetConf.
	TargetConf uint32
}

// AnchorFundingPlan describes backend funding options for an anchor PSBT.
type AnchorFundingPlan struct {
	// ChangeOutput selects how the backend handles anchor change.
	ChangeOutput AnchorChangeOutput

	// Fee selects the anchor fee strategy.
	Fee AnchorFee

	// CustomLockID optionally identifies lnd UTXO locks created while funding.
	CustomLockID []byte

	// LockExpirationSeconds optionally overrides the backend lock duration.
	LockExpirationSeconds uint64

	// SkipFunding skips backend PSBT funding and only commits asset outputs.
	SkipFunding bool
}

// CommitVirtualPsbtsRequest commits virtual PSBTs into an anchor PSBT template
// with V1 transition proofs.
type CommitVirtualPsbtsRequest struct {
	// AnchorPsbt is the caller-supplied BTC-level anchor PSBT template.
	AnchorPsbt []byte

	// VirtualPsbts are the active virtual asset PSBTs to commit.
	VirtualPsbts [][]byte

	// PassiveAssetPsbts are passive virtual asset PSBTs to commit alongside the
	// active PSBTs.
	PassiveAssetPsbts [][]byte

	// Funding controls backend anchor funding.
	Funding AnchorFundingPlan
}

// Validate checks that a virtual PSBT commit request can be sent.
func (r *CommitVirtualPsbtsRequest) Validate() error {
	if r == nil {
		return fmt.Errorf("nil commit virtual PSBTs request")
	}
	if len(r.AnchorPsbt) == 0 {
		return fmt.Errorf("anchor PSBT is required")
	}
	if len(r.VirtualPsbts) == 0 {
		return fmt.Errorf("at least one virtual PSBT is required")
	}
	if err := validateCustomAnchorLockID(r.Funding.CustomLockID); err != nil {
		return err
	}
	if err := validateCustomAnchorLockExpiration(
		r.Funding.LockExpirationSeconds,
	); err != nil {
		return err
	}

	if r.Funding.SkipFunding {
		return nil
	}

	if err := r.Funding.ChangeOutput.validate(); err != nil {
		return err
	}

	return r.Funding.Fee.validate()
}

func validateCustomAnchorLockID(lockID []byte) error {
	if len(lockID) != 0 && len(lockID) != 32 {
		return fmt.Errorf("custom lock ID must be exactly 32 bytes")
	}

	return nil
}

func validateCustomAnchorLockExpiration(seconds uint64) error {
	if seconds > maxCustomAnchorLockExpirationSeconds {
		return fmt.Errorf("lock expiration exceeds the maximum safe duration")
	}

	return nil
}

// CommitVirtualPsbtsResponse is the result of committing virtual PSBTs into an
// anchor PSBT template.
type CommitVirtualPsbtsResponse struct {
	// AnchorPsbt is the funded BTC-level anchor PSBT with asset commitments.
	AnchorPsbt []byte

	// VirtualPsbts are the updated active virtual asset PSBTs.
	VirtualPsbts [][]byte

	// PassiveAssetPsbts are the updated passive virtual asset PSBTs.
	PassiveAssetPsbts [][]byte

	// ChangeOutputIndex is the index of the anchor change output or -1 if no
	// change output was added.
	ChangeOutputIndex int32

	// LockedUTXOs are the lnd wallet outpoints locked while funding the anchor
	// transaction.
	LockedUTXOs []Outpoint
}

// PublishAndLogTransferRequest publishes a finalized anchor PSBT and logs the
// corresponding asset transfer.
type PublishAndLogTransferRequest struct {
	// AnchorPsbt is the finalized BTC-level anchor PSBT.
	AnchorPsbt []byte

	// VirtualPsbts are the active virtual asset PSBTs committed to the anchor.
	VirtualPsbts [][]byte

	// PassiveAssetPsbts are passive virtual asset PSBTs committed to the anchor.
	PassiveAssetPsbts [][]byte

	// ChangeOutputIndex is the change index returned by CommitVirtualPsbts.
	ChangeOutputIndex int32

	// LockedUTXOs are the lnd wallet locks returned by CommitVirtualPsbts.
	LockedUTXOs []Outpoint

	// SkipAnchorTxBroadcast leaves the finalized anchor transaction
	// unbroadcast.
	SkipAnchorTxBroadcast bool

	// Label is an optional short label for transfer tracking.
	Label string
}

// Validate checks that a publish-and-log transfer request can be sent.
func (r *PublishAndLogTransferRequest) Validate() error {
	if r == nil {
		return fmt.Errorf("nil publish and log transfer request")
	}
	if len(r.AnchorPsbt) == 0 {
		return fmt.Errorf("anchor PSBT is required")
	}
	if len(r.VirtualPsbts) == 0 {
		return fmt.Errorf("at least one virtual PSBT is required")
	}

	return nil
}

func (c AnchorChangeOutput) validate() error {
	switch c.Mode {
	case AnchorChangeOutputAdd, AnchorChangeOutputNoNew:
		return nil

	case AnchorChangeOutputExisting:
		if c.ExistingOutputIndex < 0 {
			return fmt.Errorf("existing change output index is negative")
		}

		return nil

	default:
		return fmt.Errorf("anchor change output is required")
	}
}

func (f AnchorFee) validate() error {
	switch f.Mode {
	case AnchorFeeSatPerVByte:
		if f.FeeRate.IsZero() {
			return fmt.Errorf("anchor fee rate is required")
		}

		return nil

	case AnchorFeeTargetConf:
		if f.TargetConf == 0 {
			return fmt.Errorf("anchor target confirmation is required")
		}

		return nil

	default:
		return fmt.Errorf("anchor fee is required")
	}
}
