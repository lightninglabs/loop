package runtime

import (
	"context"
	"errors"

	"github.com/lightninglabs/loop/swapkit/bridge"
)

// resolveOutcomeProof treats cached evidence as a candidate, not a final
// decision. A reorg can move the identical spend to another block while the
// peer is offline. Rediscovery does not erase the prior durable candidate;
// callers replace it only after the new proof passes every policy check.
func resolveOutcomeProof(ctx context.Context, supplied, cached []byte,
	verify func(context.Context, []byte) error,
	discover func(context.Context) ([]byte, error)) ([]byte, error) {

	if len(supplied) != 0 {
		return supplied, verify(ctx, supplied)
	}
	if len(cached) != 0 {
		err := verify(ctx, cached)
		if err == nil {
			return cached, nil
		}
		if !errors.Is(err, bridge.ErrPending) {
			return nil, err
		}
	}
	raw, err := discover(ctx)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, bridge.ErrPending
	}
	return raw, verify(ctx, raw)
}
