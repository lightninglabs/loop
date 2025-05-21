#!/bin/bash

set -e

# Directory of the script file, independent of where it's called from.
DIR="$(cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd)"

# Get versions from looprpc go.mod
PROTOBUF_VERSION=$(go list -f '{{.Version}}' -m google.golang.org/protobuf)
GRPC_GATEWAY_VERSION=$(go list -f '{{.Version}}' -m github.com/grpc-ecosystem/grpc-gateway/v2)

# Get lnd directory from parent go.mod to use the correct version
lnd_dir=$(cd .. && go list -f '{{.Dir}}' -m github.com/lightningnetwork/lnd)
echo "Using lnd directory: ${lnd_dir}"

echo "Building protobuf compiler docker image..."
docker build -t loop-protobuf-builder \
  --build-arg PROTOBUF_VERSION="$PROTOBUF_VERSION" \
  --build-arg GRPC_GATEWAY_VERSION="$GRPC_GATEWAY_VERSION" \
  .

echo "Compiling and formatting *.proto files..."
docker run \
  --rm \
  --user $UID:$UID \
  -e UID=$UID \
  -e LND_DIR=/lnd \
  -v "$DIR/../:/build" \
  -v "${lnd_dir}:/lnd" \
  loop-protobuf-builder