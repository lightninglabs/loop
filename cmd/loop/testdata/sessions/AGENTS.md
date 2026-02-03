# Session Recording Notes (Loop CLI)

## How to record sessions
- Use the local CLI binary: `/home/user/bin/loop`.
- Always include `--network regtest` **after** the main command and subcommands so it does not become part of the session filename.
- Record with `LOOP_SESSION_RECORD=true`, e.g.:
  - `LOOP_SESSION_RECORD=true /home/user/bin/loop quote out --network regtest 500000`
- After recording several related commands, group the resulting JSON files into a subdir under `cmd/loop/testdata/sessions/`.

## Control panel HTTP server (regtest helpers)
Base URL: `http://127.0.0.1:12345`
- Implementation: [httpcmd](https://github.com/starius/httpcmd/)
- Config: [control panel config](https://gist.github.com/starius/6604ffe27f51d55f4cf715b4202637dd)
- `/mine`:
  - Mines a block. Returns JSON with `exit_code`, `stdout`, `stderr`, `duration`, `timed_out`.
- `/deposit`:
  - Sends a deposit to the client static address. Returns JSON with a `txid` on success.
  - May fail with insufficient funds unless blocks have been mined first.
- `/reservation`:
  - Opens a reservation for instant out. Returns JSON with reservation details (id, amount, state, etc.).
- `/loop-log`:
  - Returns loopd logs (text). Use a tail filter when inspecting.
- `/loop-server-log`:
  - Returns loop server logs (text). Use a tail filter when inspecting.
  - Tip: The response is JSON with a large `stdout` field; extract first, then tail:
    - `curl -s http://127.0.0.1:12345/loop-server-log | jq -r '.stdout' > /tmp/loop-server-log.txt`
    - `tail -n 50 /tmp/loop-server-log.txt`

## Useful regtest flow observations
- Deposits may be unconfirmed for a few blocks; mine multiple blocks to move a deposit into `DEPOSITED`.
- After making a deposit, `loop static summary` reflects unconfirmed + confirmed values; `loop static listdeposits` lists confirmed deposits.
- Reservations should be opened (via `/reservation`) before `instantout`; list them with `loop reservations list`.
- `instantout` uses interactive selection; `ALL` then `y` works for a simple scenario:
  - `printf "ALL\ny\n" | LOOP_SESSION_RECORD=true /home/user/bin/loop instantout --network regtest`

## Coverage notes (high-level)
- Sessions were recorded for: terms, getinfo, quote in/out, listauth, fetchl402, getparams, setrule (error + success), suggestswaps (error + success), reservations list, instantout, listinstantouts, static withdraw/listwithdrawals/listswaps, listswaps, swapinfo, loop out (forced), abandon swap (help path), plus existing static-loop-in/basic-swaps.
- Some paths are intentionally skipped for now:
  - `stop` (would shut down loopd; replay expects a real gRPC conn).
  - Asset quote paths (`getAssetAmt`, `unmarshalFixedPoint`) require tapd/asset quotes.
  - Real dialer/macaroon path handling (`extractPathArgs`, `readMacaroon`, `getClientConn`) aren’t exercised by replay.


## Replay stability notes
- Session replay now clones the CLI command tree per run, so flag state (`IsSet`) does not leak between sessions.
- Historical warning: earlier replays could have sticky flags across runs; if you see odd ordering-dependent failures, re-check that the replay uses the cloned command path.

## Session coverage map
| Subdir | Commands / scenarios |
| --- | --- |
| `basic-swaps/` | `loop out` (success), `loop in` (success), `loop monitor` |
| `getinfo/` | `loop getinfo` |
| `instantout/` | `loop reservations list`, `loop instantout` (ALL + confirm), `loop instantout` (channel flag), `loop instantout` (select index), `loop instantout` (no confirmed reservations), `loop instantout` (cancel), `loop instantout` (invalid selection), `loop listinstantouts` |
| `l402/` | `loop listauth`, `loop fetchl402` |
| `liquidity/` | `loop getparams`, `loop setparams` (no flags, feepercent, conflict, many flags/categories, destaddr, account, includeallpeers; includes error cases), `loop setrule` (missing threshold, no args, incoming/outgoing/type in/out, clear), `loop suggestswaps` (error + success) |
| `loopin/` | `loop in` (invalid amount), `loop in` (external + conf_target error), `loop in` (route_hints + private error), `loop in` (external cancel + verbose, last_hop/amt flag), `loop in` (external force) |
| `loopout/` | `loop out` (forced success), `loop out` (invalid amount), `loop out` (addr + account error), `loop out` (invalid account address type), `loop out` (amt flag + channel + max routing fee + payment timeout), `loop out` (addr flag), `loop out` (positional addr), `loop out` (account + account_addr_type) |
| `misc/` | `loop terms` |
| `quote/` | `loop quote out` (success + verbose), `loop quote in` (help + verbose), `loop quote out` (help), `loop quote in` (deposit_outpoint success), `loop quote in` (positional + last_hop) |
| `static/` | `loop static withdraw` (no selection error), `loop static withdraw` (invalid utxo), `loop static withdraw` (all success), `loop static withdraw` (utxo + dest_addr success), `loop static listwithdrawals`, `loop static listswaps` |
| `static-loop-in/` | `loop static new`, `loop static` (help), `loop static listunspent` (incl alias), `loop static listdeposits`, `loop static summary`, `loop static in` (multiple args/flags cases), `loop static in` (duplicate outpoints), `loop static in` (positional low amount error), `loop static in` (positional + last_hop + payment_timeout), `loop static in` (all cancel) |
| `static-filters/` | `loop static listdeposits --filter ...` for each state (deposited/withdrawing/withdrawn/looping_in/looped_in/publish_expired_deposit/sweep_htlc_timeout/htlc_timeout_swept/wait_for_expiry_sweep/expired/failed) |
| `swaps/` | `loop listswaps` (success + conflicting filters + loop_out_only filters + loop_in_only), `loop swapinfo` (success + invalid id + id flag errors), `loop abandonswap` (help + invalid id + success) |

## Missing / not covered yet
- `loop stop` (would terminate loopd; replay expects a live gRPC server).
- Docs generators: `loop man`, `loop markdown` (handled in a separate docs pipeline).
- Asset/tapd scenarios (explicitly out of scope for now).
