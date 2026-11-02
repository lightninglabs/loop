package rest

import (
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ErrUnsupportedNetwork is returned when an unknown network is
// specified in the configuration.
var ErrUnsupportedNetwork = errors.New("unsupported network")

// APIError represents an error returned by the tapd REST API.
type APIError struct {
	StatusCode int
	GRPCCode   codes.Code
	Message    string
	Details    string
}

func (e *APIError) Error() string {
	if e.Details != "" {
		return fmt.Sprintf("tapd REST API error %d: %s (%s)",
			e.StatusCode, e.Message, e.Details)
	}

	return fmt.Sprintf("tapd REST API error %d: %s",
		e.StatusCode, e.Message)
}

// GRPCStatus exposes the embedded gRPC code so callers can use the same
// status helpers as the native gRPC transport.
func (e *APIError) GRPCStatus() *status.Status {
	return status.New(e.GRPCCode, e.Message)
}
