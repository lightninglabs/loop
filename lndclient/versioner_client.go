package lndclient

import (
	"context"
	"fmt"
	"strings"

	"github.com/lightningnetwork/lnd/lnrpc/verrpc"
	"google.golang.org/grpc"
)

// VersionerClient exposes the version of lnd.
type VersionerClient interface {
	// GetVersion returns the version and build information of the lnd
	// daemon.
	GetVersion(ctx context.Context) (*verrpc.Version, error)
}

type versionerClient struct {
	client      verrpc.VersionerClient
	readonlyMac serializedMacaroon
}

func newVersionerClient(conn *grpc.ClientConn,
	readonlyMac serializedMacaroon) *versionerClient {

	return &versionerClient{
		client:      verrpc.NewVersionerClient(conn),
		readonlyMac: readonlyMac,
	}
}

// GetVersion returns the version and build information of the lnd
// daemon.
//
// NOTE: This method is part of the VersionerClient interface.
func (v *versionerClient) GetVersion(ctx context.Context) (*verrpc.Version,
	error) {

	rpcCtx, cancel := context.WithTimeout(
		v.readonlyMac.WithMacaroonAuth(ctx), rpcTimeout,
	)
	defer cancel()
	return v.client.GetVersion(rpcCtx, &verrpc.VersionRequest{})
}

// VersionString returns a nice, human readable string of a version returned by
// the VersionerClient, including all build tags.
func VersionString(version *verrpc.Version) string {
	short := VersionStringShort(version)
	enabledTags := strings.Join(version.BuildTags, ",")
	return fmt.Sprintf("%s, build tags '%s'", short, enabledTags)
}

// VersionStringShort returns a nice, human readable string of a version
// returned by the VersionerClient.
func VersionStringShort(version *verrpc.Version) string {
	versionStr := fmt.Sprintf(
		"v%d.%d.%d", version.AppMajor, version.AppMinor,
		version.AppPatch,
	)
	if version.AppPreRelease != "" {
		versionStr = fmt.Sprintf(
			"%s-%s", versionStr, version.AppPreRelease,
		)
	}
	return versionStr
}
