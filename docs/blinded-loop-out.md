# Blinded Loop Out invoices

Clients advertise `accept_blinded_invoices` in `NewLoopOutSwap`. Servers must
only return bLIP 39 invoices to clients that advertise this capability. Invoice
hash, amount and fee validation still apply to swap and prepay invoices.

A blinded swap invoice carries its payment secret in encrypted recipient data,
so the client cannot recover it from the invoice. The server returns the same
32-byte secret separately in `ServerLoopOutResponse.payment_address` over the
authenticated swap RPC. This secret authorizes cancellation, routing reports
and cooperative sweep signing; it is distinct from the swap preimage.

The client persists this value with the swap and uses it after restart and
when the sweep batcher reloads the swap. Missing secrets for blinded swap
invoices and malformed response lengths are rejected. For ordinary invoices,
the separate value is optional and must match the invoice when supplied. Old
swaps continue deriving their secret from their invoice.

LND receives the complete invoice string and handles introduction-node routing
and blinded payment paths, including multipart payments. Destination-based Loop
routing plugins are disabled for blinded invoices. Fees, timeouts and outgoing
channel restrictions still apply. A blinded prepay invoice is paid through the
same normal invoice-payment path and needs no separate RPC authentication key.

This change requires a payer LND with bLIP 39 support, already covered by Loop's
minimum supported LND version, v0.19.0-beta. It does not select invoice paths or
require a new LND RPC. Servers are responsible for creating payable invoices
and checking the authentication secret against the corresponding swap.

The client can be released before server support is enabled. Existing servers
ignore the capability flag and continue returning ordinary invoices. A server
that supports blinding can later return blinded invoices to upgraded clients;
it must continue returning ordinary invoices when the flag is unset. No client
configuration or dependency on a particular server path-selection algorithm is
required.
