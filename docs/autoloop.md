# Autoloop
The loop client contains functionality to dispatch loop out swaps automatically,
according to a set of rules configured for your node's channels, within a 
budget of your choosing. 

The autoloop functionality is disabled by default, and can be enabled using the 
following command:
```
loop setparams --autoloop=true
```

Swaps that are dispatched by the autolooper can be identified in the output of 
`ListSwaps` by their label field, which will contain: `[reserved]: autoloop-out`.

Even if you do not choose to enable the autolooper, we encourage you to 
experiment with setting the parameters described in this document because the 
client will log the actions that it would have taken to provide visibility into 
its functionality. Alternatively, the `SuggestSwaps` rpc (`loop suggestswaps` 
on the CLI) provides a set of swaps that the autolooper currently recommends, 
which you can use to manually execute swaps if you'd like.

Note that autoloop parameters and rules are not persisted, so must be set on 
restart. We recommend running loopd with `--debuglevel=debug` when using this 
feature.

### Liquidity Targets
Autoloop can be configured to manage liquidity for individual channels, or for
a peer as a whole. Peer-level liquidity management will examine the liquidity 
balance of all the channels you have with a peer. This differs from channel-level
liquidity, where each channel's individual balance is checked. Note that if you
set a liquidity rule for a peer, you cannot also set a specific rule for one of
its channels.

### Liquidity Thresholds 
To setup the autolooper to dispatch swaps on your behalf, you need to set the 
liquidity balance you would like for each channel or peer. Desired liquidity 
balance is expressed using threshold incoming and outgoing percentages of 
capacity. The incoming threshold you specify indicates the minimum percentage 
of your capacity that you would like in incoming capacity. The outgoing 
threshold allows you to reserve a percentage of your balance for outgoing 
capacity, but may be set to zero if you are only concerned with incoming 
capacity.

The autolooper will perform swaps that push your incoming capacity to at least 
the incoming threshold you specify, while reserving at least the outgoing 
capacity threshold. Rules can be set as follows:

```
loop setrule {short channel id/ peer pubkey} --incoming_threshold={minimum % incoming} --outgoing_threshold={minimum % outgoing}
```

To remove a rule from consideration, its rule can simply be cleared:
```
loop setrule {short channel id/ peer pubkey} --clear
```

## Fees
The amount of fees that an automatically dispatched swap consumes can be limited
to a percentage of the swap amount using the fee percentage parameter:
```
loop setparams --feepercent={percentage of swap amount}
```

If you would like finer grained control over swap fees, there are multiple fee
related settings which can be used to tune the autolooper to your preference.
The sections that follow explain these settings in detail. Note that these fees 
are expressed on a per-swap basis, rather than as an overall budget. 

### On-Chain Fees
When performing a successful loop out swap, the loop client needs to sweep the 
on-chain HTLC sent by the server back into its own wallet. 

#### Sweep Confirmation Target
To estimate the amount of on-chain fees that the swap will require, the client 
uses a confirmation target for the sweep - the number of blocks within which 
you would like this balance swept back to your wallet. The time to acquire your
incoming liquidity is not dependent on sweep confirmation time, so we highly 
recommend setting a very large sweep confirmation target (up to 250 blocks), 
so that your sweep can go through with very low fees. 
```
loop setparams --sweepconf={target in blocks}
```

#### Fee Market Awareness
The mempool often clears overnight, or on the weekends when fewer people are 
using chain space. This is an opportune time for the autolooper to dispatch a 
swap on your behalf while you sleep! Before dispatching a swap, the autolooper 
will get a fee estimate for you on-chain sweep transaction (using its 
`sweepconftarget`), and check it against the limit that has been configured. 
The `sweeplimit` parameter can be set to configure the autolooper to only 
dispatch in low-fee environments.

```
loop setparams --sweeplimit={limit in sat/vbyte}
```


#### Miner Fee
In the event where fees spike dramatically right after a swap is dispatched, 
it may not be worthwhile to proceed with the swap. The loop client always uses 
the latest fee estimation to sweep your swap within the desired target, but to 
account for this edge case where fees dramatically spike for an extended period 
of time, a maximum miner fee can be set to cap the amount that will be paid for 
your sweep. 
```
loop setparams --maxminer={limit in satoshis}
```

### Server Fees
#### Swap Fee
The server charges a fee for facilitating swaps. The autolooper can be limited 
to a set swap fee, expressed as a percentage of the total swap amount, using 
the following command:
```
loop setparams --maxswapfee={percentage of swap volume}
```

#### No-Show Fee
In the case of a no-show, the server will charge a fee to recoup its on-chain 
costs. This value will only be charged if your client goes offline for a long 
period of time after the server has published an on-chain HTLC and never 
completes the swap, or if it decides to abort the swap due to high on-chain 
fees. Both of these cases are unlikely, but this value can still be capped in 
the autolooper. 
```
loop setparams --maxprepay={limit in satoshis}
```

### Off-Chain Fees
The loop client dispatches two off-chain payments to the loop server - one for 
the swap prepayment, and one for the swap itself. The amount that the client 
will pay in off-chain fees for each of these payments can be limited to a 
percentage of the payment amount using the following commands:

Prepayment routing fees:
 ```
 loop setparams --maxprepayfee={percentage of prepay amount}
 ```

Swap routing fees:
 ```
 loop setparams --maxroutingfee={percentage of swap amount}
 ```

## Budget
The autolooper operates within a set budget, and will stop executing swaps when 
this budget is reached. This budget includes the fees paid to the swap server, 
on-chain sweep costs and off-chain routing fees. Note that the budget does not 
include the actual swap amount, as this balance is simply shifted from off-chain 
to on-chain, rather than used up. 

The budget value is expressed in satoshis, and can be set using the `setparams` 
loop command:
```
loop setparams --autobudget={budget in satoshis}
```

Your autoloop budget can optionally be paired with a start time, which 
determines the time from which we will count autoloop swaps as being part of 
the budget. If this value is zero, it will consider all automatically 
dispatched swaps as being part of the budget. 

The start time is expressed as a unix timestamp, and can be set using the 
`setparams` loop command:
```
loop setparams --budgetstart={start time in seconds}
```

If your autolooper has used up its budget, and you would like to top it up, you 
can do so by either increasing the overall budget amount, or by increasing the 
start time to the present. For example, if you want to set your autolooper to 
have a budget of 100k sats for the month, you could set the following:
```
loop setparams --autobudget=100000 --autostart={beginning of month ts}
```

## Dispatch Control
Configuration options are also exposed to allow you to control the rate at 
which swaps are automatically dispatched, and the autolooper's propensity to 
retry channels that have previously failed. 

### In Flight Limit
The number of swaps that the autolooper will dispatch at a time is controlled 
by the `autoinflight` parameter. The default value for this parameter is 1, and 
can be increased if you would like to perform more automated swaps simultaneously. 
If you have set a very high sweep target for your automatically dispatched swaps, 
you may want to increase this value, because the autolooper will wait for the 
swap to fully complete, including the sweep confirming, before it dispatches 
another swap. 

```
loop setparams --autoinflight=2
```

### Failure Backoff
Sometimes loop out swaps fail because they cannot find an off-chain route to the 
server. This may happen because there is a temporary lack of liquidity along the 
route, or because the peer that you need to perform a swap with simply does not 
have a route to the loop server's node. These swap attempts cost you nothing, 
but we set a backoff period so that the autolooper will not continuously attempt 
to perform swaps through a very unbalanced channel that cannot facilitate a swap. 

The default value for this parameter is 24hours, and it can be updated as follows:
```
loop setparams --failurebackoff={backoff in seconds}
```

### Swap Size
By default, the autolooper will execute a swap when the amount that needs to be
rebalanced within a channel is equal to the swap server's minimum swap size. 
This means that it will dispatch swaps more regularly, and ensure that channels 
are not run down too far below their configured threshold. If you are willing 
to allow your liquidity to drop further than the minimum swap amount below your 
threshold, a custom minimum swap size can be set. If autolooper is configured 
with a larger minimum swap size, it will allow channels to drop further below
their target threshold, but will perform fewer swaps, potentially saving on 
fees.

```
loop setparams --minamt={amount in satoshis}
```

Swaps are also limited to the maximum swap amount advertised by the server. If
you would like to reduce the size of swap that autoloop created, this value can 
also be configured. 

```
loop setparams --maxamt={amount in satoshis}
```

The server's current terms are provided by the `loop terms` cli command. The 
values set for minimum and maximum swap amount must be within the range that
the server supports. 

## Manual Swap Interaction
The autolooper will not dispatch swaps over channels that are already included 
in manually dispatched swaps - for loop out, this would mean the channel is 
specified in the outgoing channel swap, and for loop in the channel's peer is 
specified as the last hop for an ongoing swap. This check is put in place to 
prevent the autolooper from interfering with swaps you have created yourself. 

## Disqualified Swaps
There are various restrictions placed on the client's autoloop functionality.
If a channel is not eligible for a swap at present, or it does not need one
based on the current set of liquidity rules, it will be listed in the 
`Disqualified` section of the output of the `SuggestSwaps` API. One of the 
following reasons will be displayed:

* Budget not started: if the start date for your budget is in the future,
  no swaps will be executed until the start date is reached. See [budget](#budget) to
  update.
* Budget elapsed: if the autolooper has elapsed the budget assigned to it for 
  fees, this reason will be returned. See [budget](#budget) to update.
* Sweep fees: this reason will be displayed if the estimated chain fee rate for
  sweeping a loop out swap is higher than the current limit. See [sweep fees](#fee-market-awareness) 
  to update.
* In flight: there is a limit to the number of automatically dispatched swaps
  that the client allows. If this limit has been reached, no further swaps
  will be automatically dispatched until the in-flight swaps complete. See 
  [in flight limit](#in-flight-limit) to update.
* Budget insufficient: if there is not enough remaining budget for a swap, 
  including the amount currently reserved for in flight swaps, an insufficient
  reason will be displayed. This differs from budget elapsed because there is
  still budget remaining, just not enough to execute a specific swap.
* Swap fee: there is a limit placed on the fee that the client will pay to the
  server for automatically dispatched swaps. The swap fee reason will be shown 
  if the fees advertised by the server are too high. See [swap fee](#swap-fee)
  to update.
* Miner fee: if the estimated on-chain fees for a swap are too high, autoloop
  will display a miner fee reason. See [miner fee](#miner-fee) to update. 
* Prepay: if the no-show fee that the server will pay in the unlikely event 
  that the client fails to complete a swap is too high, a prepay reason will
  be returned. See [no show fees](#no-show-fee) to update. 
* Backoff: if an automatically dispatched swap has recently failed for a channel,
  autoloop will backoff for a period before retrying. See [failure backoff](#failure-backoff) 
  to update. 
* Loop out: if there is currently a loop out swap in-flight on a channel, it 
  will not be used for automated swaps. This issue will resolve itself once the 
  in-flight swap completes.
* Loop in: if there is currently a loop in swap in-flight for a peer, it will 
  not be used for automated swaps. This will resolve itself once the swap is 
  completed.
* Liquidity ok: if a channel's current liquidity balance is within the bound set
  by the rule that it applies to, then a liquidity ok reason will be displayed
  to indicate that no action is required for that channel.
* Fee insufficient: if the fees that a swap will cost are more than the
  percentage of total swap amount that we allow, this reason will be displayed.
  See [fees](#fees) to update this value.


Further details for all of these reasons can be found in loopd's debug level 
logs.
