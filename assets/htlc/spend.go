package htlc

import "github.com/btcsuite/btcd/wire"

// SpendConstraints describes the canonical Bitcoin input sequence and minimum
// transaction locktime for an HTLC script path. Apply these before signing any
// input, including fee inputs. CLTV locktimes must remain in the height domain.
// These values do not establish chain maturity or a safe Lightning deadline.
type SpendConstraints struct {
	Sequence uint32
	LockTime uint32
}

// SuccessSpendConstraints returns the unchanged one-block CSV success terms.
func (s *SwapKit) SuccessSpendConstraints() SpendConstraints {
	return SpendConstraints{Sequence: SuccessSequence}
}

// TimeoutSpendConstraints returns the canonical refund spend terms. A CLTV
// refund uses a non-final sequence with BIP68 disabled and an absolute height;
// a V0 refund retains its relative CSV delay and needs no absolute locktime.
// Witness builders require this exact sequence and accept later CLTV heights.
func (s *SwapKit) TimeoutSpendConstraints() SpendConstraints {
	if s.policy == AssetHTLCCLTVV1 {
		return SpendConstraints{
			Sequence: wire.MaxTxInSequenceNum - 1,
			LockTime: s.refundHeight,
		}
	}

	return SpendConstraints{Sequence: s.csvExpiry}
}
