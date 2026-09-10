package reservation

import (
	"bytes"
	"context"
	"errors"
	"math"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/lndclient"
)

// ErrChainNotReady means the local node cannot yet verify the output.
var ErrChainNotReady = errors.New("reservation chain view is not ready")

// ChainSource checks that a proof belongs to the local chain.
type ChainSource interface {
	GetBlockHash(context.Context, int64) (chainhash.Hash, error)
}

// ChainNode supplies the local node's synchronization status and height.
type ChainNode interface {
	GetInfo(context.Context) (*lndclient.Info, error)
}

// ChainReader follows Instant Out's notification-based reservation view.
// Recovery registers again with the saved height hint; no wallet import or
// synchronous unspent assertion is required on the client.
type ChainReader struct {
	Chain         ChainSource
	Node          ChainNode
	ChainNotifier lndclient.ChainNotifierClient
}

// Validate requires LND confirmation, spend, and height services.
func (c *ChainReader) Validate() error {
	if c == nil || c.Chain == nil || c.Node == nil ||
		c.ChainNotifier == nil {

		return errors.New("incomplete reservation chain reader")
	}
	return nil
}

// Observe verifies the funding output and checks for a known spend.
// The FSM enforces the agreed confirmation depth and original CSV lifetime.
func (c *ChainReader) Observe(ctx context.Context, outpoint wire.OutPoint,
	output *wire.TxOut, firstHeight uint32) (FundingStatus, error) {

	var result FundingStatus
	if err := c.Validate(); err != nil {
		return result, err
	}
	if output == nil || output.Value <= 0 || len(output.PkScript) == 0 ||
		firstHeight == 0 || firstHeight > math.MaxInt32 {

		return result, ErrInvalidReservation
	}

	actionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	spends, spendErrors, err := c.ChainNotifier.RegisterSpendNtfn(
		actionCtx, &outpoint, output.PkScript, int32(firstHeight),
	)
	if err != nil {
		return result, err
	}
	confs, confErrors, err := c.ChainNotifier.RegisterConfirmationsNtfn(
		actionCtx, &outpoint.Hash, output.PkScript, 1,
		int32(firstHeight),
	)
	if err != nil {
		return result, err
	}

	select {
	case conf, ok := <-confs:
		if !ok || conf == nil || conf.Tx == nil ||
			conf.Tx.TxHash() != outpoint.Hash || conf.BlockHeight == 0 ||
			uint64(outpoint.Index) >= uint64(len(conf.Tx.TxOut)) {

			return result, ErrChainNotReady
		}
		got := conf.Tx.TxOut[outpoint.Index]
		if got == nil || got.Value != output.Value ||
			!bytes.Equal(got.PkScript, output.PkScript) {

			return result, ErrInvalidReservation
		}
		result.ConfirmationHeight = conf.BlockHeight

	case err := <-confErrors:
		if err == nil {
			err = ErrChainNotReady
		}
		return result, err

	case <-ctx.Done():
		return result, ctx.Err()
	}

	info, err := c.Node.GetInfo(ctx)
	if err != nil {
		return result, err
	}
	if info == nil || !info.SyncedToChain ||
		info.BlockHeight < result.ConfirmationHeight {

		return result, ErrChainNotReady
	}
	result.Height = info.BlockHeight

	// Like Instant Out, readiness means no spend is known. This does not
	// wait for an acknowledgement that LND's historical scan has finished.
	select {
	case spend, ok := <-spends:
		if !ok || spend == nil || spend.SpentOutPoint == nil ||
			*spend.SpentOutPoint != outpoint {

			return result, ErrChainNotReady
		}
		result.Spent = true

	case err := <-spendErrors:
		if err == nil {
			err = ErrChainNotReady
		}
		return result, err

	default:
	}
	return result, nil
}

// WaitForChange watches a verified reservation for a spend or a new block.
// The manager bounds the call and restores it through OnRecover. Unlike a
// nonblocking poll, this also receives delayed historical spend notifications.
func (c *ChainReader) WaitForChange(ctx context.Context, point wire.OutPoint,
	script []byte, hint uint32, status FundingStatus) (FundingStatus,
	error) {

	if err := c.Validate(); err != nil {
		return status, err
	}
	if hint == 0 || hint > math.MaxInt32 || len(script) == 0 {
		return status, ErrInvalidReservation
	}
	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	spends, spendErrors, err := c.ChainNotifier.RegisterSpendNtfn(
		watchCtx, &point, script, int32(hint),
	)
	if err != nil {
		return status, err
	}
	blocks, blockErrors, err := c.ChainNotifier.RegisterBlockEpochNtfn(watchCtx)
	if err != nil {
		return status, err
	}
	for {
		select {
		case spend, ok := <-spends:
			if !ok || spend == nil || spend.SpentOutPoint == nil ||
				*spend.SpentOutPoint != point {

				return status, ErrChainNotReady
			}
			status.Spent = true
			return status, nil

		case height, ok := <-blocks:
			if !ok || height <= 0 {
				return status, ErrChainNotReady
			}
			// LND first reports the current tip. Keep watching rather
			// than ending every subscription on that initial snapshot.
			if uint32(height) <= status.Height {
				continue
			}
			info, err := c.Node.GetInfo(ctx)
			if err != nil {
				return status, err
			}
			if info == nil || !info.SyncedToChain ||
				info.BlockHeight < uint32(height) {

				return status, ErrChainNotReady
			}
			status.Height = info.BlockHeight
			return status, nil

		case err := <-spendErrors:
			if err == nil {
				err = ErrChainNotReady
			}
			return status, err

		case err := <-blockErrors:
			if err == nil {
				err = ErrChainNotReady
			}
			return status, err

		case <-ctx.Done():
			return status, ctx.Err()
		}
	}
}
