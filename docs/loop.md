## CLI interface - loop

control plane for your loopd.

Usage:

```bash
$ loop [GLOBAL FLAGS] [COMMAND] [COMMAND FLAGS] [ARGUMENTS...]
```

Global flags:

| Name                   | Description                                               | Type   |          Default value          |  Environment variables |
|------------------------|-----------------------------------------------------------|--------|:-------------------------------:|:----------------------:|
| `--rpcserver="…"`      | loopd daemon address host:port                            | string |        `localhost:11010`        |  `LOOPCLI_RPCSERVER`   |
| `--network="…"` (`-n`) | the network loop is running on e.g. mainnet, testnet, etc | string |            `mainnet`            |   `LOOPCLI_NETWORK`    |
| `--loopdir="…"`        | path to loop's base directory                             | string |            `~/.loop`            |   `LOOPCLI_LOOPDIR`    |
| `--tlscertpath="…"`    | path to loop's TLS certificate                            | string |   `~/.loop/mainnet/tls.cert`    |  `LOOPCLI_TLSCERTPATH` |
| `--macaroonpath="…"`   | path to macaroon file                                     | string | `~/.loop/mainnet/loop.macaroon` | `LOOPCLI_MACAROONPATH` |
| `--help` (`-h`)        | show help                                                 | bool   |             `false`             |         *none*         |
| `--version` (`-v`)     | print the version                                         | bool   |             `false`             |         *none*         |

### `out` command

perform an off-chain to on-chain swap (looping out).

Attempts to loop out the target amount into either the backing lnd's 	wallet, or a targeted address.  	The amount is to be specified in satoshis.  	Optionally a BASE58/bech32 encoded bitcoin destination address may be 	specified. If not specified, a new wallet address will be generated.

Usage:

```bash
$ loop [GLOBAL FLAGS] out [COMMAND FLAGS] amt [addr]
```

The following flags are supported:

| Name                         | Description                                                                                                                                                                                                                                                                    | Type     | Default value |
|------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|:-------------:|
| `--addr="…"`                 | the optional address that the looped out funds should be sent to, if let blank the funds will go to lnd's wallet                                                                                                                                                               | string   |
| `--account="…"`              | the name of the account to generate a new address from. You can list the names of valid accounts in your backing lnd instance with "lncli wallet accounts list"                                                                                                                | string   |
| `--account_addr_type="…"`    | the address type of the extended public key specified in account. Currently only pay-to-taproot-pubkey(p2tr) is supported                                                                                                                                                      | string   |    `p2tr`     |
| `--amt="…"`                  | the amount in satoshis to loop out. To check for the minimum and maximum amounts to loop out please consult "loop terms"                                                                                                                                                       | uint     |      `0`      |
| `--htlc_confs="…"`           | the number of confirmations (in blocks) that we require for the htlc extended by the server before we reveal the preimage                                                                                                                                                      | uint     |      `1`      |
| `--conf_target="…"`          | the number of blocks from the swap initiation height that the on-chain HTLC should be swept within                                                                                                                                                                             | uint     |      `9`      |
| `--max_swap_routing_fee="…"` | the max off-chain swap routing fee in satoshis, if not specified, a default max fee will be used                                                                                                                                                                               | int      |      `0`      |
| `--fast`                     | indicate you want to swap immediately, paying potentially a higher fee. If not set the swap server might choose to wait up to 30 minutes before publishing the swap HTLC on-chain, to save on its chain fees. Not setting this flag therefore might result in a lower swap fee | bool     |    `false`    |
| `--payment_timeout="…"`      | the timeout for each individual off-chain payment attempt. If not set, the default timeout of 1 hour will be used. As the payment might be retried, the actual total time may be longer                                                                                        | duration |     `0s`      |
| `--asset_id="…"`             | the asset ID of the asset to loop out, if this is set, the loop daemon will require a connection to a taproot assets daemon                                                                                                                                                    | string   |
| `--asset_edge_node="…"`      | the pubkey of the edge node of the asset to loop out, this is required if the taproot assets daemon has multiple channels of the given asset id with different edge nodes                                                                                                      | string   |
| `--force`                    | Assumes yes during confirmation. Using this option will result in an immediate swap                                                                                                                                                                                            | bool     |    `false`    |
| `--label="…"`                | an optional label for this swap,limited to 500 characters. The label may not start with our reserved prefix: [reserved]                                                                                                                                                        | string   |
| `--verbose` (`-v`)           | show expanded details                                                                                                                                                                                                                                                          | bool     |    `false`    |
| `--channel="…"`              | the comma-separated list of short channel IDs of the channels to loop out                                                                                                                                                                                                      | string   |
| `--help` (`-h`)              | show help                                                                                                                                                                                                                                                                      | bool     |    `false`    |

### `in` command

perform an on-chain to off-chain swap (loop in).

Send the amount in satoshis specified by the amt argument  		off-chain. 		 		By default the swap client will create and broadcast the  		on-chain htlc. The fee priority of this transaction can  		optionally be set using the conf_target flag.   		The external flag can be set to publish the on chain htlc  		independently. Note that this flag cannot be set with the  		conf_target flag.

Usage:

```bash
$ loop [GLOBAL FLAGS] in [COMMAND FLAGS] amt
```

The following flags are supported:

| Name                | Description                                                                                                             | Type   | Default value |
|---------------------|-------------------------------------------------------------------------------------------------------------------------|--------|:-------------:|
| `--amt="…"`         | the amount in satoshis to loop in. To check for the minimum and maximum amounts to loop in please consult "loop terms"  | uint   |      `0`      |
| `--external`        | expect htlc to be published externally                                                                                  | bool   |    `false`    |
| `--conf_target="…"` | the target number of blocks the on-chain htlc broadcast by the swap client should confirm within                        | uint   |      `0`      |
| `--last_hop="…"`    | the pubkey of the last hop to use for this swap                                                                         | string |
| `--label="…"`       | an optional label for this swap,limited to 500 characters. The label may not start with our reserved prefix: [reserved] | string |
| `--force`           | Assumes yes during confirmation. Using this option will result in an immediate swap                                     | bool   |    `false`    |
| `--verbose` (`-v`)  | show expanded details                                                                                                   | bool   |    `false`    |
| `--route_hints="…"` | route hints that can each be individually used to assist in reaching the invoice's destination                          | string |     `[]`      |
| `--private`         | generates and passes routehints. Should be used if the connected node is only reachable via private channels            | bool   |    `false`    |
| `--help` (`-h`)     | show help                                                                                                               | bool   |    `false`    |

### `terms` command

Display the current swap terms imposed by the server.

Usage:

```bash
$ loop [GLOBAL FLAGS] terms [COMMAND FLAGS] [ARGUMENTS...]
```

The following flags are supported:

| Name            | Description | Type | Default value |
|-----------------|-------------|------|:-------------:|
| `--help` (`-h`) | show help   | bool |    `false`    |

### `monitor` command

monitor progress of any active swaps.

Allows the user to monitor progress of any active swaps.

Usage:

```bash
$ loop [GLOBAL FLAGS] monitor [COMMAND FLAGS] [ARGUMENTS...]
```

The following flags are supported:

| Name            | Description | Type | Default value |
|-----------------|-------------|------|:-------------:|
| `--help` (`-h`) | show help   | bool |    `false`    |

### `quote` command

get a quote for the cost of a swap.

Usage:

```bash
$ loop [GLOBAL FLAGS] quote [COMMAND FLAGS] [ARGUMENTS...]
```

The following flags are supported:

| Name            | Description | Type | Default value |
|-----------------|-------------|------|:-------------:|
| `--help` (`-h`) | show help   | bool |    `false`    |

### `quote in` subcommand

get a quote for the cost of a loop in swap.

Allows to determine the cost of a swap up front.Either specify an amount or deposit outpoints.

Usage:

```bash
$ loop [GLOBAL FLAGS] quote in [COMMAND FLAGS] amt
```

The following flags are supported:

| Name                     | Description                                                                                                                                                                                                    | Type   | Default value |
|--------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------|:-------------:|
| `--last_hop="…"`         | the pubkey of the last hop to use for the quote                                                                                                                                                                | string |
| `--conf_target="…"`      | the target number of blocks the on-chain htlc broadcast by the swap client should confirm within                                                                                                               | uint   |      `0`      |
| `--verbose` (`-v`)       | show expanded details                                                                                                                                                                                          | bool   |    `false`    |
| `--private`              | generates and passes routehints. Should be used if the connected node is only reachable via private channels                                                                                                   | bool   |    `false`    |
| `--route_hints="…"`      | route hints that can each be individually used to assist in reaching the invoice's destination                                                                                                                 | string |     `[]`      |
| `--deposit_outpoint="…"` | one or more static address deposit outpoints to quote for. Deposit outpoints are not to be used in combination with an amount. Eachadditional outpoint can be added by specifying --deposit_outpoint tx_id:idx | string |     `[]`      |
| `--help` (`-h`)          | show help                                                                                                                                                                                                      | bool   |    `false`    |

### `quote out` subcommand

get a quote for the cost of a loop out swap.

Allows to determine the cost of a swap up front.

Usage:

```bash
$ loop [GLOBAL FLAGS] quote out [COMMAND FLAGS] amt
```

The following flags are supported:

| Name                | Description                                                                                                                                                                                                                                                      | Type | Default value |
|---------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|------|:-------------:|
| `--conf_target="…"` | the number of blocks from the swap initiation height that the on-chain HTLC should be swept within in a Loop Out                                                                                                                                                 | uint |      `9`      |
| `--fast`            | Indicate you want to swap immediately, paying potentially a higher fee. If not set the swap server might choose to wait up to 30 minutes before publishing the swap HTLC on-chain, to save on chain fees. Not setting this flag might result in a lower swap fee | bool |    `false`    |
| `--verbose` (`-v`)  | show expanded details                                                                                                                                                                                                                                            | bool |    `false`    |
| `--help` (`-h`)     | show help                                                                                                                                                                                                                                                        | bool |    `false`    |

### `listauth` command

list all L402 tokens.

Shows a list of all L402 tokens that loopd has paid for.

Usage:

```bash
$ loop [GLOBAL FLAGS] listauth [COMMAND FLAGS] [ARGUMENTS...]
```

The following flags are supported:

| Name            | Description | Type | Default value |
|-----------------|-------------|------|:-------------:|
| `--help` (`-h`) | show help   | bool |    `false`    |

### `fetchl402` command

fetches a new L402 authentication token from the server.

Fetches a new L402 authentication token from the server. This token is required to listen to notifications from the server, such as reservation notifications. If a L402 is already present in the store, this command is a no-op.

Usage:

```bash
$ loop [GLOBAL FLAGS] fetchl402 [COMMAND FLAGS] [ARGUMENTS...]
```

The following flags are supported:

| Name            | Description | Type | Default value |
|-----------------|-------------|------|:-------------:|
| `--help` (`-h`) | show help   | bool |    `false`    |

### `listswaps` command

list all swaps in the local database.

Allows the user to get a list of all swaps that are currently stored in the database.

Usage:

```bash
$ loop [GLOBAL FLAGS] listswaps [COMMAND FLAGS] [ARGUMENTS...]
```

The following flags are supported:

| Name                  | Description                                                                                                             | Type   | Default value |
|-----------------------|-------------------------------------------------------------------------------------------------------------------------|--------|:-------------:|
| `--loop_out_only`     | only list swaps that are loop out swaps                                                                                 | bool   |    `false`    |
| `--loop_in_only`      | only list swaps that are loop in swaps                                                                                  | bool   |    `false`    |
| `--pending_only`      | only list pending swaps                                                                                                 | bool   |    `false`    |
| `--label="…"`         | an optional label for this swap,limited to 500 characters. The label may not start with our reserved prefix: [reserved] | string |
| `--channel="…"`       | the comma-separated list of short channel IDs of the channels to loop out                                               | string |
| `--last_hop="…"`      | the pubkey of the last hop to use for this swap                                                                         | string |
| `--max_swaps="…"`     | Max number of swaps to return after filtering                                                                           | uint   |      `0`      |
| `--start_time_ns="…"` | Unix timestamp in nanoseconds to select swaps initiated after this time                                                 | int    |      `0`      |
| `--help` (`-h`)       | show help                                                                                                               | bool   |    `false`    |

### `swapinfo` command

show the status of a swap.

Allows the user to get the status of a single swap currently stored in the database.

Usage:

```bash
$ loop [GLOBAL FLAGS] swapinfo [COMMAND FLAGS] id
```

The following flags are supported:

| Name            | Description        | Type | Default value |
|-----------------|--------------------|------|:-------------:|
| `--id="…"`      | the ID of the swap | uint |      `0`      |
| `--help` (`-h`) | show help          | bool |    `false`    |

### `getparams` command

show liquidity manager parameters.

Displays the current set of parameters that are set for the liquidity manager.

Usage:

```bash
$ loop [GLOBAL FLAGS] getparams [COMMAND FLAGS] [ARGUMENTS...]
```

The following flags are supported:

| Name            | Description | Type | Default value |
|-----------------|-------------|------|:-------------:|
| `--help` (`-h`) | show help   | bool |    `false`    |

### `setrule` command

set liquidity manager rule for a channel/peer.

Update or remove the liquidity rule for a channel/peer.

Usage:

```bash
$ loop [GLOBAL FLAGS] setrule [COMMAND FLAGS] {shortchanid | peerpubkey}
```

The following flags are supported:

| Name                       | Description                                                                                                            | Type   | Default value |
|----------------------------|------------------------------------------------------------------------------------------------------------------------|--------|:-------------:|
| `--type="…"`               | the type of swap to perform, set to 'out' for acquiring inbound liquidity or 'in' for acquiring outbound liquidity     | string |     `out`     |
| `--incoming_threshold="…"` | the minimum percentage of incoming liquidity to total capacity beneath which to recommend loop out to acquire incoming | int    |      `0`      |
| `--outgoing_threshold="…"` | the minimum percentage of outbound liquidity that we do not want to drop below                                         | int    |      `0`      |
| `--clear`                  | remove the rule currently set for the channel/peer                                                                     | bool   |    `false`    |
| `--help` (`-h`)            | show help                                                                                                              | bool   |    `false`    |

### `suggestswaps` command

show a list of suggested swaps.

Displays a list of suggested swaps that aim to obtain the liquidity balance as specified by the rules set in the liquidity manager.

Usage:

```bash
$ loop [GLOBAL FLAGS] suggestswaps [COMMAND FLAGS] [ARGUMENTS...]
```

The following flags are supported:

| Name            | Description | Type | Default value |
|-----------------|-------------|------|:-------------:|
| `--help` (`-h`) | show help   | bool |    `false`    |

### `setparams` command

update the parameters set for the liquidity manager.

Updates the parameters set for the liquidity manager. Note the parameters are persisted in db to save the trouble of setting them again upon loopd restart. To get the defaultvalues, use `getparams` before any `setparams`.

Usage:

```bash
$ loop [GLOBAL FLAGS] setparams [COMMAND FLAGS] [ARGUMENTS...]
```

The following flags are supported:

| Name                            | Description                                                                                                                                                                                                                                                                                  | Type     | Default value |
|---------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|:-------------:|
| `--sweeplimit="…"`              | the limit placed on our estimated sweep fee in sat/vByte                                                                                                                                                                                                                                     | int      |      `0`      |
| `--feepercent="…"`              | the maximum percentage of swap amount to be used across all fee categories                                                                                                                                                                                                                   | float    |      `0`      |
| `--maxswapfee="…"`              | the maximum percentage of swap volume we are willing to pay in server fees                                                                                                                                                                                                                   | float    |      `0`      |
| `--maxroutingfee="…"`           | the maximum percentage of off-chain payment volume that we are willing to pay in routingfees                                                                                                                                                                                                 | float    |      `0`      |
| `--maxprepayfee="…"`            | the maximum percentage of off-chain prepay volume that we are willing to pay in routing fees                                                                                                                                                                                                 | float    |      `0`      |
| `--maxprepay="…"`               | the maximum no-show (prepay) in satoshis that swap suggestions should be limited to                                                                                                                                                                                                          | uint     |      `0`      |
| `--maxminer="…"`                | the maximum miner fee in satoshis that swap suggestions should be limited to                                                                                                                                                                                                                 | uint     |      `0`      |
| `--sweepconf="…"`               | the number of blocks from htlc height that swap suggestion sweeps should target, used to estimate max miner fee                                                                                                                                                                              | int      |      `0`      |
| `--failurebackoff="…"`          | the amount of time, in seconds, that should pass before a channel that previously had a failed swap will be included in suggestions                                                                                                                                                          | uint     |      `0`      |
| `--autoloop`                    | set to true to enable automated dispatch of swaps, limited to the budget set by autobudget                                                                                                                                                                                                   | bool     |    `false`    |
| `--destaddr="…"`                | custom address to be used as destination for autoloop loop out, set to "default" in order to revert to default behavior                                                                                                                                                                      | string   |
| `--account="…"`                 | the name of the account to generate a new address from. You can list the names of valid accounts in your backing lnd instance with "lncli wallet accounts list"                                                                                                                              | string   |
| `--account_addr_type="…"`       | the address type of the extended public key specified in account. Currently only pay-to-taproot-pubkey(p2tr) is supported                                                                                                                                                                    | string   |    `p2tr`     |
| `--autobudget="…"`              | the maximum amount of fees in satoshis that automatically dispatched loop out swaps may spend                                                                                                                                                                                                | uint     |      `0`      |
| `--autobudgetrefreshperiod="…"` | the time period over which the automated loop budget is refreshed                                                                                                                                                                                                                            | duration |     `0s`      |
| `--autoinflight="…"`            | the maximum number of automatically dispatched swaps that we allow to be in flight                                                                                                                                                                                                           | uint     |      `0`      |
| `--minamt="…"`                  | the minimum amount in satoshis that the autoloop client will dispatch per-swap                                                                                                                                                                                                               | uint     |      `0`      |
| `--maxamt="…"`                  | the maximum amount in satoshis that the autoloop client will dispatch per-swap                                                                                                                                                                                                               | uint     |      `0`      |
| `--htlc_conf="…"`               | the confirmation target for loop in on-chain htlcs                                                                                                                                                                                                                                           | int      |      `0`      |
| `--easyautoloop`                | set to true to enable easy autoloop, which will automatically dispatch swaps in order to meet the target local balance                                                                                                                                                                       | bool     |    `false`    |
| `--localbalancesat="…"`         | the target size of total local balance in satoshis, used by easy autoloop                                                                                                                                                                                                                    | uint     |      `0`      |
| `--asset_easyautoloop`          | set to true to enable asset easy autoloop, which will automatically dispatch asset swaps in order to meet the target local balance                                                                                                                                                           | bool     |    `false`    |
| `--asset_id="…"`                | If set to a valid asset ID, the easyautoloop and localbalancesat flags will be set for the specified asset                                                                                                                                                                                   | string   |
| `--asset_localbalance="…"`      | the target size of total local balance in asset units, used by asset easy autoloop                                                                                                                                                                                                           | uint     |      `0`      |
| `--fast`                        | if set new swaps are expected to be published immediately, paying a potentially higher fee. If not set the swap server might choose to wait up to 30 minutes before publishing swap HTLCs on-chain, to save on chain fees. Not setting this flag therefore might result in a lower swap fees | bool     |    `false`    |
| `--help` (`-h`)                 | show help                                                                                                                                                                                                                                                                                    | bool     |    `false`    |

### `getinfo` command

show general information about the loop daemon.

Displays general information about the daemon like current version, connection parameters and basic swap information.

Usage:

```bash
$ loop [GLOBAL FLAGS] getinfo [COMMAND FLAGS] [ARGUMENTS...]
```

The following flags are supported:

| Name            | Description | Type | Default value |
|-----------------|-------------|------|:-------------:|
| `--help` (`-h`) | show help   | bool |    `false`    |

### `abandonswap` command

abandon a swap with a given swap hash.

This command overrides the database and abandons a swap with a given swap hash.  !!! This command might potentially lead to loss of funds if it is applied to swaps that are still waiting for pending user funds. Before executing this command make sure that no funds are locked by the swap.

Usage:

```bash
$ loop [GLOBAL FLAGS] abandonswap [COMMAND FLAGS] ID
```

The following flags are supported:

| Name                       | Description                                                                                                        | Type | Default value |
|----------------------------|--------------------------------------------------------------------------------------------------------------------|------|:-------------:|
| `--i_know_what_i_am_doing` | Specify this flag if you made sure that you read and understood the following consequence of applying this command | bool |    `false`    |
| `--help` (`-h`)            | show help                                                                                                          | bool |    `false`    |

### `reservations` command (aliases: `r`)

manage reservations.

With loopd running, you can use this command to manage your 		reservations. Reservations are 2-of-2 multisig utxos that 		the loop server can open to clients. The reservations are used 		to enable instant swaps.

Usage:

```bash
$ loop [GLOBAL FLAGS] reservations [COMMAND FLAGS] [ARGUMENTS...]
```

The following flags are supported:

| Name            | Description | Type | Default value |
|-----------------|-------------|------|:-------------:|
| `--help` (`-h`) | show help   | bool |    `false`    |

### `reservations list` subcommand (aliases: `l`)

list all reservations.

List all reservations.

Usage:

```bash
$ loop [GLOBAL FLAGS] reservations list [COMMAND FLAGS] [ARGUMENTS...]
```

The following flags are supported:

| Name            | Description | Type | Default value |
|-----------------|-------------|------|:-------------:|
| `--help` (`-h`) | show help   | bool |    `false`    |

### `instantout` command

perform an instant off-chain to on-chain swap (looping out).

Attempts to instantly loop out into the backing lnd's wallet. The amount 	will be chosen via the cli.

Usage:

```bash
$ loop [GLOBAL FLAGS] instantout [COMMAND FLAGS] [ARGUMENTS...]
```

The following flags are supported:

| Name            | Description                                                                                                      | Type   | Default value |
|-----------------|------------------------------------------------------------------------------------------------------------------|--------|:-------------:|
| `--channel="…"` | the comma-separated list of short channel IDs of the channels to loop out                                        | string |
| `--addr="…"`    | the optional address that the looped out funds should be sent to, if let blank the funds will go to lnd's wallet | string |
| `--help` (`-h`) | show help                                                                                                        | bool   |    `false`    |

### `listinstantouts` command

list all instant out swaps.

List all instant out swaps.

Usage:

```bash
$ loop [GLOBAL FLAGS] listinstantouts [COMMAND FLAGS] [ARGUMENTS...]
```

The following flags are supported:

| Name            | Description | Type | Default value |
|-----------------|-------------|------|:-------------:|
| `--help` (`-h`) | show help   | bool |    `false`    |

### `static` command (aliases: `s`)

perform on-chain to off-chain swaps using static addresses.

Usage:

```bash
$ loop [GLOBAL FLAGS] static [COMMAND FLAGS] [ARGUMENTS...]
```

The following flags are supported:

| Name            | Description | Type | Default value |
|-----------------|-------------|------|:-------------:|
| `--help` (`-h`) | show help   | bool |    `false`    |

### `static new` subcommand (aliases: `n`)

Create a new static loop in address.

Requests a new static loop in address from the server. Funds that are 	sent to this address will be locked by a 2:2 multisig between us and the 	loop server, or a timeout path that we can sweep once it opens up. The  	funds can either be cooperatively spent with a signature from the server 	or looped in.

Usage:

```bash
$ loop [GLOBAL FLAGS] static new [COMMAND FLAGS] [ARGUMENTS...]
```

The following flags are supported:

| Name            | Description | Type | Default value |
|-----------------|-------------|------|:-------------:|
| `--help` (`-h`) | show help   | bool |    `false`    |

### `static listunspent` subcommand (aliases: `l`)

List unspent static address outputs.

List all unspent static address outputs.

Usage:

```bash
$ loop [GLOBAL FLAGS] static listunspent [COMMAND FLAGS] [ARGUMENTS...]
```

The following flags are supported:

| Name              | Description                                                            | Type | Default value |
|-------------------|------------------------------------------------------------------------|------|:-------------:|
| `--min_confs="…"` | The minimum amount of confirmations an output should have to be listed | int  |      `0`      |
| `--max_confs="…"` | The maximum number of confirmations an output could have to be listed  | int  |      `0`      |
| `--help` (`-h`)   | show help                                                              | bool |    `false`    |

### `static listdeposits` subcommand

Displays static address deposits. A filter can be applied to only show deposits in a specific state.

Usage:

```bash
$ loop [GLOBAL FLAGS] static listdeposits [COMMAND FLAGS] [ARGUMENTS...]
```

The following flags are supported:

| Name            | Description                                                                                                                                                                                                                                                                                                    | Type   | Default value |
|-----------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------|:-------------:|
| `--filter="…"`  | specify a filter to only display deposits in the specified state. Leaving out the filter returns all deposits. The state can be one of the following:  deposited withdrawing withdrawn looping_in looped_in publish_expired_deposit sweep_htlc_timeout htlc_timeout_swept wait_for_expiry_sweep expired failed | string |
| `--help` (`-h`) | show help                                                                                                                                                                                                                                                                                                      | bool   |    `false`    |

### `static listwithdrawals` subcommand

Display a summary of past withdrawals.

Usage:

```bash
$ loop [GLOBAL FLAGS] static listwithdrawals [COMMAND FLAGS] [ARGUMENTS...]
```

The following flags are supported:

| Name            | Description | Type | Default value |
|-----------------|-------------|------|:-------------:|
| `--help` (`-h`) | show help   | bool |    `false`    |

### `static listswaps` subcommand

Shows a list of finalized static address swaps.

Usage:

```bash
$ loop [GLOBAL FLAGS] static listswaps [COMMAND FLAGS] [ARGUMENTS...]
```

The following flags are supported:

| Name            | Description | Type | Default value |
|-----------------|-------------|------|:-------------:|
| `--help` (`-h`) | show help   | bool |    `false`    |

### `static withdraw` subcommand (aliases: `w`)

Withdraw from static address deposits.

Withdraws from all or selected static address deposits by sweeping them 	to the internal wallet or an external address.

Usage:

```bash
$ loop [GLOBAL FLAGS] static withdraw [COMMAND FLAGS] [ARGUMENTS...]
```

The following flags are supported:

| Name                  | Description                                                                                                               | Type   | Default value |
|-----------------------|---------------------------------------------------------------------------------------------------------------------------|--------|:-------------:|
| `--utxo="…"`          | specify utxos as outpoints(tx:idx) which willbe withdrawn                                                                 | string |     `[]`      |
| `--all`               | withdraws all static address deposits                                                                                     | bool   |    `false`    |
| `--dest_addr="…"`     | the optional address that the withdrawn funds should be sent to, if let blank the funds will go to lnd's wallet           | string |
| `--sat_per_vbyte="…"` | (optional) a manual fee expressed in sat/vbyte that should be used when crafting the transaction                          | uint   |      `0`      |
| `--amount="…"`        | the number of satoshis that should be withdrawn from the selected deposits. The change is sent back to the static address | uint   |      `0`      |
| `--help` (`-h`)       | show help                                                                                                                 | bool   |    `false`    |

### `static summary` subcommand (aliases: `s`)

Display a summary of static address related information.

Displays various static address related information about deposits,  	withdrawals and swaps.

Usage:

```bash
$ loop [GLOBAL FLAGS] static summary [COMMAND FLAGS] [ARGUMENTS...]
```

The following flags are supported:

| Name            | Description | Type | Default value |
|-----------------|-------------|------|:-------------:|
| `--help` (`-h`) | show help   | bool |    `false`    |

### `static in` subcommand

Loop in funds from static address deposits.

Requests a loop-in swap based on static address deposits. After the 	creation of a static address funds can be sent to it. Once the funds are 	confirmed on-chain they can be swapped instantaneously. If deposited 	funds are not needed they can we withdrawn back to the local lnd wallet.

Usage:

```bash
$ loop [GLOBAL FLAGS] static in [COMMAND FLAGS] [amt] [--all | --utxo xxx:xx]
```

The following flags are supported:

| Name                    | Description                                                                                                                                                             | Type     | Default value |
|-------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------|----------|:-------------:|
| `--utxo="…"`            | specify the utxos of deposits as outpoints(tx:idx) that should be looped in                                                                                             | string   |     `[]`      |
| `--all`                 | loop in all static address deposits                                                                                                                                     | bool     |    `false`    |
| `--payment_timeout="…"` | the maximum time in seconds that the server is allowed to take for the swap payment. The client can retry the swap with adjusted parameters after the payment timed out | duration |     `0s`      |
| `--amount="…"`          | the number of satoshis that should be swapped from the selected deposits. If thereis change it is sent back to the static address                                       | uint     |      `0`      |
| `--fast`                | Usage: complete the swap faster by paying a higher fee, so the change output is available sooner                                                                        | bool     |    `false`    |
| `--last_hop="…"`        | the pubkey of the last hop to use for this swap                                                                                                                         | string   |
| `--label="…"`           | an optional label for this swap,limited to 500 characters. The label may not start with our reserved prefix: [reserved]                                                 | string   |
| `--route_hints="…"`     | route hints that can each be individually used to assist in reaching the invoice's destination                                                                          | string   |     `[]`      |
| `--private`             | generates and passes routehints. Should be used if the connected node is only reachable via private channels                                                            | bool     |    `false`    |
| `--force`               | Assumes yes during confirmation. Using this option will result in an immediate swap                                                                                     | bool     |    `false`    |
| `--verbose` (`-v`)      | show expanded details                                                                                                                                                   | bool     |    `false`    |
| `--help` (`-h`)         | show help                                                                                                                                                               | bool     |    `false`    |

