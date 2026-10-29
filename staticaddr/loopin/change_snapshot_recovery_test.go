package loopin

import (
	"bytes"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/stretchr/testify/require"
)

// TestCheckChangeAfterOwnershipRecovery checks actual SQL joins after recovery.
// A legacy keep-first mapping must not hide change owed by a newer paid swap.
func TestCheckChangeAfterOwnershipRecovery(t *testing.T) {
	for _, tc := range []struct {
		name      string
		legacy    bool
		allReused bool
	}{
		{name: "complete current mapping"},
		{name: "partial legacy mapping", legacy: true},
		{name: "unmapped requested swap", legacy: true, allReused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newOwnershipRecoveryTest(t)
			older := h.addSwap(t, Failed)
			sibling := &deposit.Deposit{
				ID: deposit.ID{2}, Hash: chainhash.Hash{0xcd},
				Value: 150_000, TimeOutSweepPkScript: []byte{0x51},
				AddressParams: h.live.AddressParams,
			}
			sibling.SetState(deposit.LoopingIn)
			require.NoError(t, h.depositStore.CreateDeposit(
				t.Context(), sibling,
			))

			if tc.allReused {
				h.addSwap(t, Failed, sibling)
			}

			// Reverse row order and vary the hash encoding to exercise
			// the store's canonical ordering of a complete snapshot.
			paid := h.addSwap(t, Succeeded, sibling, h.live)
			storedOutpoints := strings.Join(
				paid.DepositOutpoints, OutpointSeparator,
			)
			if !tc.allReused {
				storedOutpoints = strings.ToUpper(storedOutpoints)
			}
			_, err := h.db.ExecContext(t.Context(), `
				UPDATE static_address_swaps SET selected_amount = 200000,
				change_static_address_id = $1, deposit_outpoints = $2
				WHERE swap_hash = $3`, h.live.AddressParams.ID,
				storedOutpoints, paid.SwapHash[:])
			require.NoError(t, err)
			_, err = h.db.ExecContext(t.Context(), `
				UPDATE swaps SET amount_requested = 200000
				WHERE swap_hash = $1`, paid.SwapHash[:])
			require.NoError(t, err)

			if tc.legacy {
				h.migrate(t, older.SwapHash)
			} else {
				h.load(t)
			}
			h.useRealDepositManager(t)
			require.NoError(t, h.manager.reconcileLoopInDeposits(
				t.Context(), h.pending(t),
			))
			loaded, err := h.store.GetLoopInByHash(
				t.Context(), paid.SwapHash,
			)
			require.NoError(t, err)

			sweep := wire.NewMsgTx(2)
			sweep.AddTxIn(&wire.TxIn{PreviousOutPoint: sibling.OutPoint})
			sweep.AddTxIn(&wire.TxIn{PreviousOutPoint: h.live.OutPoint})
			sweep.AddTxOut(wire.NewTxOut(249_000, []byte{0x51}))

			if tc.allReused {
				require.Empty(t, loaded.Deposits)
				// Neither input maps to the paid swap, so the batched
				// lookup cannot discover its missing change by itself.
				require.NoError(t, h.manager.checkChange(t.Context(), sweep))

				packet, err := psbt.NewFromUnsignedTx(sweep)
				require.NoError(t, err)
				var encoded bytes.Buffer
				require.NoError(t, packet.Serialize(&encoded))
				signer := &sweepRequestSigner{}
				pushes := make(chan *swapserverrpc.PushStaticAddressSweeplessSigsRequest, 1)
				release := make(chan struct{})
				close(release)
				h.manager.cfg.Signer = signer
				h.manager.cfg.Server = &blockingSweepResponseServer{
					pushStarted: pushes, release: release,
				}
				req := &swapserverrpc.ServerStaticLoopInSweepNotification{
					SwapHash: paid.SwapHash[:], SweepTxPsbt: encoded.Bytes(),
					DepositToNonces: map[string][]byte{
						sibling.String(): make([]byte, musig2.PubNonceSize),
						h.live.String():  make([]byte, musig2.PubNonceSize),
					},
					PrevoutInfo: []*swapserverrpc.PrevoutInfo{
						testPrevoutInfo(sibling.OutPoint, uint64(sibling.Value),
							sibling.AddressParams.PkScript),
						testPrevoutInfo(h.live.OutPoint, uint64(h.live.Value),
							h.live.AddressParams.PkScript),
					},
				}
				require.ErrorContains(t, h.manager.handleLoopInSweepReq(
					t.Context(), req,
				), "incomplete deposit snapshot")
				require.Zero(t, signer.nextSession.Load())
				require.Empty(t, pushes)
				return
			}

			if tc.legacy {
				require.Len(t, loaded.Deposits, 1)
				// Calculating from this partial join would report no
				// change and allow the server to retain 50,000 sats.
				require.EqualValues(t, 0, loaded.ExpectedChangeAmount())
				require.ErrorContains(t, h.manager.checkChange(
					t.Context(), sweep,
				), "incomplete deposit snapshot")
				return
			}

			require.Len(t, loaded.Deposits, 2)
			require.Equal(t, sibling.OutPoint, loaded.Deposits[0].OutPoint)
			require.EqualValues(t, 50_000, loaded.ExpectedChangeAmount())
			require.ErrorContains(t, h.manager.checkChange(
				t.Context(), sweep,
			), "couldn't find expected change of 50000")

			sweep.TxOut[0].Value = 199_000
			sweep.AddTxOut(wire.NewTxOut(
				50_000, h.live.AddressParams.PkScript,
			))
			require.NoError(t, h.manager.checkChange(t.Context(), sweep))
		})
	}
}

// TestValidateChangeDepositSnapshot rejects incomplete or malformed original
// input metadata before it can suppress the expected change amount.
func TestValidateChangeDepositSnapshot(t *testing.T) {
	first := &deposit.Deposit{Hash: chainhash.Hash{0xab}}
	second := &deposit.Deposit{Hash: chainhash.Hash{0xcd}}
	for _, tc := range []struct {
		name      string
		outpoints []string
		deposits  []*deposit.Deposit
		err       string
	}{
		{
			name:     "missing original snapshot",
			deposits: []*deposit.Deposit{first},
			err:      "incomplete deposit snapshot",
		},
		{
			name:      "missing original deposit",
			outpoints: []string{first.String(), second.String()},
			deposits:  []*deposit.Deposit{second},
			err:       "incomplete deposit snapshot",
		},
		{
			name:      "invalid outpoint encoding",
			outpoints: []string{"invalid"},
			deposits:  []*deposit.Deposit{first},
			err:       "invalid deposit snapshot outpoint",
		},
		{
			name:      "nil deposit",
			outpoints: []string{first.String()},
			deposits:  []*deposit.Deposit{nil},
			err:       "deposit snapshot mismatch",
		},
		{
			name:      "wrong original deposit",
			outpoints: []string{first.String()},
			deposits:  []*deposit.Deposit{second},
			err:       "deposit snapshot mismatch",
		},
		{
			name:      "duplicate original input",
			outpoints: []string{first.String(), first.String()},
			deposits:  []*deposit.Deposit{first, first},
			err:       "duplicate deposit snapshot outpoint",
		},
		{
			name:      "canonical outpoint identity",
			outpoints: []string{strings.ToUpper(first.String()), second.String()},
			deposits:  []*deposit.Deposit{first, second},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateChangeDepositSnapshot(&StaticAddressLoopIn{
				DepositOutpoints: tc.outpoints, Deposits: tc.deposits,
			})
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
