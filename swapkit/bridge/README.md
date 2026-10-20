# Local asset wallet bridge

This package keeps the Loop client and server dependency graphs separate from
`loop/swapkit/tapsdk/runtime`. Both sides use versioned JSON messages containing
fixed-size keys, public transaction packages and proof files. Wallet keys stay
in LND; this transport does not sign transactions itself.

The shipping `loopassetd` process implements `Backend`. The server and client
connect with `NewClient(socket, token)` and invoke only the enumerated methods.
The process must bind the runtime journal to its wallet identity and Bitcoin
network before accepting transaction requests. Admission binds the client's
full asset proofs to the authenticated server registration. Server-only
`Terms.Admission` is never included in public swap status.

Use an absolute socket path directly beneath a private directory owned by the
current user and an independently generated 32-byte token in a private regular
file. `Listen` holds an exclusive process lease, rejects a live listener,
rejects symlinks and non-socket occupants, and removes an owned stale socket
after a crashed process. `Client` checks directory and socket ownership and
permissions before sending the token. Keep this bridge on the local machine;
remote HTTP listeners are unsupported.

Calls propagate the caller's deadline, capped at 30 seconds, and bound message
size to 64 MiB. Public peer protobuf messages are separately limited to
16 MiB in total; the larger private JSON budget does not enlarge that limit. Unknown methods, versions, fields and trailing JSON are rejected.
Errors distinguish invalid requests and pending chain operations; other remote
errors are opaque. The runtime may log detailed causes without recording
message payloads, tokens, signing secrets or complete proof files.

Every state-changing operation must persist its exact intent before a wallet,
signing or publication RPC. Retrying the same operation after a lost response
must reuse that intent. The transport provides authentication and framing;
these replay and authorization rules belong to the durable runtime and the
Loop protocol controllers. Importing a reported refund proof never releases a
server deposit by itself: the execution controller independently checks current
chain finality and failed Lightning payment state before release.

Run `GOWORK=off go test -race ./bridge` from the swapkit module. Tests include a
real child-process SIGKILL and stale-socket recovery, deadlines, authentication,
opaque errors, malformed messages and private-file checks.
