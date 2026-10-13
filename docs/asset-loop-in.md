# Receiving assets with Loop In

Connect loopd to the receiving node's Taproot Assets daemon. Select either an
asset ID or a compressed group public key, plus the asset channel's edge node.
A group key allows any asset in that group. The two selectors are mutually
exclusive.

Use `loop quote in --asset_id <id> --asset_edge_node <pubkey> <satoshis>`
(or `--group_key <key>`) to see the expected asset units and asset-units-per-BTC
rate. The quote RPC accepts these options in `QuoteRequest.asset_loop_in` and
returns them in `InQuoteResponse.asset_info`. Quotes are provisional: obtaining
one creates and immediately cancels a temporary hold invoice in the local node,
because tapd has no quote-only RPC for a BTC-denominated receiving amount.

To execute, run `loop in --asset_id <id> --asset_edge_node <pubkey>
--min_asset_amount <units> <satoshis>`. Use `--group_key` instead of `--asset_id`
to select a group. The confirmation displays the current rate, expected output,
and minimum alongside BTC amounts and fees. The required minimum is the user's
slippage bound in indivisible asset units. Execution obtains a fresh quote and
rejects an output below that minimum before funding. The execution quote must
cover the full 30-day invoice lifetime; tapd's built-in one-hour mock oracle does
not meet that requirement.

The server pays a BTC-denominated BOLT 11 invoice in millisatoshis. The edge
converts those millisatoshis into assets. Each multipart-payment HTLC rounds
separately, so the aggregate quote is not a guarantee of the final unit count.
Loop uses a hold invoice and sums the accepted HTLCs' actual asset units before
revealing the preimage. If the sum is below the minimum, Loop cancels the invoice.
The on-chain funds remain recoverable through the normal HTLC timeout path;
this can delay recovery and incur an on-chain fee.

`SwapInfo`, `ListSwaps`, and `Monitor` expose the immutable execution terms in
`asset_loop_in_info`, including after restart. `asset_swap_only` includes these
swaps. `asset_amount` is the execution quote, not the settled amount; inspect the
receiving node's settled invoice HTLCs for actual asset units.
