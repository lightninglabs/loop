# Autoloop
The loop client contains functionality to dispatch loop out swaps automatically,
according to a set of rules configured for your node's channels, within a 
budget of your choosing. 

The autoloop functionality is disabled by default, and can be enabled using the 
following command:
```
loop setparams --autoout=true
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

### Channel Thresholds 
To setup the autolooper to dispatch swaps on your behalf, you need to tell it 
which channels you would like it to perform swaps on, and the liquidity balance 
you would like on each channel. Desired liqudity balance is expressed using 
threshold incoming and outgoing percentages of channel capacity. The incoming 
threshold you specify indicates the minimum percentage of your channel capacity
that you would like in incoming capacity. The outgoing thresold allows you to 
reserve a percentage of your balance for outgoing capacity, but may be set to 
zero if you are only concerned with incoming capcity.

The autolooper will perform swaps that push your incoming channel capacity to 
at least the incoming threshold you specify, while reserving at least the 
outgoing capacity threshold. Rules can be set as follows:

```
loop setrule {short channel id} --incoming_threshold={minimum % incoming} --outgoing_threshold={minimum % outgoing}
```

To remove a channel from consideration, its rule can simply be cleared:
```
loop setrule {short channel id} --clear
```

## Fees
Fee control is one of the most important features of the autolooper, so we expose 
multiple fee related settings which can be used to tune the autolooper to your 
preference. Note that these fees are expressed on a per-swap basis, rather than 
as an overall budget. 

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

## Manual Swap Interaction
The autolooper will not dispatch swaps over channels that are already included 
in manually dispatched swaps - for loop out, this would mean the channel is 
specified in the outgoing channel swap, and for loop in the channel's peer is 
specified as the last hop for an ongoing swap. This check is put in place to 
prevent the autolooper from interfering with swaps you have created yourself. 
If there is an ongoing swap that does not have a restriction placed on it (no 
outgoing channel set, or last hop), then the autolooper will take no action 
until it has resolved, because it does not know how that swap will affect 
liquidity balances. 

