#!/bin/bash

set -e

# generate compiles the *.pb.go stubs from the *.proto files.
function generate() {
  # Generate the gRPC bindings for all proto files.
  for file in ./*.proto
  do
    protoc -I/usr/local/include -I. \
           --go_out=plugins=grpc,paths=source_relative:. \
      "${file}"
  done
  
  # Generate the REST reverse proxy for the client only.
  protoc -I/usr/local/include -I. \
    --grpc-gateway_out=logtostderr=true,paths=source_relative,grpc_api_configuration=rest-annotations.yaml:. \
    "client.proto"


  # Finally, generate the swagger file which describes the REST API in detail.
  protoc -I/usr/local/include -I. \
    --swagger_out=logtostderr=true,grpc_api_configuration=rest-annotations.yaml:. \
    "client.proto"
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
