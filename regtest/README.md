# Local regtest setup

To help with development of apps that use the Loop daemon (`loopd`) for
liquidity management, it is often useful to test it in controlled environments
such as on the Bitcoin `regtest` network.

Lightning Labs provides a stripped down version of the Loop server that works
only for `regtest` and is only published in compiled, binary form (currently as
a Docker image).

The `docker-compose.yml` shows an example setup and is accompanied by a quick
start script that should make getting started very easy.

## Requirements

To get this quick start demo environment operational, you need to have the
following tools installed on your system:
 - Docker
 - `docker-compose`
 - `jq`
 - `git`

## Starting the environment

Simply download the Loop repository and run the `start` command of the helper
script. This will boot up the `docker-compose` environment, mine some `regtest`
coins, connect and create channels between the two `lnd` nodes and hook up the
Loop client and servers to each other.

```shell
$ git clone https://github.com/lightninglabs/loop
$ cd loop/regtest
$ ./regtest.sh start
```

## Looping out

Looping out is the process of moving funds from your channel (off-chain) balance
into your on-chain wallet through what is called a submarine swap. We use the
`--fast` option here, otherwise the server would wait 30 minutes before
proceeding to allow for batched swaps.

```shell
$ ./regtest.sh loop out --amt 500000 --fast
Send off-chain:                            500000 sat
Receive on-chain:                          492688 sat
Estimated total fee:                         7312 sat

Fast swap requested.

CONTINUE SWAP? (y/n): y
Swap initiated
ID:             f96b2e40fd75a63eba61a6ee1e0b17788d77804c0454beb5fbbe4a42a8a06245
HTLC address:   bcrt1qwgzsmag0579twr4d8hgfvarqxn27mdmdgfh7wc2hdlupsfqtxwyq5zltqn

Run `loop monitor` to monitor progress.
```

The swap has been initiated now. Let's observe how it works:

```shell
$ ./regtest.sh loop monitor
Note: offchain cost may report as 0 after loopd restart during swap
2021-03-19T17:01:39Z LOOP_OUT INITIATED 0.005 BTC - bcrt1qwgzsmag0579twr4d8hgfvarqxn27mdmdgfh7wc2hdlupsfqtxwyq5zltqn
```

Leave the command running, it will update with every new event. Open a second
terminal window in the same folder/location and mine one block:

```shell
$ ./regtest.sh mine 1
```

You should see your previous window with the `monitor` command update:

```shell
2021-03-19T17:01:39Z LOOP_OUT INITIATED 0.005 BTC - bcrt1qwgzsmag0579twr4d8hgfvarqxn27mdmdgfh7wc2hdlupsfqtxwyq5zltqn
2021-03-19T17:04:37Z LOOP_OUT PREIMAGE_REVEALED 0.005 BTC - bcrt1qwgzsmag0579twr4d8hgfvarqxn27mdmdgfh7wc2hdlupsfqtxwyq5zltqn
```

As soon as the `PREIMAGE_REVEALED` message appears, you know the HTLC was
published to the chain. Finish the process by mining yet another block (in the
second terminal window):

```shell
$ ./regtest.sh mine 1
```

Your `monitor` window should now show the completion of the swap:

```shell
2021-03-19T17:01:39Z LOOP_OUT INITIATED 0.005 BTC - bcrt1qwgzsmag0579twr4d8hgfvarqxn27mdmdgfh7wc2hdlupsfqtxwyq5zltqn
2021-03-19T17:04:37Z LOOP_OUT PREIMAGE_REVEALED 0.005 BTC - bcrt1qwgzsmag0579twr4d8hgfvarqxn27mdmdgfh7wc2hdlupsfqtxwyq5zltqn
2021-03-19T17:06:25Z LOOP_OUT SUCCESS 0.005 BTC - bcrt1qwgzsmag0579twr4d8hgfvarqxn27mdmdgfh7wc2hdlupsfqtxwyq5zltqn (cost: server 50, onchain 6687, offchain 0)
```

Congratulations, you just did a loop out!

# Loop in

Looping in is very similar but does the opposite: swap your on-chain balance for
off-chain (channel) balance:

```shell
$ ./regtest.sh loop in --amt 500000
Send on-chain:                             500000 sat
Receive off-chain:                         492279 sat
Estimated total fee:                         7721 sat

CONTINUE SWAP? (y/n): y
Swap initiated
ID:           d95167e33e79df292e5ffeda29d34614a5a087c9378ffae003b9a25bb0f1c2e4
HTLC address (P2WSH): bcrt1q4p0pead4tfepm683fff8fl8g3kpx8spjztuquu2sctfzed9rufls2z0g2f

Run `loop monitor` to monitor progress.


$ ./regtest.sh loop monitor
Note: offchain cost may report as 0 after loopd restart during swap
2021-03-19T17:17:13Z LOOP_IN HTLC_PUBLISHED 0.005 BTC - P2WSH: bcrt1q4p0pead4tfepm683fff8fl8g3kpx8spjztuquu2sctfzed9rufls2z0g2f
```

In your second terminal window, go ahead and mine a block:

```shell
$ ./regtest.sh mine 1
```

You should see your previous window with the `monitor` command update:

```shell
2021-03-19T17:17:13Z LOOP_IN HTLC_PUBLISHED 0.005 BTC - P2WSH: bcrt1q4p0pead4tfepm683fff8fl8g3kpx8spjztuquu2sctfzed9rufls2z0g2f
2021-03-19T17:18:38Z LOOP_IN INVOICE_SETTLED 0.005 BTC - P2WSH: bcrt1q4p0pead4tfepm683fff8fl8g3kpx8spjztuquu2sctfzed9rufls2z0g2f (cost: server -499929, onchain 7650, offchain 0)
```

After mining yet another block, the process should complete:

```shell
$ ./regtest.sh mine 1
```

```shell
2021-03-19T17:17:13Z LOOP_IN HTLC_PUBLISHED 0.005 BTC - P2WSH: bcrt1q4p0pead4tfepm683fff8fl8g3kpx8spjztuquu2sctfzed9rufls2z0g2f
2021-03-19T17:18:38Z LOOP_IN INVOICE_SETTLED 0.005 BTC - P2WSH: bcrt1q4p0pead4tfepm683fff8fl8g3kpx8spjztuquu2sctfzed9rufls2z0g2f (cost: server -499929, onchain 7650, offchain 0)
2021-03-19T17:19:21Z LOOP_IN SUCCESS 0.005 BTC - P2WSH: bcrt1q4p0pead4tfepm683fff8fl8g3kpx8spjztuquu2sctfzed9rufls2z0g2f (cost: server 71, onchain 7650, offchain 0)
```

# Static Address Loop In

Static Address Loop In is a new loop-in mode that, like Loop In, allows you to 
swap your on-chain balance for off-chain (channel) balance, but in contrast to 
the legacy loop-in, you can receive the off-chain balance instantly (after funds
were deposited to a static address).
To do so a two-step process is required, the setup and the swap phase.

To setup a static address and deposit funds to it, do the following:

```shell
./regtest.sh loop static new                                                                             ✔  11:59:12 

WARNING: Be aware that loosing your l402.token file in .loop under your home directory will take your ability to spend funds sent to the static address via loop-ins or withdrawals. You will have to wait until the deposit expires and your loop client sweeps the funds back to your lnd wallet. The deposit expiry could be months in the future.

CONTINUE WITH NEW ADDRESS? (y/n): y
Received a new static loop-in address from the server: bcrt1pzgdmxftg3t6wghl2t72ewqyf3jvy2t85ud6pqsagx0zm9mj8f3aq23v3t4
```
A static address has been created. You can send funds to it across different
transactions. Each transaction output to this address is considered a deposit
that can be used individually or in combination with other deposits to be 
swapped for an off-chain payment. Let's create a few deposits and confirm them:
```shell
./regtest.sh lndclient sendcoins --addr bcrt1pzgdmxftg3t6wghl2t72ewqyf3jvy2t85ud6pqsagx0zm9mj8f3aq23v3t4 --amt 250000 --min_confs 0 -f
{
    "txid": "86ccc85449957c3472a259e447a9180aff49848f0b957a85c24819a5f432eda7"
}
./regtest.sh lndclient sendcoins --addr bcrt1pzgdmxftg3t6wghl2t72ewqyf3jvy2t85ud6pqsagx0zm9mj8f3aq23v3t4 --amt 250000 --min_confs 0 -f
{
    "txid": "09d72c89b2346dc068bb57621463e53637f3c0cca3ba8ebb7fcb2773d7f32ae3"
}
./regtest.sh lndclient sendcoins --addr bcrt1pzgdmxftg3t6wghl2t72ewqyf3jvy2t85ud6pqsagx0zm9mj8f3aq23v3t4 --amt 250000 --min_confs 0 -f
{
    "txid": "5405e5cae38f9e4e193f7b5442dd005273e2e1fab8687c42a505c1a333e8884f"
}

./regtest.sh mine 6
```
The loop client logs should show the deposits being confirmed:
```shell
[DBG] SADDR: Received deposit: 5405e5cae38f9e4e193f7b5442dd005273e2e1fab8687c42a505c1a333e8884f:0
[DBG] SADDR: Deposit 5405e5cae38f9e4e193f7b5442dd005273e2e1fab8687c42a505c1a333e8884f:0: NextState: Deposited, PreviousState: , Event: OnStart
[DBG] SADDR: Received deposit: 86ccc85449957c3472a259e447a9180aff49848f0b957a85c24819a5f432eda7:1
[DBG] SADDR: Deposit 86ccc85449957c3472a259e447a9180aff49848f0b957a85c24819a5f432eda7:1: NextState: Deposited, PreviousState: , Event: OnStart
[DBG] SADDR: Received deposit: 09d72c89b2346dc068bb57621463e53637f3c0cca3ba8ebb7fcb2773d7f32ae3:1
[DBG] SADDR: Deposit 09d72c89b2346dc068bb57621463e53637f3c0cca3ba8ebb7fcb2773d7f32ae3:1: NextState: Deposited, PreviousState: , Event: OnStart
```
Let's list the deposits in the loop client:
```shell
./regtest.sh loop static listdeposits --filter deposited                                              ✔  12:03:37 
{
    "filtered_deposits":  [
        {
            "id":  "5afc81a72464881e66aa6a5d7476b75cae26ea4c74c70424e4a68e63b9708e0c",
            "state":  "DEPOSITED",
            "outpoint":  "5405e5cae38f9e4e193f7b5442dd005273e2e1fab8687c42a505c1a333e8884f:0",
            "value":  "250000",
            "confirmation_height":  "126",
            "blocks_until_expiry":  "715"
        },
        {
            "id":  "0bf419bb047922afc186021fc186970b0b71db83444b39b0dcad4550014e0ba3",
            "state":  "DEPOSITED",
            "outpoint":  "86ccc85449957c3472a259e447a9180aff49848f0b957a85c24819a5f432eda7:1",
            "value":  "250000",
            "confirmation_height":  "126",
            "blocks_until_expiry":  "715"
        },
        {
            "id":  "ea969e2ca53b608d4cf9ca8def249d77ee1db444bd3384ae1150ea03bb03dbbd",
            "state":  "DEPOSITED",
            "outpoint":  "09d72c89b2346dc068bb57621463e53637f3c0cca3ba8ebb7fcb2773d7f32ae3:1",
            "value":  "250000",
            "confirmation_height":  "126",
            "blocks_until_expiry":  "715"
        }
    ]
}
```
These deposits can now be instantly swapped. Let's use the first one:
```shell
./regtest.sh loop static in --utxo 5405e5cae38f9e4e193f7b5442dd005273e2e1fab8687c42a505c1a333e8884f:0
On-chain fees for static address loop-ins are not included.
They were already paid when the deposits were created.

Previously deposited on-chain:             250000 sat
Receive off-chain:                         249614 sat
Estimated total fee:                          386 sat

CONTINUE SWAP? (y/n): y
{
    "swap_hash":  "43ed16958d5a5bf0e6cb9a36eefb1493f39741238f06d6c7b5365c1c5c22d29e",
    "state":  "SignHtlcTx",
    "amount":  "250000",
    "htlc_cltv":  431,
    "quoted_swap_fee_satoshis":  "386",
    "max_swap_fee_satoshis":  "386",
    "initiation_height":  131,
    "protocol_version":  "V0",
    "label":  "",
    "initiator":  "loop-cli",
    "payment_timeout_seconds":  60
}
```
We see in the client log that the swap instantly succeeds:
```shell
[INF] LOOPD: Loop in quote request received
[INF] LOOPD: Static loop-in request received
[INF] SADDR: StaticAddr loop-in 0000000000000000000000000000000000000000000000000000000000000000: Current: InitHtlcTx
[DBG] SADDR: Deposit 5405e5cae38f9e4e193f7b5442dd005273e2e1fab8687c42a505c1a333e8884f:0: NextState: LoopingIn, PreviousState: Deposited, Event: OnLoopInInitiated
[INF] SADDR: StaticAddr loop-in 43ed16958d5a5bf0e6cb9a36eefb1493f39741238f06d6c7b5365c1c5c22d29e: Current: SignHtlcTx
[INF] SADDR: StaticAddr loop-in 43ed16958d5a5bf0e6cb9a36eefb1493f39741238f06d6c7b5365c1c5c22d29e: Current: MonitorInvoiceAndHtlcTx
[DBG] SADDR: StaticAddr loop-in 43ed16958d5a5bf0e6cb9a36eefb1493f39741238f06d6c7b5365c1c5c22d29e: received off-chain payment update Settled
[INF] SADDR: StaticAddr loop-in 43ed16958d5a5bf0e6cb9a36eefb1493f39741238f06d6c7b5365c1c5c22d29e: Current: PaymentReceived
[DBG] SADDR: Deposit 5405e5cae38f9e4e193f7b5442dd005273e2e1fab8687c42a505c1a333e8884f:0: NextState: LoopedIn, PreviousState: LoopingIn, Event: OnLoopedIn
[INF] SADDR: StaticAddr loop-in 43ed16958d5a5bf0e6cb9a36eefb1493f39741238f06d6c7b5365c1c5c22d29e: Current: Succeeded
```
We can combine the remaining two deposits in another instant swap by specifying
the `--all` flag:
```shell
./regtest.sh loop static in --all                                                                     ✔  12:07:28 
On-chain fees for static address loop-ins are not included.
They were already paid when the deposits were created.

Previously deposited on-chain:             500000 sat
Receive off-chain:                         499302 sat
Estimated total fee:                          698 sat

CONTINUE SWAP? (y/n): y
{
    "swap_hash":  "950a4c0017831dc9934e93faacde1819ed5801d1c7abf6b555dc36908a3b3ca8",
    "state":  "SignHtlcTx",
    "amount":  "500000",
    "htlc_cltv":  431,
    "quoted_swap_fee_satoshis":  "698",
    "max_swap_fee_satoshis":  "698",
    "initiation_height":  131,
    "protocol_version":  "V0",
    "label":  "",
    "initiator":  "loop-cli",
    "payment_timeout_seconds":  60
}
```
For more information on the static address loop-in feature, see the 
https://docs.lightning.engineering/lightning-network-tools/loop/static-loop-in-addresses

# Using the Loop server in an existing setup

This `docker-compose` is only meant as a demo and quick start help. You can of
course also integrate the Loop server Docker image into your existing `regtest`
environment.

Simply pull the image and run it by pointing it to an existing `lnd` node:

```shell
$ docker pull lightninglabs/loopserver:latest
$ docker run -d \
    -p 11009:11009 \
    -v /some/dir/to/lnd:/root/.lnd \
    lightninglabs/loopserver:latest \
      daemon \
      --maxamt=5000000 \
      --lnd.host=some-lnd-node:10009 \
      --lnd.macaroondir=/root/.lnd/data/chain/bitcoin/regtest \
      --lnd.tlspath=/root/.lnd/tls.cert \
```

An existing Loop client (`loopd`) can then be pointed to this server with:

```shell
$ loopd \
    --network=regtest \
    --debuglevel=debug \
    --server.host=localhost:11009 \
    --server.notls \
    --lnd.host=some-other-lnd-node:10009 \
    --lnd.macaroonpath=/root/.lnd/data/chain/bitcoin/regtest/admin.macaroon \
    --lnd.tlspath=/root/.lnd/tls.cert
```

The `--server.notls` is important here as the `regtest` version of the Loop
server only supports insecure, non-TLS connections.

Also make sure you connect the Loop server and Loop client to a different `lnd`
node each since an `lnd` node cannot pay itself. Obviously there also need to be
some channels with enough liquidity between the server's and client's `lnd`
nodes (direct or indirect doesn't matter).
