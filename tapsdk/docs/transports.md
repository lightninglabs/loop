# Transports and Auth

`tap-sdk` supports gRPC and REST transports. Both satisfy the same root
`tapsdk.Client` interface and return the same SDK business types.

## gRPC

```go
client, err := tapgrpc.NewClient(&tapgrpc.Config{
    Host:     "localhost:10029",
    Network:  tapsdk.NetworkRegtest,
    TLS:      tapgrpc.TLSFromPath("/path/to/tls.cert"),
    Macaroon: tapsdk.MacaroonFromPath("/path/to/admin.macaroon"),
})
```

Use gRPC for native Go services and when you want tapd's native streaming RPCs.

To reuse an authenticated gRPC connection owned by your application:

```go
client := tapgrpc.NewClientFromConn(conn, 30*time.Second)
wallet := tapsdk.NewWallet(client, tapsdk.NetworkRegtest)
```

The caller configures TLS, per-RPC credentials and message-size limits on
`conn`.
The SDK does not add empty macaroon metadata or load macaroon files in this
mode. `client.Close()` and `wallet.Close()` leave the supplied connection open;
the caller closes it when its other users are finished. Both constructors
retain the SDK's custom-anchor and issuance-aware proof registration methods.

## REST

```go
client, err := taprest.NewClient(&taprest.Config{
    BaseURL:  "https://localhost:8089",
    Network:  tapsdk.NetworkRegtest,
    TLS:      taprest.TLSFromPath("/path/to/tls.cert"),
    Macaroon: tapsdk.MacaroonFromPath("/path/to/admin.macaroon"),
})
```

REST requires `https://` URLs. Event subscriptions use tapd's grpc-gateway
WebSocket bridge internally.

## TLS Sources

Both transports expose the same TLS source pattern:

| Source | Use |
|--------|-----|
| `nil` | Load tapd's default `~/.tapd/tls.cert` |
| `TLSFromPath(path)` | Trust a certificate from disk |
| `TLSFromData(pem)` | Trust PEM data supplied by config |
| `TLSSystemCert()` | Use the host system certificate pool |
| `TLSInsecure()` | Disable verification for local tests only |

Both transport configs support:

- `TLSMinVersion`
- `TLSPinnedCertFingerprint`

`TLSPinnedCertFingerprint` is the SHA-256 digest of the server leaf
certificate DER bytes. Colons are accepted.

```bash
openssl x509 -in ~/.tapd/tls.cert -outform DER | \
  sha256sum | awk '{print $1}'
```

## Macaroon Sources

Most callers can use root helpers:

```go
tapsdk.MacaroonFromPath("/path/to/admin.macaroon")
tapsdk.MacaroonFromDir("/path/to/network/macaroons")
tapsdk.MacaroonFromHex(hexMacaroon)
```

`MacaroonFromPath` and `MacaroonFromHex` reuse one macaroon for every tapd
sub-service. `MacaroonFromDir` loads tapd's per-service macaroon files.

The transport clients load macaroons during client construction, so invalid or
missing credentials fail early.

## Error Mapping

Both transports preserve gRPC status codes where tapd provides them. REST
errors from grpc-gateway are exposed as typed errors, so callers can continue
using `status.FromError`, `codes.Code`, and SDK sentinel errors.

## Production Notes

- Prefer certificate verification or certificate pinning.
- Treat `TLSInsecure()` as a test-only option.
- Store macaroons with the least privilege needed for the service.
- Keep universe sync hosts in trusted configuration.
