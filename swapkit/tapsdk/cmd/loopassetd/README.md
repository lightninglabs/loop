# Asset transaction runtime

`loopassetd` hosts Loop's SDK transaction/proof driver behind the authenticated
local Unix bridge. The normal Loop client and server retain swap policy,
Lightning payments, ownership, and their durable state machines. Run a separate
runtime for each wallet and use the same private journal directory on restart.

Build from the `loop/swapkit/tapsdk` module:

```sh
GOWORK=off go build -o loopassetd ./cmd/loopassetd
./loopassetd -config /absolute/path/runtime.json
```

The configuration is strict JSON. All file paths must be absolute and clean.
There are no implicit network, fee, confirmation, authentication, or wallet
defaults. Example path placeholders below must be replaced before use; choose
the fee and confirmation policy for the deployment.

```json
{
  "network": "regtest",
  "state_dir": "/srv/loop-assets/client",
  "socket": "/srv/loop-assets/client/runtime.sock",
  "token": "/srv/loop-assets/client/bridge.token",
  "lnd": {
    "address": "127.0.0.1:10009",
    "tls_cert": "/srv/credentials/lnd.cert",
    "macaroon": "/srv/credentials/lnd.macaroon"
  },
  "tapd": {
    "address": "127.0.0.1:10029",
    "tls_cert": "/srv/credentials/tapd.cert",
    "macaroon": "/srv/credentials/tapd.macaroon"
  },
  "bitcoin": {
    "url": "http://127.0.0.1:18443/",
    "tls_cert": "",
    "credentials": "/srv/credentials/bitcoin.json"
  },
  "fee_sat": 1000,
  "max_fee_sat": 2000,
  "min_confs": 3,
  "rpc_timeout_seconds": 30
}
```

The state directory must have mode 0700. The token is exactly 32 raw random
bytes, shared only with the corresponding daemon's bridge client. Token,
macaroons, and Bitcoin credentials must be regular files with mode 0400 or
0600; symbolic links are rejected. Bitcoin credentials use a separate JSON
file with `username` and `password` fields. Do not put secrets in command-line
arguments or URLs. Obtain these files through the deployment's credential
provisioning process. The executable never prints their contents.

LND and tapd always use TLS with the configured certificate. Bitcoin RPC may
use HTTP only on an explicit loopback IP; remote endpoints require HTTPS.
`bitcoin.tls_cert` supplies a custom trust certificate for HTTPS, or can be
empty to use system trust. Redirects are disabled. Startup verifies Bitcoin's
genesis block and the runtime checks tapd/LND wallet identity, network, chain
sync, and height before accepting bridge calls. The first successful check
binds the journal to that wallet identity and network; reopening it with a
different wallet or network fails. Temporary wallet RPC unavailability, sync
lag and differing tips are retried within `rpc_timeout_seconds`; identity or
network mismatches fail immediately. No bridge requests are served until the
checks succeed.

The runtime stores exact transaction/proof intents in `runtime.db` and wallet
signing bindings in `wallet-signing.db`. Preserve both journals together with
the caller's durable swap journal. See
[wallet signing](../../walletsigner/README.md) for nonce backup and replay rules.
That document also distinguishes signer API compatibility with Loop's existing
LND minimum from validation of the SDK module's pinned LND/tapd asset stack.
The Unix listener takes an exclusive process lease and recovers only stale
owned sockets; a second live process cannot take over the same socket.

SIGINT and SIGTERM cancel active RPC actions, stop accepting requests, join
handlers, and close journals. After a forced termination, restart with the same
configuration and journals. The caller's recovery worker resumes incomplete
operations; the runtime's operation IDs and nonce bindings make these retries
idempotent. This command never mines blocks or substitutes fixture keys.

Historical transaction and outpoint-spend scans persist each processed block
in `runtime.db` and yield after at most 128 new blocks. A timed-out action or
process restart resumes the saved progress. The checkpoint hash is rechecked
against the best chain; a reorg rewinds the search. Found transactions remain
subject to the usual confirmation, contract, and complete proof checks.
