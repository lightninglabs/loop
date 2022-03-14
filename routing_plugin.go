package loop

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btclog"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/clock"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/zpay32"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	// ErrRoutingPluginNotApplicable means that the selected routing plugin
	// is not able to enhance routing given the current conditions and
	// therefore shouldn't be used.
	ErrRoutingPluginNotApplicable = fmt.Errorf("routing plugin not " +
		"applicable")

	// ErrRoutingPluginNoMoreRetries means that the routing plugin can't
	// effectively help the payment with more retries.
	ErrRoutingPluginNoMoreRetries = fmt.Errorf("routing plugin can't " +
		"retry more")
)

var (
	routingPluginMx       sync.Mutex
	routingPluginInstance RoutingPlugin
)

// RoutingPlugin is a generic interface for off-chain payment helpers.
type RoutingPlugin interface {
	// Init initializes the routing plugin.
	Init(ctx context.Context, target route.Vertex,
		routeHints [][]zpay32.HopHint, amt btcutil.Amount) error

	// Done deinitializes the routing plugin (restoring any state the
	// plugin might have changed).
	Done(ctx context.Context) error

	// BeforePayment is called before each payment. Attempt counter is
	// passed, counting attempts from 1.
	BeforePayment(ctx context.Context, attempt int, maxAttempts int) error
}

// makeRoutingPlugin is a helper to instantiate routing plugins.
func makeRoutingPlugin(pluginType RoutingPluginType,
	lnd lndclient.LndServices, clock clock.Clock) RoutingPlugin {

	if pluginType == RoutingPluginLowHigh {
		return &lowToHighRoutingPlugin{
			lnd:   lnd,
			clock: clock,
		}
	}

	return nil
}

// AcquireRoutingPlugin will return a RoutingPlugin instance (or nil). As the
// LND instance used is a shared resource, currently only one requestor will be
// able to acquire a RoutingPlugin instance. If someone is already holding the
// instance a nil is returned.
func AcquireRoutingPlugin(ctx context.Context, pluginType RoutingPluginType,
	lnd lndclient.LndServices, target route.Vertex,
	routeHints [][]zpay32.HopHint, amt btcutil.Amount) (
	RoutingPlugin, error) {

	routingPluginMx.Lock()
	defer routingPluginMx.Unlock()

	// Another swap is already using the routing plugin.
	if routingPluginInstance != nil {
		return nil, nil
	}

	routingPluginInstance = makeRoutingPlugin(
		pluginType, lnd, clock.NewDefaultClock(),
	)
	if routingPluginInstance == nil {
		return nil, nil
	}

	// Initialize the plugin with the passed parameters.
	err := routingPluginInstance.Init(ctx, target, routeHints, amt)
	if err != nil {
		if err == ErrRoutingPluginNotApplicable {
			// Since the routing plugin is not applicable for this
			// payment, we can immediately destruct it.
			if err := routingPluginInstance.Done(ctx); err != nil {
				log.Errorf("Error while releasing routing "+
					"plugin: %v", err)
			}

			// ErrRoutingPluginNotApplicable is non critical, so
			// we're masking this error as we can continue the swap
			// flow without the routing plugin.
			err = nil
		}

		routingPluginInstance = nil
		return nil, err
	}

	return routingPluginInstance, nil
}

// ReleaseRoutingPlugin will release the RoutingPlugin, allowing other
// requestors to acquire the instance.
func ReleaseRoutingPlugin(ctx context.Context) {
	routingPluginMx.Lock()
	defer routingPluginMx.Unlock()

	if routingPluginInstance == nil {
		return
	}

	if err := routingPluginInstance.Done(ctx); err != nil {
		log.Errorf("Error while releasing routing plugin: %v",
			err)
	}

	routingPluginInstance = nil
}

// lowToHighRoutingPlugin is a RoutingPlugin that implements "low to high"
// routing. This means that when we're attempting to pay to a target we'll
// gradually (with a linear step function) discard inbound peers to that target
// given routing timeouts. The lowToHighRoutingPlugin itself is responsible for
// manipulating LND's Mission Control to make such routing attempts possible.
type lowToHighRoutingPlugin struct {
	lnd     lndclient.LndServices
	clock   clock.Clock
	target  route.Vertex
	amount  btcutil.Amount
	mcState map[route.Vertex]lndclient.MissionControlEntry

	// nodesByMaxFee holds nodes sorted by maximum fees that would be paid
	// to the target node for the target amount.
	nodesByMaxFee []nodeFeeInfo

	// mcChanged flags that the MC settings for the tracked nodes were
	// changed and should be reset to their original state once the plugin
	// is done.
	mcChanged bool
}

type nodeFeeInfo struct {
	node     route.Vertex
	capacity btcutil.Amount
	fee      int64
}

// Enforce that lowToHighRoutingPlugin implements the RoutingPlugin interface.
var _ RoutingPlugin = (*lowToHighRoutingPlugin)(nil)

// buildPrivateChannels creates the private channel map from the passed route
// hints. The code is taken and adapted from LND. Original source:
// lnd/routing/payment_session_source.go.
func buildPrivateChannels(routeHints [][]zpay32.HopHint,
	target route.Vertex) map[route.Vertex][]*lndclient.ChannelEdge {

	edges := make(map[route.Vertex][]*lndclient.ChannelEdge)

	// Traverse through all of the available hop hints and include them in
	// our edges map.
	for _, routeHint := range routeHints {
		// If multiple hop hints are provided within a single route
		// hint, we'll assume they must be chained together and sorted
		// in forward order in order to reach the target successfully.
		for i, hopHint := range routeHint {
			// In order to determine the end node of this hint,
			// we'll need to look at the next hint's start node. If
			// we've reached the end of the hints list, we can
			// assume we've reached the target.
			var toNode route.Vertex
			if i != len(routeHint)-1 {
				toNode = route.NewVertex(routeHint[i+1].NodeID)
			} else {
				toNode = target
			}

			fromNode := route.NewVertex(hopHint.NodeID)
			// Finally, create the channel edges from the hop hint
			// and add them to list of edges.
			edgeFrom := &lndclient.ChannelEdge{
				Node1:     fromNode,
				Node2:     toNode,
				ChannelID: hopHint.ChannelID,
				Node1Policy: &lndclient.RoutingPolicy{
					TimeLockDelta: uint32(
						hopHint.CLTVExpiryDelta,
					),
					FeeBaseMsat: int64(
						hopHint.FeeBaseMSat,
					),
					FeeRateMilliMsat: int64(
						hopHint.FeeProportionalMillionths,
					),
				},
			}
			edges[fromNode] = append(edges[fromNode], edgeFrom)

			// Note that we're adding the edge here in both
			// directions as we don't actually use it to find a path
			// but just to walk from the target until the first
			// node that peers with multiple nodes.
			edgeTo := &lndclient.ChannelEdge{
				Node1:     toNode,
				Node2:     fromNode,
				ChannelID: hopHint.ChannelID,
				Node2Policy: &lndclient.RoutingPolicy{
					TimeLockDelta: uint32(
						hopHint.CLTVExpiryDelta,
					),
					FeeBaseMsat: int64(
						hopHint.FeeBaseMSat,
					),
					FeeRateMilliMsat: int64(
						hopHint.FeeProportionalMillionths,
					),
				},
			}
			edges[toNode] = append(edges[toNode], edgeTo)
		}
	}

	return edges
}

func getNodeInfo(ctx context.Context, lnd lndclient.LightningClient,
	nodeID route.Vertex,
	privateEdges map[route.Vertex][]*lndclient.ChannelEdge) (
	*lndclient.NodeInfo, error) {

	nodeInfo, err := lnd.GetNodeInfo(ctx, nodeID, true)
	if err != nil {
		status, ok := status.FromError(err)
		if ok && status.Code() == codes.NotFound {
			// It's still possible that we should know this node
			// from the provided route hints even though it is not
			// part of the public graph. If we don't know any
			// private channels then just return the error.
			if _, ok := privateEdges[nodeID]; !ok {
				return nil, err
			}

			nodeInfo = &lndclient.NodeInfo{
				Node: &lndclient.Node{
					PubKey: nodeID,
				},
			}
		} else {
			return nil, err
		}
	}

	if len(privateEdges) > 0 {
		for _, edge := range privateEdges[nodeID] {
			nodeInfo.Channels = append(nodeInfo.Channels, *edge)
			nodeInfo.TotalCapacity += edge.Capacity
		}
	}

	return nodeInfo, nil
}

// saveMissionControlState will save the MC state for the node pairs formed by
// the passed nodes and target.
func (r *lowToHighRoutingPlugin) saveMissionControlState(ctx context.Context,
	nodes map[route.Vertex]*lndclient.NodeInfo, target route.Vertex) error {

	entries, err := r.lnd.Router.QueryMissionControl(ctx)
	if err != nil {
		return err
	}

	r.mcState = make(map[route.Vertex]lndclient.MissionControlEntry)
	for _, entry := range entries {
		// Skip pairs which we do not intend to change.
		if _, ok := nodes[entry.NodeFrom]; !ok {
			continue
		}

		if entry.NodeTo != target {
			continue
		}

		r.mcState[entry.NodeFrom] = entry
	}

	log.Debugf("Saved MC state: %v", spew.Sdump(r.mcState))
	return nil
}

// nodesByMaxFee is a helper function to order the passed nodes by overall max
// fee towards the target node if we'd want to fwd the passed amount.
func nodesByMaxFee(amt btcutil.Amount, target route.Vertex,
	nodes map[route.Vertex]*lndclient.NodeInfo) []nodeFeeInfo {

	// maxFeePerNode assigns the maximum fees that would be paid through
	// selected nodes to the target node for the target amount.
	maxFeePerNode := make(map[route.Vertex]int64)
	totalCapacityPerNode := make(map[route.Vertex]btcutil.Amount)

	amtMsat := int64(amt * 1000)
	for nodeID, node := range nodes {
		var totalCapacity btcutil.Amount
		for _, ch := range node.Channels {
			var policy *lndclient.RoutingPolicy
			if ch.Node1 == nodeID && ch.Node2 == target {
				policy = ch.Node1Policy
			} else if ch.Node1 == target && ch.Node2 == nodeID {
				policy = ch.Node2Policy
			}

			if policy == nil {
				continue
			}

			totalCapacity += ch.Capacity

			log.Debugf("'%v', policy=%v",
				node.Alias, spew.Sdump(policy))
			fee := policy.FeeBaseMsat +
				policy.FeeRateMilliMsat*amtMsat

			// For all peers we'll save the "maximum" routing fee
			// for that peer.
			if fee > maxFeePerNode[nodeID] {
				maxFeePerNode[nodeID] = fee
			}
		}
		totalCapacityPerNode[nodeID] = totalCapacity
	}

	nodesByMaxFee := make([]nodeFeeInfo, 0, len(maxFeePerNode))

	// Sort peers by maximum fee, so that we can later on disable edges
	// in a gradual way.
	for nodeID, fee := range maxFeePerNode {
		nodesByMaxFee = append(
			nodesByMaxFee, nodeFeeInfo{
				node:     nodeID,
				capacity: totalCapacityPerNode[nodeID],
				fee:      fee,
			},
		)
	}

	sort.Slice(nodesByMaxFee, func(i, j int) bool {
		return nodesByMaxFee[i].fee < nodesByMaxFee[j].fee
	})

	for i, nodeFee := range nodesByMaxFee {
		log.Tracef("nodesByMaxFee[%v] = %v (%v)", i,
			nodeFee.node.String(), nodeFee.fee)
	}

	return nodesByMaxFee
}

// Init will initialize the "low to high" routing plugin. It'll save the MC
// state and also preinit the internal state of the routing plugin. When the
// instance is released, the saved MC state can be restored.
func (r *lowToHighRoutingPlugin) Init(ctx context.Context, target route.Vertex,
	routeHints [][]zpay32.HopHint, amt btcutil.Amount) error {

	// Prepare the private edges from the passed route hints.
	privateEdges := buildPrivateChannels(routeHints, target)

	// Save the original target as the current "exit" node: where
	// our payments should flow forward.
	exit := target

	// Walk until the first fork (if there's any). This first
	// fork will be where we're going to try to manipulate success
	// probabilities to increasingly prefer more expensive edges.

	// We track all visited peers, so we won't end up walking
	// back and forth on graphs that have no forks.
	visited := map[route.Vertex]struct{}{
		target: {},
	}

	var (
		targetNodeInfo *lndclient.NodeInfo
		err            error
	)

	for {
		targetNodeInfo, err = getNodeInfo(
			ctx, r.lnd.Client, target, privateEdges,
		)
		if err != nil {
			return err
		}

		// If the target node has only one or more channels but all
		// connects to the same peer, then we need to walk further.
		var (
			peer  route.Vertex
			peers []route.Vertex
		)
		for _, edge := range targetNodeInfo.Channels {
			if target != edge.Node1 {
				peer = edge.Node1
			} else {
				peer = edge.Node2
			}

			if _, ok := visited[peer]; !ok {
				visited[peer] = struct{}{}
				peers = append(peers, peer)
			}
		}

		if len(peers) == 1 {
			exit = target
			target = peers[0]
			continue
		}

		// If there are no more peers to visit then we can't use
		// this routing plugin.
		if len(peers) == 0 {
			return ErrRoutingPluginNotApplicable
		}

		// Found the first fork to our target.
		break
	}

	log.Debugf("Low/high plugin target: '%v' %v", targetNodeInfo.Alias,
		targetNodeInfo.PubKey.String())

	// Gather node info (including channels) for the nodes we're
	// interested in.
	targetChanged := exit != target
	nodes := make(map[route.Vertex]*lndclient.NodeInfo)
	for _, edge := range targetNodeInfo.Channels {
		// Skip edges to the exit node since from there the route to
		// the invoice target is always constructed from the same hops.
		if targetChanged &&
			(edge.Node1 == exit || edge.Node2 == exit) {

			continue
		}

		var peer route.Vertex
		if edge.Node1 == target {
			peer = edge.Node2
		} else {
			peer = edge.Node1
		}

		nodeInfo, err := getNodeInfo(ctx, r.lnd.Client, peer, nil)
		if err != nil {
			return err
		}

		nodes[peer] = nodeInfo
	}

	// Get the nodes ordered by routing fee towards the target.
	r.nodesByMaxFee = nodesByMaxFee(amt, target, nodes)
	r.target = target
	r.amount = amt

	// Save MC state.
	err = r.saveMissionControlState(ctx, nodes, target)
	if err != nil {
		return err
	}

	return nil
}

// BeforePayment will reconfigure the mission control on each payment attempt.
func (r *lowToHighRoutingPlugin) BeforePayment(ctx context.Context,
	currAttempt int, maxAttempts int) error {

	queryRoutesReq := lndclient.QueryRoutesRequest{
		Source:            &r.lnd.NodePubkey,
		PubKey:            r.target,
		AmtMsat:           lnwire.MilliSatoshi(r.amount * 1000),
		FeeLimitMsat:      lnwire.MilliSatoshi(r.amount * 1000),
		UseMissionControl: true,
	}

	// If logging in trace level, query routes and log to see how what path
	// we find before MC is manipulated.
	if log.Level() == btclog.LevelTrace {
		res, err := r.lnd.Client.QueryRoutes(ctx, queryRoutesReq)
		log.Tracef("BeforePayment() QueryRoutes(1)=%v, err=%v",
			spew.Sdump(res), err)
	}

	// Do not do anything unless we tried to route the payment at least
	// once.
	if currAttempt < 2 {
		return nil
	}

	// Calculate the limit until we'll disable edges. The way we calculate
	// this limit is that we take the minimum and maximum fee peers which
	// define our fee range. Within this fee range we'll scale linearly
	// where each step euqals to the range divided by maxAttempts.
	minFee := r.nodesByMaxFee[0].fee
	maxFee := r.nodesByMaxFee[len(r.nodesByMaxFee)-1].fee
	limit := minFee +
		((maxFee-minFee)/int64(maxAttempts))*int64(currAttempt)

	// With the forced MC import we can safely set the pair history
	// timestamps to the current time as import will always just override
	// current MC state.
	now := r.clock.Now()

	allowed := 0
	entries := make(
		[]lndclient.MissionControlEntry, 0, len(r.nodesByMaxFee),
	)

	for _, nodeFeeInfo := range r.nodesByMaxFee {
		if nodeFeeInfo.fee < limit {
			log.Debugf("Discouraging payments from %v to %v",
				nodeFeeInfo.node, r.target)
			entries = append(
				entries, lndclient.MissionControlEntry{
					NodeFrom: nodeFeeInfo.node,
					NodeTo:   r.target,
					FailTime: now,
					FailAmt:  1,
				})
		} else {
			log.Debugf("Encouraging payments from %v to %v",
				nodeFeeInfo.node, r.target)
			entries = append(
				entries, lndclient.MissionControlEntry{
					NodeFrom:    nodeFeeInfo.node,
					NodeTo:      r.target,
					SuccessTime: now,
					SuccessAmt: lnwire.MilliSatoshi(
						nodeFeeInfo.capacity * 1000,
					),
				})
			allowed++
		}
	}

	// There's no point retrying the payment since we discouraged using
	// all inbound peers to the target.
	if allowed == 0 {
		return ErrRoutingPluginNoMoreRetries
	}

	err := r.lnd.Router.ImportMissionControl(ctx, entries, true)
	if err != nil {
		return err
	}

	// Flag that we have changed the MC state.
	r.mcChanged = true

	log.Tracef("Imported MC state: %v", spew.Sdump(entries))

	// If logging in trace level, query routes and log to see how our
	// changes affected path finding.
	if log.Level() == btclog.LevelTrace {
		res, err := r.lnd.Client.QueryRoutes(ctx, queryRoutesReq)
		log.Tracef("BeforePayment() QueryRoutes(2)=%v, err=%v",
			spew.Sdump(res), err)
	}

	return nil
}

// Done will attempt to reconstruct the MC state for the affected node pairs to
// the same state as it was before using the routing plugin. For those node
// pairs where the beginning state was empty, we set success for the maximum
// capacity for the sake of simplicity.
func (r *lowToHighRoutingPlugin) Done(ctx context.Context) error {
	if r.mcState == nil {
		return nil
	}

	defer func() {
		r.mcState = nil
	}()

	// If none of the selected pairs were manipulated we can skip ahead.
	if !r.mcChanged {
		log.Debugf("MC state not changed, skipping restore")
		return nil
	}

	// With the forced import we're safe to just set the pair history
	// timestamps to the current time as import will always succeed and
	// override current MC state.
	now := r.clock.Now()
	entries := make(
		[]lndclient.MissionControlEntry, 0, len(r.nodesByMaxFee),
	)
	for _, nodeInfo := range r.nodesByMaxFee {
		// We didn't have MC state for this node pair before, so just
		// set it to succeed the max amount and fail anything more than
		// that. This way we don't restrict forwarding for normal cases.
		if _, ok := r.mcState[nodeInfo.node]; !ok {
			capacity := lnwire.MilliSatoshi(
				nodeInfo.capacity * 1000,
			)
			entries = append(
				entries, lndclient.MissionControlEntry{
					NodeFrom:    nodeInfo.node,
					NodeTo:      r.target,
					FailTime:    now,
					FailAmt:     capacity + 1,
					SuccessTime: now,
					SuccessAmt:  capacity,
				})
		} else {
			// We did have a MC entry for this pair, so we just bump
			// the time to now + 1 sec.
			entry := r.mcState[nodeInfo.node]

			if !entry.FailTime.IsZero() {
				entry.FailTime = now
			}

			if !entry.SuccessTime.IsZero() {
				entry.SuccessTime = now
			}

			entries = append(entries, entry)
		}
	}

	err := r.lnd.Router.ImportMissionControl(ctx, entries, true)
	if err != nil {
		return err
	}

	log.Debugf("Restored partial MC state: %v",
		spew.Sdump(entries))

	return nil
}
