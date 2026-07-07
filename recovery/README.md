# Recovery Package

This package implements local recovery for Loop's static-address and L402
state.

## Goal

Recovery is generation-based. In this package, a generation is anchored by:

- one paid L402 token
- the static-address parameters tied to that L402

The current V0 static-address implementation represents a generation locally as
one concrete static address. The backup stores the fields needed to recreate
that concrete address today and also stores the stable receive/change
key-family metadata planned multi-address recovery will scan from later. The
backup itself is not rewritten when later code issues more addresses.

The recovery flow is designed to let a fresh or repaired Loop instance rebuild
that generation after local disk loss, data-directory replacement, or partial
corruption.

Recovery uses a single immutable backup per L402 generation. Once written,
that backup file is never updated in place.

## Backup Model

The daemon writes at most one encrypted backup file for each paid L402 token
ID:

`<loop-data-dir>/L402_backup_<l402-created-at-unix-ns>_<l402-token-id>.enc`

In the normal layout this resolves inside the active network-specific Loop data
directory, for example:

`~/.loop/mainnet/L402_backup_1776159001000000000_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef.enc`

If `loop recover` is called without `--backup_file`, Loop scans the active
network directory for files with this name shape. It decrypts candidates with
the local lnd-derived key, filters them by network, validates the complete
recoverable generation against the filename metadata, and selects the valid
candidate with the latest timestamp in its filename.

## What Is Backed Up

Each encrypted backup stores:

- a backup format version
- the Bitcoin network
- the paid L402 token ID
- the paid L402 token creation time
- the raw paid `l402.token` file
- the static-address protocol version
- the L402-bound server pubkey
- the static-address client pubkey
- the static-address expiry
- the legacy concrete static-address client key family
- the planned multi-address receive key family
- the planned multi-address change key family
- the legacy first height
- the multi-address first height

The static-address fields are written once. The L402-bound server pubkey,
protocol version, expiry, planned multi-address receive/change key families,
Bitcoin network, and multi-address first height are the stable address-space
metadata for future scanning. The stored client pubkey, legacy client key
family, and legacy first height let the current V0 restore path find the
matching wallet child and recreate the one concrete static-address row.

Current V0 backups initialize `legacy_first_height` from the legacy concrete
static-address initiation height and `multi_address_first_height` from the
current block height when the backup is written. They are separate fields so
the future multi-address scan floor is independent from the legacy concrete
address import hint.

The Taproot address string, `pkScript`, and scan lookahead/gap limit are not
backed up. The address and `pkScript` can be derived from the stored key and
script parameters, and the gap limit is restore policy rather than immutable
backup data.

The L402 file is preserved as a raw blob so restore remains compatible with the
Aperture token-store file format.

Deposit FSM state is not serialized into the backup. After restore, the deposit
manager asks lnd for wallet-visible static-address UTXOs and recreates active
deposit state from that view. Historical finalized or spent deposit transitions
are not replayed from the backup.

## Why Root And Legacy Fields Are Both Stored

The server pubkey, protocol version, expiry, planned multi-address
receive/change key families, Bitcoin network, and multi-address first height
define the stable fields future restore code will combine with lnd-derived
client keys and a chain scan. They are not used by the current V0 restore path.

The current V0 restore path recreates the existing concrete address row
directly, so the backup also stores that row's client pubkey, legacy client key
family, and legacy first height. Those fields let restore find the matching
local wallet child and import the concrete address from the right chain height.

## Encryption Model

The file is encrypted with `secretbox` using a symmetric key derived from lnd
via `Signer.DeriveSharedKey`.

The derivation uses:

- a fixed NUMS public key
- the legacy static-address key family
- key index `0`

This ties backup decryption to the same lnd seed that controls the static
address keys without introducing a user-managed recovery password in this
implementation.

Operationally, this means the backup is not standalone. Loop cannot decrypt or
restore it without the backing `lnd` wallet that can derive the same key. A
replacement `lnd` restored from the same seed/key material is sufficient, but an
unrelated `lnd` is not. Keep the encrypted Loop backup together with the
corresponding `lnd` recovery material; the Loop backup file by itself is not
enough to recover static-address access.

## When Backups Are Written

The backup is only written once a complete recoverable generation exists. A
complete recoverable generation requires both of the following to exist locally:

- a paid `l402.token`
- a concrete static address bound to that token

Pending tokens are not backed up.

If a valid immutable backup for the current paid token ID and creation time
already exists, backup creation is a no-op. A corrupt or undecryptable file with
the same token ID in its name does not suppress creation of a valid backup.

## Startup Behavior

Startup is responsible for materializing the current generation before the
backup is written.

On startup `loopd`:

1. creates the recovery service
2. if the install is fresh, attempts to restore the latest selectable backup
   from the active network directory
3. if nothing was restored, asks the static-address manager for the current
   static address
4. if the address does not exist yet, fetches the paid L402, derives the client
   key, requests the static address from the server, imports the tapscript into
   lnd, and stores the static-address row
5. writes the immutable backup for the resulting paid-L402/static-address
   generation

This gives recovery the "one backup per L402" property without later backup
refreshes.

### Existing Users

For existing users that already have a paid L402 and a concrete static address,
the first startup with the upgraded client backfills the missing immutable
backup for the active generation.

### Fresh Installs

For fresh installations, startup first checks whether a selectable immutable
backup exists in the active Loop data directory.

If one is selected and passes full restore validation, Loop restores it instead
of creating a new paid L402 generation.

If no backup is restored, startup materializes the initial paid L402 plus
concrete static address so the backup can be written immediately.

The `loop static new` command is therefore no longer the only creation point.
It returns the current static address and only falls back to on-demand creation
if startup initialization did not complete earlier.

## Restore Flow

`loop recover --backup_file <path>` restores a specific immutable backup. If
`--backup_file` is omitted, Loop uses the same active-directory selection logic
described above and restores the selected backup after full validation.

Restore performs the following steps:

1. derive the local encryption key from lnd
2. resolve the explicit backup path or select a backup from the active network
   directory
3. read, decrypt, and unmarshal the backup file
4. validate the backup version, Bitcoin network, filename metadata, paid-token
   metadata, and required static-address fields
5. reconstruct the concrete static-address parameters and find the matching
   client key in lnd before writing token files
6. restore the paid `l402.token` file if it is absent, or verify that an
   existing file has identical contents
7. import the tapscript into lnd and create or reuse the local concrete
   static-address record
8. if static-address restore fails after token files were written, remove only
   the token files written by this restore attempt
9. trigger best-effort deposit reconciliation

Client-key reconstruction uses the following strategy:

- scan child indexes `0` through `20` in the legacy static-address client key
  family using `DeriveKey`
- accept the child whose derived pubkey matches the backed-up client pubkey

The multi-address scan-and-rebuild flow is not active yet. The immutable backup
already contains the address-space metadata that flow will need.

## Future Multi-Address Generation

The planned multi-address model uses two dedicated client-side key families:

- `swap.StaticMultiAddressKeyFamily` for externally visible static-address
  deposits
- `swap.StaticAddressChangeKeyFamily` for outputs that return value back into
  the static-address address space

The legacy `swap.StaticAddressKeyFamily` remains the V0 concrete static-address
family and the static-address HTLC key family.

The future `static_addresses` table remains a table of concrete derived
addresses. Each row represents one address child and stores:

- the client pubkey
- the server pubkey
- the client key family
- the client key index
- the resulting `pkScript`
- the protocol version
- the address initiation height

The immutable backup does not store every row. Instead it stores the
address-space metadata that allows those rows to be rediscovered by scanning.

For each future receive or change address:

1. the client chooses the appropriate key family
2. the client derives the next pubkey from lnd for that family
3. the client combines that pubkey with the L402-bound server pubkey using the
   static-address MuSig2 construction for the backed-up protocol version
4. the taproot tweak commits to the static-address timeout leaf
5. the resulting taproot output key yields the final P2TR `pkScript`
6. the concrete child row is stored locally in `static_addresses`

The client key used in the MuSig2 aggregate key should also be the client's key
in the timeout path for that concrete multi-address output.

Because the backup is immutable, future restore must regenerate candidate
receive and change children from the backed-up key families and the restored
lnd key material, rescan from the backed-up multi-address first height, and
rebuild local table rows from what is found on chain. The lookahead/gap limit
used during that scan is a restore parameter, not immutable backup data. Restore
must not depend on a mutable "last issued child index" snapshot.

## Server Proof For Multi-Address Inputs

For a future static swap or withdrawal that spends multi-address inputs, the
server-side proof model is:

1. the paid L402 authenticates the request and identifies the generation
2. the L402 selects the fixed generation server pubkey and the fixed
   protocol/expiry parameters
3. for each input, the client sends the concrete client pubkey that was used to
   construct that input's address
4. the server recomputes the timeout leaf for the backed-up protocol version
   and expiry
5. the server recomputes the MuSig2 aggregate key from the concrete client
   pubkey for that input, the server pubkey bound to the L402 generation, and
   the taproot tweak implied by the timeout leaf
6. the server derives the expected taproot output key and the expected P2TR
   `pkScript`
7. the server compares that derived `pkScript` with the prevout `pkScript` of
   the input being authorized

If they match, the input belongs to that L402 generation because the output
commits to the generation's server key and the concrete client pubkey used for
that input.

This proof is about generation membership, not about proving a particular child
index to the server. The immutable backup therefore only needs the stable
address-space metadata, while exact row discovery remains a client-side wallet
and chain-scan problem.

## Operational Limits

Restore in this implementation recreates the V0 one-address model only.

Some practical consequences follow from that:

- restoring an older immutable backup is best done into a fresh Loop data
  directory, or into a directory that already contains the same token and
  static-address row
- only one concrete static address can be recreated directly by this restore
  code
- conflicting local `l402.token` contents or a different existing
  static-address row cause restore to fail rather than overwrite local state
- active deposits are rebuilt best-effort from wallet reconciliation, not by
  replaying every stored deposit transition

## Why The Backup Is Immutable

The multi-address work needs recovery to be based on stable root material, not
on mutable local cursor snapshots.

Using one immutable backup per L402 enforces that discipline:

- the backup must describe a recoverable generation root
- restore must be able to rediscover state from deterministic wallet- and
  chain-derived scanning
- later address issuance must not depend on backup files being rewritten

That discipline keeps later address issuance independent from backup-file
rewrites.

## Package Boundaries

This package owns:

- backup payload definition
- backup encryption and decryption
- immutable backup-file discovery and selection
- paid L402 token-file backup and restore
- V0 static-address key re-derivation and restore orchestration
- static-address metadata fields for future multi-address restore
- post-restore deposit reconciliation orchestration

This package does not own:

- CLI command handling
- gRPC transport
- the static-address server protocol
- the future multi-address scanning implementation
- `loopd` startup wiring
