module github.com/lightninglabs/loop

require (
	github.com/btcsuite/btcd v0.21.0-beta.0.20210429225535-ce697fe7e82b
	github.com/btcsuite/btclog v0.0.0-20170628155309-84c8d2346e9f
	github.com/btcsuite/btcutil v1.0.3-0.20201208143702-a53e38424cce
	github.com/btcsuite/btcwallet/wtxmgr v1.3.0
	github.com/coreos/bbolt v1.3.3
	github.com/fortytw2/leaktest v1.3.0
	github.com/golang/protobuf v1.4.3
	github.com/grpc-ecosystem/grpc-gateway v1.14.3
	github.com/jessevdk/go-flags v1.4.0
	github.com/lightninglabs/aperture v0.1.6-beta
	github.com/lightninglabs/lndclient v0.11.1-9
	github.com/lightninglabs/protobuf-hex-display v1.4.3-hex-display
	github.com/lightningnetwork/lnd v0.13.0-beta.rc2
	github.com/lightningnetwork/lnd/cert v1.0.3
	github.com/lightningnetwork/lnd/clock v1.0.1
	github.com/lightningnetwork/lnd/queue v1.0.4
	github.com/lightningnetwork/lnd/ticker v1.0.0
	github.com/stretchr/testify v1.7.0
	github.com/urfave/cli v1.20.0
	golang.org/x/net v0.0.0-20200520004742-59133d7f0dd7
	google.golang.org/genproto v0.0.0-20190927181202-20e1ac93f88c
	google.golang.org/grpc v1.29.1
	gopkg.in/macaroon-bakery.v2 v2.0.1
	gopkg.in/macaroon.v2 v2.1.0
)

// Fix incompatibility of etcd go.mod package.
// See https://github.com/etcd-io/etcd/issues/11154
replace go.etcd.io/etcd => go.etcd.io/etcd v0.5.0-alpha.5.0.20201125193152-8a03d2e9614b

go 1.15
