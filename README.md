# Lightning Loop
 
Lightning Loop is a non-custodial service offered by
[Lightning Labs](https://lightning.engineering/) to bridge on-chain and
off-chain Bitcoin using submarine swaps. This repository is home to the Loop
client and depends on the Lightning Network daemon
[lnd](https://github.com/lightningnetwork/lnd). All of lnd’s supported chain
backends are fully supported when using the Loop client: Neutrino, Bitcoin
Core, and btcd.

In the current iteration of the Loop software, only off-chain to on-chain
swaps are supported, where the Loop client sends funds off-chain in
exchange for the funds back on-chain. This is called a **Loop Out**.

The service can be used in various situations:

- Acquiring inbound channel liquidity from arbitrary nodes on the Lightning
    network
- Depositing funds to a Bitcoin on-chain address without closing active
    channels
- Paying to on-chain fallback addresses in the case of insufficient route
    liquidity

Loop also allow offers an experimental testnet version of on-chain to off-chain
swaps, called **Loop In**. This allows you to use on-chain funds to increase
the local balance of a channel.

Potential uses for Loop In:

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

Now that loopd is running, you can initiate a simple Loop Out. This will pay
out Lightning off-chain funds and you will receive Bitcoin on-chain funds in
return. There will be some chain and routing fees associated with this swap.

```
loop out <amt_in_satoshis>
```

This will take some time, as it requires an on-chain confirmation. When the
swap is initiated successfully, `loopd` will see the process through.

To query in-flight swap statuses, run `loop monitor`.

## Resume

When `loopd` is terminated (or killed) for whatever reason, it will pickup
pending swaps after a restart. 

Information about pending swaps is stored persistently in the swap database.
Its location is `~/.loopd/<network>/loop.db`.

## Multiple Simultaneous Swaps

It is possible to execute multiple swaps simultaneously. Just keep loopd 
running.

