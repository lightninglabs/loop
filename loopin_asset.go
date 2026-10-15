package loop

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop/assets"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/taproot-assets/rfqmsg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/routing/route"
)

// validateAssetLoopIn validates a selector and pins the last hop to its edge.
func validateAssetLoopIn(r *LoopInRequest, client *assets.TapdClient,
	requireMinimum bool) error {

	if r.AssetId != nil && r.GroupKey != nil {
		return fmt.Errorf("asset id and group key are mutually exclusive")
	}
	if r.GroupKey == nil {
		if len(r.AssetId) != 32 {
			return fmt.Errorf("asset id must be a 32 byte value")
		}
	} else {
		if len(r.GroupKey) != 33 {
			return fmt.Errorf("group key must be a 33 byte public key")
		}
		if _, err := btcec.ParsePubKey(r.GroupKey); err != nil {
			return fmt.Errorf("invalid group key: %w", err)
		}
	}
	if client == nil {
		return fmt.Errorf("asset client must be set")
	}
	if r.Private || len(r.RouteHints) != 0 {
		return fmt.Errorf("private and route hints are not supported " +
			"for asset loop ins")
	}
	if len(r.AssetEdgeNode) != 33 {
		return fmt.Errorf("asset edge node is required and must be " +
			"a 33 byte public key")
	}
	pub, err := btcec.ParsePubKey(r.AssetEdgeNode)
	if err != nil {
		return fmt.Errorf("invalid asset edge node: %w", err)
	}
	peer := route.NewVertex(pub)
	if r.LastHop != nil && *r.LastHop != peer {
		return fmt.Errorf("last hop and asset edge node must match")
	}
	r.LastHop = &peer
	if requireMinimum && r.MinAssetAmount == 0 {
		return fmt.Errorf("minimum asset amount must be positive")
	}
	return nil
}

// assetLoopInTerms copies an accepted RFQ into its persistent representation.
func assetLoopInTerms(r *LoopInRequest,
	invoice *assets.AssetInvoice) *loopdb.LoopInAssetSwap {

	q := invoice.AcceptedBuyQuote
	return &loopdb.LoopInAssetSwap{
		AssetId: bytes.Clone(r.AssetId), GroupKey: bytes.Clone(r.GroupKey),
		AssetEdgeNode:  bytes.Clone(r.AssetEdgeNode),
		MinAssetAmount: r.MinAssetAmount, AssetAmount: invoice.AssetAmount,
		RateCoefficient: q.AskAssetRate.Coefficient,
		RateScale:       q.AskAssetRate.Scale, QuoteExpiry: q.Expiry,
	}
}

// quoteAssetLoopIn obtains tapd's BTC-denominated receive quote without
// creating a payable invoice: a temporary hold invoice is canceled before
// returning. tapd has no quote-only RPC for a BTC-denominated receive amount.
// Execution obtains a fresh RFQ; the user's minimum is the slippage bound.
func (s *Client) quoteAssetLoopIn(ctx context.Context,
	r *AssetLoopInQuoteRequest, amount btcutil.Amount) (
	*loopdb.LoopInAssetSwap, error) {

	if amount <= 0 {
		return nil, fmt.Errorf("asset invoice amount must be positive")
	}
	var hash lntypes.Hash
	if _, err := rand.Read(hash[:]); err != nil {
		return nil, err
	}
	invoice, err := s.AssetClient.AddAssetInvoice(
		ctx, r.AssetId, r.GroupKey, r.AssetEdgeNode, &lnrpc.Invoice{
			Memo: "loop in quote", ValueMsat: int64(amount) * 1000,
			Expiry: 300,
		}, &hash, r.MinAssetAmount,
	)
	// Also attempt cancellation after an ambiguous RPC failure.
	cleanupCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), probeInvoiceCleanupTimeout,
	)
	defer cancel()
	cancelErr := s.lndServices.Invoices.CancelInvoice(cleanupCtx, hash)
	if err != nil {
		return nil, err
	}
	if cancelErr != nil {
		return nil, fmt.Errorf("cancel temporary asset invoice: %w", cancelErr)
	}
	peer, err := route.NewVertexFromStr(invoice.AcceptedBuyQuote.Peer)
	if err != nil || !bytes.Equal(peer[:], r.AssetEdgeNode) {
		return nil, fmt.Errorf("asset quote peer does not match edge")
	}
	if invoice.AcceptedBuyQuote.Expiry <= uint64(time.Now().Unix()) {
		return nil, fmt.Errorf("asset quote is expired")
	}
	return assetLoopInTerms(&LoopInRequest{
		AssetId: r.AssetId, GroupKey: r.GroupKey,
		AssetEdgeNode: r.AssetEdgeNode, MinAssetAmount: r.MinAssetAmount,
	}, invoice), nil
}

// receivedAssetUnits sums only accepted HTLCs, rejecting missing asset data
// and overflow. tapd verifies group membership against the invoice's RFQ.
func receivedAssetUnits(info *loopdb.LoopInAssetSwap,
	htlcs []lndclient.InvoiceHtlc) (uint64, error) {

	var total uint64
	for _, htlc := range htlcs {
		if htlc.State != lnrpc.InvoiceHTLCState_ACCEPTED {
			continue
		}
		var data rfqmsg.JsonHtlc
		if err := json.Unmarshal(htlc.CustomChannelData, &data); err != nil {
			return 0, fmt.Errorf("decode asset HTLC: %w", err)
		}
		if len(data.Balances) == 0 {
			return 0, fmt.Errorf("accepted HTLC has no asset balances")
		}
		for _, balance := range data.Balances {
			if balance == nil || (len(info.AssetId) != 0 &&
				balance.AssetID != hex.EncodeToString(info.AssetId)) {

				return 0, fmt.Errorf("unexpected asset in accepted HTLC")
			}
			if math.MaxUint64-total < balance.Amount {
				return 0, fmt.Errorf("asset HTLC amount overflow")
			}
			total += balance.Amount
		}
	}
	if total < info.MinAssetAmount {
		return total, fmt.Errorf("received asset output %d is below "+
			"minimum %d", total, info.MinAssetAmount)
	}
	return total, nil
}

// settleAssetInvoice releases the preimage only after checking the actual
// asset units. A short MPP payment is canceled, leaving the on-chain timeout
// path available. This check also runs after resuming a persisted swap.
func (s *loopInSwap) settleAssetInvoice(ctx context.Context,
	update lndclient.InvoiceUpdate) error {

	_, err := receivedAssetUnits(s.AssetSwapInfo, update.Htlcs)
	if err != nil {
		s.log.Warnf("Rejecting asset invoice: %v", err)
		return s.lnd.Invoices.CancelInvoice(ctx, s.hash)
	}
	return s.lnd.Invoices.SettleInvoice(ctx, s.Preimage)
}
