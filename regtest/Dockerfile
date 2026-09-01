FROM --platform=${BUILDPLATFORM} golang:1.26-alpine AS builder

ARG TARGETOS
ARG TARGETARCH

RUN apk add --no-cache alpine-sdk git

WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
    -o /bin/loopserver-regtest ./cmd/loopserver-regtest

FROM --platform=${TARGETPLATFORM} alpine:3.22

RUN apk add --no-cache ca-certificates
COPY --from=builder /bin/loopserver-regtest /bin/loopserver-regtest

EXPOSE 11009
ENTRYPOINT ["/bin/loopserver-regtest"]
