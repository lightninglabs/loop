# Lightning Loop

## Loop Out Swap (off -> on-chain)

```
       loop out 500   
                  |
                  |
                  v
   .-----------------------------.
   | Loop CLI                    |
   | ./cmd/loop                  |
   |                             |
   |                             |
   |       .-------------------. |            .--------------.                   .---------------.
   |       | Loop Client (lib) | |            | LND node     |                   | Bitcoin node  |
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
           | Loop Server        |  htlc        | LND node     |                  | Bitcoin node  |
           |                    |<-------------|              |                  |               |
           |                    |              |              |   on-chain       |               |
           |                    |              |              |   htlc           |               |
           |                    |--------------|              |----------------->|               |
           |                    |              |              |                  |               |
           '--------------------'              '--------------'                  '---------------'

```

## Setup

LND and the swaplet are using go modules. Make sure that the `GO111MODULE` env
variable is set to `on`.

In order to execute a swap, LND needs to be rebuilt with sub servers enabled.

### LND

* Checkout branch `master`

- `make install tags="signrpc walletrpc chainrpc"` to build and install lnd
  with required sub-servers enabled.

- Make sure there are no macaroons in the lnd dir
  `~/.lnd/data/chain/bitcoin/mainnet`. If there are, lnd has been started
  before and in that case, it could be that `admin.macaroon` doesn't contain
  signer permission. Delete `macaroons.db` and `*.macaroon`. 

  DO NOT DELETE `wallet.db` !

- Start lnd

### Loopd
- `git clone https://github.com/lightninglabs/loop.git
- `cd loopd/cmd`
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

  `loop uncharge <amt_msat>` 
  
  When the swap is initiated successfully, `loopd` will see the process through.

* To query and track the swap status, run `loop` without arguments.    

## Resume
When `loopd` is terminated (or killed) for whatever reason, it will pickup
pending swaps after a restart. 

Information about pending swaps is stored persistently in the swap database.
Its location is `~/.loopd/<network>/loop.db`.

## Multiple simultaneous swaps

It is possible to execute multiple swaps simultaneously.

