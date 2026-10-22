// Package server implements a deliberately small Loop server for regtest.
//
// Unlike the unit-test mocks in the loop package, this server creates real
// Lightning invoices, publishes real Bitcoin transactions and performs the
// MuSig2 exchange required by static-address Loop In. It is not intended for
// any public network.
package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/lntypes"
)

const (
	defaultMinSwapAmount = btcutil.Amount(50_000)
	defaultMaxSwapAmount = btcutil.Amount(5_000_000)

	defaultLoopOutMinCltvDelta = int32(30)
	defaultLoopOutMaxCltvDelta = int32(250)
	defaultLoopInCltvDelta     = int32(100)
	minLoopInCltvDelta         = int32(100)
	maxLoopInCltvDelta         = int32(1_500)

	defaultFeeBaseSat = btcutil.Amount(100)
	defaultFeePPM     = uint64(1_000)
	defaultPrepaySat  = btcutil.Amount(100)

	defaultStaticAddressExpiry = uint32(4_320)
	defaultPaymentTimeout      = time.Minute
)

// BitcoinClient is the subset of Bitcoin Core's RPC client used by the
// regtest server. Keeping it narrow makes transaction validation testable.
type BitcoinClient interface {
	GetTxOut(txHash *chainhash.Hash, index uint32,
		mempool bool) (*btcjson.GetTxOutResult, error)

	GetRawTransaction(txHash *chainhash.Hash) (*btcutil.Tx, error)
}

var _ BitcoinClient = (*rpcclient.Client)(nil)

// Config contains the dependencies and policy knobs for a regtest server.
type Config struct {
	Lnd     *lndclient.LndServices
	Bitcoin BitcoinClient

	MinSwapAmount btcutil.Amount
	MaxSwapAmount btcutil.Amount

	LoopOutMinCltvDelta int32
	LoopOutMaxCltvDelta int32
	LoopInCltvDelta     int32

	FeeBaseSat btcutil.Amount
	FeePPM     uint64
	PrepaySat  btcutil.Amount

	StaticAddressExpiry uint32
	PaymentTimeout      time.Duration

	Logger *log.Logger
}

// Server implements the public Loop swap and static-address services. Swap
// state is intentionally in-memory: the binary is a disposable regtest tool.
// Static address keys remain valid for the lifetime of the process.
type Server struct {
	swapserverrpc.UnimplementedSwapServerServer
	swapserverrpc.UnimplementedStaticAddressServerServer

	cfg Config

	ctx    context.Context
	cancel context.CancelFunc

	mu          sync.RWMutex
	loopOuts    map[lntypes.Hash]*loopOutSwap
	loopIns     map[lntypes.Hash]*loopInSwap
	staticSwaps map[lntypes.Hash]*staticLoopInSwap
	addresses   map[string]*staticAddress
	lockedUTXOs map[string]lntypes.Hash

	notifications *notificationHub
	wg            sync.WaitGroup
}

// New constructs a regtest server and applies safe demo defaults for all
// omitted policy values.
func New(parent context.Context, cfg Config) (*Server, error) {
	if cfg.Lnd == nil {
		return nil, errors.New("lnd services are required")
	}
	if cfg.Bitcoin == nil {
		return nil, errors.New("bitcoin client is required")
	}
	if cfg.Lnd.ChainParams == nil || cfg.Lnd.ChainParams.Name != "regtest" {
		return nil, fmt.Errorf("regtest chain required, got %v",
			cfg.Lnd.ChainParams)
	}

	if cfg.MinSwapAmount == 0 {
		cfg.MinSwapAmount = defaultMinSwapAmount
	}
	if cfg.MaxSwapAmount == 0 {
		cfg.MaxSwapAmount = defaultMaxSwapAmount
	}
	if cfg.MinSwapAmount <= 0 || cfg.MaxSwapAmount < cfg.MinSwapAmount {
		return nil, errors.New("invalid swap amount range")
	}

	if cfg.LoopOutMinCltvDelta == 0 {
		cfg.LoopOutMinCltvDelta = defaultLoopOutMinCltvDelta
	}
	if cfg.LoopOutMaxCltvDelta == 0 {
		cfg.LoopOutMaxCltvDelta = defaultLoopOutMaxCltvDelta
	}
	if cfg.LoopInCltvDelta == 0 {
		cfg.LoopInCltvDelta = defaultLoopInCltvDelta
	}
	if cfg.LoopOutMinCltvDelta <= 0 ||
		cfg.LoopOutMaxCltvDelta < cfg.LoopOutMinCltvDelta {

		return nil, errors.New("invalid Loop Out CLTV range")
	}
	if cfg.LoopInCltvDelta < minLoopInCltvDelta ||
		cfg.LoopInCltvDelta > maxLoopInCltvDelta {

		return nil, fmt.Errorf(
			"Loop In CLTV delta must be within [%d,%d]",
			minLoopInCltvDelta, maxLoopInCltvDelta,
		)
	}

	if cfg.FeeBaseSat == 0 {
		cfg.FeeBaseSat = defaultFeeBaseSat
	}
	if cfg.FeePPM == 0 {
		cfg.FeePPM = defaultFeePPM
	}
	if cfg.PrepaySat == 0 {
		cfg.PrepaySat = defaultPrepaySat
	}
	if cfg.StaticAddressExpiry == 0 {
		cfg.StaticAddressExpiry = defaultStaticAddressExpiry
	}
	if cfg.PaymentTimeout == 0 {
		cfg.PaymentTimeout = defaultPaymentTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = log.New(os.Stdout, "loopserver-regtest: ",
			log.LstdFlags|log.Lmicroseconds)
	}

	ctx, cancel := context.WithCancel(parent)

	return &Server{
		cfg:           cfg,
		ctx:           ctx,
		cancel:        cancel,
		loopOuts:      make(map[lntypes.Hash]*loopOutSwap),
		loopIns:       make(map[lntypes.Hash]*loopInSwap),
		staticSwaps:   make(map[lntypes.Hash]*staticLoopInSwap),
		addresses:     make(map[string]*staticAddress),
		lockedUTXOs:   make(map[string]lntypes.Hash),
		notifications: newNotificationHub(),
	}, nil
}

// Stop cancels all active swaps and waits for their goroutines to exit.
func (s *Server) Stop() {
	s.cancel()
	s.wg.Wait()
}

func (s *Server) goSwap(run func(context.Context)) {
	s.wg.Go(func() {
		run(s.ctx)
	})
}
