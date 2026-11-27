package swapkit

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLoopInTimelocks checks every ordering rule of an asset Loop In at its
// exact boundary.
func TestLoopInTimelocks(t *testing.T) {
	// The deposit refund opens at 1100, ten blocks after the last height
	// the recovery window allows.
	valid := LoopInTimelocks{
		Tip: 900, DepositHeight: 100, DepositCSV: 1000,
		HTLCExpiry: 1050, RecoveryDelta: 49, ExecutionDelta: 20,
	}
	require.NoError(t, valid.Validate())

	tests := []struct {
		name   string
		mutate func(*LoopInTimelocks)
		ok     bool
	}{{
		name:   "deposit refund one block after recovery window",
		mutate: func(l *LoopInTimelocks) { l.RecoveryDelta = 49 },
		ok:     true,
	}, {
		name:   "deposit refund at end of recovery window",
		mutate: func(l *LoopInTimelocks) { l.RecoveryDelta = 50 },
	}, {
		name:   "tip exactly leaves execution window",
		mutate: func(l *LoopInTimelocks) { l.Tip = 1030 },
		ok:     true,
	}, {
		name:   "tip one block inside execution window",
		mutate: func(l *LoopInTimelocks) { l.Tip = 1031 },
	}, {
		name:   "tip below deposit confirmation",
		mutate: func(l *LoopInTimelocks) { l.Tip = 99 },
	}, {
		name:   "unconfirmed deposit",
		mutate: func(l *LoopInTimelocks) { l.DepositHeight = 0 },
	}, {
		name:   "zero CSV",
		mutate: func(l *LoopInTimelocks) { l.DepositCSV = 0 },
	}, {
		name: "execution delta below minimum",
		mutate: func(l *LoopInTimelocks) {
			l.ExecutionDelta = MinExecutionDelta - 1
		},
	}, {
		name:   "timestamp HTLC expiry",
		mutate: func(l *LoopInTimelocks) { l.HTLCExpiry = 500_000_000 },
	}, {
		name:   "zero HTLC expiry",
		mutate: func(l *LoopInTimelocks) { l.HTLCExpiry = 0 },
	}, {
		name: "deltas do not overflow",
		mutate: func(l *LoopInTimelocks) {
			l.RecoveryDelta = math.MaxUint32
			l.ExecutionDelta = math.MaxUint32
		},
	}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			locks := valid
			test.mutate(&locks)
			err := locks.Validate()
			if test.ok {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
		})
	}

	// The payment may stay pending until exactly one execution window
	// before the HTLC refund opens.
	require.NoError(t, valid.ValidatePayment(1030))
	require.ErrorIs(t, valid.ValidatePayment(1031), ErrTimelockOrder)
	require.ErrorIs(t, valid.ValidatePayment(899), ErrTimelockOrder)
	require.ErrorIs(t, valid.ValidatePayment(0), ErrTimelockOrder)

	// A payment check never passes on an invalid contract ordering.
	invalid := valid
	invalid.RecoveryDelta = 50
	require.ErrorIs(t, invalid.ValidatePayment(1000), ErrTimelockOrder)
}

// TestLoopOutTimelocks checks every ordering rule of an asset Loop Out at its
// exact boundary.
func TestLoopOutTimelocks(t *testing.T) {
	valid := LoopOutTimelocks{
		Tip: 900, ReservationExpiry: 1000, HTLCExpiry: 1050,
		ConfirmationDelta: 20, ClaimDelta: 10, SettleDelta: 30,
	}
	require.NoError(t, valid.Validate())

	tests := []struct {
		name   string
		mutate func(*LoopOutTimelocks)
		ok     bool
	}{{
		name:   "fallback confirms at reservation expiry",
		mutate: func(l *LoopOutTimelocks) { l.Tip = 980 },
		ok:     true,
	}, {
		name:   "fallback confirms after reservation expiry",
		mutate: func(l *LoopOutTimelocks) { l.Tip = 981 },
	}, {
		name:   "claim confirms at HTLC expiry",
		mutate: func(l *LoopOutTimelocks) { l.HTLCExpiry = 930 },
		ok:     true,
	}, {
		name:   "claim confirms after HTLC expiry",
		mutate: func(l *LoopOutTimelocks) { l.HTLCExpiry = 929 },
	}, {
		name:   "zero tip",
		mutate: func(l *LoopOutTimelocks) { l.Tip = 0 },
	}, {
		name:   "zero confirmation delta",
		mutate: func(l *LoopOutTimelocks) { l.ConfirmationDelta = 0 },
	}, {
		name:   "zero claim delta",
		mutate: func(l *LoopOutTimelocks) { l.ClaimDelta = 0 },
	}, {
		name: "timestamp reservation expiry",
		mutate: func(l *LoopOutTimelocks) {
			l.ReservationExpiry = 500_000_000
		},
	}, {
		name:   "zero HTLC expiry",
		mutate: func(l *LoopOutTimelocks) { l.HTLCExpiry = 0 },
	}, {
		name: "deltas do not overflow",
		mutate: func(l *LoopOutTimelocks) {
			l.ConfirmationDelta = math.MaxUint32
			l.ClaimDelta = math.MaxUint32
		},
	}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			locks := valid
			test.mutate(&locks)
			err := locks.Validate()
			if test.ok {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
		})
	}

	// The incoming payment must outlive the HTLC by the settle window.
	require.NoError(t, valid.ValidatePayment(1080))
	require.ErrorIs(t, valid.ValidatePayment(1079), ErrTimelockOrder)
	require.ErrorIs(t, valid.ValidatePayment(0), ErrTimelockOrder)

	unsettled := valid
	unsettled.SettleDelta = 0
	require.ErrorIs(t, unsettled.ValidatePayment(2000), ErrTimelockOrder)
}
