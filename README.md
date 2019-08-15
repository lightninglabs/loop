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
  * on-chain to off-chain, where teh Loop client sends funds to an on-chain
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

The Loop client is current in an early beta state, and offers a simple command
line application. Future APIs will be added to support implementation or use of
the Loop service.

The Loop daemon exposes a [gRPC API](https://lightning.engineering/loop/#lightning-loop-grpc-api-reference)
(defaults to port 11010) and a [REST API](https://lightning.engineering/loop/rest/index.html)
(defaults to port 8081).

The [GitHub issue tracker](https://github.com/lightninglabs/loop/issues) can be
used to request specific improvements or register and get help with any
problems. Community support is also available in the
[LND Slack](https://join.slack.com/t/lightningcommunity/shared_invite/enQtMzQ0OTQyNjE5NjU1LWRiMGNmOTZiNzU0MTVmYzc1ZGFkZTUyNzUwOGJjMjYwNWRkNWQzZWE3MTkwZjdjZGE5ZGNiNGVkMzI2MDU4ZTE)
.

## Setup and Install

LND and the loop client are using Go modules. Make sure that the `GO111MODULE`
env variable is set to `on`.

In order to execute a swap, **You need to run lnd 0.6.0+, or master built with
sub-servers enabled.**

### LND

If you are building from source, and not using a 0.6.0 or higher release of
lnd, make sure that you are using the `master` branch of lnd. You can get this
by git cloning the repository

```
git clone https://github.com/lightningnetwork/lnd.git
```

Once the lnd repository is cloned, it will need to be built with special build
tags that enable the swap. This enables the required lnd rpc services.

```
cd lnd
make install tags="signrpc walletrpc chainrpc invoicesrpc routerrpc"
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

  Attempts loop out the target amount into either the backing lnd's
  wallet, or a targeted address.

  The amount is to be specified in satoshis.

  Optionally a BASE58/bech32 encoded bitcoin destination address may be
  specified. If not specified, a new wallet address will be generated.

OPTIONS:
   --channel value  the 8-byte compact channel ID of the channel to loop out (default: 0)
   --addr value     the optional address that the looped out funds should be sent to, if let blank the funds will go to lnd's wallet
   --amt value      the amount in satoshis to loop out (default: 0)
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
   --amt value  the amount in satoshis to loop in (default: 0)
   --external   expect htlc to be published externally
```

The `--external` argument allows the on-chain HTLC transacting to be published
_externally_. This allows for a number of use cases like using this address to
withdraw from an exchange _into_ your Lightning channel!

A Loop In swap can be executed a follows: 
```
loop in <amt_in_satoshis>
```
## Resume

When `loopd` is terminated (or killed) for whatever reason, it will pickup
pending swaps after a restart. 

Information about pending swaps is stored persistently in the swap database.
Its location is `~/.loopd/<network>/loop.db`.

## Multiple Simultaneous Swaps

It is possible to execute multiple swaps simultaneously. Just keep loopd 
running.

