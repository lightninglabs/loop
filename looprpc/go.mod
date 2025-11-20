module github.com/lightninglabs/loop/looprpc

go 1.24.9

require (
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.11.3
	github.com/lightninglabs/loop/swapserverrpc v1.0.13
	google.golang.org/grpc v1.64.1
	google.golang.org/protobuf v1.34.2
	gopkg.in/macaroon-bakery.v2 v2.3.0
)

require (
	github.com/go-macaroon-bakery/macaroonpb v1.0.0 // indirect
	github.com/golang/protobuf v1.5.4 // indirect
	github.com/rogpeppe/fastuuid v1.2.0 // indirect
	golang.org/x/crypto v0.45.0 // indirect
	golang.org/x/net v0.47.0 // indirect
	golang.org/x/sys v0.38.0 // indirect
	golang.org/x/text v0.31.0 // indirect
	google.golang.org/genproto v0.0.0-20230822172742-b8732ec3820d // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20240730163845-b1a4ccb954bf // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20240730163845-b1a4ccb954bf // indirect
	gopkg.in/errgo.v1 v1.0.1 // indirect
	gopkg.in/macaroon.v2 v2.1.0 // indirect
)

// Avoid fetching gonum vanity domains. The domain is unstable and causes
// "go mod check" failures in CI.
replace gonum.org/v1/gonum => github.com/gonum/gonum v0.11.0

replace gonum.org/v1/plot => github.com/gonum/plot v0.10.1
