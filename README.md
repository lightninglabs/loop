# Lightning Loop
 
Lightning Loop is a non-custodial service offered by
[Lightning Labs](https://lightning.engineering/) to bridge on-chain and
off-chain Bitcoin using submarine swaps. This repository is home to the Loop
client and depends on the Lightning Network daemon
[lnd](https://github.com/lightningnetwork/lnd). All of lnd’s supported chain
backends are fully supported when using the Loop client: Neutrino, Bitcoin
Core, and btcd.

In the current iteration of the Loop software, two swap types are supported:
  * off-chain to on-chain, where the Loop client sends funds off-chain in
  * on-chain to off-chain, where the Loop client sends funds to an on-chain
    address using an off-chain channel

We call off-chain to on-chain swaps, a **Loop Out**.  The service can be used
in various situations:

- Acquiring inbound channel liquidity from arbitrary nodes on the Lightning
    network
- Depositing funds to a Bitcoin on-chain address without closing active
    channels
- Paying to on-chain fallback addresses in the case of insufficient route
    liquidity

We call our on-chain to off-chain swaps, a **Loop In**.  This allows you to use
on-chain funds to increase the local balance of a channel, effectively
"refilling" an existing channel.

Potential uses for **Loop In**:

- Refilling depleted channels with funds from cold-wallets or exchange
    withdrawals
- Servicing off-chain Lightning withdrawals using on-chain payments, with no
    funds in channels required
- As a failsafe payment method that can be used when channel liquidity along a
    route is insufficient

## Development and Support

The Loop client is currently in an early beta state, and offers a simple command
line application. Future APIs will be added to support implementation or use of
the Loop service.

The Loop daemon exposes a [gRPC API](https://lightning.engineering/loopapi/#lightning-loop-grpc-api-reference)
(defaults to port 11010) and a [REST API](https://lightning.engineering/loopapi/index.html#loop-rest-api-reference)
(defaults to port 8081).

The [GitHub issue tracker](https://github.com/lightninglabs/loop/issues) can be
used to request specific improvements or register and get help with any
problems. Community support is also available in the
[LND Slack](https://lightning.engineering/slack.html)
.

## Setup and Install

LND and the loop client are using Go modules. Make sure that the `GO111MODULE`
env variable is set to `on`.

In order to execute a swap, **you need to run a compatible lnd version built
with the correct sub-servers enabled.**

### LND

To run loop, you need a compatible version of `lnd` running. It is generally
recommended to always keep both `lnd` and `loop` updated to the most recent
released version. If you need to run an older version of `lnd`, please consult
the following table for supported versions.

Loop Version                  | Compatible LND Version(s)
------------------------------|------------------
`>= v0.6.3-beta`              | >= `v0.10.1-beta`
`v0.6.0-beta` - `v0.6.2-beta` | >= `v0.10.0-beta`
`<= 0.5.1-beta`               | >= `v0.7.1-beta`

If you are building from source make sure you are using the latest tagged
version of lnd. You can get this by git cloning the repository and checking out
a specific tag:

```
git clone https://github.com/lightningnetwork/lnd.git
cd lnd
git checkout v0.10.0-beta
```

Once the lnd repository is cloned, it will need to be built with special build
tags that enable the swap. This enables the required lnd rpc services.

```
make install tags="signrpc walletrpc chainrpc invoicesrpc"
```

Check to see if you have already installed lnd. If you have, you will need to
delete the `.macaroon` files from your lnd directory and restart lnd.

**Do not delete any other files other than the `.macaroon` files**

```
// Example on Linux to see macaroons in the default directory:
ls ~/.lnd/data/chain/bitcoin/mainnet
```

This should show no `.macaroon` files. If it does? Stop lnd, delete macaroons,
restart lnd.

```
lncli stop
```

Now delete the .macaroon files and restart lnd. (don't delete any other files)

### Loopd

After lnd is installed, you will need to clone the Lightning Loop repo and 
install the command line interface and swap client service.

```
git clone https://github.com/lightninglabs/loop.git
cd loop/cmd
go install ./...
```

## Execute a Swap

After you have lnd and the Loop client installed, you can execute a Loop swap.

The Loop client needs its own short-lived daemon which will deal with the swaps
in progress.

Command to start `loopd`::

```
loopd

// Or if you want to do everything in the same terminal and background loopd
loopd &

// For testnet mode, you'll need to specify the network as mainnet is the
default:
loopd --network=testnet
```

By default `loopd` attempts to connect to the lnd instance running on
`localhost:10009` and reads the macaroon and tls certificate from `~/.lnd`.
This can be altered using command line flags. See `loopd --help`.

`loopd` only listens on localhost and uses an unencrypted and unauthenticated
connection.

### Loop Out Swaps

Now that loopd is running, you can initiate a simple Loop Out. This will pay
out Lightning off-chain funds and you will receive Bitcoin on-chain funds in
return. There will be some chain and routing fees associated with this swap.
```
NAME:
   loop out - perform an off-chain to on-chain swap (looping out)

USAGE:
   loop out [command options] amt [addr]

DESCRIPTION:

  Attempts to loop out the target amount into either the backing lnd's
  wallet, or a targeted address.

  The amount is to be specified in satoshis.

  Optionally a BASE58/bech32 encoded bitcoin destination address may be
  specified. If not specified, a new wallet address will be generated.

OPTIONS:
   --channel value               the 8-byte compact channel ID of the channel to loop out (default: 0)
   --addr value                  the optional address that the looped out funds should be sent to, if let blank the funds will go to lnd's wallet
   --amt value                   the amount in satoshis to loop out (default: 0)
   --conf_target value           the number of blocks from the swap initiation height that the on-chain HTLC should be swept within (default: 6)
   --max_swap_routing_fee value  the max off-chain swap routing fee in satoshis, if not specified, a default max fee will be used (default: 0)
   --fast                        Indicate you want to swap immediately, paying potentially a higher fee. If not set the swap server might choose to wait up to 30 minutes before publishing the swap HTLC on-chain, to save on its chain fees. Not setting this flag therefore might result in a lower swap fee.
```

It's possible to receive more inbound capacity on a particular channel
(`--channel`), and also have the `loop` daemon send the coins to a target
address (`addr`). The latter option allows ones to effectively send on-chain
from their existing channels!


```
loop out <amt_in_satoshis>
```

This will take some time, as it requires an on-chain confirmation. When the
swap is initiated successfully, `loopd` will see the process through.

To query in-flight swap statuses, run `loop monitor`.

### Fees explained

The following is an example output of a 0.01 BTC fast (non-batched) Loop Out
swap from `testnet`:

```bash
$ loop out --amt 1000000 --fast
Max swap fees for 1000000 sat Loop Out: 36046 sat
Fast swap requested.
CONTINUE SWAP? (y/n), expand fee detail (x): x

Estimated on-chain sweep fee:        149 sat
Max on-chain sweep fee:              14900 sat
Max off-chain swap routing fee:      20010 sat
Max off-chain prepay routing fee:    36 sat
Max no show penalty (prepay):        1337 sat
Max swap fee:                        1100 sat
CONTINUE SWAP? (y/n):
```

Explanation:

- **Max swap fees for <x> sat Loop Out** (36046 sat): The absolute maximum in
  fees that need to be paid. This includes on-chain and off-chain fees. This
  represents the ceiling or worst-case scenario. The actual fees will likely be
  lower. This is the sum of `14900 + 20010 + 36 + 1100` (see below).
- **Estimated on-chain sweep fee** (149 sat): The estimated cost to sweep the
  HTLC in case of success, calculated based on the _current_ on-chain fees.
  This value is called `miner_fee` in the gRPC/REST responses.
- **Max on-chain sweep fee** (14900 sat): The maximum on-chain fee the daemon
  is going to allow for sweeping the HTLC in case of success. A fee estimation
  based on the `--conf_target` flag is always performed before sweeping. The
  factor of `100` times the estimated fee is applied in case the fees spike
  between the time the swap is initiated and the time the HTLC can be swept. But
  that is the absolute worst-case fee that will be paid. If there is no fee
  spike, a normal, much lower fee will be used.
- **Max off-chain swap routing fee** (20010 sat): The maximum off-chain
  routing fee that the daemon should pay when finding a route to pay the
  Lightning invoice. This is a hard limit. If no route with a lower or equal fee
  is found, the payment (and the swap) is aborted. This value is calculated
  statically based on the swap amount (see `maxRoutingFeeBase` and
  `maxRoutingFeeRate` in `cmd/loop/main.go`).
- **Max off-chain prepay routing fee** (36 sat): The maximum off-chain routing
  fee that the daemon should pay when finding a route to pay the prepay fee.
  This is a hard limit. If no route with a lower or equal fee is found, the
  payment (and the swap) is aborted. This value is calculated statically based
  on the prepay amount (see `maxRoutingFeeBase` and `maxRoutingFeeRate` in
  `cmd/loop/main.go`).
- **Max no show penalty (prepay)** (1337 sat): This is the amount that has to be
  pre-paid (off-chain) before the server publishes the HTLC on-chain. This is
  necessary to ensure the server's on-chain fees are paid if the client aborts
  and never completes the swap _after_ the HTLC has been published on-chain.
  If the swap completes normally, this amount is counted towards the full swap
  amount and therefore is actually a pre-payment and not a fee. This value is
  called `prepay_amt` in the gRPC/REST responses.
- **Max swap fee** (1100 sat): The maximum amount of service fees we allow the
  server to charge for executing the swap. The client aborts the swap if the
  fee proposed by the server exceeds this maximum. It is therefore recommended
  to obtain the maximum by asking the server for a quote first. The actual fees
  might be lower than this maximum if user specific discounts are applied. This
  value is called `swap_fee` in the gRPC/REST responses.

#### Fast vs. batched swaps

By default, Loop Outs are executed as normal speed swaps. This means the server
will wait up to 30 minutes until it publishes the HTLC on-chain to improve the
chances that it can be batched together with other user's swaps to reduce the
on-chain footprint and fees. The server offers a reduced swap fee for slow swaps
to incentivize users to batch more.

If a swap should be executed immediately, the `--fast` flag can be used. Fast
swaps won't benefit from a reduced swap fee.

### Loop In Swaps

Additionally, Loop In is now also supported for mainnet as well. A Loop In swap
lets one refill their channel (ability to send more coins) by sending to a
special script on-chain.
```
NAME:
   loop in - perform an on-chain to off-chain swap (loop in)

USAGE:
   loop in [command options] amt

DESCRIPTION:

    Send the amount in satoshis specified by the amt argument off-chain.

OPTIONS:
   --amt value          the amount in satoshis to loop in (default: 0)
   --external           expect htlc to be published externally
   --conf_target value  the target number of blocks the on-chain htlc broadcast by the swap client should confirm within (default: 0)
   --last_hop value     the pubkey of the last hop to use for this swap
```

The `--external` argument allows the on-chain HTLC transacting to be published
_externally_. This allows for a number of use cases like using this address to
withdraw from an exchange _into_ your Lightning channel!

A Loop In swap can be executed a follows: 
```
loop in <amt_in_satoshis>
```

#### Fees explained

The following is an example output of a 0.01 BTC Loop In swap from `testnet`:

```bash
$ loop in --amt 1000000
Max swap fees for 1000000 sat Loop In: 1562 sat
CONTINUE SWAP? (y/n), expand fee detail (x): x

Estimated on-chain HTLC fee:         154 sat
Max swap fee:                        1100 sat
CONTINUE SWAP? (y/n):
```

Explanation:

- **Estimated on-chain HTLC fee** (154 sat): The estimated on-chain fee that the
  daemon has to pay to publish the HTLC. This is an estimation from `lnd`'s
  wallet based on the available UTXOs and current network fees. This value is
  called `miner_fee` in the gRPC/REST responses.
- **Max swap fee** (1100 sat): The maximum amount of service fees we allow the
  server to charge for executing the swap. The client aborts the swap if the
  fee proposed by the server exceeds this maximum. It is therefore recommended
  to obtain the maximum by asking the server for a quote first. The actual fees
  might be lower than this maximum if user specific discounts are applied. This
  value is called `swap_fee` in the gRPC/REST responses.

## Resume

When `loopd` is terminated (or killed) for whatever reason, it will pickup
pending swaps after a restart. 

Information about pending swaps is stored persistently in the swap database.
Its location is `~/.loopd/<network>/loop.db`.

## Authentication and transport security

The gRPC and REST connections of `loopd` are encrypted with TLS and secured with
macaroon authentication the same way `lnd` is.

If no custom loop directory is set then the TLS certificate is stored in
`~/.loop/<network>/tls.cert` and the base macaroon in
`~/.loop/<network>/loop.macaroon`.

The `loop` command will pick up these file automatically on mainnet if no custom
loop directory is used. For other networks it should be sufficient to add the
`--network` flag to tell the CLI in what sub directory to look for the files.

For more information on macaroons,
[see the macaroon documentation of lnd.](https://github.com/lightningnetwork/lnd/blob/master/docs/macaroons.md)

**NOTE**: Loop's macaroons are independent from `lnd`'s. The same macaroon
cannot be used for both `loopd` and `lnd`.

## Multiple Simultaneous Swaps

It is possible to execute multiple swaps simultaneously. Just keep loopd 
running.

## API Support Level

### Server

The Loop server API and on-chain scripts are kept backwards compatible as long
as reasonably possible.

### Client

When breaking changes to the Loop client daemon API are made, old fields will be
marked as deprecated. Deprecated fields will remain supported until the next
minor release. 
