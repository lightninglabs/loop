module github.com/lightninglabs/loop

require (
	github.com/btcsuite/btcd v0.22.0-beta.0.20220316175102-8d5c75c28923
	github.com/btcsuite/btcd/btcec/v2 v2.1.3
	github.com/btcsuite/btcd/btcutil v1.1.1
	// github.com/btcsuite/btcd/txscript v1.1.0
	github.com/btcsuite/btcd/chaincfg/chainhash v1.0.1
	github.com/btcsuite/btclog v0.0.0-20170628155309-84c8d2346e9f
	github.com/btcsuite/btcwallet/wtxmgr v1.5.0
	github.com/coreos/bbolt v1.3.3
	github.com/davecgh/go-spew v1.1.1
	github.com/fortytw2/leaktest v1.3.0
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.5.0
	github.com/jessevdk/go-flags v1.4.0
	github.com/lightninglabs/aperture v0.1.6-beta
	github.com/lightninglabs/lndclient v0.14.2-3
	github.com/lightninglabs/loop/swapserverrpc v1.0.0
	github.com/lightninglabs/protobuf-hex-display v1.4.3-hex-display
	github.com/lightningnetwork/lnd v0.14.2-beta.rc1
	github.com/lightningnetwork/lnd/cert v1.1.1
	github.com/lightningnetwork/lnd/clock v1.1.0
	github.com/lightningnetwork/lnd/queue v1.1.0
	github.com/lightningnetwork/lnd/ticker v1.1.0
	github.com/lightningnetwork/lnd/tor v1.0.0
	github.com/stretchr/testify v1.7.0
	github.com/urfave/cli v1.22.4
	golang.org/x/net v0.0.0-20211015210444-4f30a5c0130f
	google.golang.org/grpc v1.39.0
	google.golang.org/protobuf v1.27.1
	gopkg.in/macaroon-bakery.v2 v2.0.1
	gopkg.in/macaroon.v2 v2.1.0
)

go 1.15

replace github.com/lightninglabs/loop/swapserverrpc => ./swapserverrpc

replace github.com/lightningnetwork/lnd => github.com/guggero/lnd v0.11.0-beta.rc4.0.20220322204826-af4562a8b1b2

replace github.com/lightninglabs/lndclient => github.com/lightninglabs/lndclient v1.0.1-0.20220316193259-eeb62a615ee6

replace github.com/lightninglabs/aperture => github.com/positiveblue/aperture v0.1.17-beta.0.20220311010926-8edab73a0ccc
