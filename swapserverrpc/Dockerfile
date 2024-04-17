FROM golang:1.21.9-bookworm

RUN apt-get update && apt-get install -y \
  git \
  protobuf-compiler='3.21.12*' \
  clang-format='1:14.0*'

# We don't want any default values for these variables to make sure they're
# explicitly provided by parsing the go.mod file. Otherwise we might forget to
# update them here if we bump the versions.
ARG PROTOBUF_VERSION

ENV PROTOC_GEN_GO_GRPC_VERSION="v1.1.0"
ENV GOCACHE=/tmp/build/.cache
ENV GOMODCACHE=/tmp/build/.modcache

RUN cd /tmp \
  && mkdir -p /tmp/build/.cache \
  && mkdir -p /tmp/build/.modcache \
  && go install google.golang.org/protobuf/cmd/protoc-gen-go@${PROTOBUF_VERSION} \
  && go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@${PROTOC_GEN_GO_GRPC_VERSION} \
  && chmod -R 777 /tmp/build/

WORKDIR /build

CMD ["/bin/bash", "/build/swapserverrpc/gen_protos.sh"]
