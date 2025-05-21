#!/bin/bash

# The script uses the following env var:
# LND_DIR - the place where it can find LND of the pinned version, to use its
# lnrpc/ subdir for .proto imports.

set -e

# generate compiles the *.pb.go stubs from the *.proto files.
function generate() {
  proto_paths=(-I/usr/local/include -I. -I.. -I"$LND_DIR")

  # Generate the gRPC bindings for all proto files.
  for file in ./*.proto
  do
    protoc "${proto_paths[@]}" \
      --go_out . --go_opt paths=source_relative \
      --go-grpc_out . --go-grpc_opt paths=source_relative \
      "${file}"
  done

  # Generate the REST reverse proxy for the client only.
  protoc "${proto_paths[@]}" \
    --grpc-gateway_out . \
    --grpc-gateway_opt logtostderr=true \
    --grpc-gateway_opt paths=source_relative \
    --grpc-gateway_opt grpc_api_configuration=client.yaml \
    client.proto

  # Finally, generate the swagger file which describes the REST API in detail.
  protoc "${proto_paths[@]}" \
    --openapiv2_out . \
    --openapiv2_opt logtostderr=true \
    --openapiv2_opt grpc_api_configuration=client.yaml \
    --openapiv2_opt json_names_for_fields=false \
    client.proto

  # Generate the JSON/WASM client stubs.
  falafel=$(which falafel)
  pkg="looprpc"
  opts="package_name=$pkg,js_stubs=1"
  protoc "${proto_paths[@]}" \
    --plugin=protoc-gen-custom=$falafel\
    --custom_out=. \
    --custom_opt="$opts" \
    client.proto
}

# format formats the *.proto files with the clang-format utility.
function format() {
  find . -name "*.proto" -print0 | xargs -0 clang-format --style=file -i
}

# Compile and format the looprpc package.
pushd looprpc
format
generate
popd
