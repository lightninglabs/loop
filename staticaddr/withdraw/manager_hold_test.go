package withdraw

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	btcaddr "github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/loop/test"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/input"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

const (
	holdStep          = "hold"
	publishStep       = "publish"
	watchStep         = "watch"
	transitionStep    = "transition"
	updateStep        = "update"
	releaseStep       = "release"
	rejectPublication = "reject"
	cancelPublication = "cancel"
)

func init() {
	UseLogger(btclog.Disabled)
}

type heldWithdrawalDeposits struct {
	DepositManager

	deposits []*deposit.Deposit
	held     bool
	steps    []string
	fail     string
	err      error
}

func (d *heldWithdrawalDeposits) EnsureDepositsFresh(context.Context) error {
	return nil
}

func (d *heldWithdrawalDeposits) AllOutpointsActiveDeposits(
	_ []wire.OutPoint, state fsm.StateType) ([]*deposit.Deposit, bool) {

	for _, dep := range d.deposits {
		if !dep.IsInState(state) {
			return nil, false
		}
	}
	return d.deposits, true
}

func (d *heldWithdrawalDeposits) HoldActiveDeposits(
	outpoints []wire.OutPoint) (func(), error) {

	d.steps = append(d.steps, holdStep)
	if len(outpoints) != len(d.deposits) {
		return nil, errors.New("hold does not cover selected deposits")
	}
	for i, dep := range d.deposits {
		if outpoints[i] != dep.OutPoint {
			return nil, errors.New("hold selects a different deposit")
		}
	}
	if d.fail == holdStep {
		return nil, d.err
	}
	d.held = true
	return func() {
		d.steps = append(d.steps, releaseStep)
		d.held = false
	}, nil
}

func (d *heldWithdrawalDeposits) TransitionDeposits(_ context.Context,
	deposits []*deposit.Deposit, _ fsm.EventType, state fsm.StateType) error {

	d.steps = append(d.steps, transitionStep)
	if !d.held {
		return errors.New("transition without active deposit hold")
	}
	if d.fail == transitionStep {
		return d.err
	}
	for _, dep := range deposits {
		dep.SetState(state)
	}
	return nil
}

func (d *heldWithdrawalDeposits) UpdateDeposit(context.Context,
	*deposit.Deposit) error {

	d.steps = append(d.steps, updateStep)
	if !d.held {
		return errors.New("persistence without active deposit hold")
	}
	if d.fail == updateStep {
		return d.err
	}
	return nil
}

type heldWithdrawalAddress struct {
	params *script.Parameters
	addr   *script.StaticAddress
}

func (a *heldWithdrawalAddress) GetStaticAddressParameters(
	context.Context) (*script.Parameters, error) {

	return a.params, nil
}

func (a *heldWithdrawalAddress) GetStaticAddress(context.Context) (
	*script.StaticAddress, error) {

	return a.addr, nil
}

type heldWithdrawalServer struct {
	swapserverrpc.StaticAddressServerClient
}

func (*heldWithdrawalServer) ServerPsbtWithdrawDeposits(_ context.Context,
	req *swapserverrpc.ServerPsbtWithdrawRequest, _ ...grpc.CallOption) (
	*swapserverrpc.ServerPsbtWithdrawResponse, error) {

	packet, err := psbt.NewFromRawBytes(
		bytes.NewReader(req.WithdrawalPsbt), false,
	)
	if err != nil {
		return nil, err
	}
	sigs := make(map[string]*swapserverrpc.ServerPsbtWithdrawSigningInfo)
	for outpoint := range req.DepositToNonces {
		sigs[outpoint] = &swapserverrpc.ServerPsbtWithdrawSigningInfo{
			Nonce: make([]byte, 66),
			Sig:   make([]byte, input.MuSig2PartialSigSize),
		}
	}
	hash := packet.UnsignedTx.TxHash()
	return &swapserverrpc.ServerPsbtWithdrawResponse{
		Txid: hash.CloneBytes(), SigningInfo: sigs,
	}, nil
}

type heldWithdrawalWallet struct {
	lndclient.WalletKitClient

	publish func(context.Context, *wire.MsgTx) error
}

func (w *heldWithdrawalWallet) PublishTransaction(ctx context.Context,
	tx *wire.MsgTx, _ string) error {

	return w.publish(ctx, tx)
}

type heldWithdrawalNotifier struct {
	lndclient.ChainNotifierClient

	deposits *heldWithdrawalDeposits
}

func (n *heldWithdrawalNotifier) RegisterSpendNtfn(context.Context,
	*wire.OutPoint, []byte, int32, ...lndclient.NotifierOption) (
	chan *chainntnfs.SpendDetail, chan error, error) {

	n.deposits.steps = append(n.deposits.steps, watchStep)
	if n.deposits.fail == watchStep {
		return nil, nil, n.deposits.err
	}
	return make(chan *chainntnfs.SpendDetail), make(chan error), nil
}

// TestWithdrawalHoldPreservesOrdering blocks the real withdrawal publication
// path and checks that its hold spans publication, monitoring, transitions and
// persistence. It also verifies the pre-existing state and transaction behavior
// on errors and fee bumps, including the checked initial persistence write.
func TestWithdrawalHoldPreservesOrdering(t *testing.T) {
	tests := []struct {
		name    string
		fail    string
		feeBump bool
		steps   []string
	}{
		{name: "success", steps: []string{
			holdStep, publishStep, watchStep, transitionStep,
			updateStep, releaseStep,
		}},
		{name: "hold failure", fail: holdStep, steps: []string{holdStep}},
		{name: "publication error", fail: publishStep, steps: []string{
			holdStep, publishStep, releaseStep,
		}},
		{name: "publication rejection", fail: rejectPublication, steps: []string{
			holdStep, publishStep, releaseStep,
		}},
		{name: "cancellation", fail: cancelPublication, steps: []string{
			holdStep, publishStep, releaseStep,
		}},
		{name: "watcher error", fail: watchStep, steps: []string{
			holdStep, publishStep, watchStep, releaseStep,
		}},
		{name: "transition error", fail: transitionStep, steps: []string{
			holdStep, publishStep, watchStep, transitionStep,
			releaseStep,
		}},
		{name: "persistence error", fail: updateStep, steps: []string{
			holdStep, publishStep, watchStep, transitionStep,
			updateStep, releaseStep,
		}},
		{name: "fee bump", feeBump: true, steps: []string{
			holdStep, publishStep, transitionStep, updateStep,
			releaseStep,
		}},
		{name: "rejected fee bump", feeBump: true, fail: rejectPublication,
			steps: []string{holdStep, publishStep, releaseStep}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			_, clientKey := test.CreateKey(1)
			_, serverKey := test.CreateKey(2)
			addr, err := script.NewStaticAddress(
				input.MuSig2Version100RC2, 100, clientKey, serverKey,
			)
			require.NoError(t, err)
			pkScript, err := addr.StaticAddressScript()
			require.NoError(t, err)
			dest, err := btcaddr.NewAddressWitnessPubKeyHash(
				make([]byte, 20), &chaincfg.RegressionNetParams,
			)
			require.NoError(t, err)
			destScript, err := txscript.PayToAddrScript(dest)
			require.NoError(t, err)
			d := &deposit.Deposit{
				ID: deposit.ID{1}, Value: 100_000,
				Hash:                 chainhash.Hash{1},
				ConfirmationHeight:   1,
				TimeOutSweepPkScript: destScript,
			}
			d.SetState(deposit.Deposited)
			if tc.feeBump {
				d.SetState(deposit.Withdrawing)
				d.FinalizedWithdrawalTx = wire.NewMsgTx(2)
				d.FinalizedWithdrawalTx.AddTxIn(&wire.TxIn{
					PreviousOutPoint: d.OutPoint,
				})
				d.FinalizedWithdrawalTx.AddTxOut(&wire.TxOut{
					Value: 99_000, PkScript: destScript,
				})
			}
			previousTx, previousState := d.FinalizedWithdrawalTx, d.GetState()
			dm := &heldWithdrawalDeposits{
				deposits: []*deposit.Deposit{d}, fail: tc.fail,
				err: errors.New("injected " + tc.fail + " error"),
			}
			db := loopdb.NewTestDB(t)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			depositStore := deposit.NewSqlStore(db.BaseDB)
			require.NoError(t, depositStore.CreateDeposit(ctx, d))
			store := NewSqlStore(
				loopdb.NewTypedStore[Querier](db), depositStore,
			)
			entered, resume := make(chan struct{}), make(chan struct{})
			wallet := &heldWithdrawalWallet{
				publish: func(ctx context.Context, _ *wire.MsgTx) error {
					dm.steps = append(dm.steps, publishStep)
					close(entered)
					select {
					case <-resume:
					case <-ctx.Done():
						return ctx.Err()
					}
					switch tc.fail {
					case publishStep:
						return dm.err
					case rejectPublication:
						return chain.ErrInsufficientFee
					case cancelPublication:
						cancel()
						return ctx.Err()
					default:
						return nil
					}
				},
			}
			m, err := NewManager(&ManagerConfig{
				DepositManager: dm, WalletKit: wallet, Store: store,
				ChainParams:               &chaincfg.RegressionNetParams,
				Signer:                    test.NewMockLnd().Signer,
				StaticAddressServerClient: &heldWithdrawalServer{},
				ChainNotifier:             &heldWithdrawalNotifier{deposits: dm},
				AddressManager: &heldWithdrawalAddress{
					addr: addr, params: &script.Parameters{
						ClientPubkey: clientKey,
						ServerPubkey: serverKey, PkScript: pkScript,
					},
				},
			}, 1)
			require.NoError(t, err)
			if previousTx != nil {
				m.finalizedWithdrawalTxns[previousTx.TxHash()] = previousTx
			}
			result := make(chan error, 1)
			go func() {
				_, _, err := m.WithdrawDeposits(ctx,
					[]wire.OutPoint{d.OutPoint}, dest.String(), 10, 0)
				result <- err
			}()
			if tc.fail != holdStep {
				select {
				case <-entered:
				case err := <-result:
					t.Fatalf("withdrawal ended before publication: %v", err)
				case <-time.After(5 * time.Second):
					t.Fatal("publication did not start")
				}
				require.True(t, dm.held)
				require.Equal(t, previousState, d.GetState())
				require.Equal(t, previousTx, d.FinalizedWithdrawalTx)
				close(resume)
			}
			select {
			case err = <-result:
			case <-time.After(5 * time.Second):
				t.Fatal("withdrawal did not finish")
			}
			switch tc.fail {
			case "", rejectPublication:
				require.NoError(t, err)
			case cancelPublication:
				require.ErrorIs(t, err, context.Canceled)
			default:
				require.ErrorIs(t, err, dm.err)
			}
			require.False(t, dm.held)
			require.Equal(t, tc.steps, dm.steps)
			switch tc.fail {
			case holdStep, publishStep, rejectPublication, cancelPublication, watchStep:
				require.Equal(t, previousState, d.GetState())
				require.Equal(t, previousTx, d.FinalizedWithdrawalTx)
				if previousTx != nil {
					require.Equal(t, map[chainhash.Hash]*wire.MsgTx{
						previousTx.TxHash(): previousTx,
					}, m.finalizedWithdrawalTxns)
				} else {
					require.Empty(t, m.finalizedWithdrawalTxns)
				}
			case transitionStep:
				require.Equal(t, previousState, d.GetState())
				require.NotNil(t, d.FinalizedWithdrawalTx)
			default:
				require.Equal(t, deposit.Withdrawing, d.GetState())
				require.NotNil(t, d.FinalizedWithdrawalTx)
				require.Equal(t, map[chainhash.Hash]*wire.MsgTx{
					d.FinalizedWithdrawalTx.TxHash(): d.FinalizedWithdrawalTx,
				}, m.finalizedWithdrawalTxns)
			}
		})
	}
}
