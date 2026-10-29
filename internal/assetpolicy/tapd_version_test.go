package policy

import (
	"context"
	"errors"
	"testing"

	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type infoClient struct {
	response *taprpc.GetInfoResponse
	err      error
}

func (c infoClient) GetInfo(context.Context, *taprpc.GetInfoRequest,
	...grpc.CallOption) (*taprpc.GetInfoResponse, error) {

	return c.response, c.err
}

// TestRequireOnchainSwapTapd fails closed when the running backend's version
// is unavailable, malformed, or older than the required verifier.
func TestRequireOnchainSwapTapd(t *testing.T) {
	rpcErr := errors.New("tapd unavailable")
	for _, test := range []struct {
		name   string
		client infoClient
		err    error
	}{
		{
			name: "minimum",
			client: infoClient{response: &taprpc.GetInfoResponse{
				Version: "0.8.6-alpha",
			}},
		},
		{
			name: "older verifier",
			client: infoClient{response: &taprpc.GetInfoResponse{
				Version: "0.8.5-alpha",
			}},
			err: ErrTapdVersionUnsupported,
		},
		{
			name: "unparseable",
			client: infoClient{response: &taprpc.GetInfoResponse{
				Version: "unknown",
			}},
			err: ErrTapdVersionUnsupported,
		},
		{
			name: "missing response",
			err:  ErrTapdVersionUnsupported,
		},
		{
			name: "RPC failure", client: infoClient{err: rpcErr},
			err: rpcErr,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := RequireOnchainSwapTapd(t.Context(), test.client)
			if test.err == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, test.err)
		})
	}
}
