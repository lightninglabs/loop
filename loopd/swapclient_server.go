package loopd

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightninglabs/aperture/l402"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/assets"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/instantout"
	"github.com/lightninglabs/loop/instantout/reservation"
	"github.com/lightninglabs/loop/labels"
	"github.com/lightninglabs/loop/liquidity"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/loop/staticaddr/address"
	"github.com/lightninglabs/loop/staticaddr/deposit"
	"github.com/lightninglabs/loop/staticaddr/loopin"
	"github.com/lightninglabs/loop/staticaddr/withdraw"
	"github.com/lightninglabs/loop/swap"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/queue"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/zpay32"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	completedSwapsCount = 5

	// minConfTarget is the minimum confirmation target we'll allow clients
	// to specify. This is driven by the minimum confirmation target allowed
	// by the backing fee estimator.
	minConfTarget = 2

	defaultLoopdInitiator = "loopd"
)

var (
	// errIncorrectChain is returned when the format of the
	// destination address provided does not match the active chain.
	errIncorrectChain = errors.New("invalid address format for the " +
		"active chain")

	// errConfTargetTooLow is returned when the chosen confirmation target
	// is below the allowed minimum.
	errConfTargetTooLow = errors.New("confirmation target too low")

	// errBalanceTooLow is returned when the loop out amount can't be
	// satisfied given total balance of the selection of channels to loop
	// out on.
	errBalanceTooLow = errors.New(
		"channel balance too low for loop out amount",
	)

	// errInvalidAddress is returned when the destination address is of
	// an unsupported format such as P2PK or P2TR addresses.
	errInvalidAddress = errors.New(
		"invalid or unsupported address",
	)
)

// swapClientServer implements the grpc service exposed by loopd.
type swapClientServer struct {
	// Required by the grpc-gateway/v2 library for forward compatibility.
	looprpc.UnimplementedSwapClientServer
	looprpc.UnimplementedDebugServer

	config               *Config
	network              lndclient.Network
	impl                 *loop.Client
	liquidityMgr         *liquidity.Manager
	lnd                  *lndclient.LndServices
	reservationManager   *reservation.Manager
	instantOutManager    *instantout.Manager
	staticAddressManager *address.Manager
	depositManager       *deposit.Manager
	withdrawalManager    *withdraw.Manager
	staticLoopInManager  *loopin.Manager
	assetClient          *assets.TapdClient
	swaps                map[lntypes.Hash]loop.SwapInfo
	subscribers          map[int]chan<- interface{}
	statusChan           chan loop.SwapInfo
	nextSubscriberID     int
	swapsLock            sync.Mutex
	mainCtx              context.Context
}

// LoopOut initiates a loop out swap with the given parameters. The call returns
// after the swap has been set up with the swap server. From that point onwards,
// progress can be tracked via the LoopOutStatus stream that is returned from
// Monitor().
func (s *swapClientServer) LoopOut(ctx context.Context,
	in *looprpc.LoopOutRequest) (
	*looprpc.SwapResponse, error) {

	log.Infof("Loop out request received")

	// Note that LoopOutRequest.PaymentTimeout is unsigned and therefore
	// cannot be negative.
	paymentTimeout := time.Duration(in.PaymentTimeout) * time.Second

	// Make sure we don't exceed the total allowed payment timeout.
	if paymentTimeout > s.config.TotalPaymentTimeout {
		return nil, fmt.Errorf("payment timeout %v exceeds maximum "+
			"allowed timeout of %v", paymentTimeout,
			s.config.TotalPaymentTimeout)
	}

	var sweepAddr btcutil.Address
	var isExternalAddr bool
	var err error
	//nolint:lll
	switch {
	case in.Dest != "" && in.Account != "":
		return nil, fmt.Errorf("destination address and external " +
			"account address cannot be set at the same time")

	case in.Dest != "":
		// Decode the client provided destination address for the loop
		// out sweep.
		sweepAddr, err = btcutil.DecodeAddress(
			in.Dest, s.lnd.ChainParams,
		)
		if err != nil {
			return nil, fmt.Errorf("decode address: %v", err)
		}

		isExternalAddr = true

	case in.Account != "" && in.AccountAddrType == looprpc.AddressType_ADDRESS_TYPE_UNKNOWN:
		return nil, liquidity.ErrAccountAndAddrType

	case in.Account != "":
		// Derive a new receiving address from the stated account.
		addrType, err := toWalletAddrType(in.AccountAddrType)
		if err != nil {
			return nil, err
		}

		// Check if account with address type exists.
		if !s.accountExists(ctx, in.Account, addrType) {
			return nil, fmt.Errorf("the provided account does " +
				"not exist")
		}

		sweepAddr, err = s.lnd.WalletKit.NextAddr(
			ctx, in.Account, addrType, false,
		)
		if err != nil {
			return nil, fmt.Errorf("NextAddr from account error: "+
				"%v", err)
		}

		isExternalAddr = true

	default:
		// Generate sweep address if none specified.
		sweepAddr, err = s.lnd.WalletKit.NextAddr(
			context.Background(), "",
			walletrpc.AddressType_WITNESS_PUBKEY_HASH, false,
		)
		if err != nil {
			return nil, fmt.Errorf("NextAddr error: %v", err)
		}
	}

	sweepConfTarget, err := validateLoopOutRequest(
		ctx, s.lnd.Client, s.lnd.ChainParams, in, sweepAddr,
		s.impl.LoopOutMaxParts,
	)
	if err != nil {
		return nil, err
	}

	// Infer if the publication deadline is set in milliseconds.
	publicationDeadline := getPublicationDeadline(in.SwapPublicationDeadline)

	req := &loop.OutRequest{
		Amount:                  btcutil.Amount(in.Amt),
		DestAddr:                sweepAddr,
		IsExternalAddr:          isExternalAddr,
		MaxMinerFee:             btcutil.Amount(in.MaxMinerFee),
		MaxPrepayAmount:         btcutil.Amount(in.MaxPrepayAmt),
		MaxPrepayRoutingFee:     btcutil.Amount(in.MaxPrepayRoutingFee),
		MaxSwapRoutingFee:       btcutil.Amount(in.MaxSwapRoutingFee),
		MaxSwapFee:              btcutil.Amount(in.MaxSwapFee),
		SweepConfTarget:         sweepConfTarget,
		HtlcConfirmations:       in.HtlcConfirmations,
		SwapPublicationDeadline: publicationDeadline,
		Label:                   in.Label,
		Initiator:               in.Initiator,
		PaymentTimeout:          paymentTimeout,
	}

	// If the asset id is set, we need to set the asset amount and asset id
	// in the request.
	if in.AssetInfo != nil {
		if len(in.AssetInfo.AssetId) != 0 &&
			len(in.AssetInfo.AssetId) != 32 {

			return nil, fmt.Errorf(
				"asset id must be set to a 32 byte value",
			)
		}

		if len(in.AssetRfqInfo.PrepayRfqId) != 0 &&
			len(in.AssetRfqInfo.PrepayRfqId) != 32 {

			return nil, fmt.Errorf(
				"prepay rfq id must be set to a 32 byte value",
			)
		}

		if len(in.AssetRfqInfo.SwapRfqId) != 0 &&
			len(in.AssetRfqInfo.SwapRfqId) != 32 {

			return nil, fmt.Errorf(
				"swap rfq id must be set to a 32 byte value",
			)
		}

		req.AssetId = in.AssetInfo.AssetId
		req.AssetPrepayRfqId = in.AssetRfqInfo.PrepayRfqId
		req.AssetSwapRfqId = in.AssetRfqInfo.SwapRfqId
	}

	switch {
	case in.LoopOutChannel != 0 && len(in.OutgoingChanSet) > 0: // nolint:staticcheck
		return nil, errors.New("loop_out_channel and outgoing_" +
			"chan_ids are mutually exclusive")

	case in.LoopOutChannel != 0: // nolint:staticcheck
		req.OutgoingChanSet = loopdb.ChannelSet{in.LoopOutChannel} // nolint:staticcheck

	default:
		req.OutgoingChanSet = in.OutgoingChanSet
	}

	info, err := s.impl.LoopOut(ctx, req)
	if err != nil {
		log.Errorf("LoopOut: %v", err)
		return nil, err
	}

	htlcAddress := info.HtlcAddress.String()
	resp := &looprpc.SwapResponse{
		Id:            info.SwapHash.String(),
		IdBytes:       info.SwapHash[:],
		HtlcAddress:   htlcAddress,
		ServerMessage: info.ServerMessage,
	}

	if loopdb.CurrentProtocolVersion() < loopdb.ProtocolVersionHtlcV3 {
		resp.HtlcAddressP2Wsh = htlcAddress
	} else {
		resp.HtlcAddressP2Tr = htlcAddress
	}

	return resp, nil
}

// accountExists returns true if account under the address type exists in the
// backing lnd instance and false otherwise.
func (s *swapClientServer) accountExists(ctx context.Context, account string,
	addrType walletrpc.AddressType) bool {

	accounts, err := s.lnd.WalletKit.ListAccounts(ctx, account, addrType)
	if err != nil {
		return false
	}

	for _, a := range accounts {
		if a.Name == account {
			return true
		}
	}

	return false
}

func toWalletAddrType(addrType looprpc.AddressType) (walletrpc.AddressType,
	error) {

	switch addrType {
	case looprpc.AddressType_TAPROOT_PUBKEY:
		return walletrpc.AddressType_TAPROOT_PUBKEY, nil

	default:
		return walletrpc.AddressType_UNKNOWN,
			fmt.Errorf("unknown address type")
	}
}

func (s *swapClientServer) marshallSwap(ctx context.Context,
	loopSwap *loop.SwapInfo) (*looprpc.SwapStatus, error) {

	var (
		state         looprpc.SwapState
		failureReason = looprpc.FailureReason_FAILURE_REASON_NONE
	)

	// Set our state var for non-failure states. If we get a failure, we
	// will update our failure reason. To remain backwards compatible with
	// previous versions where we squashed all failure reasons to a single
	// failure state, we set a failure reason for all our different failure
	// states, and set our failed state for all of them.
	switch loopSwap.State {
	case loopdb.StateInitiated:
		state = looprpc.SwapState_INITIATED

	case loopdb.StatePreimageRevealed:
		state = looprpc.SwapState_PREIMAGE_REVEALED

	case loopdb.StateHtlcPublished:
		state = looprpc.SwapState_HTLC_PUBLISHED

	case loopdb.StateInvoiceSettled:
		state = looprpc.SwapState_INVOICE_SETTLED

	case loopdb.StateSuccess:
		state = looprpc.SwapState_SUCCESS

	case loopdb.StateFailOffchainPayments:
		failureReason = looprpc.FailureReason_FAILURE_REASON_OFFCHAIN

	case loopdb.StateFailTimeout:
		failureReason = looprpc.FailureReason_FAILURE_REASON_TIMEOUT

	case loopdb.StateFailSweepTimeout:
		failureReason = looprpc.FailureReason_FAILURE_REASON_SWEEP_TIMEOUT

	case loopdb.StateFailInsufficientValue:
		failureReason = looprpc.FailureReason_FAILURE_REASON_INSUFFICIENT_VALUE

	case loopdb.StateFailTemporary:
		failureReason = looprpc.FailureReason_FAILURE_REASON_TEMPORARY

	case loopdb.StateFailIncorrectHtlcAmt:
		failureReason = looprpc.FailureReason_FAILURE_REASON_INCORRECT_AMOUNT

	case loopdb.StateFailAbandoned:
		failureReason = looprpc.FailureReason_FAILURE_REASON_ABANDONED

	case loopdb.StateFailInsufficientConfirmedBalance:
		failureReason = looprpc.FailureReason_FAILURE_REASON_INSUFFICIENT_CONFIRMED_BALANCE

	case loopdb.StateFailIncorrectHtlcAmtSwept:
		failureReason = looprpc.FailureReason_FAILURE_REASON_INCORRECT_HTLC_AMT_SWEPT

	default:
		return nil, fmt.Errorf("unknown swap state: %v", loopSwap.State)
	}

	// If we have a failure reason, we have a failure state, so should use
	// our catchall failed state.
	if failureReason != looprpc.FailureReason_FAILURE_REASON_NONE {
		state = looprpc.SwapState_FAILED
	}

	var swapType looprpc.SwapType
	var (
		htlcAddress      string
		htlcAddressP2TR  string
		htlcAddressP2WSH string
	)
	var outGoingChanSet []uint64
	var lastHop []byte
	var assetInfo *looprpc.AssetLoopOutInfo

	switch loopSwap.SwapType {
	case swap.TypeIn:
		swapType = looprpc.SwapType_LOOP_IN

		if loopSwap.HtlcAddressP2TR != nil {
			htlcAddressP2TR = loopSwap.HtlcAddressP2TR.EncodeAddress()
			htlcAddress = htlcAddressP2TR
		} else {
			htlcAddressP2WSH =
				loopSwap.HtlcAddressP2WSH.EncodeAddress()
			htlcAddress = htlcAddressP2WSH
		}

		if loopSwap.LastHop != nil {
			lastHop = loopSwap.LastHop[:]
		}

	case swap.TypeOut:
		swapType = looprpc.SwapType_LOOP_OUT
		if loopSwap.HtlcAddressP2WSH != nil {
			htlcAddressP2WSH = loopSwap.HtlcAddressP2WSH.EncodeAddress()
			htlcAddress = htlcAddressP2WSH
		} else {
			htlcAddressP2TR = loopSwap.HtlcAddressP2TR.EncodeAddress()
			htlcAddress = htlcAddressP2TR
		}

		outGoingChanSet = loopSwap.OutgoingChanSet

		if loopSwap.AssetSwapInfo != nil {
			assetName, err := s.assetClient.GetAssetName(
				ctx, loopSwap.AssetSwapInfo.AssetId,
			)
			if err != nil {
				return nil, err
			}

			assetInfo = &looprpc.AssetLoopOutInfo{
				AssetId: hex.EncodeToString(loopSwap.AssetSwapInfo.AssetId), // nolint:lll
				AssetCostOffchain: loopSwap.AssetSwapInfo.PrepayPaidAmt +
					loopSwap.AssetSwapInfo.SwapPaidAmt, // nolint:lll
				AssetName: assetName,
			}
		}

	default:
		return nil, errors.New("unknown swap type")
	}

	return &looprpc.SwapStatus{
		Amt:              int64(loopSwap.AmountRequested),
		Id:               loopSwap.SwapHash.String(),
		IdBytes:          loopSwap.SwapHash[:],
		State:            state,
		FailureReason:    failureReason,
		InitiationTime:   loopSwap.InitiationTime.UnixNano(),
		LastUpdateTime:   loopSwap.LastUpdate.UnixNano(),
		HtlcAddress:      htlcAddress,
		HtlcAddressP2Tr:  htlcAddressP2TR,
		HtlcAddressP2Wsh: htlcAddressP2WSH,
		Type:             swapType,
		CostServer:       int64(loopSwap.Cost.Server),
		CostOnchain:      int64(loopSwap.Cost.Onchain),
		CostOffchain:     int64(loopSwap.Cost.Offchain),
		Label:            loopSwap.Label,
		LastHop:          lastHop,
		OutgoingChanSet:  outGoingChanSet,
		AssetInfo:        assetInfo,
	}, nil
}

// Monitor will return a stream of swap updates for currently active swaps.
func (s *swapClientServer) Monitor(in *looprpc.MonitorRequest,
	server looprpc.SwapClient_MonitorServer) error {

	log.Infof("Monitor request received")

	send := func(info loop.SwapInfo) error {
		rpcSwap, err := s.marshallSwap(server.Context(), &info)
		if err != nil {
			return err
		}

		return server.Send(rpcSwap)
	}

	// Start a notification queue for this subscriber.
	queue := queue.NewConcurrentQueue(20)
	queue.Start()

	// Add this subscriber to the global subscriber list. Also create a
	// snapshot of all pending and completed swaps within the lock, to
	// prevent subscribers from receiving duplicate updates.
	s.swapsLock.Lock()

	id := s.nextSubscriberID
	s.nextSubscriberID++
	s.subscribers[id] = queue.ChanIn()

	var pendingSwaps, completedSwaps []loop.SwapInfo
	for _, swap := range s.swaps {
		if swap.State.Type() == loopdb.StateTypePending {
			pendingSwaps = append(pendingSwaps, swap)
		} else {
			completedSwaps = append(completedSwaps, swap)
		}
	}

	s.swapsLock.Unlock()

	defer func() {
		s.swapsLock.Lock()
		delete(s.subscribers, id)
		s.swapsLock.Unlock()
		queue.Stop()
	}()

	// Sort completed swaps new to old.
	sort.Slice(completedSwaps, func(i, j int) bool {
		return completedSwaps[i].LastUpdate.After(
			completedSwaps[j].LastUpdate,
		)
	})

	// Discard all but top x latest.
	if len(completedSwaps) > completedSwapsCount {
		completedSwaps = completedSwaps[:completedSwapsCount]
	}

	// Concatenate both sets.
	filteredSwaps := append(pendingSwaps, completedSwaps...) // nolint: gocritic

	// Sort again, but this time old to new.
	sort.Slice(filteredSwaps, func(i, j int) bool {
		return filteredSwaps[i].LastUpdate.Before(
			filteredSwaps[j].LastUpdate,
		)
	})

	// Return swaps to caller.
	for _, swap := range filteredSwaps {
		if err := send(swap); err != nil {
			return err
		}
	}

	// As long as the client is connected, keep passing through swap
	// updates.
	for {
		select {
		case queueItem, ok := <-queue.ChanOut():
			if !ok {
				return nil
			}

			swap := queueItem.(loop.SwapInfo)
			if err := send(swap); err != nil {
				return err
			}

		// The client cancels the subscription.
		case <-server.Context().Done():
			return nil

		// The server is shutting down.
		case <-s.mainCtx.Done():
			return fmt.Errorf("server is shutting down")
		}
	}
}

// ListSwaps returns a list of all currently known swaps and their current
// status.
func (s *swapClientServer) ListSwaps(ctx context.Context,
	req *looprpc.ListSwapsRequest) (*looprpc.ListSwapsResponse, error) {

	var (
		rpcSwaps = []*looprpc.SwapStatus{}
		idx      = 0
	)

	s.swapsLock.Lock()
	defer s.swapsLock.Unlock()

	// We can just use the server's in-memory cache as that contains the
	// most up-to-date state including temporary failures which aren't
	// persisted to disk. The swaps field is a map, that's why we need an
	// additional index.
	for _, swp := range s.swaps {
		swp := swp

		// Filter the swap based on the provided filter.
		if !filterSwap(&swp, req.ListSwapFilter) {
			continue
		}

		rpcSwap, err := s.marshallSwap(ctx, &swp)
		if err != nil {
			return nil, err
		}
		rpcSwaps = append(rpcSwaps, rpcSwap)
		idx++
	}
	return &looprpc.ListSwapsResponse{Swaps: rpcSwaps}, nil
}

// filterSwap filters the given swap based on the provided filter.
func filterSwap(swapInfo *loop.SwapInfo, filter *looprpc.ListSwapsFilter) bool {
	if filter == nil {
		return true
	}

	// If the swap type filter is set, we only return swaps that match the
	// filter.
	if filter.SwapType != looprpc.ListSwapsFilter_ANY {
		switch filter.SwapType {
		case looprpc.ListSwapsFilter_LOOP_IN:
			if swapInfo.SwapType != swap.TypeIn {
				return false
			}

		case looprpc.ListSwapsFilter_LOOP_OUT:
			if swapInfo.SwapType != swap.TypeOut {
				return false
			}
		}
	}

	// If the pending only filter is set, we only return pending swaps.
	if filter.PendingOnly && !swapInfo.State.IsPending() {
		return false
	}

	// If the swap is of type loop out and the outgoing channel filter is
	// set, we only return swaps that match the filter.
	if swapInfo.SwapType == swap.TypeOut && filter.OutgoingChanSet != nil {
		// First we sort both channel sets to make sure we can compare
		// them.
		sort.Slice(swapInfo.OutgoingChanSet, func(i, j int) bool {
			return swapInfo.OutgoingChanSet[i] <
				swapInfo.OutgoingChanSet[j]
		})
		sort.Slice(filter.OutgoingChanSet, func(i, j int) bool {
			return filter.OutgoingChanSet[i] <
				filter.OutgoingChanSet[j]
		})

		// Compare the outgoing channel set by using reflect.DeepEqual
		// which compares the underlying arrays.
		if !reflect.DeepEqual(swapInfo.OutgoingChanSet,
			filter.OutgoingChanSet) {

			return false
		}
	}

	// If the swap is of type loop in and the last hop filter is set, we
	// only return swaps that match the filter.
	if swapInfo.SwapType == swap.TypeIn && filter.LoopInLastHop != nil {
		// Compare the last hop by using reflect.DeepEqual which
		// compares the underlying arrays.
		if !reflect.DeepEqual(swapInfo.LastHop, filter.LoopInLastHop) {
			return false
		}
	}

	// If a label filter is set, we only return swaps that softly match the
	// filter.
	if filter.Label != "" {
		if !strings.Contains(swapInfo.Label, filter.Label) {
			return false
		}
	}

	// If we only want to return asset swaps, we only return swaps that have
	// an asset id set.
	if filter.AssetSwapOnly && swapInfo.AssetSwapInfo == nil {
		return false
	}

	return true
}

// SwapInfo returns all known details about a single swap.
func (s *swapClientServer) SwapInfo(ctx context.Context,
	req *looprpc.SwapInfoRequest) (*looprpc.SwapStatus, error) {

	swapHash, err := lntypes.MakeHash(req.Id)
	if err != nil {
		return nil, fmt.Errorf("error parsing swap hash: %v", err)
	}

	// Just return the server's in-memory cache here too as we also want to
	// return temporary failures to the client.
	swp, ok := s.swaps[swapHash]
	if !ok {
		return nil, fmt.Errorf("swap with hash %s not found", req.Id)
	}
	return s.marshallSwap(ctx, &swp)
}

// AbandonSwap requests the server to abandon a swap with the given hash.
func (s *swapClientServer) AbandonSwap(ctx context.Context,
	req *looprpc.AbandonSwapRequest) (*looprpc.AbandonSwapResponse,
	error) {

	if !req.IKnowWhatIAmDoing {
		return nil, fmt.Errorf("please read the AbandonSwap API " +
			"documentation")
	}

	swapHash, err := lntypes.MakeHash(req.Id)
	if err != nil {
		return nil, fmt.Errorf("error parsing swap hash: %v", err)
	}

	s.swapsLock.Lock()
	swap, ok := s.swaps[swapHash]
	s.swapsLock.Unlock()
	if !ok {
		return nil, fmt.Errorf("swap with hash %s not found", req.Id)
	}

	if swap.SwapType.IsOut() {
		return nil, fmt.Errorf("abandoning loop out swaps is not " +
			"supported yet")
	}

	// If the swap is in a final state, we cannot abandon it.
	if swap.State.IsFinal() {
		return nil, fmt.Errorf("cannot abandon swap in final state, "+
			"state = %s, hash = %s", swap.State.String(), swapHash)
	}

	err = s.impl.AbandonSwap(ctx, &loop.AbandonSwapRequest{
		SwapHash: swapHash,
	})
	if err != nil {
		return nil, fmt.Errorf("error abandoning swap: %v", err)
	}

	return &looprpc.AbandonSwapResponse{}, nil
}

// LoopOutTerms returns the terms that the server enforces for loop out swaps.
func (s *swapClientServer) LoopOutTerms(ctx context.Context,
	_ *looprpc.TermsRequest) (*looprpc.OutTermsResponse, error) {

	log.Infof("Loop out terms request received")

	terms, err := s.impl.LoopOutTerms(ctx, defaultLoopdInitiator)
	if err != nil {
		log.Errorf("Terms request: %v", err)
		return nil, err
	}

	return &looprpc.OutTermsResponse{
		MinSwapAmount: int64(terms.MinSwapAmount),
		MaxSwapAmount: int64(terms.MaxSwapAmount),
		MinCltvDelta:  terms.MinCltvDelta,
		MaxCltvDelta:  terms.MaxCltvDelta,
	}, nil
}

// LoopOutQuote returns a quote for a loop out swap with the provided
// parameters.
func (s *swapClientServer) LoopOutQuote(ctx context.Context,
	req *looprpc.QuoteRequest) (*looprpc.OutQuoteResponse, error) {

	confTarget, err := validateConfTarget(
		req.ConfTarget, loop.DefaultSweepConfTarget,
	)
	if err != nil {
		return nil, err
	}

	publicactionDeadline := getPublicationDeadline(
		req.SwapPublicationDeadline,
	)

	loopOutQuoteReq := &loop.LoopOutQuoteRequest{
		Amount:                  btcutil.Amount(req.Amt),
		SweepConfTarget:         confTarget,
		SwapPublicationDeadline: publicactionDeadline,
		Initiator:               defaultLoopdInitiator,
	}

	if req.AssetInfo != nil {
		if req.AssetInfo.AssetId == nil ||
			req.AssetInfo.AssetEdgeNode == nil {

			return nil, fmt.Errorf(
				"asset id and edge node must both be set")
		}
		loopOutQuoteReq.AssetRFQRequest = &loop.AssetRFQRequest{
			AssetId:            req.AssetInfo.AssetId,
			AssetEdgeNode:      req.AssetInfo.AssetEdgeNode,
			Expiry:             req.AssetInfo.Expiry,
			MaxLimitMultiplier: req.AssetInfo.MaxLimitMultiplier,
		}
	}

	quote, err := s.impl.LoopOutQuote(ctx, loopOutQuoteReq)
	if err != nil {
		return nil, err
	}

	response := &looprpc.OutQuoteResponse{
		HtlcSweepFeeSat: int64(quote.MinerFee),
		PrepayAmtSat:    int64(quote.PrepayAmount),
		SwapFeeSat:      int64(quote.SwapFee),
		SwapPaymentDest: quote.SwapPaymentDest[:],
		ConfTarget:      confTarget,
	}

	if quote.LoopOutRfq != nil {
		response.AssetRfqInfo = &looprpc.AssetRfqInfo{
			PrepayRfqId:    quote.LoopOutRfq.PrepayRfqId,
			PrepayAssetAmt: quote.LoopOutRfq.PrepayAssetAmt,
			SwapRfqId:      quote.LoopOutRfq.SwapRfqId,
			SwapAssetAmt:   quote.LoopOutRfq.SwapAssetAmt,
			AssetName:      quote.LoopOutRfq.AssetName,
		}
	}

	return response, nil
}

// GetLoopInTerms returns the terms that the server enforces for swaps.
func (s *swapClientServer) GetLoopInTerms(ctx context.Context,
	_ *looprpc.TermsRequest) (*looprpc.InTermsResponse, error) {

	log.Infof("Loop in terms request received")

	terms, err := s.impl.LoopInTerms(ctx, defaultLoopdInitiator)
	if err != nil {
		log.Errorf("Terms request: %v", err)
		return nil, err
	}

	return &looprpc.InTermsResponse{
		MinSwapAmount: int64(terms.MinSwapAmount),
		MaxSwapAmount: int64(terms.MaxSwapAmount),
	}, nil
}

// GetLoopInQuote returns a quote for a swap with the provided parameters.
func (s *swapClientServer) GetLoopInQuote(ctx context.Context,
	req *looprpc.QuoteRequest) (*looprpc.InQuoteResponse, error) {

	log.Infof("Loop in quote request received")

	var (
		numDeposits = uint32(len(req.DepositOutpoints))
		err         error
	)

	htlcConfTarget, err := validateLoopInRequest(
		req.ConfTarget, req.ExternalHtlc, numDeposits, req.Amt,
	)
	if err != nil {
		return nil, err
	}

	// Retrieve deposits to calculate their total value.
	var depositList *looprpc.ListStaticAddressDepositsResponse
	amount := btcutil.Amount(req.Amt)
	if len(req.DepositOutpoints) > 0 {
		depositList, err = s.ListStaticAddressDeposits(
			ctx, &looprpc.ListStaticAddressDepositsRequest{
				Outpoints: req.DepositOutpoints,
			},
		)
		if err != nil {
			return nil, err
		}

		if depositList == nil {
			return nil, fmt.Errorf("no summary returned for " +
				"deposit outpoints")
		}

		// The requested amount should be 0 here if the request
		// contained deposit outpoints.
		if amount != 0 && len(depositList.FilteredDeposits) > 0 {
			return nil, fmt.Errorf("amount should be 0 for " +
				"deposit quotes")
		}

		// In case we quote for deposits we send the server both the
		// total value and the number of deposits. This is so the server
		// can probe the total amount and calculate the per input fee.
		if amount == 0 && len(depositList.FilteredDeposits) > 0 {
			for _, deposit := range depositList.FilteredDeposits {
				amount += btcutil.Amount(deposit.Value)
			}
		}
	}

	var (
		routeHints [][]zpay32.HopHint
		lastHop    *route.Vertex
	)

	if req.LoopInLastHop != nil {
		lastHopVertex, err := route.NewVertexFromBytes(
			req.LoopInLastHop,
		)
		if err != nil {
			return nil, err
		}
		lastHop = &lastHopVertex
	}

	if len(req.LoopInRouteHints) != 0 {
		routeHints, err = unmarshallRouteHints(req.LoopInRouteHints)
		if err != nil {
			return nil, err
		}
	}

	quote, err := s.impl.LoopInQuote(ctx, &loop.LoopInQuoteRequest{
		Amount:         amount,
		HtlcConfTarget: htlcConfTarget,
		ExternalHtlc:   req.ExternalHtlc,
		LastHop:        lastHop,
		RouteHints:     routeHints,
		Private:        req.Private,
		Initiator:      defaultLoopdInitiator,
		NumDeposits:    numDeposits,
	})
	if err != nil {
		return nil, err
	}

	return &looprpc.InQuoteResponse{
		HtlcPublishFeeSat: int64(quote.MinerFee),
		SwapFeeSat:        int64(quote.SwapFee),
		ConfTarget:        htlcConfTarget,
	}, nil
}

// unmarshallRouteHints unmarshalls a list of route hints.
func unmarshallRouteHints(rpcRouteHints []*swapserverrpc.RouteHint) (
	[][]zpay32.HopHint, error) {

	routeHints := make([][]zpay32.HopHint, 0, len(rpcRouteHints))
	for _, rpcRouteHint := range rpcRouteHints {
		routeHint := make(
			[]zpay32.HopHint, 0, len(rpcRouteHint.HopHints),
		)
		for _, rpcHint := range rpcRouteHint.HopHints {
			hint, err := unmarshallHopHint(rpcHint)
			if err != nil {
				return nil, err
			}

			routeHint = append(routeHint, hint)
		}
		routeHints = append(routeHints, routeHint)
	}

	return routeHints, nil
}

// unmarshallHopHint unmarshalls a single hop hint.
func unmarshallHopHint(rpcHint *swapserverrpc.HopHint) (zpay32.HopHint, error) {
	pubBytes, err := hex.DecodeString(rpcHint.NodeId)
	if err != nil {
		return zpay32.HopHint{}, err
	}

	pubkey, err := btcec.ParsePubKey(pubBytes)
	if err != nil {
		return zpay32.HopHint{}, err
	}

	return zpay32.HopHint{
		NodeID:                    pubkey,
		ChannelID:                 rpcHint.ChanId,
		FeeBaseMSat:               rpcHint.FeeBaseMsat,
		FeeProportionalMillionths: rpcHint.FeeProportionalMillionths,
		CLTVExpiryDelta:           uint16(rpcHint.CltvExpiryDelta),
	}, nil
}

// Probe requests the server to probe the client's node to test inbound
// liquidity.
func (s *swapClientServer) Probe(ctx context.Context,
	req *looprpc.ProbeRequest) (*looprpc.ProbeResponse, error) {

	log.Infof("Probe request received")

	var lastHop *route.Vertex
	if req.LastHop != nil {
		lastHopVertex, err := route.NewVertexFromBytes(req.LastHop)
		if err != nil {
			return nil, err
		}

		lastHop = &lastHopVertex
	}

	routeHints, err := unmarshallRouteHints(req.RouteHints)
	if err != nil {
		return nil, err
	}

	err = s.impl.Probe(ctx, &loop.ProbeRequest{
		Amount:     btcutil.Amount(req.Amt),
		LastHop:    lastHop,
		RouteHints: routeHints,
	})
	if err != nil {
		return nil, err
	}

	return &looprpc.ProbeResponse{}, nil
}

func (s *swapClientServer) LoopIn(ctx context.Context,
	in *looprpc.LoopInRequest) (*looprpc.SwapResponse, error) {

	log.Infof("Loop in request received")

	htlcConfTarget, err := validateLoopInRequest(
		in.HtlcConfTarget, in.ExternalHtlc, 0, in.Amt,
	)
	if err != nil {
		return nil, err
	}

	// Check that the label is valid.
	if err := labels.Validate(in.Label); err != nil {
		return nil, err
	}

	routeHints, err := unmarshallRouteHints(in.RouteHints)
	if err != nil {
		return nil, err
	}

	req := &loop.LoopInRequest{
		Amount:         btcutil.Amount(in.Amt),
		MaxMinerFee:    btcutil.Amount(in.MaxMinerFee),
		MaxSwapFee:     btcutil.Amount(in.MaxSwapFee),
		HtlcConfTarget: htlcConfTarget,
		ExternalHtlc:   in.ExternalHtlc,
		Label:          in.Label,
		Initiator:      in.Initiator,
		Private:        in.Private,
		RouteHints:     routeHints,
	}
	if in.LastHop != nil {
		lastHop, err := route.NewVertexFromBytes(in.LastHop)
		if err != nil {
			return nil, err
		}
		req.LastHop = &lastHop
	}
	swapInfo, err := s.impl.LoopIn(ctx, req)
	if err != nil {
		log.Errorf("Loop in: %v", err)
		return nil, err
	}

	response := &looprpc.SwapResponse{
		Id:            swapInfo.SwapHash.String(),
		IdBytes:       swapInfo.SwapHash[:],
		ServerMessage: swapInfo.ServerMessage,
	}

	if loopdb.CurrentProtocolVersion() < loopdb.ProtocolVersionHtlcV3 {
		p2wshAddr := swapInfo.HtlcAddressP2WSH.String()
		response.HtlcAddress = p2wshAddr
		response.HtlcAddressP2Wsh = p2wshAddr
	} else {
		p2trAddr := swapInfo.HtlcAddressP2TR.String()
		response.HtlcAddress = p2trAddr
		response.HtlcAddressP2Tr = p2trAddr
	}

	return response, nil
}

// GetL402Tokens returns all tokens that are contained in the L402 token store.
func (s *swapClientServer) GetL402Tokens(ctx context.Context,
	_ *looprpc.TokensRequest) (*looprpc.TokensResponse, error) {

	log.Infof("Get L402 tokens request received")

	tokens, err := s.impl.L402Store.AllTokens()
	if err != nil {
		return nil, err
	}

	rpcTokens := make([]*looprpc.L402Token, len(tokens))
	idx := 0
	for key, token := range tokens {
		macBytes, err := token.BaseMacaroon().MarshalBinary()
		if err != nil {
			return nil, err
		}

		id, err := l402.DecodeIdentifier(
			bytes.NewReader(token.BaseMacaroon().Id()),
		)
		if err != nil {
			return nil, err
		}
		rpcTokens[idx] = &looprpc.L402Token{
			BaseMacaroon:       macBytes,
			PaymentHash:        token.PaymentHash[:],
			PaymentPreimage:    token.Preimage[:],
			AmountPaidMsat:     int64(token.AmountPaid),
			RoutingFeePaidMsat: int64(token.RoutingFeePaid),
			TimeCreated:        token.TimeCreated.Unix(),
			Expired:            !token.IsValid(),
			StorageName:        key,
			Id: hex.EncodeToString(
				id.TokenID[:],
			),
		}
		idx++
	}

	return &looprpc.TokensResponse{Tokens: rpcTokens}, nil
}

// GetLsatTokens returns all tokens that are contained in the L402 token store.
// Deprecated: use GetL402Tokens.
// This API is provided to maintain backward compatibility with gRPC clients
// (e.g. `loop listauth`, Terminal Web, RTL).
// Type LsatToken used by GetLsatTokens in the past was renamed to L402Token,
// but this does not affect binary encoding, so we can use type L402Token here.
func (s *swapClientServer) GetLsatTokens(ctx context.Context,
	req *looprpc.TokensRequest) (*looprpc.TokensResponse, error) {

	log.Warnf("Received deprecated call GetLsatTokens. Please update the " +
		"client software. Calling GetL402Tokens now.")

	return s.GetL402Tokens(ctx, req)
}

// FetchL402Token fetches a L402 Token from the server. This is required to
// listen for server notifications such as reservations. If a token is already
// in the local L402, nothing will happen.
func (s *swapClientServer) FetchL402Token(ctx context.Context,
	_ *looprpc.FetchL402TokenRequest) (*looprpc.FetchL402TokenResponse,
	error) {

	err := s.impl.Server.FetchL402(ctx)
	if err != nil {
		return nil, err
	}

	return &looprpc.FetchL402TokenResponse{}, nil
}

// GetInfo returns basic information about the loop daemon and details to swaps
// from the swap store.
func (s *swapClientServer) GetInfo(ctx context.Context,
	_ *looprpc.GetInfoRequest) (*looprpc.GetInfoResponse, error) {

	// Fetch loop-outs from the loop db.
	outSwaps, err := s.impl.Store.FetchLoopOutSwaps(ctx)
	if err != nil {
		return nil, err
	}

	// Collect loop-out stats.
	loopOutStats := &looprpc.LoopStats{}
	for _, out := range outSwaps {
		switch out.State().State.Type() {
		case loopdb.StateTypeSuccess:
			loopOutStats.SuccessCount++
			loopOutStats.SumSucceededAmt += int64(
				out.Contract.AmountRequested,
			)

		case loopdb.StateTypePending:
			loopOutStats.PendingCount++
			loopOutStats.SumPendingAmt += int64(
				out.Contract.AmountRequested,
			)

		case loopdb.StateTypeFail:
			loopOutStats.FailCount++
		}
	}

	// Fetch loop-ins from the loop db.
	inSwaps, err := s.impl.Store.FetchLoopInSwaps(ctx)
	if err != nil {
		return nil, err
	}

	// Collect loop-in stats.
	loopInStats := &looprpc.LoopStats{}
	for _, in := range inSwaps {
		switch in.State().State.Type() {
		case loopdb.StateTypeSuccess:
			loopInStats.SuccessCount++
			loopInStats.SumSucceededAmt += int64(
				in.Contract.AmountRequested,
			)

		case loopdb.StateTypePending:
			loopInStats.PendingCount++
			loopInStats.SumPendingAmt += int64(
				in.Contract.AmountRequested,
			)

		case loopdb.StateTypeFail:
			loopInStats.FailCount++
		}
	}

	return &looprpc.GetInfoResponse{
		Version:      loop.Version(),
		Network:      s.config.Network,
		RpcListen:    s.config.RPCListen,
		RestListen:   s.config.RESTListen,
		MacaroonPath: s.config.MacaroonPath,
		TlsCertPath:  s.config.TLSCertPath,
		LoopOutStats: loopOutStats,
		LoopInStats:  loopInStats,
	}, nil
}

// GetLiquidityParams gets our current liquidity manager's parameters.
func (s *swapClientServer) GetLiquidityParams(_ context.Context,
	_ *looprpc.GetLiquidityParamsRequest) (*looprpc.LiquidityParameters,
	error) {

	cfg := s.liquidityMgr.GetParameters()

	rpcCfg, err := liquidity.ParametersToRpc(cfg)
	if err != nil {
		return nil, err
	}

	return rpcCfg, nil
}

// SetLiquidityParams attempts to set our current liquidity manager's
// parameters.
func (s *swapClientServer) SetLiquidityParams(ctx context.Context,
	in *looprpc.SetLiquidityParamsRequest) (*looprpc.SetLiquidityParamsResponse,
	error) {

	err := s.liquidityMgr.SetParameters(ctx, in.Parameters)
	if err != nil {
		return nil, err
	}

	return &looprpc.SetLiquidityParamsResponse{}, nil
}

// SuggestSwaps provides a list of suggested swaps based on lnd's current
// channel balances and rules set by the liquidity manager.
func (s *swapClientServer) SuggestSwaps(ctx context.Context,
	_ *looprpc.SuggestSwapsRequest) (*looprpc.SuggestSwapsResponse, error) {

	suggestions, err := s.liquidityMgr.SuggestSwaps(ctx)
	switch err {
	case liquidity.ErrNoRules:
		return nil, status.Error(codes.FailedPrecondition, err.Error())

	case nil:

	default:
		return nil, err
	}

	resp := &looprpc.SuggestSwapsResponse{
		LoopOut: make(
			[]*looprpc.LoopOutRequest, len(suggestions.OutSwaps),
		),
		LoopIn: make(
			[]*looprpc.LoopInRequest, len(suggestions.InSwaps),
		),
	}

	for i, swap := range suggestions.OutSwaps {
		resp.LoopOut[i] = &looprpc.LoopOutRequest{
			Amt:                 int64(swap.Amount),
			OutgoingChanSet:     swap.OutgoingChanSet,
			MaxSwapFee:          int64(swap.MaxSwapFee),
			MaxMinerFee:         int64(swap.MaxMinerFee),
			MaxPrepayAmt:        int64(swap.MaxPrepayAmount),
			MaxSwapRoutingFee:   int64(swap.MaxSwapRoutingFee),
			MaxPrepayRoutingFee: int64(swap.MaxPrepayRoutingFee),
			SweepConfTarget:     swap.SweepConfTarget,
		}
	}

	for i, swap := range suggestions.InSwaps {
		loopIn := &looprpc.LoopInRequest{
			Amt:            int64(swap.Amount),
			MaxSwapFee:     int64(swap.MaxSwapFee),
			MaxMinerFee:    int64(swap.MaxMinerFee),
			HtlcConfTarget: swap.HtlcConfTarget,
		}

		if swap.LastHop != nil {
			loopIn.LastHop = swap.LastHop[:]
		}

		resp.LoopIn[i] = loopIn
	}

	for id, reason := range suggestions.DisqualifiedChans {
		autoloopReason, err := rpcAutoloopReason(reason)
		if err != nil {
			return nil, err
		}

		exclChan := &looprpc.Disqualified{
			Reason:    autoloopReason,
			ChannelId: id.ToUint64(),
		}

		resp.Disqualified = append(resp.Disqualified, exclChan)
	}

	for pubkey, reason := range suggestions.DisqualifiedPeers {
		autoloopReason, err := rpcAutoloopReason(reason)
		if err != nil {
			return nil, err
		}

		clonedPubkey := route.Vertex{}
		copy(clonedPubkey[:], pubkey[:])

		exclChan := &looprpc.Disqualified{
			Reason: autoloopReason,
			Pubkey: clonedPubkey[:],
		}

		resp.Disqualified = append(resp.Disqualified, exclChan)
	}

	return resp, nil
}

// ListReservations lists all existing reservations the client has ever made.
func (s *swapClientServer) ListReservations(ctx context.Context,
	_ *looprpc.ListReservationsRequest) (
	*looprpc.ListReservationsResponse, error) {

	if s.reservationManager == nil {
		return nil, status.Error(codes.Unimplemented,
			"Restart loop with --experimental")
	}
	reservations, err := s.reservationManager.GetReservations(
		ctx,
	)
	if err != nil {
		return nil, err
	}

	return &looprpc.ListReservationsResponse{
		Reservations: ToClientReservations(
			reservations,
		),
	}, nil
}

// InstantOut initiates an instant out swap.
func (s *swapClientServer) InstantOut(ctx context.Context,
	req *looprpc.InstantOutRequest) (*looprpc.InstantOutResponse,
	error) {

	reservationIds := make([]reservation.ID, len(req.ReservationIds))
	for i, id := range req.ReservationIds {
		if len(id) != reservation.IdLength {
			return nil, fmt.Errorf("invalid reservation id: "+
				"expected %v bytes, got %d",
				reservation.IdLength, len(id))
		}

		var resId reservation.ID
		copy(resId[:], id)

		reservationIds[i] = resId
	}

	instantOutFsm, err := s.instantOutManager.NewInstantOut(
		ctx, reservationIds, req.DestAddr,
	)
	if err != nil {
		return nil, err
	}

	res := &looprpc.InstantOutResponse{
		InstantOutHash: instantOutFsm.InstantOut.SwapHash[:],
		State:          string(instantOutFsm.InstantOut.State),
	}

	if instantOutFsm.InstantOut.SweepTxHash != nil {
		res.SweepTxId = instantOutFsm.InstantOut.SweepTxHash.String()
	}

	return res, nil
}

// InstantOutQuote returns a quote for an instant out swap with the provided
// parameters.
func (s *swapClientServer) InstantOutQuote(ctx context.Context,
	req *looprpc.InstantOutQuoteRequest) (
	*looprpc.InstantOutQuoteResponse, error) {

	quote, err := s.instantOutManager.GetInstantOutQuote(
		ctx, btcutil.Amount(req.Amt), int(req.NumReservations),
	)
	if err != nil {
		return nil, err
	}

	return &looprpc.InstantOutQuoteResponse{
		ServiceFeeSat: int64(quote.ServiceFee),
		SweepFeeSat:   int64(quote.OnChainFee),
	}, nil
}

// ListInstantOuts returns a list of all currently known instant out swaps and
// their current status.
func (s *swapClientServer) ListInstantOuts(ctx context.Context,
	_ *looprpc.ListInstantOutsRequest) (
	*looprpc.ListInstantOutsResponse, error) {

	instantOuts, err := s.instantOutManager.ListInstantOuts(ctx)
	if err != nil {
		return nil, err
	}

	rpcSwaps := make([]*looprpc.InstantOut, 0, len(instantOuts))
	for _, instantOut := range instantOuts {
		rpcSwaps = append(rpcSwaps, rpcInstantOut(instantOut))
	}

	return &looprpc.ListInstantOutsResponse{
		Swaps: rpcSwaps,
	}, nil
}

func rpcInstantOut(instantOut *instantout.InstantOut) *looprpc.InstantOut {
	var sweepTxId string
	if instantOut.SweepTxHash != nil {
		sweepTxId = instantOut.SweepTxHash.String()
	}

	reservations := make([][]byte, len(instantOut.Reservations))
	for i, res := range instantOut.Reservations {
		reservations[i] = res.ID[:]
	}

	return &looprpc.InstantOut{
		SwapHash:       instantOut.SwapHash[:],
		State:          string(instantOut.State),
		Amount:         uint64(instantOut.Value),
		SweepTxId:      sweepTxId,
		ReservationIds: reservations,
	}
}

// NewStaticAddress is the rpc endpoint for loop clients to request a new static
// address.
func (s *swapClientServer) NewStaticAddress(ctx context.Context,
	_ *looprpc.NewStaticAddressRequest) (
	*looprpc.NewStaticAddressResponse, error) {

	staticAddress, err := s.staticAddressManager.NewAddress(ctx)
	if err != nil {
		return nil, err
	}

	return &looprpc.NewStaticAddressResponse{
		Address: staticAddress.String(),
	}, nil
}

// ListUnspentDeposits returns a list of utxos behind the static address.
func (s *swapClientServer) ListUnspentDeposits(ctx context.Context,
	req *looprpc.ListUnspentDepositsRequest) (
	*looprpc.ListUnspentDepositsResponse, error) {

	// List all unspent utxos the wallet sees, regardless of the number of
	// confirmations.
	staticAddress, utxos, err := s.staticAddressManager.ListUnspentRaw(
		ctx, req.MinConfs, req.MaxConfs,
	)
	if err != nil {
		return nil, err
	}

	// Prepare the list response.
	var respUtxos []*looprpc.Utxo
	for _, u := range utxos {
		utxo := &looprpc.Utxo{
			StaticAddress: staticAddress.String(),
			AmountSat:     int64(u.Value),
			Confirmations: u.Confirmations,
			Outpoint:      u.OutPoint.String(),
		}
		respUtxos = append(respUtxos, utxo)
	}

	return &looprpc.ListUnspentDepositsResponse{Utxos: respUtxos}, nil
}

// WithdrawDeposits tries to obtain a partial signature from the server to spend
// the selected deposits to the client's wallet.
func (s *swapClientServer) WithdrawDeposits(ctx context.Context,
	req *looprpc.WithdrawDepositsRequest) (
	*looprpc.WithdrawDepositsResponse, error) {

	var (
		isAllSelected  = req.All
		isUtxoSelected = len(req.Outpoints) > 0
		outpoints      []wire.OutPoint
		err            error
	)

	switch {
	case isAllSelected == isUtxoSelected:
		return nil, fmt.Errorf("must select either all or some utxos")

	case isAllSelected:
		deposits, err := s.depositManager.GetActiveDepositsInState(
			deposit.Deposited,
		)
		if err != nil {
			return nil, err
		}

		for _, d := range deposits {
			outpoints = append(outpoints, d.OutPoint)
		}

	case isUtxoSelected:
		outpoints, err = toServerOutpoints(req.Outpoints)
		if err != nil {
			return nil, err
		}
	}

	txhash, pkScript, err := s.withdrawalManager.DeliverWithdrawalRequest(
		ctx, outpoints, req.DestAddr, req.SatPerVbyte,
	)
	if err != nil {
		return nil, err
	}

	return &looprpc.WithdrawDepositsResponse{
		WithdrawalTxHash: txhash,
		PkScript:         pkScript,
	}, err
}

// ListStaticAddressDeposits returns a list of all sufficiently confirmed
// deposits behind the static address and displays properties like value,
// state or blocks til expiry.
func (s *swapClientServer) ListStaticAddressDeposits(ctx context.Context,
	req *looprpc.ListStaticAddressDepositsRequest) (
	*looprpc.ListStaticAddressDepositsResponse, error) {

	outpoints := req.Outpoints
	if req.StateFilter != looprpc.DepositState_UNKNOWN_STATE &&
		len(outpoints) > 0 {

		return nil, fmt.Errorf("can either filter by state or " +
			"outpoints")
	}

	allDeposits, err := s.depositManager.GetAllDeposits(ctx)
	if err != nil {
		return nil, err
	}

	// Deposits filtered by state or outpoints.
	var filteredDeposits []*looprpc.Deposit
	if len(outpoints) > 0 {
		f := func(d *deposit.Deposit) bool {
			for _, outpoint := range outpoints {
				if outpoint == d.OutPoint.String() {
					return true
				}
			}
			return false
		}
		filteredDeposits = filter(allDeposits, f)

		if len(outpoints) != len(filteredDeposits) {
			return nil, fmt.Errorf("not all outpoints found in " +
				"deposits")
		}
	} else {
		f := func(d *deposit.Deposit) bool {
			if req.StateFilter == looprpc.DepositState_UNKNOWN_STATE {
				// Per default, we return deposits in all
				// states.
				return true
			}

			return d.IsInState(toServerState(req.StateFilter))
		}
		filteredDeposits = filter(allDeposits, f)
	}

	// Calculate the blocks until expiry for each deposit.
	lndInfo, err := s.lnd.Client.GetInfo(ctx)
	if err != nil {
		return nil, err
	}

	bestBlockHeight := int64(lndInfo.BlockHeight)
	params, err := s.staticAddressManager.GetStaticAddressParameters(ctx)
	if err != nil {
		return nil, err
	}
	for i := 0; i < len(filteredDeposits); i++ {
		filteredDeposits[i].BlocksUntilExpiry =
			filteredDeposits[i].ConfirmationHeight +
				int64(params.Expiry) - bestBlockHeight
	}

	return &looprpc.ListStaticAddressDepositsResponse{
		FilteredDeposits: filteredDeposits,
	}, nil
}

// ListStaticAddressSwaps returns a list of all swaps that are currently pending
// or previously succeeded.
func (s *swapClientServer) ListStaticAddressSwaps(ctx context.Context,
	_ *looprpc.ListStaticAddressSwapsRequest) (
	*looprpc.ListStaticAddressSwapsResponse, error) {

	swaps, err := s.staticLoopInManager.GetAllSwaps(ctx)
	if err != nil {
		return nil, err
	}

	if len(swaps) == 0 {
		return &looprpc.ListStaticAddressSwapsResponse{}, nil
	}

	var clientSwaps []*looprpc.StaticAddressLoopInSwap
	for _, swp := range swaps {
		chainParams, err := s.network.ChainParams()
		if err != nil {
			return nil, fmt.Errorf("error getting chain params")
		}
		swapPayReq, err := zpay32.Decode(swp.SwapInvoice, chainParams)
		if err != nil {
			return nil, fmt.Errorf("error decoding swap invoice: "+
				"%v", err)
		}
		swap := &looprpc.StaticAddressLoopInSwap{
			SwapHash:         swp.SwapHash[:],
			DepositOutpoints: swp.DepositOutpoints,
			State: toClientStaticAddressLoopInState(
				swp.GetState(),
			),
			SwapAmountSatoshis: int64(swp.TotalDepositAmount()),
			PaymentRequestAmountSatoshis: int64(
				swapPayReq.MilliSat.ToSatoshis(),
			),
		}

		clientSwaps = append(clientSwaps, swap)
	}

	return &looprpc.ListStaticAddressSwapsResponse{
		Swaps: clientSwaps,
	}, nil
}

// GetStaticAddressSummary returns a summary static address related information.
// Amongst deposits and withdrawals and their total values it also includes a
// list of detailed deposit information filtered by their state.
func (s *swapClientServer) GetStaticAddressSummary(ctx context.Context,
	_ *looprpc.StaticAddressSummaryRequest) (
	*looprpc.StaticAddressSummaryResponse, error) {

	allDeposits, err := s.depositManager.GetAllDeposits(ctx)
	if err != nil {
		return nil, err
	}

	var (
		totalNumDeposits = len(allDeposits)
		valueUnconfirmed int64
		valueDeposited   int64
		valueExpired     int64
		valueWithdrawn   int64
		valueLoopedIn    int64
		htlcTimeoutSwept int64
	)

	// Value unconfirmed.
	utxos, err := s.staticAddressManager.ListUnspent(
		ctx, 0, deposit.MinConfs-1,
	)
	if err != nil {
		return nil, err
	}
	for _, u := range utxos {
		valueUnconfirmed += int64(u.Value)
	}

	// Confirmed total values by category.
	for _, d := range allDeposits {
		value := int64(d.Value)
		switch d.GetState() {
		case deposit.Deposited:
			valueDeposited += value

		case deposit.Expired:
			valueExpired += value

		case deposit.Withdrawn:
			valueWithdrawn += value

		case deposit.LoopedIn:
			valueLoopedIn += value

		case deposit.HtlcTimeoutSwept:
			htlcTimeoutSwept += value
		}
	}

	params, err := s.staticAddressManager.GetStaticAddressParameters(ctx)
	if err != nil {
		return nil, err
	}

	address, err := s.staticAddressManager.GetTaprootAddress(
		params.ClientPubkey, params.ServerPubkey, int64(params.Expiry),
	)
	if err != nil {
		return nil, err
	}

	return &looprpc.StaticAddressSummaryResponse{
		StaticAddress:                  address.String(),
		RelativeExpiryBlocks:           uint64(params.Expiry),
		TotalNumDeposits:               uint32(totalNumDeposits),
		ValueUnconfirmedSatoshis:       valueUnconfirmed,
		ValueDepositedSatoshis:         valueDeposited,
		ValueExpiredSatoshis:           valueExpired,
		ValueWithdrawnSatoshis:         valueWithdrawn,
		ValueLoopedInSatoshis:          valueLoopedIn,
		ValueHtlcTimeoutSweepsSatoshis: htlcTimeoutSwept,
	}, nil
}

// StaticAddressLoopIn initiates a loop-in request using static address
// deposits.
func (s *swapClientServer) StaticAddressLoopIn(ctx context.Context,
	in *looprpc.StaticAddressLoopInRequest) (
	*looprpc.StaticAddressLoopInResponse, error) {

	log.Infof("Static loop-in request received")

	routeHints, err := unmarshallRouteHints(in.RouteHints)
	if err != nil {
		return nil, err
	}

	req := &loop.StaticAddressLoopInRequest{
		DepositOutpoints:      in.Outpoints,
		MaxSwapFee:            btcutil.Amount(in.MaxSwapFeeSatoshis),
		Label:                 in.Label,
		Initiator:             in.Initiator,
		Private:               in.Private,
		RouteHints:            routeHints,
		PaymentTimeoutSeconds: in.PaymentTimeoutSeconds,
	}

	if in.LastHop != nil {
		lastHop, err := route.NewVertexFromBytes(in.LastHop)
		if err != nil {
			return nil, err
		}
		req.LastHop = &lastHop
	}

	loopIn, err := s.staticLoopInManager.DeliverLoopInRequest(ctx, req)
	if err != nil {
		return nil, err
	}

	return &looprpc.StaticAddressLoopInResponse{
		SwapHash:              loopIn.SwapHash[:],
		State:                 string(loopIn.GetState()),
		Amount:                uint64(loopIn.TotalDepositAmount()),
		HtlcCltv:              loopIn.HtlcCltvExpiry,
		MaxSwapFeeSatoshis:    int64(loopIn.MaxSwapFee),
		InitiationHeight:      loopIn.InitiationHeight,
		ProtocolVersion:       loopIn.ProtocolVersion.String(),
		Initiator:             loopIn.Initiator,
		Label:                 loopIn.Label,
		PaymentTimeoutSeconds: loopIn.PaymentTimeoutSeconds,
		QuotedSwapFeeSatoshis: int64(loopIn.QuotedSwapFee),
	}, nil
}

type filterFunc func(deposits *deposit.Deposit) bool

func filter(deposits []*deposit.Deposit, f filterFunc) []*looprpc.Deposit {
	var clientDeposits []*looprpc.Deposit
	for _, d := range deposits {
		if !f(d) {
			continue
		}

		hash := d.Hash
		outpoint := wire.NewOutPoint(&hash, d.Index).String()
		deposit := &looprpc.Deposit{
			Id: d.ID[:],
			State: toClientDepositState(
				d.GetState(),
			),
			Outpoint:           outpoint,
			Value:              int64(d.Value),
			ConfirmationHeight: d.ConfirmationHeight,
		}

		clientDeposits = append(clientDeposits, deposit)
	}

	return clientDeposits
}

func toClientDepositState(state fsm.StateType) looprpc.DepositState {
	switch state {
	case deposit.Deposited:
		return looprpc.DepositState_DEPOSITED

	case deposit.Withdrawing:
		return looprpc.DepositState_WITHDRAWING

	case deposit.Withdrawn:
		return looprpc.DepositState_WITHDRAWN

	case deposit.PublishExpirySweep:
		return looprpc.DepositState_PUBLISH_EXPIRED

	case deposit.LoopingIn:
		return looprpc.DepositState_LOOPING_IN

	case deposit.LoopedIn:
		return looprpc.DepositState_LOOPED_IN

	case deposit.SweepHtlcTimeout:
		return looprpc.DepositState_SWEEP_HTLC_TIMEOUT

	case deposit.HtlcTimeoutSwept:
		return looprpc.DepositState_HTLC_TIMEOUT_SWEPT

	case deposit.WaitForExpirySweep:
		return looprpc.DepositState_WAIT_FOR_EXPIRY_SWEEP

	case deposit.Expired:
		return looprpc.DepositState_EXPIRED

	default:
		return looprpc.DepositState_UNKNOWN_STATE
	}
}

func toClientStaticAddressLoopInState(
	state fsm.StateType) looprpc.StaticAddressLoopInSwapState {

	switch state {
	case loopin.InitHtlcTx:
		return looprpc.StaticAddressLoopInSwapState_INIT_HTLC

	case loopin.SignHtlcTx:
		return looprpc.StaticAddressLoopInSwapState_SIGN_HTLC_TX

	case loopin.MonitorInvoiceAndHtlcTx:
		return looprpc.StaticAddressLoopInSwapState_MONITOR_INVOICE_HTLC_TX

	case loopin.PaymentReceived:
		return looprpc.StaticAddressLoopInSwapState_PAYMENT_RECEIVED

	case loopin.SweepHtlcTimeout:
		return looprpc.StaticAddressLoopInSwapState_SWEEP_STATIC_ADDRESS_HTLC_TIMEOUT

	case loopin.MonitorHtlcTimeoutSweep:
		return looprpc.StaticAddressLoopInSwapState_MONITOR_HTLC_TIMEOUT_SWEEP

	case loopin.HtlcTimeoutSwept:
		return looprpc.StaticAddressLoopInSwapState_HTLC_STATIC_ADDRESS_TIMEOUT_SWEPT

	case loopin.Succeeded:
		return looprpc.StaticAddressLoopInSwapState_SUCCEEDED

	case loopin.SucceededTransitioningFailed:
		return looprpc.StaticAddressLoopInSwapState_SUCCEEDED_TRANSITIONING_FAILED

	case loopin.UnlockDeposits:
		return looprpc.StaticAddressLoopInSwapState_UNLOCK_DEPOSITS

	case loopin.Failed:
		return looprpc.StaticAddressLoopInSwapState_FAILED_STATIC_ADDRESS_SWAP

	default:
		return looprpc.StaticAddressLoopInSwapState_UNKNOWN_STATIC_ADDRESS_SWAP_STATE
	}
}

func toServerState(state looprpc.DepositState) fsm.StateType {
	switch state {
	case looprpc.DepositState_DEPOSITED:
		return deposit.Deposited

	case looprpc.DepositState_WITHDRAWING:
		return deposit.Withdrawing

	case looprpc.DepositState_WITHDRAWN:
		return deposit.Withdrawn

	case looprpc.DepositState_PUBLISH_EXPIRED:
		return deposit.PublishExpirySweep

	case looprpc.DepositState_LOOPING_IN:
		return deposit.LoopingIn

	case looprpc.DepositState_LOOPED_IN:
		return deposit.LoopedIn

	case looprpc.DepositState_SWEEP_HTLC_TIMEOUT:
		return deposit.SweepHtlcTimeout

	case looprpc.DepositState_HTLC_TIMEOUT_SWEPT:
		return deposit.HtlcTimeoutSwept

	case looprpc.DepositState_WAIT_FOR_EXPIRY_SWEEP:
		return deposit.WaitForExpirySweep

	case looprpc.DepositState_EXPIRED:
		return deposit.Expired

	default:
		return fsm.EmptyState
	}
}

func toServerOutpoints(outpoints []*looprpc.OutPoint) ([]wire.OutPoint,
	error) {

	var serverOutpoints []wire.OutPoint
	for _, o := range outpoints {
		outpointStr := fmt.Sprintf("%s:%d", o.TxidStr, o.OutputIndex)
		newOutpoint, err := wire.NewOutPointFromString(outpointStr)
		if err != nil {
			return nil, err
		}

		serverOutpoints = append(serverOutpoints, *newOutpoint)
	}

	return serverOutpoints, nil
}

func rpcAutoloopReason(reason liquidity.Reason) (looprpc.AutoReason, error) {
	switch reason {
	case liquidity.ReasonNone:
		return looprpc.AutoReason_AUTO_REASON_UNKNOWN, nil

	case liquidity.ReasonBudgetNotStarted:
		return looprpc.AutoReason_AUTO_REASON_BUDGET_NOT_STARTED, nil

	case liquidity.ReasonSweepFees:
		return looprpc.AutoReason_AUTO_REASON_SWEEP_FEES, nil

	case liquidity.ReasonBudgetElapsed:
		return looprpc.AutoReason_AUTO_REASON_BUDGET_ELAPSED, nil

	case liquidity.ReasonInFlight:
		return looprpc.AutoReason_AUTO_REASON_IN_FLIGHT, nil

	case liquidity.ReasonSwapFee:
		return looprpc.AutoReason_AUTO_REASON_SWAP_FEE, nil

	case liquidity.ReasonMinerFee:
		return looprpc.AutoReason_AUTO_REASON_MINER_FEE, nil

	case liquidity.ReasonPrepay:
		return looprpc.AutoReason_AUTO_REASON_PREPAY, nil

	case liquidity.ReasonFailureBackoff:
		return looprpc.AutoReason_AUTO_REASON_FAILURE_BACKOFF, nil

	case liquidity.ReasonLoopOut:
		return looprpc.AutoReason_AUTO_REASON_LOOP_OUT, nil

	case liquidity.ReasonLoopIn:
		return looprpc.AutoReason_AUTO_REASON_LOOP_IN, nil

	case liquidity.ReasonLiquidityOk:
		return looprpc.AutoReason_AUTO_REASON_LIQUIDITY_OK, nil

	case liquidity.ReasonBudgetInsufficient:
		return looprpc.AutoReason_AUTO_REASON_BUDGET_INSUFFICIENT, nil

	case liquidity.ReasonFeePPMInsufficient:
		return looprpc.AutoReason_AUTO_REASON_SWAP_FEE, nil

	default:
		return 0, fmt.Errorf("unknown autoloop reason: %v", reason)
	}
}

// processStatusUpdates reads updates on the status channel and processes them.
//
// NOTE: This must run inside a goroutine as it blocks until the main context
// shuts down.
func (s *swapClientServer) processStatusUpdates(mainCtx context.Context) {
	for {
		select {
		// On updates, refresh the server's in-memory state and inform
		// subscribers about the changes.
		case swp := <-s.statusChan:
			s.swapsLock.Lock()
			s.swaps[swp.SwapHash] = swp

			for _, subscriber := range s.subscribers {
				select {
				case subscriber <- swp:
				case <-mainCtx.Done():
					s.swapsLock.Unlock()
					return
				}
			}

			s.swapsLock.Unlock()

		// Server is shutting down.
		case <-mainCtx.Done():
			return
		}
	}
}

// validateConfTarget ensures the given confirmation target is valid. If one
// isn't specified (0 value), then the default target is used.
func validateConfTarget(target, defaultTarget int32) (int32, error) {
	switch {
	case target == 0:
		return defaultTarget, nil

	// Ensure the target respects our minimum threshold.
	case target < minConfTarget:
		return 0, fmt.Errorf("%w: A confirmation target of at "+
			"least %v must be provided", errConfTargetTooLow,
			minConfTarget)

	default:
		return target, nil
	}
}

// validateLoopInRequest fails if the mutually exclusive conf target and
// external parameters are both set.
func validateLoopInRequest(htlcConfTarget int32, external bool,
	numDeposits uint32, amount int64) (int32, error) {

	if amount == 0 && numDeposits == 0 {
		return 0, errors.New("either amount or deposits must be set")
	}

	// If the htlc is going to be externally set, the htlcConfTarget should
	// not be set, because it has no relevance when the htlc is external.
	if external && htlcConfTarget != 0 {
		return 0, errors.New("external and htlc conf target cannot " +
			"both be set")
	}

	// If the htlc is being externally published, we do not need to set a
	// confirmation target.
	if external {
		return 0, nil
	}

	// If the loop in uses static address deposits, we do not need to set a
	// confirmation target since the HTLC won't be published by the client.
	if numDeposits > 0 {
		return 0, nil
	}

	return validateConfTarget(htlcConfTarget, loop.DefaultHtlcConfTarget)
}

// validateLoopOutRequest validates the confirmation target, destination
// address and label of the loop out request. It also checks that the requested
// loop amount is valid given the available balance.
func validateLoopOutRequest(ctx context.Context, lnd lndclient.LightningClient,
	chainParams *chaincfg.Params, req *looprpc.LoopOutRequest,
	sweepAddr btcutil.Address, maxParts uint32) (int32, error) {

	// Check that the provided destination address has the correct format
	// for the active network.
	if !sweepAddr.IsForNet(chainParams) {
		return 0, fmt.Errorf("%w: Current active network is %s",
			errIncorrectChain, chainParams.Name)
	}

	// Check that the provided destination address is a supported
	// address format.
	switch sweepAddr.(type) {
	case *btcutil.AddressTaproot,
		*btcutil.AddressWitnessScriptHash,
		*btcutil.AddressWitnessPubKeyHash,
		*btcutil.AddressScriptHash,
		*btcutil.AddressPubKeyHash:

	default:
		return 0, errInvalidAddress
	}

	// If this is an asset payment, we'll check that we have the necessary
	// outbound asset capacaity to fulfill the request.
	if req.AssetInfo != nil {
		// Todo(sputn1ck) actually check outbound capacity.
		return validateConfTarget(
			req.SweepConfTarget, loop.DefaultSweepConfTarget,
		)
	}

	// Check that the label is valid.
	if err := labels.Validate(req.Label); err != nil {
		return 0, err
	}

	channels, err := lnd.ListChannels(ctx, false, false)
	if err != nil {
		return 0, err
	}

	unlimitedChannels := len(req.OutgoingChanSet) == 0
	outgoingChanSetMap := make(map[uint64]bool)
	for _, chanID := range req.OutgoingChanSet {
		outgoingChanSetMap[chanID] = true
	}

	var activeChannelSet []lndclient.ChannelInfo
	for _, c := range channels {
		// Don't bother looking at inactive channels.
		if !c.Active {
			continue
		}

		// If no outgoing channel set was specified then all active
		// channels are considered. However, if a channel set was
		// specified then only the specified channels are considered.
		if unlimitedChannels || outgoingChanSetMap[c.ChannelID] {
			activeChannelSet = append(activeChannelSet, c)
		}
	}

	// Determine if the loop out request is theoretically possible given
	// the amount requested, the maximum possible routing fees,
	// the available channel set and the fact that equal splitting is
	// used for MPP.
	requiredBalance := btcutil.Amount(req.Amt + req.MaxSwapRoutingFee)
	isRoutable, _ := hasBandwidth(activeChannelSet, requiredBalance,
		int(maxParts))

	if !isRoutable {
		return 0, fmt.Errorf("%w: Requested swap amount of %d "+
			"sats along with the maximum routing fee of %d sats "+
			"is more than what can be routed given current state "+
			"of the channel set", errBalanceTooLow, req.Amt,
			req.MaxSwapRoutingFee)
	}

	return validateConfTarget(
		req.SweepConfTarget, loop.DefaultSweepConfTarget,
	)
}

// hasBandwidth simulates the MPP splitting logic that will be used by LND when
// attempting to route the payment. This function is used to evaluate if a
// payment will be routable given the splitting logic used by LND.
// It returns true if the amount is routable given the channel set and the
// maximum number of shards allowed. If the amount is routable then the number
// of shards used is also returned. This function makes an assumption that the
// minimum loop amount divided by max parts will not be less than the minimum
// shard amount. If the MPP logic changes, then this function should be updated.
func hasBandwidth(channels []lndclient.ChannelInfo, amt btcutil.Amount,
	maxParts int) (bool, int) {

	log.Tracef("Checking if %v sats can be routed with %v parts over "+
		"channel set of length %v", amt, maxParts, len(channels))

	localBalances := make([]btcutil.Amount, len(channels))
	var totalBandwidth btcutil.Amount
	for i, channel := range channels {
		log.Tracef("Channel %v: local=%v remote=%v", channel.ChannelID,
			channel.LocalBalance, channel.RemoteBalance)

		localBalances[i] = channel.LocalBalance
		totalBandwidth += channel.LocalBalance
	}

	log.Tracef("Total bandwidth: %v", totalBandwidth)
	if totalBandwidth < amt {
		return false, 0
	}

	logLocalBalances := func(shard int) {
		log.Tracef("Local balances for %v shards:", shard)
		for i, balance := range localBalances {
			log.Tracef("Channel %v: localBalances[%v]=%v",
				channels[i].ChannelID, i, balance)
		}
	}

	split := amt
	for shard := 0; shard <= maxParts; {
		log.Tracef("Trying to split %v sats into %v parts", amt, shard)

		paid := false
		for i := 0; i < len(localBalances); i++ {
			// TODO(hieblmi): Consider channel reserves because the
			//      channel can't send its full local balance.
			if localBalances[i] >= split {
				log.Tracef("len(shards)=%v: Local channel "+
					"balance %v can pay %v sats",
					shard, localBalances[i], split)

				localBalances[i] -= split
				log.Tracef("len(shards)=%v: Subtracted "+
					"%v sats from localBalance[%v]=%v",
					shard, split, i, localBalances[i])

				amt -= split
				log.Tracef("len(shards)=%v: Remaining total "+
					"amount amt=%v", shard, amt)

				paid = true
				shard++

				break
			}
		}

		logLocalBalances(shard)

		if amt == 0 {
			log.Tracef("Payment is routable with %v part(s)", shard)

			return true, shard
		}

		if !paid {
			log.Tracef("len(shards)=%v: No channel could pay %v "+
				"sats, halving payment to %v and trying again",
				split/2)

			split /= 2
		} else {
			log.Tracef("len(shards)=%v: Payment was made, trying "+
				"to pay remaining sats %v", shard, amt)

			split = amt
		}
	}

	log.Tracef("Payment is not routable, remaining amount that can't be "+
		"sent: %v sats", amt)

	logLocalBalances(maxParts)

	return false, 0
}

// getPublicationDeadline returns the publication deadline for a swap given the
// unix timestamp. If the timestamp is believed to be in milliseconds, then it
// is converted to seconds.
func getPublicationDeadline(unixTimestamp uint64) time.Time {
	length := len(fmt.Sprintf("%d", unixTimestamp))
	if length >= 13 {
		// Likely a millisecond timestamp
		secs := unixTimestamp / 1000
		nsecs := (unixTimestamp % 1000) * 1e6
		return time.Unix(int64(secs), int64(nsecs))
	} else {
		// Likely a second timestamp
		return time.Unix(int64(unixTimestamp), 0)
	}
}

// ToClientReservations converts a slice of server
// reservations to a slice of client reservations.
func ToClientReservations(
	res []*reservation.Reservation) []*looprpc.ClientReservation {

	var result []*looprpc.ClientReservation
	for _, r := range res {
		result = append(result, toClientReservation(r))
	}

	return result
}

// toClientReservation converts a server reservation to a
// client reservation.
func toClientReservation(
	res *reservation.Reservation) *looprpc.ClientReservation {

	var (
		txid string
		vout uint32
	)
	if res.Outpoint != nil {
		txid = res.Outpoint.Hash.String()
		vout = res.Outpoint.Index
	}

	return &looprpc.ClientReservation{
		ReservationId: res.ID[:],
		State:         string(res.State),
		Amount:        uint64(res.Value),
		TxId:          txid,
		Vout:          vout,
		Expiry:        res.Expiry,
	}
}
