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
