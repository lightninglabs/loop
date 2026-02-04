package main

import (
	"context"

	"github.com/urfave/cli/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
)

// grpcTransport customizes gRPC dialing and interceptors for sessions.
type grpcTransport interface {
	// Dial returns a gRPC connection for the CLI.
	Dial(cmd *cli.Command) (daemonConn, func(), error)

	// UnaryInterceptor returns the unary interceptor to apply for session
	// flows.
	UnaryInterceptor() grpc.UnaryClientInterceptor

	// StreamInterceptor returns the stream interceptor to apply for session
	// flows.
	StreamInterceptor() grpc.StreamClientInterceptor
}

// daemonConn is the client connection interface required by stop/wait flows.
type daemonConn interface {
	grpc.ClientConnInterface

	// GetState reports the current connectivity state of the client
	// channel.
	GetState() connectivity.State

	// WaitForStateChange blocks until the state changes or the context
	// expires.
	WaitForStateChange(ctx context.Context,
		sourceState connectivity.State) bool

	// Connect forces the channel out of idle mode.
	Connect()
}

// directGrpcTransport establishes real gRPC connections to loopd.
type directGrpcTransport struct{}

// Dial opens a direct gRPC connection.
func (t *directGrpcTransport) Dial(
	cmd *cli.Command) (daemonConn, func(), error) {

	return dialDirectConn(cmd)
}

// UnaryInterceptor returns nil because direct connections do not need wrapping.
func (t *directGrpcTransport) UnaryInterceptor() grpc.UnaryClientInterceptor {
	return nil
}

// StreamInterceptor returns nil because direct connections do not need
// wrapping.
func (t *directGrpcTransport) StreamInterceptor() grpc.StreamClientInterceptor {
	return nil
}

// sessionTransport defines the active gRPC transport for CLI commands.
var sessionTransport grpcTransport = &directGrpcTransport{}

// hookGrpc installs the active gRPC session transport hook.
func hookGrpc(transport grpcTransport) func() {
	prev := sessionTransport
	sessionTransport = transport

	return func() { sessionTransport = prev }
}

// dialDirectConn returns the standard CLI gRPC connection.
func dialDirectConn(cmd *cli.Command) (daemonConn, func(), error) {
	rpcServer := cmd.String("rpcserver")
	tlsCertPath, macaroonPath, err := extractPathArgs(cmd)
	if err != nil {
		return nil, nil, err
	}

	return getClientConn(rpcServer, tlsCertPath, macaroonPath)
}
