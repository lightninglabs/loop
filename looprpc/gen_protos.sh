#!/bin/sh

set -e

# Generate the gRPC bindings for all proto files.
for file in ./*.proto
do
	protoc -I/usr/local/include -I. \
	       --go_out=plugins=grpc,paths=source_relative:. \
		${file}

done

# Only generate the REST proxy and definitions for the client component.
protoc -I/usr/local/include -I. \
       --grpc-gateway_out=logtostderr=true,paths=source_relative,grpc_api_configuration=rest-annotations.yaml:. \
       client.proto

protoc -I/usr/local/include -I. \
       -I$GOPATH/src/github.com/grpc-ecosystem/grpc-gateway/third_party/googleapis \
       --swagger_out=logtostderr=true,grpc_api_configuration=rest-annotations.yaml:. \
       client.proto
