FROM --platform=${BUILDPLATFORM} golang:1.26-alpine as builder

# Copy in the local repository to build from.
COPY . /go/src/github.com/lightningnetwork/loop

# Force Go to use the cgo based DNS resolver. This is required to ensure DNS
# queries required to connect to linked containers succeed.
ENV GODEBUG netdns=cgo

# Explicitly turn on the use of modules (until this becomes the default).
ENV GO111MODULE on

# The platform the resulting image is built for. buildx sets these
# automatically, once per requested platform. The builder stage above stays
# pinned to ${BUILDPLATFORM} so the Go toolchain always runs natively; we
# cross-compile to the target platform rather than emulating the toolchain.
ARG TARGETOS
ARG TARGETARCH

# Install dependencies and install/build lnd.
#
# When cross-compiling, "go install" writes to $GOPATH/bin/$GOOS_$GOARCH
# instead of $GOPATH/bin, so the binaries are moved back to keep the COPY
# below platform independent. GOBIN cannot be used for this: setting it while
# cross-compiling is a hard error in the Go tool.
RUN apk add --no-cache --update \
    git \
    make \
    &&  cd /go/src/github.com/lightningnetwork/loop \
    &&  GOOS=${TARGETOS} GOARCH=${TARGETARCH} make install \
    &&  if [ -d /go/bin/${TARGETOS}_${TARGETARCH} ]; then \
            mv /go/bin/${TARGETOS}_${TARGETARCH}/* /go/bin/; \
        fi

# Start a new, final image to reduce size.
#
# This stage must resolve to the target platform, which is the default for
# every stage. Do not pin it to ${BUILDPLATFORM}: that builds every requested
# platform on the build machine's architecture, producing an image whose
# manifest advertises linux/arm64 while the filesystem is amd64.
FROM alpine as final

# Expose lnd ports (server, rpc).
EXPOSE 8081 11010

# Copy the binaries and entrypoint from the builder image.
COPY --from=builder /go/bin/loopd /bin/
COPY --from=builder /go/bin/loop /bin/

# Add bash.
RUN apk add --no-cache \
    bash \
    ca-certificates
