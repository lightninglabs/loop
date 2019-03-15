# Lightning Loop
 
Lightning Loop is a non-custodial service offered by
[Lightning Labs](https://lightning.engineering/) to bridge on-chain and
off-chain Bitcoin using submarine swaps. This repository is home to the Loop
client and depends on the Lightning Network daemon
[lnd](https://github.com/lightningnetwork/lnd). All of lnd’s supported chain
backends are fully supported when using the Loop client: Neutrino, Bitcoin
Core, and btcd.

In the current iteration of the Loop software, only off-chain to on-chain
exchanges are supported, where the Loop client sends funds off-chain in
exchange for the funds back on-chain.

The service can be used in various situations:

- Acquiring inbound channel liquidity from arbitrary nodes on the Lightning
    network
- Depositing funds to a Bitcoin on-chain address without closing active
    channels
- Paying to on-chain fallback addresses in the case of insufficient route
    liquidity

Future iterations of the Loop software will also allow on-chain to off-chain
swaps. These swaps can be useful for additional use-cases:

- Refilling depleted channels with funds from cold-wallets or exchange
    withdrawals
- Servicing off-chain Lightning withdrawals using on-chain payments, with no
    funds in channels required
- As a failsafe payment method that can be used when channel liquidity along a
    route is insufficient

## Development and Support

The Loop client is current in an early beta state, and offers a simple command
line application. Future APIs will be added to support implementation or use of
the Loop service.

The [GitHub issue tracker](https://github.com/lightninglabs/loop/issues) can be
used to request specific improvements or register and get help with any
problems. Community support is also available in the
[LND Slack](https://join.slack.com/t/lightningcommunity/shared_invite/enQtMzQ0OTQyNjE5NjU1LWRiMGNmOTZiNzU0MTVmYzc1ZGFkZTUyNzUwOGJjMjYwNWRkNWQzZWE3MTkwZjdjZGE5ZGNiNGVkMzI2MDU4ZTE)
.

## Setup and Install

LND and the loop client are using Go modules. Make sure that the `GO111MODULE`
env variable is set to `on`.

In order to execute a swap, LND will need to be rebuilt with sub servers
enabled.

### LND

* Checkout branch `master`

- `make install tags="signrpc walletrpc chainrpc"` to build and install lnd
  with required sub-servers enabled.

- Make sure there are no macaroons in the lnd dir
  `~/.lnd/data/chain/bitcoin/mainnet`. If there are, lnd has been started
  before and in that case, it could be that `admin.macaroon` doesn't contain
  signer permission. Stop lnd, delete `*.macaroon` files, restart lnd to
  regenerate them with the signer permission.

  DO NOT DELETE `wallet.db` !

- Start lnd

### Loopd
- `git clone https://github.com/lightninglabs/loop.git`
- `cd loop/cmd`
- `go install ./...`

## Execute a swap

* Swaps are executed by a client daemon process. Run:

  `loopd`

  By default `loopd` attempts to connect to an lnd instance running on
  `localhost:10009` and reads the macaroon and tls certificate from `~/.lnd`.
  This can be altered using command line flags. See `loopd --help`.

  `loopd` only listens on localhost and uses an unencrypted and unauthenticated
  connection.

* To initiate a swap, run:

  `loop out <amt_sat>` 
  
  When the swap is initiated successfully, `loopd` will see the process through.

* To query and track the swap status, run `loop` without arguments.    

## Resume
When `loopd` is terminated (or killed) for whatever reason, it will pickup
pending swaps after a restart. 

Information about pending swaps is stored persistently in the swap database.
Its location is `~/.loopd/<network>/loop.db`.

## Multiple simultaneous swaps

It is possible to execute multiple swaps simultaneously.

