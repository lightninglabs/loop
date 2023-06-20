FROM --platform=${BUILDPLATFORM} golang:1.20.4-alpine as builder

# Copy in the local repository to build from.
COPY . /go/src/github.com/lightningnetwork/loop

# Force Go to use the cgo based DNS resolver. This is required to ensure DNS
# queries required to connect to linked containers succeed.
ENV GODEBUG netdns=cgo

# Explicitly turn on the use of modules (until this becomes the default).
ENV GO111MODULE on

# Install dependencies and install/build lnd.
RUN apk add --no-cache --update alpine-sdk \
    git \
    make \
&&  cd /go/src/github.com/lightningnetwork/loop \
&&  make install

# Start a new, final image to reduce size.
FROM --platform=${BUILDPLATFORM} alpine as final

# Expose lnd ports (server, rpc).
EXPOSE 8081 11010

# Copy the binaries and entrypoint from the builder image.
COPY --from=builder /go/bin/loopd /bin/
COPY --from=builder /go/bin/loop /bin/

# Add bash.
RUN apk add --no-cache \
    bash \
    ca-certificates
