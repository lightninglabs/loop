# Loop Client Release Notes

- **Release date:** 2021-02-09
- **Release page:**
  [v0.11.3-beta](https://github.com/lightninglabs/loop/releases/tag/v0.11.3-beta)
- **Previous release:** [v0.11.2-beta](release-notes-0.11.2.md)
- **Next release:** [v0.11.4-beta](release-notes-0.11.4.md)

#### New Features

* If lnd is locked when the loop client starts up, it will wait for lnd to be
  unlocked. Previous versions would exit with an error.
* Loop will no longer need all `lnd` subserver macaroons to be present in the
  `--lnd.macaroondir`. Instead the new `--lnd.macaroonpath` option can be
  pointed to a single macaroon, for example the `admin.macaroon` or a custom
  baked one with the exact permissions needed for Loop. If the now deprecated
  flag/option `--lnd.macaroondir` is used, it will fall back to use only the
  `admin.macaroon` from that directory.
* The rules used for autoloop have been relaxed to allow autoloop to dispatch
  swaps even if there are manually initiated swaps that are not limited to a
  single channel in progress. This change was made to allow autoloop to coexist
  with manual swaps.
* The `SuggestSwaps` endpoint has been updated to include reasons that indicate
  why the Autolooper is not currently dispatching swaps for the set of rules
  that the client is configured with. See the
  [autoloop documentation](../autoloop.md) for a detailed explanations of
  these reasons.

#### Breaking Changes

* The `AutoOut`, `AutoOutBudgetSat` and `AutoOutBudgetStartSec` fields in the
  `LiquidityParameters` message used in the experimental autoloop API have been
  renamed to `Autoloop`, `AutoloopBudgetSat` and `AutoloopBudgetStartSec`.
* The `autoout` flag for enabling automatic dispatch of loop out swaps has been
  renamed to `autoloop` so that it can cover loop out and loop in.
* The `SuggestSwaps` rpc call will now fail with a `FailedPrecondition` grpc
  error code if no rules are configured for the autolooper. Previously the rpc
  would return an empty response.

#### Bug Fixes

- Fixed compile time compatibility with `lnd v0.12.0-beta`.

#### Maintenance

#### Contributors (Alphabetical Order)

- Alex Bosworth
- Carla Kirk-Cohen
- Oliver Gugger
