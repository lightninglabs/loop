# Lightning Loop
Lightning Loop is a non-custodial service offered by
[Lightning Labs](https://lightning.engineering/) that makes it easy to move
bitcoin into and out of the Lightning Network.

## Features
- Automated channel balancing
- Privacy-forward non-custodial swaps
- Opportunistic transaction batching to save on fees
- Progress monitoring of in-flight swaps

## Use Cases
- Automate channel balancing with AutoLoop ([Learn more](https://github.com/lightninglabs/loop/blob/master/docs/autoloop.md))
- Deposit to a Bitcoin address without closing channels with Loop In
- Convert outbound liquidity into inbound liquidity with Loop Out
- Refill depleted Lightning channels with Loop In

## Installation
Download the latest binaries from the [releases](https://github.com/lightninglabs/loop/releases) page.

## Execution
The Loop client needs its own short-lived daemon to facilitate swaps. To start `loopd`:

```
loopd
```

To use Loop in testnet, simply pass the network flag:
```
loopd --network=testnet
```

By default `loopd` attempts to connect to the `lnd` instance running on
`localhost:10009` and reads the macaroon and tls certificate from `~/.lnd`.
This can be altered using command line flags. See `loopd --help`.

## Usage

### AutoLoop
AutoLoop makes it easy to keep your channels balanced. Checkout our [autoloop documentation](https://docs.lightning.engineering/advanced-best-practices/advanced-best-practices-overview/autoloop) for details.

### Loop Out
Use Loop Out to move bitcoins on Lightning into an on-chain Bitcoin address.

To execute a Loop Out:
```
loop out <amt_in_satoshis>
```

Other notable options:
- Use the `--fast` flag to swap immediately (Note: This opts-out of fee savings made possible by transaction batching)
- Use the `--channel` flag to loop out on specific channels
- Use the `--addr` flag to specify the address the looped out funds should be sent to (Note: By default funds are sent to the lnd wallet)

Run `loop monitor` to monitor the status of a swap.

### Loop In
Use Loop In to convert on-chain bitcoin into spendable Lightning funds.

To execute a Loop In:
```
loop in <amt_in_satoshis>
```

### More info
For more information about using Loop checkout our [Loop FAQs](./docs/faqs.md).

## Development

### Regtest
To get started with local development against a stripped down dummy Loop server
running in a local `regtest` Bitcoin network, take a look at the
[`regtest` server environment example documentation](./regtest/README.md).

### Testnet
To use Loop in testnet, simply pass the network flag:
```
loopd --network=testnet
```

### Submit feature requests
The [GitHub issue tracker](https://github.com/lightninglabs/loop/issues) can be
used to request specific improvements or report bugs.

### Join us on Slack
Join us on the
[LND Slack](https://lightning.engineering/slack.html) and join the #loop
channel to ask questions and interact with the community.

## LND
Note that Loop requires `lnd` to be built with **all of its subservers**. Download the latest [official release binary](https://github.com/lightningnetwork/lnd/releases/latest) or build `lnd` from source by following the [installation instructions](https://github.com/lightningnetwork/lnd/blob/master/docs/INSTALL.md). If you choose to build `lnd` from source, use the following command to enable all the relevant subservers:

```
make install tags="signrpc walletrpc chainrpc invoicesrpc"
```

## API
The Loop daemon exposes a [gRPC API](https://lightning.engineering/loopapi/#lightning-loop-grpc-api-reference)
(defaults to port 11010) and a [REST API](https://lightning.engineering/loopapi/index.html#loop-rest-api-reference)
(defaults to port 8081).

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

## Build from source
If you’d prefer to build from source:
```
git clone https://github.com/lightninglabs/loop.git
cd loop/cmd
go install ./...
```
