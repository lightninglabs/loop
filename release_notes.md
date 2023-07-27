# Loop Client Release Notes
This file tracks release notes for the loop client.

### Developers: 
* When new features are added to the repo, a short description of the feature should be added under the "Next Release" heading.
* This should be done in the same PR as the change so that our release notes stay in sync!

### Release Manager: 
* All of the items under the "Next Release" heading should be included in the release notes.
* As part of the PR that bumps the client version, cut everything below the 'Next Release' heading.
* These notes can either be pasted in a temporary doc, or you can get them from the PR diff once it is merged.
* The notes are just a guideline as to the changes that have been made since the last release, they can be updated.
* Once the version bump PR is merged and tagged, add the release notes to the tag on GitHub.

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
