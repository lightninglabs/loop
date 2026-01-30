package script

import (
	"fmt"
	"sync"

	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

// Evaluator handles Starlark script execution and caching.
type Evaluator struct {
	// Cache compiled programs by script hash.
	cacheMu sync.RWMutex
	cache   map[string]*starlark.Program
}

// NewEvaluator creates a new Starlark evaluator.
func NewEvaluator() (*Evaluator, error) {
	return &Evaluator{
		cache: make(map[string]*starlark.Program),
	}, nil
}

// Evaluate compiles (if needed) and evaluates a Starlark script with the given
// context.
func (e *Evaluator) Evaluate(script string,
	ctx *AutoloopContext) ([]SwapDecision, error) {

	// Build the predeclared globals with context data and builtins.
	globals := e.buildGlobals(ctx)

	// Create a new thread for execution.
	thread := &starlark.Thread{Name: "autoloop"}

	// Execute the script using the new API with FileOptions.
	result, err := starlark.ExecFileOptions(
		&syntax.FileOptions{},
		thread, "autoloop.star", script, globals,
	)
	if err != nil {
		return nil, fmt.Errorf("Starlark execution error: %w", err)
	}

	// Extract decisions from the result.
	return extractDecisions(result)
}

// buildGlobals creates the global variables available to scripts.
func (e *Evaluator) buildGlobals(ctx *AutoloopContext) starlark.StringDict {
	// Convert channels to Starlark list.
	channels := make([]starlark.Value, len(ctx.Channels))
	for i := range ctx.Channels {
		channels[i] = &ctx.Channels[i]
	}

	// Convert peers to Starlark list.
	peers := make([]starlark.Value, len(ctx.Peers))
	for i := range ctx.Peers {
		peers[i] = &ctx.Peers[i]
	}

	return starlark.StringDict{
		// Context data.
		"channels":       starlark.NewList(channels),
		"peers":          starlark.NewList(peers),
		"total_local":    starlark.MakeInt64(ctx.TotalLocal),
		"total_remote":   starlark.MakeInt64(ctx.TotalRemote),
		"total_capacity": starlark.MakeInt64(ctx.TotalCapacity),
		"restrictions":   &ctx.Restrictions,
		"budget":         &ctx.Budget,
		"in_flight":      &ctx.InFlight,
		"current_time":   starlark.MakeInt64(ctx.CurrentTime.Unix()),

		// Builtin functions.
		"loop_out": starlark.NewBuiltin("loop_out", loopOutBuiltin),
		"loop_in":  starlark.NewBuiltin("loop_in", loopInBuiltin),
	}
}

// Validate checks if a script compiles without errors.
func (e *Evaluator) Validate(script string) error {
	_, err := starlark.ExecFileOptions(
		&syntax.FileOptions{},
		&starlark.Thread{Name: "validate"},
		"autoloop.star",
		script,
		starlark.StringDict{
			// Provide minimal globals for validation.
			"channels":       starlark.NewList(nil),
			"peers":          starlark.NewList(nil),
			"total_local":    starlark.MakeInt(0),
			"total_remote":   starlark.MakeInt(0),
			"total_capacity": starlark.MakeInt(0),
			"restrictions":   &SwapRestrictions{},
			"budget":         &BudgetInfo{},
			"in_flight":      &InFlightInfo{},
			"current_time":   starlark.MakeInt(0),
			"loop_out":       starlark.NewBuiltin("loop_out", loopOutBuiltin),
			"loop_in":        starlark.NewBuiltin("loop_in", loopInBuiltin),
		},
	)
	return err
}

// ClearCache clears the compiled program cache.
func (e *Evaluator) ClearCache() {
	e.cacheMu.Lock()
	e.cache = make(map[string]*starlark.Program)
	e.cacheMu.Unlock()
}

// extractDecisions extracts SwapDecision values from script results.
func extractDecisions(globals starlark.StringDict) ([]SwapDecision, error) {
	// Look for "decisions" in the result.
	decisionsVal, ok := globals["decisions"]
	if !ok {
		return []SwapDecision{}, nil
	}

	// Must be a list.
	list, ok := decisionsVal.(*starlark.List)
	if !ok {
		return nil, fmt.Errorf("decisions must be a list, got %s",
			decisionsVal.Type())
	}

	decisions := make([]SwapDecision, list.Len())
	for i := 0; i < list.Len(); i++ {
		item := list.Index(i)
		d, err := valueToDecision(item)
		if err != nil {
			return nil, fmt.Errorf("decision %d: %w", i, err)
		}
		decisions[i] = d
	}

	return decisions, nil
}

// valueToDecision converts a Starlark value to a SwapDecision.
func valueToDecision(val starlark.Value) (SwapDecision, error) {
	d := SwapDecision{}

	// Must be a dict.
	dict, ok := val.(*starlark.Dict)
	if !ok {
		return d, fmt.Errorf("decision must be a dict, got %s", val.Type())
	}

	// Extract type.
	typeVal, found, err := dict.Get(starlark.String("type"))
	if err != nil {
		return d, err
	}
	if !found {
		return d, fmt.Errorf("missing 'type' field")
	}
	typeStr, ok := typeVal.(starlark.String)
	if !ok {
		return d, fmt.Errorf("'type' must be a string")
	}
	d.Type = string(typeStr)

	// Extract amount.
	amountVal, found, err := dict.Get(starlark.String("amount"))
	if err != nil {
		return d, err
	}
	if !found {
		return d, fmt.Errorf("missing 'amount' field")
	}
	amountInt, ok := amountVal.(starlark.Int)
	if !ok {
		return d, fmt.Errorf("'amount' must be an int")
	}
	amount, ok := amountInt.Int64()
	if !ok {
		return d, fmt.Errorf("'amount' overflow")
	}
	d.Amount = amount

	// Extract channel_ids (optional for loop_out).
	channelIDsVal, found, err := dict.Get(starlark.String("channel_ids"))
	if err != nil {
		return d, err
	}
	if found {
		channelList, ok := channelIDsVal.(*starlark.List)
		if !ok {
			return d, fmt.Errorf("'channel_ids' must be a list")
		}
		d.ChannelIDs = make([]uint64, channelList.Len())
		for i := 0; i < channelList.Len(); i++ {
			idVal := channelList.Index(i)
			idInt, ok := idVal.(starlark.Int)
			if !ok {
				return d, fmt.Errorf("channel_id must be an int")
			}
			id, ok := idInt.Uint64()
			if !ok {
				return d, fmt.Errorf("channel_id overflow")
			}
			d.ChannelIDs[i] = id
		}
	}

	// Extract peer_pubkey (optional for loop_in).
	peerVal, found, err := dict.Get(starlark.String("peer_pubkey"))
	if err != nil {
		return d, err
	}
	if found {
		peerStr, ok := peerVal.(starlark.String)
		if !ok {
			return d, fmt.Errorf("'peer_pubkey' must be a string")
		}
		d.PeerPubkey = string(peerStr)
	}

	// Extract priority (optional, default 1).
	priorityVal, found, err := dict.Get(starlark.String("priority"))
	if err != nil {
		return d, err
	}
	if found {
		priorityInt, ok := priorityVal.(starlark.Int)
		if !ok {
			return d, fmt.Errorf("'priority' must be an int")
		}
		priority, ok := priorityInt.Int64()
		if !ok {
			return d, fmt.Errorf("'priority' overflow")
		}
		d.Priority = int(priority)
	} else {
		d.Priority = 1
	}

	return d, nil
}

// ValidateDecisions checks that swap decisions are valid.
func ValidateDecisions(decisions []SwapDecision, ctx *AutoloopContext) error {
	for i, d := range decisions {
		if err := validateDecision(d, ctx); err != nil {
			return fmt.Errorf("decision %d: %w", i, err)
		}
	}
	return nil
}

func validateDecision(d SwapDecision, ctx *AutoloopContext) error {
	// Check type.
	if d.Type != SwapTypeLoopOut && d.Type != SwapTypeLoopIn {
		return fmt.Errorf("invalid swap type: %s", d.Type)
	}

	// Check amount.
	if d.Amount <= 0 {
		return fmt.Errorf("amount must be positive: %d", d.Amount)
	}

	// Type-specific validation.
	switch d.Type {
	case SwapTypeLoopOut:
		// Check amount against restrictions.
		if d.Amount < ctx.Restrictions.MinLoopOut {
			return fmt.Errorf(
				"amount %d below minimum %d",
				d.Amount, ctx.Restrictions.MinLoopOut,
			)
		}
		if d.Amount > ctx.Restrictions.MaxLoopOut {
			return fmt.Errorf(
				"amount %d above maximum %d",
				d.Amount, ctx.Restrictions.MaxLoopOut,
			)
		}
		// Check channel IDs exist.
		if len(d.ChannelIDs) == 0 {
			return fmt.Errorf("loop out requires at least one channel ID")
		}
		for _, chanID := range d.ChannelIDs {
			if !channelExists(chanID, ctx.Channels) {
				return fmt.Errorf("channel %d not found", chanID)
			}
		}

	case SwapTypeLoopIn:
		// Check amount against restrictions.
		if d.Amount < ctx.Restrictions.MinLoopIn {
			return fmt.Errorf(
				"amount %d below minimum %d",
				d.Amount, ctx.Restrictions.MinLoopIn,
			)
		}
		if d.Amount > ctx.Restrictions.MaxLoopIn {
			return fmt.Errorf(
				"amount %d above maximum %d",
				d.Amount, ctx.Restrictions.MaxLoopIn,
			)
		}
		// Check peer pubkey.
		if d.PeerPubkey == "" {
			return fmt.Errorf("loop in requires peer pubkey")
		}
		if !peerExists(d.PeerPubkey, ctx.Peers) {
			return fmt.Errorf("peer %s not found", d.PeerPubkey)
		}
	}

	return nil
}

func channelExists(chanID uint64, channels []ChannelInfo) bool {
	for _, c := range channels {
		if c.ChannelID == chanID {
			return true
		}
	}
	return false
}

func peerExists(pubkey string, peers []PeerInfo) bool {
	for _, p := range peers {
		if p.Pubkey == pubkey {
			return true
		}
	}
	return false
}

// SortByPriority sorts swap decisions by priority (highest first).
func SortByPriority(decisions []SwapDecision) {
	// Simple insertion sort since we expect small lists.
	for i := 1; i < len(decisions); i++ {
		key := decisions[i]
		j := i - 1
		for j >= 0 && decisions[j].Priority < key.Priority {
			decisions[j+1] = decisions[j]
			j--
		}
		decisions[j+1] = key
	}
}

// ValidateScript is a package-level function for validating Starlark scripts.
func ValidateScript(script string) error {
	eval, err := NewEvaluator()
	if err != nil {
		return err
	}
	return eval.Validate(script)
}
