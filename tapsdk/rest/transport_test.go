package rest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lightninglabs/loop/tapsdk/macaroon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestTransportDoGetReturnsTypedGatewayError verifies that grpc-gateway
// errors preserve their gRPC code through the REST transport.
func TestTransportDoGetReturnsTypedGatewayError(t *testing.T) {
	t.Parallel()

	// Mirror tapd's real wire format: grpc-gateway emits `details` as
	// a JSON array of protobuf Any messages. Earlier revisions typed
	// this field as `string`, which silently fell through to the
	// opaque path and dropped the gRPC code.
	const body = `{"code":3,"message":"bad input","details":` +
		`[{"@type":"type.googleapis.com/x","reason":"bad"}]}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {

		assert.Equal(t, "/v1/test", r.URL.Path)

		w.WriteHeader(http.StatusBadRequest)
		_, err := fmt.Fprint(w, body)
		assert.NoError(t, err)
	}))
	defer srv.Close()

	tp := &transport{
		baseURL:   srv.URL,
		client:    srv.Client(),
		timeout:   time.Second,
		macaroons: macaroon.Pouch{},
	}

	err := tp.doGet(
		context.Background(), "/v1/test", macaroon.AdminServiceMac,
		nil,
	)
	require.Error(t, err)

	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
	require.Equal(t, codes.InvalidArgument, apiErr.GRPCCode)
	require.Equal(t, "bad input", apiErr.Message)
	require.Contains(t, apiErr.Details, `"reason":"bad"`)

	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.InvalidArgument, st.Code())
}

// TestTransportDoGetOpaqueErrorUsesUnknownCode verifies that non-JSON
// failures still map to a typed error with an unknown gRPC code.
func TestTransportDoGetOpaqueErrorUsesUnknownCode(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,
		_ *http.Request) {

		w.WriteHeader(http.StatusInternalServerError)
		_, err := fmt.Fprint(w, "upstream exploded")
		assert.NoError(t, err)
	}))
	defer srv.Close()

	tp := &transport{
		baseURL:   srv.URL,
		client:    srv.Client(),
		timeout:   time.Second,
		macaroons: macaroon.Pouch{},
	}

	err := tp.doGet(
		context.Background(), "/v1/test", macaroon.AdminServiceMac,
		nil,
	)
	require.Error(t, err)

	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusInternalServerError, apiErr.StatusCode)
	require.Equal(t, codes.Unknown, apiErr.GRPCCode)
	require.Equal(t, "upstream exploded", apiErr.Message)

	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.Unknown, st.Code())
}
