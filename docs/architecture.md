# Loop Architecture

Lightning Loop's architecture for orchestrating submarine swaps is based on a
client/server concept.

The client has a swap execution daemon `loopd` controlled by a CLI application
`loop` which uses a gRPC API. The client daemon initiates swaps and handles
their progress through swap phases. To manage keys and the connection to the
LN/BTC network layers the client daemon connects to a local lnd wallet.

Client daemons communicate with the Loop server daemon, which is opaque but
operates in a similar way. The server is not a trusted component in this
architecture; the client daemon validates that the terms of the swap are
acceptable and the server cannot access the swap funds unless the swap enters
the "complete" phase.

## Loop Out Swap (off -> on-chain)

Phases:

1. Initiation: Client queries for terms of a swap
2. Fee: Client sends a small fee HTLC that is unrestricted
3. Funding: Client sends a funding HTLC locked to a preimage they generate
4. Payment: Server sends the funds on-chain locked to the funding preimage hash
5. Complete: Client uses the preimage to take the on-chain funds.
6. Final: The server uses the on-chain-revealed preimage to claim funding HTLC

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
