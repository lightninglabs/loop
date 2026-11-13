package tapsdk

import (
	"context"
	"time"

	sdk "github.com/lightninglabs/tap-sdk"
	sdkgrpc "github.com/lightninglabs/tap-sdk/grpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

const defaultClientTimeout = 30 * time.Second

// anchorWalletClient preserves the SDK's optional custom-anchor operations.
type anchorWalletClient interface {
	sdk.WalletKitClient
	sdk.CustomAnchorWalletKitClient
	sdk.CustomAnchorCapabilityProvider
}

// issuanceProofClient preserves registration of a specific grouped issuance.
type issuanceProofClient interface {
	sdk.ProofClient

	RegisterTransferWithIssuance(context.Context, sdk.AssetRef, sdk.AssetID,
		sdk.PubKey, sdk.Outpoint) (*sdk.RegisteredAsset, error)
}

// connectionClient composes the released SDK clients without owning their
// shared connection. All RPC encoding and asset operations remain in the SDK.
type connectionClient struct {
	sdk.WalletClient
	anchorWalletClient
	issuanceProofClient
	sdk.UniverseClient
	sdk.MintClient
	sdk.EventClient
}

// NewClientFromConn adapts an existing authenticated tapd connection to the SDK.
// The caller owns transport security, credentials, message limits and connection
// shutdown. Closing the returned client leaves that connection open. Non-positive
// timeouts use 30 seconds; streaming calls retain the caller's context lifetime.
func NewClientFromConn(conn grpc.ClientConnInterface,
	timeout time.Duration) sdk.Client {

	if timeout <= 0 {
		timeout = defaultClientTimeout
	}
	conn = credentialConn{ClientConnInterface: conn}

	return &connectionClient{
		WalletClient: sdkgrpc.NewWalletClient(conn, timeout, ""),
		anchorWalletClient: sdkgrpc.NewWalletKitClient(
			conn, timeout, "",
		),
		issuanceProofClient: sdkgrpc.NewProofClient(conn, timeout, ""),
		UniverseClient:      sdkgrpc.NewUniverseClient(conn, timeout, ""),
		MintClient:          sdkgrpc.NewMintClient(conn, timeout, ""),
		EventClient: sdkgrpc.NewEventClient(
			conn, timeout, "", "",
		),
	}
}

// Close leaves the caller-owned connection open.
func (*connectionClient) Close() error {
	return nil
}

// credentialConn removes the empty macaroon appended by SDK v0.1.1. The
// existing context metadata and connection credentials remain authoritative.
type credentialConn struct {
	grpc.ClientConnInterface
}

func (c credentialConn) Invoke(ctx context.Context, method string,
	args, reply any, opts ...grpc.CallOption) error {

	return c.ClientConnInterface.Invoke(
		withoutSDKMacaroon(ctx), method, args, reply, opts...,
	)
}

func (c credentialConn) NewStream(ctx context.Context, desc *grpc.StreamDesc,
	method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {

	return c.ClientConnInterface.NewStream(
		withoutSDKMacaroon(ctx), desc, method, opts...,
	)
}

// withoutSDKMacaroon removes only the SDK's trailing empty credential. The
// metadata returned by FromOutgoingContext is a copy, so the caller is unchanged.
func withoutSDKMacaroon(ctx context.Context) context.Context {
	md, _ := metadata.FromOutgoingContext(ctx)
	values := md.Get("macaroon")
	if len(values) == 0 || values[len(values)-1] != "" {
		return ctx
	}
	if len(values) == 1 {
		delete(md, "macaroon")
	} else {
		md.Set("macaroon", values[:len(values)-1]...)
	}
	return metadata.NewOutgoingContext(ctx, md)
}
