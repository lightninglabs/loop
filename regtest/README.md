# Source-built Loop regtest server

This directory runs a complete, disposable Loop environment without the
proprietary `lightninglabs/loopserver` image. The server is built from
`cmd/loopserver-regtest` in this repository and executes the real protocol:

- Loop Out uses real hold invoices, a real P2TR HTLC, and a real client sweep.
- Loop In uses the probe-invoice handshake, a real client-funded P2TR HTLC, a
  real Lightning payment, and a real server claim.
- Static-address Loop In creates a real Taproot deposit address, verifies and
  co-signs the safety transactions, pays the client invoice, and settles the
  selected deposits on chain.

The topology contains Bitcoin Core, a client and server `lnd`, the source-built
Loop server, `loopd`, and Aperture. Aperture is retained because static-address
ownership is tied to the client's paid L402 token and its authenticated
notification stream.

> [!WARNING]
> This binary deliberately has regtest-only policy, in-memory state, cheap
> deterministic fees, and no operational hardening. It refuses non-regtest
> nodes, but it must still never be exposed to an untrusted network or used
> with valuable funds.

## Requirements

- Docker with either `docker compose` or `docker-compose`
- `jq`
- Bash

## Start the environment

From any directory in the checkout:

```shell
./regtest/regtest.sh start
```

`start` always removes any previous regtest containers and volumes so the
in-memory server, the client database, and Aperture's L402 state cannot drift
apart. Do not use this disposable topology for data you need to retain.

The helper builds both Loop binaries, starts the containers, mines spendable
coins, funds both Lightning nodes, and opens one channel in each direction. It
shares Aperture's certificate with `loopd` through a read-only volume, pays the
regtest L402 challenge, and waits until the authenticated client is ready.

Useful commands are:

```shell
./regtest/regtest.sh info
./regtest/regtest.sh logs
./regtest/regtest.sh loop getinfo
./regtest/regtest.sh mine 1
./regtest/regtest.sh stop
```

`stop` removes the Compose volumes. Server swap state is intentionally not
durable, so do not restart the server while a swap is in progress.

## Run all three flows automatically

After `start`, the acceptance script runs a Loop Out, a standard Loop In, and a
static-address deposit Loop In through the real `loopd` gRPC API. It mines the
required confirmations and fails if any state machine reports failure:

```shell
./regtest/e2e.sh
```

Set `TIMEOUT_SECONDS` if the host needs more time:

```shell
TIMEOUT_SECONDS=300 ./regtest/e2e.sh
```

Set `START_TIMEOUT_SECONDS` to change the default three-minute readiness timeout
used by `regtest.sh start`.

## Run a Loop Out manually

Start the swap:

```shell
./regtest/regtest.sh loop out --amt 500000 --fast --force
```

The server waits until both hold invoices are accepted and then funds the exact
P2TR HTLC. Confirm it:

```shell
./regtest/regtest.sh mine 1
```

The client reveals the preimage and publishes its sweep. A Loop Out sweep uses
three confirmations for its terminal success state:

```shell
./regtest/regtest.sh mine 3
./regtest/regtest.sh loop listswaps --loop_out_only
```

## Run a standard Loop In manually

Start the swap. During initiation the server really pays the client's hold
probe, waits for the client to cancel it, and only then returns the contract:

```shell
./regtest/regtest.sh loop in --amt 500000 --force
```

Confirm the client-funded P2TR HTLC. The server then pays the real swap invoice
and publishes its success-path claim:

```shell
./regtest/regtest.sh mine 1
./regtest/regtest.sh mine 1
./regtest/regtest.sh loop listswaps --loop_in_only
```

## Run a static-address deposit Loop In manually

Request the authenticated static address:

```shell
./regtest/regtest.sh loop static new
```

Send one or more deposits to the returned address and confirm them. For example:

```shell
ADDRESS=bcrt1p...
./regtest/regtest.sh lndclient sendcoins \
  --addr "$ADDRESS" --amt 500000 --min_confs 0 --force
./regtest/regtest.sh mine 6
./regtest/regtest.sh loop static listdeposits --filter deposited
```

Loop in every available deposit:

```shell
./regtest/regtest.sh loop static in --all --fast --force
./regtest/regtest.sh loop static listswaps
```

The client only accepts the off-chain payment after it has three independently
fee-bumped, fully signed fallback transactions. The server then settles the
deposit and broadcasts the resulting transaction. Mine any transaction left in
the mempool:

```shell
./regtest/regtest.sh bitcoin getrawmempool
./regtest/regtest.sh mine 1
```

## Run the server against an existing regtest

Standard Loop In and Loop Out can use the binary directly:

```shell
go build -o loopserver-regtest ./cmd/loopserver-regtest

./loopserver-regtest \
  --listen=127.0.0.1:11009 \
  --lnd.host=127.0.0.1:10009 \
  --lnd.macaroonpath=/path/to/lnd/admin.macaroon \
  --lnd.tlspath=/path/to/lnd/tls.cert \
  --bitcoin.host=127.0.0.1:18443 \
  --bitcoin.user=lightning \
  --bitcoin.password=lightning
```

The listener is plaintext by default. Supply both `--tls.certpath` and
`--tls.keypath` to serve TLS, as the Compose topology does for Aperture's gRPC
backend.

Point a regtest `loopd` at it with `--server.notls`:

```shell
loopd \
  --experimental \
  --network=regtest \
  --server.host=127.0.0.1:11009 \
  --server.notls \
  --lnd.host=127.0.0.1:10010 \
  --lnd.macaroonpath=/path/to/client-lnd/admin.macaroon \
  --lnd.tlspath=/path/to/client-lnd/tls.cert
```

For static-address swaps, use the Compose topology or put the server behind an
L402-compatible Aperture instance. A direct connection has no paid token with
which the notification manager can authenticate static-address ownership.

## Scope and limitations

The server is intentionally a protocol test fixture, not a miniature production
service. It has no database, multi-tenant authorization, batching, dynamic fee
market, liquidity management, accounting, monitoring, or upgrade guarantees.
It validates contract keys, hashes, amounts, outpoints, scripts, invoices, and
signing requests needed to keep the real regtest funds safe for the lifetime of
the process.
