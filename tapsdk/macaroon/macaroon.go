package macaroon

import (
	"context"
	"encoding/hex"
	"os"

	"google.golang.org/grpc/metadata"
)

// TaprpcServiceMac is the name of a macaroon that can be used to authenticate
// with a specific taprpc service.
type TaprpcServiceMac string

//nolint:revive
const (
	AdminServiceMac     TaprpcServiceMac = "admin.macaroon"
	WalletKitServiceMac TaprpcServiceMac = "walletkit.macaroon"
	ProofServiceMac     TaprpcServiceMac = "proof.macaroon"
	UniverseServiceMac  TaprpcServiceMac = "universe.macaroon"
	MintServiceMac      TaprpcServiceMac = "mint.macaroon"
	ReadOnlyServiceMac  TaprpcServiceMac = "readonly.macaroon"
)

var (
	// macaroonServices is the default list of macaroon file names
	// that the SDK will attempt to load if a macaroon directory is given
	// instead of a single custom macaroon.
	macaroonServices = []TaprpcServiceMac{
		WalletKitServiceMac,
		AdminServiceMac,
		ProofServiceMac,
		UniverseServiceMac,
		MintServiceMac,
		ReadOnlyServiceMac,
	}
)

// SerializedMacaroon is a type that represents a hex-encoded macaroon. We'll
// use this primarily vs the raw binary format as the gRPC metadata feature
// requires that all keys and values be strings.
type SerializedMacaroon string

// newSerializedMacaroon reads a new serializedMacaroon from that target
// macaroon path. If the file can't be found, then an error is returned.
func newSerializedMacaroon(macaroonPath string) (SerializedMacaroon, error) {
	macBytes, err := os.ReadFile(macaroonPath)
	if err != nil {
		return "", err
	}

	return SerializedMacaroon(hex.EncodeToString(macBytes)), nil
}

// WithMacaroonAuth modifies the passed context to include the macaroon KV
// metadata of the target macaroon. This method can be used to add the macaroon
// at call time, rather than when the connection to the gRPC server is created.
// An empty macaroon leaves authentication to the context or connection
// credentials.
func (s SerializedMacaroon) WithMacaroonAuth(
	ctx context.Context) context.Context {

	if s == "" {
		return ctx
	}

	return metadata.AppendToOutgoingContext(ctx, "macaroon", string(s))
}

// Pouch holds the set of macaroons we need to interact with tapd.
// Each sub-server has its own macaroon, and for the remaining temporary
// calls that directly hit tapd, we'll use the admin macaroon.
type Pouch map[TaprpcServiceMac]SerializedMacaroon
