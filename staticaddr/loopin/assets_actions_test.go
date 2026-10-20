package loopin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lightninglabs/loop/staticaddr/loopin/assetclient"
	"github.com/lightninglabs/loop/staticaddr/loopin/assetexec"
	"github.com/lightninglabs/loop/swapkit/bridge"
	"github.com/lightninglabs/loop/swapserverrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

const validHint = "valid"

type claimHintServer struct {
	assetclient.Server

	get func(context.Context) (*swapserverrpc.AssetLoopInStatus, error)
}

func (s claimHintServer) GetAssetLoopIn(ctx context.Context,
	_ *swapserverrpc.AssetLoopInReference, _ ...grpc.CallOption) (
	*swapserverrpc.AssetLoopInStatus, error) {

	return s.get(ctx)
}

type claimHintRuntime struct {
	call func(context.Context, bridge.Operation) ([]byte, error)
}

func (r claimHintRuntime) Call(ctx context.Context, method bridge.Method,
	request, response any) error {

	if method != bridge.InspectClaim {
		return errors.New("unexpected runtime method")
	}
	proof, err := r.call(ctx, request.(bridge.Operation))
	*response.(*[]byte) = proof
	return err
}

// TestClaimHintsCannotVetoChainDiscovery ensures untrusted proof hints never
// suppress independent recovery or consume its remaining action deadline.
func TestClaimHintsCannotVetoChainDiscovery(t *testing.T) {
	for _, scenario := range []string{
		validHint, "invalid", "stale", "empty", "verification stalled",
		"server stalled", "missing", "unclaimed", "chain failure",
	} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			testClaimHint(t, scenario)
		})
	}
}

func testClaimHint(t *testing.T, scenario string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	offer := assetexec.Offer{Hash: [32]byte{1}}
	fallback := assetexec.Fallback{Bundle: []byte("original bundle")}
	chainFailure := errors.New("chain temporarily unavailable")
	proofFailure := errors.New("strict supplied-proof rejection")
	calls := 0
	server := claimHintServer{get: func(ctx context.Context) (
		*swapserverrpc.AssetLoopInStatus, error) {

		if scenario == "server stalled" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		proof := []byte("untrusted peer proof")
		if scenario == "missing" {
			proof = nil
		}
		return &swapserverrpc.AssetLoopInStatus{
			Claimed:         scenario != "unclaimed",
			ResolutionProof: proof,
		}, nil
	}}
	runtime := claimHintRuntime{call: func(ctx context.Context,
		request bridge.Operation) ([]byte, error) {

		calls++
		require.Equal(t, offer.Hash, request.Offer.Hash)
		require.Equal(t, fallback.Bundle, request.Fallback.Bundle)
		if len(request.Evidence) == 0 {
			require.NoError(t, ctx.Err())
			if scenario == "chain failure" {
				return nil, chainFailure
			}
			return []byte("independent confirmed chain proof"), nil
		}
		require.Equal(t, 1, calls)
		require.Equal(
			t, []byte("untrusted peer proof"), request.Evidence,
		)
		switch scenario {
		case validHint:
			return []byte("strictly verified peer proof"), nil
		case "empty":
			return nil, nil
		case "verification stalled":
			<-ctx.Done()
			return nil, ctx.Err()
		default:
			unverified := []byte("discarded unverified output")
			return unverified, proofFailure
		}
	}}
	actions := &runtimeActions{manager: &Manager{assetCfg: &AssetConfig{
		Server: server, Runtime: runtime,
	}}}
	proof, err := actions.InspectClaim(ctx, offer, fallback)
	require.NoError(t, ctx.Err())
	if scenario == "chain failure" {
		require.ErrorIs(t, err, chainFailure)
		require.Empty(t, proof)
	} else {
		require.NoError(t, err)
		want := "independent confirmed chain proof"
		if scenario == validHint {
			want = "strictly verified peer proof"
		}
		require.Equal(t, []byte(want), proof)
	}
	wantCalls := 2
	switch scenario {
	case validHint, "server stalled", "missing", "unclaimed":
		wantCalls = 1
	}
	require.Equal(t, wantCalls, calls)
}
