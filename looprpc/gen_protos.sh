#!/bin/sh

set -e

# Generate the gRPC bindings for all proto files.
for file in ./*.proto
do
	protoc -I/usr/local/include -I. \
	       -I$GOPATH/src/github.com/grpc-ecosystem/grpc-gateway/third_party/googleapis \
	       --go_out=plugins=grpc,paths=source_relative:. \
		${file}

done

# Only generate the REST proxy and definitions for the client component.
protoc -I/usr/local/include -I. \
       -I$GOPATH/src/github.com/grpc-ecosystem/grpc-gateway/third_party/googleapis \
       --grpc-gateway_out=logtostderr=true:. \
       client.proto

protoc -I/usr/local/include -I. \
       -I$GOPATH/src/github.com/grpc-ecosystem/grpc-gateway/third_party/googleapis \
       --swagger_out=logtostderr=true:. \
       client.proto
