module github.com/lightninglabs/loop

require (
	github.com/btcsuite/btcd v0.0.0-20190115013929-ed77733ec07d
	github.com/btcsuite/btclog v0.0.0-20170628155309-84c8d2346e9f
	github.com/btcsuite/btcutil v0.0.0-20190112041146-bf1e1be93589
	github.com/coreos/bbolt v0.0.0-20180223184059-7ee3ded59d4835e10f3e7d0f7603c42aa5e83820
	github.com/fortytw2/leaktest v1.3.0
	github.com/golang/protobuf v1.2.0
	github.com/jessevdk/go-flags v0.0.0-20170926144705-f88afde2fa19
	github.com/lightningnetwork/lnd v0.0.0
	github.com/urfave/cli v1.20.0
	golang.org/x/net v0.0.0-20181201002055-351d144fa1fc
	google.golang.org/genproto v0.0.0-20181127195345-31ac5d88444a
	google.golang.org/grpc v1.16.0
	gopkg.in/macaroon.v2 v2.0.0
)

replace github.com/lightningnetwork/lnd => github.com/joostjager/lnd v0.4.1-beta.0.20190204111535-7159d0a5ef0a
