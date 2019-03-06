# Swaplet

## Uncharge swap (off -> on-chain)

```
       swapcli uncharge 500   
                  |
                  |
                  v
   .-----------------------------.
   | Swap CLI                    |
   | ./cmd/swapcli               |
   |                             |
   |                             |
   |       .-------------------. |            .--------------.                   .---------------.
   |       | Swap Client (lib) | |            | LND node     |                   | Bitcoin node  |
   |       | ./                |<-------------|              |-------------------|               |
   |       |                   | |            |              |   on-chain        |               |
   |       |                   |------------->|              |   htlc            |               |
   |       |                   | | off-chain  |              |                   |               |
   |       '-------------------' | htlc       '--------------'                   '---------------'
   '-----------------|-----------'                    |                                  ^
                     |                                |                                  |
                     |                                v                                  |
                     |                              .--.                               .--.               
                     |                          _ -(    )- _                       _ -(    )- _           
                     |                     .--,(            ),--.             .--,(            ),--.      
             initiate|                 _.-(                       )-._    _.-(                       )-._ 
             swap    |                (       LIGHTNING NETWORK       )  (        BITCOIN NETWORK        )
                     |                 '-._(                     )_.-'    '-._(                     )_.-' 
                     |                      '__,(            ),__'             '__,(            ),__'     
                     |                           - ._(__)_. -                       - ._(__)_. -          
                     |                                |                                  ^
                     |                                |                                  |
                     v                                v                                  |
           .--------------------.  off-chain   .--------------.                  .---------------.
           | Swap Server        |  htlc        | LND node     |                  | Bitcoin node  |
           |                    |<-------------|              |                  |               |
           |                    |              |              |   on-chain       |               |
           |                    |              |              |   htlc           |               |
           |                    |--------------|              |----------------->|               |
           |                    |              |              |                  |               |
           '--------------------'              '--------------'                  '---------------'

```

## Setup

LND and the swaplet are using go modules. Make sure that the `GO111MODULE` env variable is set to `on`.

In order to execute a swap, LND needs to be rebuilt with sub servers enabled.

### LND

* Checkout branch `master`

- `make install tags="signrpc walletrpc chainrpc"` to build and install lnd with required sub-servers enabled.

- Make sure there are no macaroons in the lnd dir `~/.lnd/data/chain/bitcoin/mainnet`. If there are, lnd has been started before and in that case, it could be that `admin.macaroon` doesn't contain signer permission. Delete `macaroons.db` and `*.macaroon`. 

  DO NOT DELETE `wallet.db` !

- Start lnd

### Swaplet
- `git clone git@gitlab.com:lightning-labs/swaplet.git`
- `cd swaplet/cmd`
- `go install ./...`

## Execute a swap

* Swaps are executed by a client daemon process. Run:

  `swapd`

  By default `swapd` attempts to connect to an lnd instance running on `localhost:10009` and reads the macaroon and tls certificate from `~/.lnd`. This can be altered using command line flags. See `swapd --help`.

  `swapd` only listens on localhost and uses an unencrypted and unauthenticated connection.

* To initiate a swap, run:

  `swapcli uncharge <amt_msat>` 
  
  When the swap is initiated successfully, `swapd` will see the process through.

* To query and track the swap status, run `swapcli` without arguments.    

## Resume
When `swapd` is terminated (or killed) for whatever reason, it will pickup pending swaps after a restart. 

Information about pending swaps is stored persistently in the swap database. Its location is `~/.swaplet/<network>/swapclient.db`.

## Multiple simultaneous swaps

It is possible to execute multiple swaps simultaneously.

