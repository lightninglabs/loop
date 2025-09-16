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

### Standard Loop-In (on -> off-chain)

The reverse of a Loop-Out. The client sends funds to an on-chain HTLC with a
2-of-2 MuSig2 keyspend path. Once the server detects that the HTLC transaction
is confirmed, it pays the Lightning invoice provided by the client to "loop in"
the funds to their node's channels and sweeps the HTLC. The client cooperates
with the server to cosign the sweep transaction to save on onchain fees.

### Instant Loop-Out

A faster, more efficient Loop-Out that prioritizes a cooperative path, built on
**Reservations** (on-chain UTXOs controlled by a 2-of-2 MuSig2 key between the
client and server).

1.  **Cooperative Path ("Sweepless Sweep"):** After the client pays the
    off-chain swap invoice, the client and server cooperatively sign a
    transaction that spends the reservation UTXO directly to the client's final
    destination address. This avoids publishing an HTLC, saving fees and chain
    space.

2.  **Fallback Path:** If the cooperative path fails, the system falls back to
    creating a standard on-chain HTLC from the reservation UTXO, which is then
    swept by the client. This ensures the swap remains non-custodial.

### Static Address (for Loop-In)

Provides a permanent, reusable on-chain address for receiving funds that can
then be swapped for an off-chain Lightning payment.

-   **Address Type:** The address is a P2TR (Taproot) output with two spending
    paths:
    1.  **Keyspend Path (Cooperative):** The internal key is a 2-of-2 MuSig2
        aggregate key held by the client and server. This path is used to
        cooperatively sign transactions for performing a Loop-In or withdrawing
        funds.
    2.  **Scriptspend Path (Timeout):** A tapleaf contains a script that allows
        the client to unilaterally sweep their funds after a CSV timeout. This
        is a safety mechanism ensuring the client never loses access to their
        deposits.

-   **Loop-In Flow:** When a user wants to loop in funds from this address, the
    client and server use the cooperative keyspend path to create and sign an
    HTLC transaction, which then follows a standard Loop-In flow. When the
    client gets the LN payment, they cooperate with the server to sweep the
    deposit directly to the server's wallet instead of publishing the HTLC tx.
