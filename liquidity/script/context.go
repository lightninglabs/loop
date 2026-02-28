package script

import (
	"time"

	"go.starlark.net/starlark"
)

// AutoloopContext provides all data available to Starlark scripts for making
// swap decisions.
type AutoloopContext struct {
	// Channels provides access to all channel information.
	Channels []ChannelInfo

	// Peers provides aggregated per-peer data.
	Peers []PeerInfo

	// TotalLocal is the sum of all local balances in satoshis.
	TotalLocal int64

	// TotalRemote is the sum of all remote balances in satoshis.
	TotalRemote int64

	// TotalCapacity is the sum of all channel capacities in satoshis.
	TotalCapacity int64

	// Restrictions contains server-imposed swap limits.
	Restrictions SwapRestrictions

	// Budget contains budget information for autoloop.
	Budget BudgetInfo

	// InFlight contains information about ongoing swaps.
	InFlight InFlightInfo

	// CurrentTime is the current time.
	CurrentTime time.Time
}

// ChannelInfo represents a single channel's state.
type ChannelInfo struct {
	// ChannelID is the short channel ID.
	ChannelID uint64

	// PeerPubkey is the hex-encoded public key of the channel peer.
	PeerPubkey string

	// Capacity is the total channel capacity in satoshis.
	Capacity int64

	// LocalBalance is the local balance in satoshis.
	LocalBalance int64

	// RemoteBalance is the remote balance in satoshis.
	RemoteBalance int64

	// Active indicates whether the channel is active.
	Active bool

	// Private indicates whether the channel is private.
	Private bool

	// LocalPercent is the local balance as a percentage of capacity (0-100).
	LocalPercent float64

	// RemotePercent is the remote balance as a percentage of capacity (0-100).
	RemotePercent float64

	// HasLoopOutSwap indicates whether this channel has an ongoing loop out.
	HasLoopOutSwap bool

	// HasLoopInSwap indicates whether this channel's peer has an ongoing loop in.
	HasLoopInSwap bool

	// RecentlyFailed indicates a swap on this channel failed within backoff.
	RecentlyFailed bool

	// FailedAt is the Unix timestamp of the last failure (0 if none).
	FailedAt int64

	// IsCustomChannel indicates whether this is an asset/custom channel.
	IsCustomChannel bool
}

// Starlark interface implementations for ChannelInfo.
var (
	_ starlark.Value    = (*ChannelInfo)(nil)
	_ starlark.HasAttrs = (*ChannelInfo)(nil)
)

// String implements starlark.Value.
func (c *ChannelInfo) String() string {
	return "<channel>"
}

// Type implements starlark.Value.
func (c *ChannelInfo) Type() string {
	return "channel"
}

// Freeze implements starlark.Value.
func (c *ChannelInfo) Freeze() {}

// Truth implements starlark.Value.
func (c *ChannelInfo) Truth() starlark.Bool {
	return starlark.True
}

// Hash implements starlark.Value.
func (c *ChannelInfo) Hash() (uint32, error) {
	return uint32(c.ChannelID), nil
}

// Attr implements starlark.HasAttrs.
func (c *ChannelInfo) Attr(name string) (starlark.Value, error) {
	switch name {
	case "channel_id":
		return starlark.MakeInt64(int64(c.ChannelID)), nil
	case "peer_pubkey":
		return starlark.String(c.PeerPubkey), nil
	case "capacity":
		return starlark.MakeInt64(c.Capacity), nil
	case "local_balance":
		return starlark.MakeInt64(c.LocalBalance), nil
	case "remote_balance":
		return starlark.MakeInt64(c.RemoteBalance), nil
	case "active":
		return starlark.Bool(c.Active), nil
	case "private":
		return starlark.Bool(c.Private), nil
	case "local_percent":
		return starlark.Float(c.LocalPercent), nil
	case "remote_percent":
		return starlark.Float(c.RemotePercent), nil
	case "has_loop_out_swap":
		return starlark.Bool(c.HasLoopOutSwap), nil
	case "has_loop_in_swap":
		return starlark.Bool(c.HasLoopInSwap), nil
	case "recently_failed":
		return starlark.Bool(c.RecentlyFailed), nil
	case "failed_at":
		return starlark.MakeInt64(c.FailedAt), nil
	case "is_custom_channel":
		return starlark.Bool(c.IsCustomChannel), nil
	default:
		return nil, starlark.NoSuchAttrError(name)
	}
}

// AttrNames implements starlark.HasAttrs.
func (c *ChannelInfo) AttrNames() []string {
	return []string{
		"channel_id", "peer_pubkey", "capacity", "local_balance",
		"remote_balance", "active", "private", "local_percent",
		"remote_percent", "has_loop_out_swap", "has_loop_in_swap",
		"recently_failed", "failed_at", "is_custom_channel",
	}
}

// PeerInfo provides aggregated data about a peer across all channels.
type PeerInfo struct {
	// Pubkey is the hex-encoded public key of the peer.
	Pubkey string

	// TotalCapacity is the sum of all channel capacities with this peer.
	TotalCapacity int64

	// TotalLocal is the sum of all local balances with this peer.
	TotalLocal int64

	// TotalRemote is the sum of all remote balances with this peer.
	TotalRemote int64

	// ChannelCount is the number of channels with this peer.
	ChannelCount int

	// ChannelIDs lists all channel IDs with this peer.
	ChannelIDs []uint64

	// LocalPercent is the total local balance as a percentage of capacity.
	LocalPercent float64

	// RemotePercent is the total remote balance as a percentage of capacity.
	RemotePercent float64

	// HasLoopInSwap indicates an ongoing loop in with this peer.
	HasLoopInSwap bool
}

// Starlark interface implementations for PeerInfo.
var (
	_ starlark.Value    = (*PeerInfo)(nil)
	_ starlark.HasAttrs = (*PeerInfo)(nil)
)

// String implements starlark.Value.
func (p *PeerInfo) String() string {
	return "<peer>"
}

// Type implements starlark.Value.
func (p *PeerInfo) Type() string {
	return "peer"
}

// Freeze implements starlark.Value.
func (p *PeerInfo) Freeze() {}

// Truth implements starlark.Value.
func (p *PeerInfo) Truth() starlark.Bool {
	return starlark.True
}

// Hash implements starlark.Value.
func (p *PeerInfo) Hash() (uint32, error) {
	h := uint32(0)
	for _, b := range p.Pubkey {
		h = h*31 + uint32(b)
	}
	return h, nil
}

// Attr implements starlark.HasAttrs.
func (p *PeerInfo) Attr(name string) (starlark.Value, error) {
	switch name {
	case "pubkey":
		return starlark.String(p.Pubkey), nil
	case "total_capacity":
		return starlark.MakeInt64(p.TotalCapacity), nil
	case "total_local":
		return starlark.MakeInt64(p.TotalLocal), nil
	case "total_remote":
		return starlark.MakeInt64(p.TotalRemote), nil
	case "channel_count":
		return starlark.MakeInt(p.ChannelCount), nil
	case "channel_ids":
		ids := make([]starlark.Value, len(p.ChannelIDs))
		for i, id := range p.ChannelIDs {
			ids[i] = starlark.MakeInt64(int64(id))
		}
		return starlark.NewList(ids), nil
	case "local_percent":
		return starlark.Float(p.LocalPercent), nil
	case "remote_percent":
		return starlark.Float(p.RemotePercent), nil
	case "has_loop_in_swap":
		return starlark.Bool(p.HasLoopInSwap), nil
	default:
		return nil, starlark.NoSuchAttrError(name)
	}
}

// AttrNames implements starlark.HasAttrs.
func (p *PeerInfo) AttrNames() []string {
	return []string{
		"pubkey", "total_capacity", "total_local", "total_remote",
		"channel_count", "channel_ids", "local_percent", "remote_percent",
		"has_loop_in_swap",
	}
}

// SwapRestrictions contains server-imposed limits on swap amounts.
type SwapRestrictions struct {
	// MinLoopOut is the minimum loop out amount in satoshis.
	MinLoopOut int64

	// MaxLoopOut is the maximum loop out amount in satoshis.
	MaxLoopOut int64

	// MinLoopIn is the minimum loop in amount in satoshis.
	MinLoopIn int64

	// MaxLoopIn is the maximum loop in amount in satoshis.
	MaxLoopIn int64
}

// Starlark interface implementations for SwapRestrictions.
var (
	_ starlark.Value    = (*SwapRestrictions)(nil)
	_ starlark.HasAttrs = (*SwapRestrictions)(nil)
)

// String implements starlark.Value.
func (r *SwapRestrictions) String() string {
	return "<restrictions>"
}

// Type implements starlark.Value.
func (r *SwapRestrictions) Type() string {
	return "restrictions"
}

// Freeze implements starlark.Value.
func (r *SwapRestrictions) Freeze() {}

// Truth implements starlark.Value.
func (r *SwapRestrictions) Truth() starlark.Bool {
	return starlark.True
}

// Hash implements starlark.Value.
func (r *SwapRestrictions) Hash() (uint32, error) {
	return 0, nil
}

// Attr implements starlark.HasAttrs.
func (r *SwapRestrictions) Attr(name string) (starlark.Value, error) {
	switch name {
	case "min_loop_out":
		return starlark.MakeInt64(r.MinLoopOut), nil
	case "max_loop_out":
		return starlark.MakeInt64(r.MaxLoopOut), nil
	case "min_loop_in":
		return starlark.MakeInt64(r.MinLoopIn), nil
	case "max_loop_in":
		return starlark.MakeInt64(r.MaxLoopIn), nil
	default:
		return nil, starlark.NoSuchAttrError(name)
	}
}

// AttrNames implements starlark.HasAttrs.
func (r *SwapRestrictions) AttrNames() []string {
	return []string{"min_loop_out", "max_loop_out", "min_loop_in", "max_loop_in"}
}

// BudgetInfo contains budget information for autoloop.
type BudgetInfo struct {
	// TotalBudget is the total autoloop budget in satoshis.
	TotalBudget int64

	// SpentAmount is the amount already spent in this budget period.
	SpentAmount int64

	// PendingAmount is the amount pending in in-flight swaps.
	PendingAmount int64

	// RemainingAmount is the remaining budget (total - spent - pending).
	RemainingAmount int64
}

// Starlark interface implementations for BudgetInfo.
var (
	_ starlark.Value    = (*BudgetInfo)(nil)
	_ starlark.HasAttrs = (*BudgetInfo)(nil)
)

// String implements starlark.Value.
func (b *BudgetInfo) String() string {
	return "<budget>"
}

// Type implements starlark.Value.
func (b *BudgetInfo) Type() string {
	return "budget"
}

// Freeze implements starlark.Value.
func (b *BudgetInfo) Freeze() {}

// Truth implements starlark.Value.
func (b *BudgetInfo) Truth() starlark.Bool {
	return starlark.True
}

// Hash implements starlark.Value.
func (b *BudgetInfo) Hash() (uint32, error) {
	return 0, nil
}

// Attr implements starlark.HasAttrs.
func (b *BudgetInfo) Attr(name string) (starlark.Value, error) {
	switch name {
	case "total_budget":
		return starlark.MakeInt64(b.TotalBudget), nil
	case "spent_amount":
		return starlark.MakeInt64(b.SpentAmount), nil
	case "pending_amount":
		return starlark.MakeInt64(b.PendingAmount), nil
	case "remaining_amount":
		return starlark.MakeInt64(b.RemainingAmount), nil
	default:
		return nil, starlark.NoSuchAttrError(name)
	}
}

// AttrNames implements starlark.HasAttrs.
func (b *BudgetInfo) AttrNames() []string {
	return []string{
		"total_budget", "spent_amount", "pending_amount", "remaining_amount",
	}
}

// InFlightInfo contains information about ongoing swaps.
type InFlightInfo struct {
	// LoopOutCount is the number of in-flight loop out swaps.
	LoopOutCount int

	// LoopInCount is the number of in-flight loop in swaps.
	LoopInCount int

	// TotalCount is the total number of in-flight swaps.
	TotalCount int

	// MaxAllowed is the maximum allowed in-flight swaps (MaxAutoInFlight).
	MaxAllowed int

	// LoopOutChannels lists channel IDs with ongoing loop out swaps.
	LoopOutChannels []uint64

	// LoopInPeers lists peer pubkeys with ongoing loop in swaps.
	LoopInPeers []string
}

// Starlark interface implementations for InFlightInfo.
var (
	_ starlark.Value    = (*InFlightInfo)(nil)
	_ starlark.HasAttrs = (*InFlightInfo)(nil)
)

// String implements starlark.Value.
func (f *InFlightInfo) String() string {
	return "<in_flight>"
}

// Type implements starlark.Value.
func (f *InFlightInfo) Type() string {
	return "in_flight"
}

// Freeze implements starlark.Value.
func (f *InFlightInfo) Freeze() {}

// Truth implements starlark.Value.
func (f *InFlightInfo) Truth() starlark.Bool {
	return starlark.True
}

// Hash implements starlark.Value.
func (f *InFlightInfo) Hash() (uint32, error) {
	return 0, nil
}

// Attr implements starlark.HasAttrs.
func (f *InFlightInfo) Attr(name string) (starlark.Value, error) {
	switch name {
	case "loop_out_count":
		return starlark.MakeInt(f.LoopOutCount), nil
	case "loop_in_count":
		return starlark.MakeInt(f.LoopInCount), nil
	case "total_count":
		return starlark.MakeInt(f.TotalCount), nil
	case "max_allowed":
		return starlark.MakeInt(f.MaxAllowed), nil
	case "loop_out_channels":
		ids := make([]starlark.Value, len(f.LoopOutChannels))
		for i, id := range f.LoopOutChannels {
			ids[i] = starlark.MakeInt64(int64(id))
		}
		return starlark.NewList(ids), nil
	case "loop_in_peers":
		peers := make([]starlark.Value, len(f.LoopInPeers))
		for i, p := range f.LoopInPeers {
			peers[i] = starlark.String(p)
		}
		return starlark.NewList(peers), nil
	default:
		return nil, starlark.NoSuchAttrError(name)
	}
}

// AttrNames implements starlark.HasAttrs.
func (f *InFlightInfo) AttrNames() []string {
	return []string{
		"loop_out_count", "loop_in_count", "total_count", "max_allowed",
		"loop_out_channels", "loop_in_peers",
	}
}

// SwapDecision represents a swap decision made by a Starlark script.
type SwapDecision struct {
	// Type is "loop_out" or "loop_in".
	Type string

	// Amount is the swap amount in satoshis.
	Amount int64

	// ChannelIDs specifies outgoing channels for loop out (ignored for loop in).
	ChannelIDs []uint64

	// PeerPubkey specifies the target peer for loop in (ignored for loop out).
	PeerPubkey string

	// Priority determines execution order (higher = first).
	Priority int
}

// SwapTypeLoopOut is the type value for loop out swaps.
const SwapTypeLoopOut = "loop_out"

// SwapTypeLoopIn is the type value for loop in swaps.
const SwapTypeLoopIn = "loop_in"
