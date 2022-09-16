#!/bin/bash

set -e

# generate compiles the *.pb.go stubs from the *.proto files.
function generate() {
  # Generate the gRPC bindings for all proto files.
  for file in ./*.proto
  do
    protoc -I/usr/local/include -I. -I.. \
      --go_out . --go_opt paths=source_relative \
      --go-grpc_out . --go-grpc_opt paths=source_relative \
      "${file}"
  done
  
  # Generate the REST reverse proxy for the client only.
  protoc -I/usr/local/include -I. -I.. \
    --grpc-gateway_out . \
    --grpc-gateway_opt logtostderr=true \
    --grpc-gateway_opt paths=source_relative \
    --grpc-gateway_opt grpc_api_configuration=client.yaml \
    client.proto

  # Finally, generate the swagger file which describes the REST API in detail.
  protoc -I/usr/local/include -I. -I.. \
    --openapiv2_out . \
    --openapiv2_opt logtostderr=true \
    --openapiv2_opt grpc_api_configuration=client.yaml \
    --openapiv2_opt json_names_for_fields=false \
    client.proto

  # Generate the JSON/WASM client stubs.
  falafel=$(which falafel)
  pkg="looprpc"
  opts="package_name=$pkg,js_stubs=1"
  protoc -I/usr/local/include -I. -I.. \
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
