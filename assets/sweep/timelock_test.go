package sweep

import (
	"math"
	"testing"
	"time"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/tappsbt"
	"github.com/stretchr/testify/require"
)

// TestValidateAnchorAbsoluteTimeLocks covers the boundary between height and
// timestamp locks as well as the Bitcoin sequence flag that activates nLockTime.
func TestValidateAnchorAbsoluteTimeLocks(t *testing.T) {
	t.Parallel()

	const (
		height   = 800_000
		lockTime = 1_700_000_000
	)
	tests := []struct {
		name        string
		requirement uint64
		lockTime    uint32
		sequences   []uint32
		wantErr     bool
	}{
		{
			name:      "unlocked",
			sequences: []uint32{wire.MaxTxInSequenceNum},
		},
		{
			name:        "exact height",
			requirement: height,
			lockTime:    height,
			sequences:   []uint32{wire.MaxTxInSequenceNum - 1},
		},
		{
			name:        "later height",
			requirement: height,
			lockTime:    height + 1,
			sequences:   []uint32{wire.MaxTxInSequenceNum - 1},
		},
		{
			name:        "earlier height",
			requirement: height,
			lockTime:    height - 1,
			sequences:   []uint32{wire.MaxTxInSequenceNum - 1},
			wantErr:     true,
		},
		{
			name:        "exact timestamp",
			requirement: lockTime,
			lockTime:    lockTime,
			sequences:   []uint32{wire.MaxTxInSequenceNum - 1},
		},
		{
			name:        "earlier timestamp",
			requirement: lockTime,
			lockTime:    lockTime - 1,
			sequences:   []uint32{wire.MaxTxInSequenceNum - 1},
			wantErr:     true,
		},
		{
			name:        "height cannot become timestamp",
			requirement: height,
			lockTime:    lockTime,
			sequences:   []uint32{wire.MaxTxInSequenceNum - 1},
			wantErr:     true,
		},
		{
			name:        "timestamp cannot become height",
			requirement: lockTime,
			lockTime:    height,
			sequences:   []uint32{wire.MaxTxInSequenceNum - 1},
			wantErr:     true,
		},
		{
			name:        "ambiguous asset threshold",
			requirement: txscript.LockTimeThreshold,
			lockTime:    txscript.LockTimeThreshold,
			sequences:   []uint32{wire.MaxTxInSequenceNum - 1},
			wantErr:     true,
		},
		{
			name:        "bitcoin threshold is a timestamp",
			requirement: txscript.LockTimeThreshold - 1,
			lockTime:    txscript.LockTimeThreshold,
			sequences:   []uint32{wire.MaxTxInSequenceNum - 1},
			wantErr:     true,
		},
		{
			name:        "maximum timestamp",
			requirement: math.MaxUint32,
			lockTime:    math.MaxUint32,
			sequences:   []uint32{wire.MaxTxInSequenceNum - 1},
		},
		{
			name:        "absolute value cannot truncate",
			requirement: uint64(math.MaxUint32) + 1,
			sequences:   []uint32{wire.MaxTxInSequenceNum - 1},
			wantErr:     true,
		},
		{
			name:        "all sequences final bypass lock",
			requirement: height,
			lockTime:    height,
			sequences:   []uint32{wire.MaxTxInSequenceNum},
			wantErr:     true,
		},
		{
			name:        "fee input activates absolute lock",
			requirement: height,
			lockTime:    height,
			sequences: []uint32{
				wire.MaxTxInSequenceNum,
				wire.MaxTxInSequenceNum - 1,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			pkt := newTimeLockTestPacket(test.requirement, 0)
			tx := wire.NewMsgTx(2)
			tx.LockTime = test.lockTime
			for idx, sequence := range test.sequences {
				tx.AddTxIn(&wire.TxIn{
					PreviousOutPoint: wire.OutPoint{
						Index: uint32(idx),
					},
					Sequence: sequence,
				})
			}

			err := validateAnchorTimeLocks(pkt, tx)
			if test.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)

			// An accepted lock must actually prevent Bitcoin inclusion
			// before the asset's required height or timestamp. Use the
			// consensus finality implementation as an independent check.
			if test.requirement == 0 {
				return
			}
			beforeHeight := int32(height)
			beforeTime := time.Unix(lockTime, 0)
			if test.requirement < txscript.LockTimeThreshold {
				beforeHeight = int32(test.requirement - 1)
			} else {
				beforeTime = time.Unix(
					int64(test.requirement-1), 0,
				)
			}
			require.False(t, blockchain.IsFinalizedTransaction(
				btcutil.NewTx(tx), beforeHeight, beforeTime,
			))
		})
	}
}

// TestValidateAnchorRelativeTimeLocks checks BIP 68's type, disable and delay
// mask semantics rather than comparing the complete sequence integers.
func TestValidateAnchorRelativeTimeLocks(t *testing.T) {
	t.Parallel()

	const delay = 144
	tests := []struct {
		name        string
		requirement uint64
		sequence    uint32
		version     int32
		wantErr     bool
	}{
		{
			name:     "zero requirement skips version and sequence",
			sequence: wire.MaxTxInSequenceNum,
			version:  1,
		},
		{
			name:        "disabled requirement skips enforcement",
			requirement: wire.SequenceLockTimeDisabled | delay,
			sequence:    wire.MaxTxInSequenceNum,
			version:     1,
		},
		{
			name:        "relative value cannot truncate",
			requirement: uint64(math.MaxUint32) + 1,
			version:     2,
			wantErr:     true,
		},
		{
			name: "overflow is not excused by disable bit",
			requirement: uint64(math.MaxUint32) + 1 |
				wire.SequenceLockTimeDisabled,
			version: 2,
			wantErr: true,
		},
		{
			name:        "exact block delay",
			requirement: delay,
			sequence:    delay,
			version:     2,
		},
		{
			name:        "longer block delay",
			requirement: delay,
			sequence:    delay + 1,
			version:     2,
		},
		{
			name:        "shorter block delay",
			requirement: delay,
			sequence:    delay - 1,
			version:     2,
			wantErr:     true,
		},
		{
			name:        "maximum block delay",
			requirement: wire.SequenceLockTimeMask,
			sequence:    wire.SequenceLockTimeMask,
			version:     2,
		},
		{
			name:        "version one does not enforce BIP 68",
			requirement: delay,
			sequence:    delay,
			version:     1,
			wantErr:     true,
		},
		{
			name:        "sequence disable bypasses delay",
			requirement: delay,
			sequence:    wire.SequenceLockTimeDisabled | delay,
			version:     2,
			wantErr:     true,
		},
		{
			name:        "final sequence bypasses delay",
			requirement: delay,
			sequence:    wire.MaxTxInSequenceNum,
			version:     2,
			wantErr:     true,
		},
		{
			name:        "block delay cannot become time delay",
			requirement: delay,
			sequence:    wire.SequenceLockTimeIsSeconds | delay,
			version:     2,
			wantErr:     true,
		},
		{
			name:        "time delay cannot become block delay",
			requirement: wire.SequenceLockTimeIsSeconds | delay,
			sequence:    delay,
			version:     2,
			wantErr:     true,
		},
		{
			name:        "exact time delay",
			requirement: wire.SequenceLockTimeIsSeconds | delay,
			sequence:    wire.SequenceLockTimeIsSeconds | delay,
			version:     2,
		},
		{
			name:        "shorter time delay",
			requirement: wire.SequenceLockTimeIsSeconds | delay,
			sequence:    wire.SequenceLockTimeIsSeconds | (delay - 1),
			version:     2,
			wantErr:     true,
		},
		{
			name:        "reserved sequence bits cannot inflate delay",
			requirement: delay,
			sequence:    1<<30 | (delay - 1),
			version:     2,
			wantErr:     true,
		},
		{
			name:        "reserved requirement bits do not add delay",
			requirement: 1<<30 | delay,
			sequence:    delay,
			version:     2,
		},
		{
			name:        "nonzero requirement with zero masked delay",
			requirement: 1 << 30,
			version:     2,
		},
		{
			name:        "zero masked delay still requires version two",
			requirement: 1 << 30,
			version:     1,
			wantErr:     true,
		},
		{
			name:        "zero time delay still requires matching type",
			requirement: wire.SequenceLockTimeIsSeconds,
			version:     2,
			wantErr:     true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			pkt := newTimeLockTestPacket(0, test.requirement)
			tx := wire.NewMsgTx(test.version)
			tx.AddTxIn(&wire.TxIn{Sequence: test.sequence})

			err := validateAnchorTimeLocks(pkt, tx)
			if test.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestValidateAnchorTimeLocksAllInputsAndOutputs ensures no input or output is
// omitted when several asset transitions share one Bitcoin transaction.
func TestValidateAnchorTimeLocksAllInputsAndOutputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		modify  func(*tappsbt.VPacket, *wire.MsgTx)
		wantErr bool
	}{
		{
			name: "all input anchors enforce delay",
		},
		{
			name: "one input anchor has insufficient delay",
			modify: func(_ *tappsbt.VPacket, tx *wire.MsgTx) {
				tx.TxIn[1].Sequence--
			},
			wantErr: true,
		},
		{
			name: "one input anchor disables sequence locks",
			modify: func(_ *tappsbt.VPacket, tx *wire.MsgTx) {
				tx.TxIn[1].Sequence |= wire.SequenceLockTimeDisabled
			},
			wantErr: true,
		},
		{
			name: "fee input cannot replace missing asset anchor",
			modify: func(_ *tappsbt.VPacket, tx *wire.MsgTx) {
				tx.TxIn[1].PreviousOutPoint.Index = 2
			},
			wantErr: true,
		},
		{
			name: "strongest output delay enforced",
			modify: func(pkt *tappsbt.VPacket, _ *wire.MsgTx) {
				pkt.Outputs = append(pkt.Outputs,
					newTimeLockTestPacket(0, 145).Outputs[0],
				)
			},
			wantErr: true,
		},
		{
			name: "outputs cannot require conflicting relative types",
			modify: func(pkt *tappsbt.VPacket, _ *wire.MsgTx) {
				pkt.Outputs = append(pkt.Outputs,
					newTimeLockTestPacket(0,
						wire.SequenceLockTimeIsSeconds|144,
					).Outputs[0],
				)
			},
			wantErr: true,
		},
		{
			name: "outputs cannot require conflicting absolute types",
			modify: func(pkt *tappsbt.VPacket, tx *wire.MsgTx) {
				pkt.Outputs[0] = newTimeLockTestPacket(
					800_000, 144,
				).Outputs[0]
				pkt.Outputs = append(pkt.Outputs,
					newTimeLockTestPacket(
						1_700_000_000, 144,
					).Outputs[0],
				)
				tx.LockTime = 1_700_000_000
			},
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			pkt := newTimeLockTestPacket(0, 144)
			pkt.Inputs = append(pkt.Inputs, &tappsbt.VInput{
				PrevID: asset.PrevID{
					OutPoint: wire.OutPoint{Index: 1},
				},
			})
			tx := wire.NewMsgTx(2)
			for idx := range 2 {
				tx.AddTxIn(&wire.TxIn{
					PreviousOutPoint: wire.OutPoint{
						Index: uint32(idx),
					},
					Sequence: 144,
				})
			}
			if test.modify != nil {
				test.modify(pkt, tx)
			}

			err := validateAnchorTimeLocks(pkt, tx)
			if test.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// newTimeLockTestPacket supplies only the fields used by anchor timelock
// validation. The caller already validates the remaining packet fields.
func newTimeLockTestPacket(lockTime, relativeLockTime uint64) *tappsbt.VPacket {
	return &tappsbt.VPacket{
		Inputs: []*tappsbt.VInput{{}},
		Outputs: []*tappsbt.VOutput{{
			LockTime:         lockTime,
			RelativeLockTime: relativeLockTime,
			Asset: &asset.Asset{
				LockTime:         lockTime,
				RelativeLockTime: relativeLockTime,
			},
		}},
	}
}
