package script

import (
	"fmt"

	"go.starlark.net/starlark"
)

// loopOutBuiltin creates a loop out swap decision.
// Usage: loop_out(amount, channel_ids) or loop_out(amount, channel_ids, priority)
func loopOutBuiltin(thread *starlark.Thread, fn *starlark.Builtin,
	args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {

	var amount starlark.Int
	var channelIDs *starlark.List
	var priority starlark.Int = starlark.MakeInt(1)

	if err := starlark.UnpackArgs(fn.Name(), args, kwargs,
		"amount", &amount,
		"channel_ids", &channelIDs,
		"priority?", &priority,
	); err != nil {
		return nil, err
	}

	// Validate amount.
	amtVal, ok := amount.Int64()
	if !ok {
		return nil, fmt.Errorf("amount overflow")
	}
	if amtVal <= 0 {
		return nil, fmt.Errorf("amount must be positive")
	}

	// Convert channel IDs.
	ids := make([]starlark.Value, channelIDs.Len())
	for i := 0; i < channelIDs.Len(); i++ {
		ids[i] = channelIDs.Index(i)
	}

	priorityVal, ok := priority.Int64()
	if !ok {
		return nil, fmt.Errorf("priority overflow")
	}

	// Create the decision dict.
	dict := starlark.NewDict(4)
	if err := dict.SetKey(starlark.String("type"),
		starlark.String(SwapTypeLoopOut)); err != nil {
		return nil, err
	}
	if err := dict.SetKey(starlark.String("amount"), amount); err != nil {
		return nil, err
	}
	if err := dict.SetKey(starlark.String("channel_ids"),
		starlark.NewList(ids)); err != nil {
		return nil, err
	}
	if err := dict.SetKey(starlark.String("priority"),
		starlark.MakeInt64(priorityVal)); err != nil {
		return nil, err
	}

	return dict, nil
}

// loopInBuiltin creates a loop in swap decision.
// Usage: loop_in(amount, peer_pubkey) or loop_in(amount, peer_pubkey, priority)
func loopInBuiltin(thread *starlark.Thread, fn *starlark.Builtin,
	args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {

	var amount starlark.Int
	var peerPubkey starlark.String
	var priority starlark.Int = starlark.MakeInt(1)

	if err := starlark.UnpackArgs(fn.Name(), args, kwargs,
		"amount", &amount,
		"peer_pubkey", &peerPubkey,
		"priority?", &priority,
	); err != nil {
		return nil, err
	}

	// Validate amount.
	amtVal, ok := amount.Int64()
	if !ok {
		return nil, fmt.Errorf("amount overflow")
	}
	if amtVal <= 0 {
		return nil, fmt.Errorf("amount must be positive")
	}

	// Validate peer pubkey.
	if len(peerPubkey) == 0 {
		return nil, fmt.Errorf("peer_pubkey cannot be empty")
	}

	priorityVal, ok := priority.Int64()
	if !ok {
		return nil, fmt.Errorf("priority overflow")
	}

	// Create the decision dict.
	dict := starlark.NewDict(4)
	if err := dict.SetKey(starlark.String("type"),
		starlark.String(SwapTypeLoopIn)); err != nil {
		return nil, err
	}
	if err := dict.SetKey(starlark.String("amount"), amount); err != nil {
		return nil, err
	}
	if err := dict.SetKey(starlark.String("peer_pubkey"), peerPubkey); err != nil {
		return nil, err
	}
	if err := dict.SetKey(starlark.String("priority"),
		starlark.MakeInt64(priorityVal)); err != nil {
		return nil, err
	}

	return dict, nil
}
