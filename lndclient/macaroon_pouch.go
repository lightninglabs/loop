package lndclient

import (
	"context"
	"encoding/hex"
	"io/ioutil"
	"path/filepath"

	"google.golang.org/grpc/metadata"
)

// serializedMacaroon is a type that represents a hex-encoded macaroon. We'll
// use this primarily vs the raw binary format as the gRPC metadata feature
// requires that all keys and values be strings.
type serializedMacaroon string

// newSerializedMacaroon reads a new serializedMacaroon from that target
// macaroon path. If the file can't be found, then an error is returned.
func newSerializedMacaroon(macaroonPath string) (serializedMacaroon, error) {
	macBytes, err := ioutil.ReadFile(macaroonPath)
	if err != nil {
		return "", err
	}

	return serializedMacaroon(hex.EncodeToString(macBytes)), nil
}

// WithMacaroonAuth modifies the passed context to include the macaroon KV
// metadata of the target macaroon. This method can be used to add the macaroon
// at call time, rather than when the connection to the gRPC server is created.
func (s serializedMacaroon) WithMacaroonAuth(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "macaroon", string(s))
}

// macaroonPouch holds the set of macaroons we need to interact with lnd for
// Loop. Each sub-server has its own macaroon, and for the remaining temporary
// calls that directly hit lnd, we'll use the admin macaroon.
type macaroonPouch struct {
	// invoiceMac is the macaroon for the invoices sub-server.
	invoiceMac serializedMacaroon

	// chainMac is the macaroon for the ChainNotifier sub-server.
	chainMac serializedMacaroon

	// signerMac is the macaroon for the Signer sub-server.
	signerMac serializedMacaroon

	// walletKitMac is the macaroon for the WalletKit sub-server.
	walletKitMac serializedMacaroon

	// routerMac is the macaroon for the router sub-server.
	routerMac serializedMacaroon

	// adminMac is the primary admin macaroon for lnd.
	adminMac serializedMacaroon
}

// newMacaroonPouch returns a new instance of a fully populated macaroonPouch
// given the directory where all the macaroons are stored.
func newMacaroonPouch(macaroonDir string) (*macaroonPouch, error) {
	m := &macaroonPouch{}

	var err error

	m.invoiceMac, err = newSerializedMacaroon(
		filepath.Join(macaroonDir, defaultInvoiceMacaroonFilename),
	)
	if err != nil {
		return nil, err
	}

	m.chainMac, err = newSerializedMacaroon(
		filepath.Join(macaroonDir, defaultChainMacaroonFilename),
	)
	if err != nil {
		return nil, err
	}

	m.signerMac, err = newSerializedMacaroon(
		filepath.Join(macaroonDir, defaultSignerFilename),
	)
	if err != nil {
		return nil, err
	}

	m.walletKitMac, err = newSerializedMacaroon(
		filepath.Join(macaroonDir, defaultWalletKitMacaroonFilename),
	)
	if err != nil {
		return nil, err
	}

	m.routerMac, err = newSerializedMacaroon(
		filepath.Join(macaroonDir, defaultRouterMacaroonFilename),
	)
	if err != nil {
		return nil, err
	}

	m.adminMac, err = newSerializedMacaroon(
		filepath.Join(macaroonDir, defaultAdminMacaroonFilename),
	)
	if err != nil {
		return nil, err
	}

	return m, nil
}
