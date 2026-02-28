package liquidity

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/liquidity/script"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// scriptEvaluator is a cached Starlark evaluator for scriptable autoloop.
var scriptEvaluator *script.Evaluator

// getScriptEvaluator returns a cached Starlark evaluator, creating one if
// needed.
func getScriptEvaluator() (*script.Evaluator, error) {
	if scriptEvaluator != nil {
		return scriptEvaluator, nil
	}
	eval, err := script.NewEvaluator()
	if err != nil {
		return nil, err
	}
	scriptEvaluator = eval
	return scriptEvaluator, nil
}

// scriptableAutoLoop executes Starlark-based autoloop logic.
func (m *Manager) scriptableAutoLoop(ctx context.Context) error {
	// Get the script from parameters.
	scriptCode := m.params.ScriptableScript
	if scriptCode == "" {
		return fmt.Errorf("scriptable autoloop enabled but no script configured")
	}

	// Get the Starlark evaluator.
	eval, err := getScriptEvaluator()
	if err != nil {
		return fmt.Errorf("failed to get script evaluator: %w", err)
	}

	// Build the autoloop context for Starlark.
	scriptCtx, err := m.buildScriptContext(ctx)
	if err != nil {
		return fmt.Errorf("failed to build script context: %w", err)
	}

	// Evaluate the Starlark script.
	log.Debugf("scriptable autoloop: evaluating Starlark script")
	decisions, err := eval.Evaluate(scriptCode, scriptCtx)
	if err != nil {
		return fmt.Errorf("Starlark evaluation failed: %w", err)
	}

	if len(decisions) == 0 {
		log.Debugf("scriptable autoloop: no swap decisions")
		return nil
	}

	log.Infof("scriptable autoloop: Starlark script returned %d decisions",
		len(decisions))

	// Sort decisions by priority.
	script.SortByPriority(decisions)

	// Validate decisions.
	if err := script.ValidateDecisions(decisions, scriptCtx); err != nil {
		return fmt.Errorf("invalid swap decisions: %w", err)
	}

	// Dispatch swaps based on decisions.
	for _, d := range decisions {
		// Check if we have room for more in-flight swaps.
		if scriptCtx.InFlight.TotalCount >= scriptCtx.InFlight.MaxAllowed {
			log.Debugf("scriptable autoloop: max in-flight reached, "+
				"skipping remaining %d decisions", len(decisions))
			break
		}

		switch d.Type {
		case script.SwapTypeLoopOut:
			err := m.dispatchScriptableLoopOut(ctx, d, scriptCtx)
			if err != nil {
				log.Errorf("scriptable autoloop: loop out "+
					"dispatch failed: %v", err)
				continue
			}
			scriptCtx.InFlight.TotalCount++

		case script.SwapTypeLoopIn:
			err := m.dispatchScriptableLoopIn(ctx, d)
			if err != nil {
				log.Errorf("scriptable autoloop: loop in "+
					"dispatch failed: %v", err)
				continue
			}
			scriptCtx.InFlight.TotalCount++
		}
	}

	return nil
}

// buildScriptContext creates the AutoloopContext for Starlark script
// evaluation.
func (m *Manager) buildScriptContext(
	ctx context.Context) (*script.AutoloopContext, error) {

	// Get existing swaps.
	loopOut, err := m.cfg.ListLoopOut(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list loop outs: %w", err)
	}

	loopIn, err := m.cfg.ListLoopIn(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list loop ins: %w", err)
	}

	// Get swap summary for budget and in-flight info.
	summary := m.checkExistingAutoLoops(ctx, loopOut, loopIn)

	// Get swap traffic for failure and ongoing swap info.
	traffic := m.currentSwapTraffic(loopOut, loopIn)

	// Get all channels.
	channels, err := m.cfg.Lnd.Client.ListChannels(ctx, false, false)
	if err != nil {
		return nil, fmt.Errorf("failed to list channels: %w", err)
	}

	// Get server restrictions.
	loopOutRestrictions, err := m.cfg.Restrictions(
		ctx, swap.TypeOut, getInitiator(m.params),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get loop out restrictions: %w", err)
	}

	loopInRestrictions, err := m.cfg.Restrictions(
		ctx, swap.TypeIn, getInitiator(m.params),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get loop in restrictions: %w", err)
	}

	// Build channel info list and calculate totals.
	var totalLocal, totalRemote, totalCapacity int64
	channelInfos := make([]script.ChannelInfo, 0, len(channels))
	peerMap := make(map[string]*script.PeerInfo)

	for _, ch := range channels {
		isCustom := channelIsCustom(ch)

		// Skip custom channels for totals calculation.
		if !isCustom {
			totalLocal += int64(ch.LocalBalance)
			totalRemote += int64(ch.RemoteBalance)
			totalCapacity += int64(ch.Capacity)
		}

		peerPubkey := hex.EncodeToString(ch.PubKeyBytes[:])
		shortChanID := lnwire.NewShortChanIDFromInt(ch.ChannelID)

		// Calculate percentages.
		var localPercent, remotePercent float64
		if ch.Capacity > 0 {
			localPercent = float64(ch.LocalBalance) / float64(ch.Capacity) * 100
			remotePercent = float64(ch.RemoteBalance) / float64(ch.Capacity) * 100
		}

		// Check for ongoing swaps.
		hasLoopOut := traffic.ongoingLoopOut[shortChanID]
		hasLoopIn := traffic.ongoingLoopIn[ch.PubKeyBytes]

		// Check for recent failures.
		var failedAt int64
		recentlyFailed := false
		if failTime, ok := traffic.failedLoopOut[shortChanID]; ok {
			failedAt = failTime.Unix()
			recentlyFailed = true
		}

		info := script.ChannelInfo{
			ChannelID:       ch.ChannelID,
			PeerPubkey:      peerPubkey,
			Capacity:        int64(ch.Capacity),
			LocalBalance:    int64(ch.LocalBalance),
			RemoteBalance:   int64(ch.RemoteBalance),
			Active:          ch.Active,
			Private:         ch.Private,
			LocalPercent:    localPercent,
			RemotePercent:   remotePercent,
			HasLoopOutSwap:  hasLoopOut,
			HasLoopInSwap:   hasLoopIn,
			RecentlyFailed:  recentlyFailed,
			FailedAt:        failedAt,
			IsCustomChannel: isCustom,
		}
		channelInfos = append(channelInfos, info)

		// Aggregate peer info.
		peer, ok := peerMap[peerPubkey]
		if !ok {
			peer = &script.PeerInfo{
				Pubkey:        peerPubkey,
				HasLoopInSwap: hasLoopIn,
			}
			peerMap[peerPubkey] = peer
		}
		peer.TotalCapacity += int64(ch.Capacity)
		peer.TotalLocal += int64(ch.LocalBalance)
		peer.TotalRemote += int64(ch.RemoteBalance)
		peer.ChannelCount++
		peer.ChannelIDs = append(peer.ChannelIDs, ch.ChannelID)
	}

	// Calculate peer percentages.
	peers := make([]script.PeerInfo, 0, len(peerMap))
	for _, p := range peerMap {
		if p.TotalCapacity > 0 {
			p.LocalPercent = float64(p.TotalLocal) / float64(p.TotalCapacity) * 100
			p.RemotePercent = float64(p.TotalRemote) / float64(p.TotalCapacity) * 100
		}
		peers = append(peers, *p)
	}

	// Build in-flight channel/peer lists.
	loopOutChannelIDs := make([]uint64, 0, len(traffic.ongoingLoopOut))
	for chanID := range traffic.ongoingLoopOut {
		loopOutChannelIDs = append(loopOutChannelIDs, chanID.ToUint64())
	}
	loopInPeerList := make([]string, 0, len(traffic.ongoingLoopIn))
	for peer := range traffic.ongoingLoopIn {
		loopInPeerList = append(loopInPeerList, hex.EncodeToString(peer[:]))
	}

	inFlight := script.InFlightInfo{
		LoopOutCount:    len(traffic.ongoingLoopOut),
		LoopInCount:     len(traffic.ongoingLoopIn),
		TotalCount:      summary.inFlightCount,
		MaxAllowed:      m.params.MaxAutoInFlight,
		LoopOutChannels: loopOutChannelIDs,
		LoopInPeers:     loopInPeerList,
	}

	// Build budget info.
	budget := script.BudgetInfo{
		TotalBudget:     int64(m.params.AutoFeeBudget),
		SpentAmount:     int64(summary.spentFees),
		PendingAmount:   int64(summary.pendingFees),
		RemainingAmount: int64(m.params.AutoFeeBudget) - int64(summary.spentFees) - int64(summary.pendingFees),
	}

	return &script.AutoloopContext{
		Channels:      channelInfos,
		Peers:         peers,
		TotalLocal:    totalLocal,
		TotalRemote:   totalRemote,
		TotalCapacity: totalCapacity,
		Restrictions: script.SwapRestrictions{
			MinLoopOut: int64(loopOutRestrictions.Minimum),
			MaxLoopOut: int64(loopOutRestrictions.Maximum),
			MinLoopIn:  int64(loopInRestrictions.Minimum),
			MaxLoopIn:  int64(loopInRestrictions.Maximum),
		},
		Budget:      budget,
		InFlight:    inFlight,
		CurrentTime: m.cfg.Clock.Now(),
	}, nil
}

// dispatchScriptableLoopOut dispatches a loop out swap based on a script
// decision.
func (m *Manager) dispatchScriptableLoopOut(ctx context.Context,
	d script.SwapDecision, scriptCtx *script.AutoloopContext) error {

	if len(d.ChannelIDs) == 0 {
		return fmt.Errorf("no channel IDs specified for loop out")
	}

	// Get the first channel to determine the peer.
	chanID := d.ChannelIDs[0]
	var peerPubkey route.Vertex
	for _, ch := range scriptCtx.Channels {
		if ch.ChannelID == chanID {
			pubkeyBytes, err := hex.DecodeString(ch.PeerPubkey)
			if err != nil {
				return fmt.Errorf("invalid peer pubkey: %w", err)
			}
			copy(peerPubkey[:], pubkeyBytes)
			break
		}
	}

	// Build outgoing channel set.
	outgoing := make([]lnwire.ShortChannelID, len(d.ChannelIDs))
	for i, id := range d.ChannelIDs {
		outgoing[i] = lnwire.NewShortChanIDFromInt(id)
	}

	// Use default fee params for scriptable mode (similar to easy autoloop).
	params := m.params
	switch feeLimit := params.FeeLimit.(type) {
	case *FeePortion:
		if feeLimit.PartsPerMillion == 0 {
			params.FeeLimit = &FeePortion{
				PartsPerMillion: defaultFeePPM,
			}
		}
	default:
		params.FeeLimit = &FeePortion{
			PartsPerMillion: defaultFeePPM,
		}
	}

	// Build the swap.
	builder := newLoopOutBuilder(m.cfg)
	swapAmt := btcutil.Amount(d.Amount)

	suggestion, err := builder.buildSwap(ctx, peerPubkey, outgoing, swapAmt, params)
	if err != nil {
		return fmt.Errorf("failed to build swap: %w", err)
	}

	loopOutSuggestion, ok := suggestion.(*loopOutSwapSuggestion)
	if !ok {
		return fmt.Errorf("unexpected suggestion type: %T", suggestion)
	}

	log.Infof("scriptable autoloop: dispatching loop out for %v sats "+
		"via channel(s) %v", d.Amount, d.ChannelIDs)

	// Dispatch the sticky loop out.
	go m.dispatchStickyLoopOut(
		ctx, loopOutSuggestion.OutRequest,
		defaultAmountBackoffRetry, defaultAmountBackoff,
	)

	return nil
}

// dispatchScriptableLoopIn dispatches a loop in swap based on a script
// decision.
func (m *Manager) dispatchScriptableLoopIn(ctx context.Context,
	d script.SwapDecision) error {

	if d.PeerPubkey == "" {
		return fmt.Errorf("no peer pubkey specified for loop in")
	}

	// Decode peer pubkey.
	pubkeyBytes, err := hex.DecodeString(d.PeerPubkey)
	if err != nil {
		return fmt.Errorf("invalid peer pubkey: %w", err)
	}
	var lastHop route.Vertex
	copy(lastHop[:], pubkeyBytes)

	// Build the loop in request.
	params := m.params
	switch feeLimit := params.FeeLimit.(type) {
	case *FeePortion:
		if feeLimit.PartsPerMillion == 0 {
			params.FeeLimit = &FeePortion{
				PartsPerMillion: defaultFeePPM,
			}
		}
	default:
		params.FeeLimit = &FeePortion{
			PartsPerMillion: defaultFeePPM,
		}
	}

	// Get a quote for the loop in.
	quote, err := m.cfg.LoopInQuote(ctx, &loop.LoopInQuoteRequest{
		Amount:         btcutil.Amount(d.Amount),
		HtlcConfTarget: params.HtlcConfTarget,
		LastHop:        &lastHop,
		Initiator:      getInitiator(params),
	})
	if err != nil {
		return fmt.Errorf("failed to get loop in quote: %w", err)
	}

	// Check fees.
	feeLimit, ok := params.FeeLimit.(*FeePortion)
	if !ok {
		return fmt.Errorf("unexpected fee limit type for loop in")
	}

	maxFee := ppmToSat(btcutil.Amount(d.Amount), feeLimit.PartsPerMillion)
	totalFees := quote.SwapFee + quote.MinerFee
	if totalFees > maxFee {
		return fmt.Errorf("loop in fees %v exceed max %v", totalFees, maxFee)
	}

	log.Infof("scriptable autoloop: dispatching loop in for %v sats "+
		"with peer %s", d.Amount, d.PeerPubkey)

	// Dispatch the loop in.
	_, err = m.cfg.LoopIn(ctx, &loop.LoopInRequest{
		Amount:         btcutil.Amount(d.Amount),
		MaxSwapFee:     quote.SwapFee,
		MaxMinerFee:    quote.MinerFee,
		HtlcConfTarget: params.HtlcConfTarget,
		LastHop:        &lastHop,
		Initiator:      getInitiator(params),
	})
	if err != nil {
		return fmt.Errorf("failed to dispatch loop in: %w", err)
	}

	return nil
}
