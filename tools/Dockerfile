FROM golang:1.24.6

RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates \
 && rm -rf /var/lib/apt/lists/*

ENV CGO_ENABLED=0 \
    GOCACHE=/tmp/build/.cache \
    GOMODCACHE=/tmp/build/.modcache \
    GOFLAGS="-buildvcs=false"

COPY . /tmp/tools

RUN mkdir -p /tmp/build/.cache /tmp/build/.modcache \
 && cd /tmp/tools \
 && go install -trimpath github.com/golangci/golangci-lint/v2/cmd/golangci-lint \
 && chmod -R 777 /tmp/build/

WORKDIR /build
