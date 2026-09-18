module github.com/lightninglabs/loop/swapserverrpc

require (
	google.golang.org/grpc v1.83.2
	google.golang.org/protobuf v1.36.11
)

require (
	golang.org/x/exp v0.0.0-20240909161429-701f63a606c0 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto v0.0.0-20230410155749-daa745c078e1 // indirect
)

// Avoid fetching gonum vanity domains. The domain is unstable and causes
// "go mod check" failures in CI.
replace gonum.org/v1/gonum => github.com/gonum/gonum v0.11.0

go 1.25.12
