package withdraw

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/chain"
	fsmv2 "github.com/lightninglabs/loop/fsm/v2"
	"github.com/lightninglabs/loop/staticaddr/staticutil"
	staticaddressrpc "github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/input"
)

// signWorkflowPlan signs exactly the persisted plan using fresh ephemeral
// sessions. The server RPC remains unchanged: PSBT + public client nonces.
func (m *Manager) signWorkflowPlan(ctx context.Context, data WorkflowData) ([]byte, error) {
	tx, err := validateWorkflowCandidate(data, data.UnsignedTx, false)
	if err != nil {
		return nil, err
	}
	params, err := m.cfg.AddressManager.GetStaticAddressParameters(ctx)
	if err != nil {
		return nil, err
	}
	address, err := m.cfg.AddressManager.GetStaticAddress(ctx)
	if err != nil {
		return nil, err
	}
	sessions := make(map[string]*input.MuSig2SessionInfo, len(data.Inputs))
	nonces := make(map[string][]byte, len(data.Inputs))
	indexes := make(map[string]int, len(data.Inputs))
	// Even partial session construction must release sessions after failure.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		for _, session := range sessions {
			_ = m.cfg.Signer.MuSig2Cleanup(cleanupCtx, session.SessionID)
		}
	}()
	packet, err := psbt.NewFromUnsignedTx(tx)
	if err != nil {
		return nil, err
	}
	prevouts := make(map[wire.OutPoint]*wire.TxOut, len(data.Inputs))
	for i, in := range data.Inputs {
		if !bytes.Equal(in.PkScript, params.PkScript) {
			return nil, errors.New("withdrawal prevout no longer matches static address")
		}
		session, err := staticutil.CreateMusig2Session(ctx, m.cfg.Signer, params, address)
		if err != nil {
			return nil, err
		}
		sessions[in.Outpoint] = session
		nonces[in.Outpoint] = bytes.Clone(session.PublicNonce[:])
		indexes[in.Outpoint] = i
		prevout := &wire.TxOut{Value: in.Value, PkScript: bytes.Clone(in.PkScript)}
		prevouts[tx.TxIn[i].PreviousOutPoint] = prevout
		packet.Inputs[i].WitnessUtxo = prevout
	}
	var encoded bytes.Buffer
	if err := packet.Serialize(&encoded); err != nil {
		return nil, err
	}
	response, err := m.cfg.StaticAddressServerClient.ServerPsbtWithdrawDeposits(ctx,
		&staticaddressrpc.ServerPsbtWithdrawRequest{
			WithdrawalPsbt: encoded.Bytes(), DepositToNonces: nonces,
		})
	if err != nil {
		return nil, err
	}
	txid := tx.TxHash()
	if response == nil || !bytes.Equal(txid[:], response.Txid) ||
		len(response.SigningInfo) != len(data.Inputs) {

		return nil, errors.New("withdrawal signing response does not match intent")
	}
	for _, in := range data.Inputs {
		if _, ok := response.SigningInfo[in.Outpoint]; !ok {
			return nil, errors.New("withdrawal signing response is missing an input")
		}
	}
	signed, err := m.signMusig2Tx(ctx, txscript.NewMultiPrevOutFetcher(prevouts),
		m.cfg.Signer, tx, sessions, response.SigningInfo, indexes)
	if err != nil {
		return nil, err
	}
	raw, err := encodeWorkflowTx(signed)
	if err != nil {
		return nil, err
	}
	if _, err := validateWorkflowCandidate(data, raw, true); err != nil {
		return nil, err
	}
	return raw, nil
}

func (m *Manager) ensurePublisher(view fsmv2.Snapshot[WorkflowData]) {
	runtime := m.workflows[view.ID]
	if len(view.Data.Attempts) == 0 {
		return
	}
	attempt := view.Data.Attempts[len(view.Data.Attempts)-1]
	if runtime.publishCancel != nil && runtime.publishAttempt == attempt.ID {
		return
	}
	if runtime.publishCancel != nil {
		runtime.publishCancel()
	}
	workCtx, cancel := context.WithCancel(m.lifecycle)
	runtime.publishCancel, runtime.publishAttempt = cancel, attempt.ID
	wake := make(chan struct{}, 1)
	runtime.publishWake = wake
	m.wg.Go(func() {
		tx, err := decodeWorkflowTx(attempt.Tx)
		if err != nil {
			return
		}
		timer := time.NewTicker(30 * time.Second)
		defer timer.Stop()
		notified := false
		for {
			publishCtx, stop := context.WithTimeout(workCtx, 30*time.Second)
			err := m.cfg.WalletKit.PublishTransaction(publishCtx, tx,
				fmt.Sprintf("deposit-withdrawal-%s", attempt.TxID))
			stop()
			known := err == nil || strings.Contains(err.Error(), chain.ErrSameNonWitnessData.Error())
			if known && !notified {
				m.deliverResult(workCtx, workflowResult{id: view.ID, event: workflowPublished,
					message: workflowEvent{Attempt: attempt.ID, TxID: attempt.TxID}})
				notified = true
			} else if err != nil {
				m.deliverResult(workCtx, workflowResult{id: view.ID, event: workflowPublished,
					message: workflowEvent{Attempt: attempt.ID, TxID: attempt.TxID}, err: err})
			}
			select {
			case <-workCtx.Done():
				return
			case <-timer.C:
			case <-wake:
			}
		}
	})
}

func (m *Manager) ensureSpendMonitors(view fsmv2.Snapshot[WorkflowData]) {
	runtime := m.workflows[view.ID]
	for _, in := range view.Data.Inputs {
		if len(in.PkScript) == 0 || runtime.monitors[in.Outpoint] != nil {
			continue
		}
		point, err := wire.NewOutPointFromString(in.Outpoint)
		if err != nil || point.String() != in.Outpoint {
			continue
		}
		workCtx, cancel := context.WithCancel(m.lifecycle)
		runtime.monitors[in.Outpoint] = cancel
		m.wg.Go(func() {
			for workCtx.Err() == nil {
				watchCtx, stop := context.WithCancel(workCtx)
				spends, failures, err := m.cfg.ChainNotifier.RegisterSpendNtfn(watchCtx,
					point, in.PkScript, int32(in.ConfirmationHeight))
				if err == nil {
					select {
					case detail, ok := <-spends:
						if ok && detail != nil && detail.SpendingTx != nil && detail.SpenderTxHash != nil {
							raw, encodeErr := encodeWorkflowTx(detail.SpendingTx)
							if encodeErr == nil {
								m.deliverResult(workCtx, workflowResult{id: view.ID, event: workflowSpend,
									message: workflowEvent{Tx: raw, TxID: detail.SpenderTxHash.String()}})
							}
							// Keep the watch alive through confirmation/reorg reconciliation.
							// Re-register after a bounded delay, so closed subscriptions retry.
						}
					case err := <-failures:
						if err != nil {
							m.deliverResult(workCtx, workflowResult{id: view.ID, event: workflowSpend, err: err})
						}
					case <-workCtx.Done():
						stop()
						return
					}
				}
				stop()
				if !waitWithdrawalRetry(workCtx) {
					return
				}
			}
		})
	}
}

func waitWithdrawalRetry(ctx context.Context) bool {
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (m *Manager) ensureConfirmation(view fsmv2.Snapshot[WorkflowData]) {
	runtime := m.workflows[view.ID]
	key := fmt.Sprintf("%s/%s/%d/%s", view.State, view.Data.WinningTxID,
		view.Data.ConfirmationHeight, view.Data.ConfirmationBlockHash)
	if runtime.confirmationCancel != nil && runtime.confirmationKey == key {
		return
	}
	if runtime.confirmationCancel != nil {
		runtime.confirmationCancel()
	}
	workCtx, cancel := context.WithCancel(m.lifecycle)
	runtime.confirmationCancel, runtime.confirmationKey = cancel, key
	m.wg.Go(func() {
		hash, err := chainhash.NewHashFromStr(view.Data.WinningTxID)
		if err != nil {
			return
		}
		hint := int32(view.Data.InitiationHeight)
		if view.Data.ConfirmationHeight > 0 {
			hint = int32(view.Data.ConfirmationHeight)
		}
		for workCtx.Err() == nil {
			watchCtx, stop := context.WithCancel(workCtx)
			confirmations, failures, err := m.cfg.ChainNotifier.RegisterConfirmationsNtfn(
				watchCtx, hash, view.Data.DestinationScript, MinConfs, hint)
			if err == nil {
				select {
				case confirmed, ok := <-confirmations:
					if ok && confirmed != nil && confirmed.BlockHash != nil && confirmed.Tx != nil &&
						confirmed.Tx.TxHash() == *hash {

						event := workflowConfirmed
						if view.State == WorkflowFinalizing {
							event = workflowFinalize
						}
						if view.State == WorkflowFinalizing && confirmed.BlockHash.String() != view.Data.ConfirmationBlockHash {
							event = workflowReorg
						}
						m.deliverResult(workCtx, workflowResult{id: view.ID, event: event,
							message: workflowEvent{TxID: hash.String(), Height: confirmed.BlockHeight,
								BlockHash: confirmed.BlockHash.String()}})
						stop()
						return
					}
				case err := <-failures:
					// A changed/removed confirmation invalidates finalization evidence.
					if view.State == WorkflowFinalizing || view.State == WorkflowConfirming {
						m.deliverResult(workCtx, workflowResult{id: view.ID, event: workflowReorg,
							message: workflowEvent{TxID: hash.String()}})
						stop()
						return
					}
					m.deliverResult(workCtx, workflowResult{id: view.ID, event: workflowConfirmed, err: err})
				case <-workCtx.Done():
					stop()
					return
				}
			}
			stop()
			if !waitWithdrawalRetry(workCtx) {
				return
			}
		}
	})
}
