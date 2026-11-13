package reservation

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightninglabs/taproot-assets/address"
	"github.com/lightninglabs/taproot-assets/asset"
	"github.com/lightninglabs/taproot-assets/commitment"
	"github.com/lightninglabs/taproot-assets/proof"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/lightninglabs/taproot-assets/tapscript"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

// verifierLog captures the package logger so a test can count ERR lines.
func newVerifierHarness(t *testing.T, r *Reservation) *clientHarness {
	t.Helper()
	db := loopdb.NewTestDB(t)
	h := &clientHarness{
		t:     t,
		id:    r.ID,
		quote: testPurchaseQuote(r),
		store: &failingStore{
			Store: NewSqlStore(db.BaseDB),
		},
		preimage:    lntypes.Preimage{9},
		probeResult: ProbeSucceeded,
		status: FundingStatus{
			Height:             101,
			ConfirmationHeight: 100,
		},
	}
	h.cfg = &Config{
		Store:       h.store,
		Server:      h,
		Payments:    h,
		Wallet:      h,
		Clock:       clock.NewTestClock(time.Unix(1800000000, 0)),
		NotifyAdmin: func(ID, error) { h.notifications++ },
	}
	h.store.Store.(*SqlStore).clock = h.cfg.Clock
	require.NoError(t, h.store.CreateReservation(t.Context(), r))
	h.restart()
	return h
}

// verifierProof is a well-formed single-transition proof file for the shared
// reservation contract, built exactly like TestReservationProofBinding.
type verifierProof struct {
	raw      []byte
	tx       *wire.MsgTx
	outpoint wire.OutPoint
	header   wire.BlockHeader
	assetID  asset.ID
}

const verifierProofAmount = 100

func newVerifierProof(t *testing.T, server,
	client *btcec.PublicKey) *verifierProof {

	t.Helper()
	genesis := asset.Genesis{
		FirstPrevOut: wire.OutPoint{Index: 4},
		Tag:          "reservation proof",
		Type:         asset.Normal,
	}
	kit, err := NewKit(server, client, genesis.ID(), 1440,
		&address.RegressionNetTap)
	require.NoError(t, err)
	opTrue, err := kit.TapScriptKey()
	require.NoError(t, err)
	a, err := asset.New(genesis, verifierProofAmount, 0, 0, opTrue, nil,
		asset.WithAssetVersion(asset.V1))
	require.NoError(t, err)
	version := commitment.TapCommitmentV2
	root, err := commitment.FromAssets(&version, a)
	require.NoError(t, err)
	_, inclusion, err := root.Proof(
		a.TapCommitmentKey(), a.AssetCommitmentKey(),
	)
	require.NoError(t, err)
	key, err := input.MuSig2CombineKeys(input.MuSig2Version100RC2,
		[]*btcec.PublicKey{server, client}, true, &input.MuSig2Tweaks{})
	require.NoError(t, err)
	timeout, err := kit.GenTimeoutPathScript()
	require.NoError(t, err)
	sibling, err := commitment.NewPreimageFromLeaf(
		txscript.NewBaseTapLeaf(timeout),
	)
	require.NoError(t, err)
	siblingHash, err := sibling.TapHash()
	require.NoError(t, err)
	script, err := tapscript.PayToAddrScript(
		*key.PreTweakedKey, siblingHash, *root,
	)
	require.NoError(t, err)
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&genesis.FirstPrevOut, nil, nil))
	tx.AddTxOut(wire.NewTxOut(330, []byte{txscript.OP_TRUE}))
	tx.AddTxOut(wire.NewTxOut(2000, script))
	p := proof.Proof{
		AnchorTx:    *tx,
		Asset:       *a,
		BlockHeight: 100,
		InclusionProof: proof.TaprootProof{
			OutputIndex: 1,
			InternalKey: key.PreTweakedKey,
			CommitmentProof: &proof.CommitmentProof{
				Proof:              *inclusion,
				TapSiblingPreimage: sibling,
			},
		},
	}
	file, err := proof.NewFile(proof.V0, p)
	require.NoError(t, err)
	var raw bytes.Buffer
	require.NoError(t, file.Encode(&raw))
	return &verifierProof{
		raw: raw.Bytes(), tx: tx, outpoint: p.OutPoint(),
		header: p.BlockHeader, assetID: genesis.ID(),
	}
}

// verifierTapd is the local tapd. Like tapd v0.8.8 (rpcserver.go VerifyProof)
// it answers any verification error, including a failed chain-backend lookup,
// with Valid=false and a nil RPC error.
type verifierTapd struct {
	verifyCalls  int
	invalidCalls int

	// chain, when set, is the chain backend tapd checks the anchor header
	// against (tapd ChainBridge.VerifyBlock calls GetBlockHash by height).
	chain *verifierChain
}

func (v *verifierTapd) GetInfo(context.Context, *taprpc.GetInfoRequest,
	...grpc.CallOption) (*taprpc.GetInfoResponse, error) {

	return &taprpc.GetInfoResponse{Version: testTapdVersion}, nil
}

func (v *verifierTapd) VerifyProof(ctx context.Context, file *taprpc.ProofFile,
	_ ...grpc.CallOption) (*taprpc.VerifyProofResponse, error) {

	v.verifyCalls++
	if v.invalidCalls > 0 {
		v.invalidCalls--
		return &taprpc.VerifyProofResponse{Valid: false}, nil
	}
	if v.chain != nil {
		decoded, err := proof.DecodeFile(file.RawProofFile)
		if err != nil {
			return nil, err
		}
		last, err := decoded.LastProof()
		if err != nil {
			return nil, err
		}
		hash, err := v.chain.GetBlockHash(ctx, int64(last.BlockHeight))
		if err != nil || hash != last.BlockHeader.BlockHash() {
			// A failed header check is a verification error.
			// tapd reports it as an invalid proof, not as an
			// RPC error.
			return &taprpc.VerifyProofResponse{Valid: false}, nil
		}
	}
	return &taprpc.VerifyProofResponse{
		Valid: true, DecodedProof: decodedTestProof(file),
	}, nil
}

type verifierKeys struct{ key keychain.KeyDescriptor }

func (k verifierKeys) DeriveNextKey(context.Context, int32) (
	*keychain.KeyDescriptor, error) {

	return nil, errors.New("unused")
}

func (k verifierKeys) DeriveKey(context.Context, *keychain.KeyLocator) (
	*keychain.KeyDescriptor, error) {

	key := k.key
	return &key, nil
}

// verifierChain is the local LND chain view shared by every chain interface.
type verifierChain struct {
	lndclient.ChainNotifierClient

	mu     sync.Mutex
	tip    uint32
	tx     *wire.MsgTx
	header wire.BlockHeader
	height uint32

	// syncGaps makes the next GetInfo calls report an unsynced node.
	syncGaps int
	watches  int
}

func (c *verifierChain) GetBlockHash(_ context.Context, height int64) (
	chainhash.Hash, error) {

	c.mu.Lock()
	defer c.mu.Unlock()
	if height != int64(c.height) || uint32(height) > c.tip {
		return chainhash.Hash{}, errors.New("block number out of range")
	}
	return c.header.BlockHash(), nil
}

func (c *verifierChain) GetInfo(context.Context) (*lndclient.Info, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	synced := true
	if c.syncGaps > 0 {
		c.syncGaps--
		synced = false
	}
	return &lndclient.Info{SyncedToChain: synced, BlockHeight: c.tip}, nil
}

// streamError mirrors lndclient: its receive goroutine pushes the stream
// error into the buffered error channel once the stream context ends.
func verifierStreamError(ctx context.Context) chan error {
	errs := make(chan error, 1)
	go func() {
		<-ctx.Done()
		errs <- status.FromContextError(ctx.Err()).Err()
	}()
	return errs
}

func (c *verifierChain) RegisterConfirmationsNtfn(ctx context.Context,
	_ *chainhash.Hash, _ []byte, _, _ int32,
	_ ...lndclient.NotifierOption) (chan *chainntnfs.TxConfirmation,
	chan error, error) {

	confs := make(chan *chainntnfs.TxConfirmation, 1)
	confs <- &chainntnfs.TxConfirmation{Tx: c.tx, BlockHeight: c.height}
	return confs, verifierStreamError(ctx), nil
}

func (c *verifierChain) RegisterSpendNtfn(ctx context.Context, _ *wire.OutPoint,
	_ []byte, _ int32, _ ...lndclient.NotifierOption) (
	chan *chainntnfs.SpendDetail, chan error, error) {

	spends := make(chan *chainntnfs.SpendDetail, 1)
	return spends, verifierStreamError(ctx), nil
}

func (c *verifierChain) RegisterBlockEpochNtfn(ctx context.Context) (chan int32,
	chan error, error) {

	c.mu.Lock()
	c.watches++
	tip := int32(c.tip)
	c.mu.Unlock()
	blocks := make(chan int32)
	go func() {
		// LND first reports the current tip, then stays quiet.
		select {
		case blocks <- tip:
		case <-ctx.Done():
		}
	}()
	return blocks, verifierStreamError(ctx), nil
}

// verifierProofServer serves the real proof file instead of placeholder bytes.
type verifierProofServer struct {
	*clientHarness

	raw     []byte
	fetches int
}

func (s *verifierProofServer) GetAssetReservationProof(context.Context,
	*swapserverrpc.AssetReservationSelector, ...grpc.CallOption) (
	*swapserverrpc.AssetReservationProof, error) {

	s.fetches++
	return &swapserverrpc.AssetReservationProof{
		ReservationId: s.quote.ReservationId,
		Outpoint:      s.outpoint.String(),
		Proof:         bytes.Clone(s.raw),
	}, nil
}

// realVerifier is a paid purchase whose delivery is checked by the real
// WalletVerifier, VerifyReservationProof and ChainReader.
type realVerifier struct {
	h      *clientHarness
	tapd   *verifierTapd
	chain  *verifierChain
	server *verifierProofServer
}

func newRealVerifier(t *testing.T) *realVerifier {
	t.Helper()
	r := testReservation()
	_, serverKey := btcec.PrivKeyFromBytes([]byte{2})
	fixture := newVerifierProof(t, serverKey, r.ClientKey.PubKey)
	r.AssetID = fixture.assetID
	r.Amount = verifierProofAmount
	r.Terms = Terms{AssetID: r.AssetID, Amount: r.Amount}
	r.State = RequestQuote
	h := newVerifierHarness(t, r)
	h.outpoint = fixture.outpoint

	// Quote, probe, approve and settle with the package's node doubles.
	h.event(OnRecover, nil)
	h.approve()
	h.settle()
	h.event(OnRecover, nil)
	h.state(WaitForDelivery)

	v := &realVerifier{
		h:    h,
		tapd: &verifierTapd{},
		chain: &verifierChain{
			tip: 102, tx: fixture.tx, header: fixture.header,
			height: 100,
		},
		server: &verifierProofServer{
			clientHarness: h, raw: fixture.raw,
		},
	}
	wallet := &WalletVerifier{
		Keys:      verifierKeys{key: r.ClientKey},
		Tap:       v.tapd,
		Params:    &address.RegressionNetTap,
		KeyFamily: 99,
		Chain: &ChainReader{
			Chain: v.chain, Node: v.chain, ChainNotifier: v.chain,
		},
	}
	require.NoError(t, wallet.Validate())
	h.cfg.Wallet = wallet
	h.cfg.Server = v.server
	h.deliver = true
	h.restart()
	return v
}

// TestRealVerifierReachesReady proves the fixture: with a healthy node the
// real verification path takes the paid purchase to Ready.
func TestRealVerifierReachesReady(t *testing.T) {
	v := newRealVerifier(t)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	require.NoError(t, v.h.machine.SendEvent(ctx, OnRecover, nil))
	require.NoError(t, v.h.machine.LastActionError)
	r := v.h.record()
	require.Equal(t, Ready, r.State)
	require.EqualValues(t, 100, r.ConfirmationHeight)
	require.NotEmpty(t, r.ReservationProof)
	require.Equal(t, 1, v.tapd.verifyCalls)
	require.Zero(t, v.h.notifications)
}

// TestVerifyWaitsForLaggingChainView checks that a local node which has not
// reached the proof's anchor block is not taken for an invalid proof. tapd
// reports an unverifiable proof and an invalid one alike, so the client reads
// its own chain first and retries a paid purchase instead of parking it.
func TestVerifyWaitsForLaggingChainView(t *testing.T) {
	v := newRealVerifier(t)
	h := v.h
	v.tapd.chain = v.chain
	v.chain.tip = 99
	require.NoError(t, h.machine.SendEvent(t.Context(), OnRecover, nil))
	h.state(VerifyReservation)
	require.Error(t, h.machine.LastActionError)
	require.NotErrorIs(t, h.machine.LastActionError, ErrInvalidReservation)
	require.Zero(t, v.tapd.verifyCalls)
	require.Zero(t, h.notifications)

	// The node catches up and the same purchase verifies.
	v.chain.tip = 102
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	require.NoError(t, h.machine.SendEvent(ctx, OnRecover, nil))
	h.state(Ready)
	require.Equal(t, 1, v.tapd.verifyCalls)
	require.Zero(t, h.notifications)
}

// TestVerifyRejectsProofFromAnotherChain keeps the definitive case: a proof
// whose anchor block differs from the local block at that height is invalid.
func TestVerifyRejectsProofFromAnotherChain(t *testing.T) {
	v := newRealVerifier(t)
	h := v.h
	v.chain.header.Nonce++
	require.NoError(t, h.machine.SendEvent(t.Context(), OnRecover, nil))
	h.state(NeedAdminAttention)
	require.Zero(t, v.tapd.verifyCalls)
}
