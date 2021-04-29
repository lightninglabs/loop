module github.com/lightninglabs/loop

require (
	github.com/btcsuite/btcd v0.21.0-beta.0.20201208033208-6bd4c64a54fa
	github.com/btcsuite/btclog v0.0.0-20170628155309-84c8d2346e9f
	github.com/btcsuite/btcutil v1.0.2
	github.com/btcsuite/btcwallet/wtxmgr v1.2.0
	github.com/coreos/bbolt v1.3.3
	github.com/fortytw2/leaktest v1.3.0
	github.com/golang/protobuf v1.3.2
	github.com/grpc-ecosystem/grpc-gateway v1.14.3
	github.com/jessevdk/go-flags v1.4.0
	github.com/lightninglabs/aperture v0.1.6-beta
	github.com/lightninglabs/lndclient v0.11.0-5
	github.com/lightninglabs/protobuf-hex-display v1.3.3-0.20191212020323-b444784ce75d
	github.com/lightningnetwork/lnd v0.12.0-beta.rc3
	github.com/lightningnetwork/lnd/cert v1.0.3
	github.com/lightningnetwork/lnd/clock v1.0.1
	github.com/lightningnetwork/lnd/queue v1.0.4
	github.com/lightningnetwork/lnd/ticker v1.0.0
	github.com/stretchr/testify v1.5.1
	github.com/urfave/cli v1.20.0
	golang.org/x/net v0.0.0-20191112182307-2180aed22343
	google.golang.org/genproto v0.0.0-20190927181202-20e1ac93f88c
	google.golang.org/grpc v1.25.1
	gopkg.in/macaroon-bakery.v2 v2.0.1
	gopkg.in/macaroon.v2 v2.1.0
)

go 1.15
