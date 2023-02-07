module github.com/lightninglabs/loop

require (
	github.com/btcsuite/btcd v0.23.5-0.20230125025938-be056b0a0b2f
	github.com/btcsuite/btcd/btcec/v2 v2.3.2
	github.com/btcsuite/btcd/btcutil v1.1.3
	github.com/btcsuite/btcd/btcutil/psbt v1.1.5
	github.com/btcsuite/btcd/chaincfg/chainhash v1.0.2
	github.com/btcsuite/btclog v0.0.0-20170628155309-84c8d2346e9f
	github.com/btcsuite/btcwallet v0.16.6-0.20221208210930-6f3f55efb230
	github.com/btcsuite/btcwallet/wtxmgr v1.5.0
	github.com/coreos/bbolt v1.3.3
	github.com/davecgh/go-spew v1.1.1
	github.com/decred/dcrd/dcrec/secp256k1/v4 v4.0.1
	github.com/fortytw2/leaktest v1.3.0
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.5.0
	github.com/jessevdk/go-flags v1.4.0
	github.com/lightninglabs/aperture v0.1.18-beta
	github.com/lightninglabs/lndclient v0.16.0-8
	github.com/lightninglabs/loop/swapserverrpc v1.0.1
	github.com/lightninglabs/protobuf-hex-display v1.4.3-hex-display
	github.com/lightningnetwork/lnd v0.15.0-beta.rc6.0.20230206122140-0cf0a7dd3b08
	github.com/lightningnetwork/lnd/cert v1.2.0
	github.com/lightningnetwork/lnd/clock v1.1.0
	github.com/lightningnetwork/lnd/queue v1.1.0
	github.com/lightningnetwork/lnd/ticker v1.1.0
	github.com/lightningnetwork/lnd/tor v1.1.0
	github.com/stretchr/testify v1.8.0
	github.com/urfave/cli v1.22.9
	golang.org/x/net v0.4.0
	google.golang.org/grpc v1.41.0
	google.golang.org/protobuf v1.27.1
	gopkg.in/macaroon-bakery.v2 v2.0.1
	gopkg.in/macaroon.v2 v2.1.0
)

// TODO(bhandras): remove when the next swapserverrpc is tagged.
replace github.com/lightninglabs/loop/swapserverrpc => ./swapserverrpc

go 1.16
