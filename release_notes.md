# Loop Client Release Notes

### Developers: 

### Release Manager: 

## Next release

#### New Features
This new feature enables loop out and autoloop out operations to sweep htlcs to addresses generated from an extened public key.
A precondition for using the loop client in this fashion is onboarding a xpub account in the backing lnd instance, e.g.
`lncli wallet accounts import xpub... my_loop_account --address_type p2tr --master_key_fingerprint 0df50`.
Loop outs can then be instructed to sweep to a new derived address from the specified account, e.g:
`loop out --amt 10000000 --account my_loop_account --address_type p2tr`.
To use this functionality with autoloop out one has to set the backing lnd account and address type via liquidity parameters for autoloop, e.g.
`loop --network regtest setparams --autoloop=true --account=my_loop_account --account_addr_type=p2tr ...`

#### Breaking Changes

#### Bug Fixes

#### Maintenance
