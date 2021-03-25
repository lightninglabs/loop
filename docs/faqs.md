
# Frequently Asked Questions

## How does Loop recover from a crash?
When `loopd` is terminated (or killed) for whatever reason, it will pickup
pending swaps after a restart.

Information about pending swaps is stored persistently in the swap database.
Its location is `~/.loopd/<network>/loop.db`.

## Can Loop handle multiple simultaneous swaps?
It is possible to execute multiple swaps simultaneously. Just keep loopd
running.

## What are the fees?

You can pass the `--verbose` flag when using Loop to get a detailed fee
breakdown

### Loop Out Fees

An explanation of each fee:

- **Estimated on-chain fee**: The estimated cost to sweep the
  HTLC in case of success, calculated based on the _current_ on-chain fees.
  This value is called `miner_fee` in the gRPC/REST responses.
- **Max on-chain fee**: The maximum on-chain fee the daemon
  is going to allow for sweeping the HTLC in case of success. A fee estimation
  based on the `--conf_target` flag is always performed before sweeping. The
  factor of `100` times the estimated fee is applied in case the fees spike
  between the time the swap is initiated and the time the HTLC can be swept. But
  that is the absolute worst-case fee that will be paid. If there is no fee
  spike, a normal, much lower fee will be used.
- **Max off-chain swap routing fee**: The maximum off-chain
  routing fee that the daemon should pay when finding a route to pay the
  Lightning invoice. This is a hard limit. If no route with a lower or equal fee
  is found, the payment (and the swap) is aborted. This value is calculated
  statically based on the swap amount (see `maxRoutingFeeBase` and
  `maxRoutingFeeRate` in `cmd/loop/main.go`).
- **Max off-chain prepay routing fee**: The maximum off-chain routing
  fee that the daemon should pay when finding a route to pay the prepay fee.
  This is a hard limit. If no route with a lower or equal fee is found, the
  payment (and the swap) is aborted. This value is calculated statically based
  on the prepay amount (see `maxRoutingFeeBase` and `maxRoutingFeeRate` in
  `cmd/loop/main.go`).
- **No show penalty (prepay)**: This is the amount that has to be
  pre-paid (off-chain) before the server publishes the HTLC on-chain. This is
  necessary to ensure the server's on-chain fees are paid if the client aborts
  and never completes the swap _after_ the HTLC has been published on-chain.
  If the swap completes normally, this amount is counted towards the full swap
  amount and therefore is actually a pre-payment and not a fee. This value is
  called `prepay_amt` in the gRPC/REST responses.

### Loop In Fees

An explanation of each fee:

- **Estimated on-chain fee**: The estimated on-chain fee that the
  daemon has to pay to publish the HTLC. This is an estimation from `lnd`'s
  wallet based on the available UTXOs and current network fees. This value is
  called `miner_fee` in the gRPC/REST responses.
