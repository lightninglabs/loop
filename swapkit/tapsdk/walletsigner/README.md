# Wallet signing

`walletsigner` signs Loop's SDK anchor packages with keys held by LND. It takes
authenticated `signrpc.SignerClient` and `walletrpc.WalletKitClient` connections;
the application never reads a private signing key. The package validates SDK
signing requests, but its caller remains responsible for authorizing the offer,
inputs, recipients, fees, and settlement conditions.

The signer RPCs and fields used here are present in LND v0.18.4-beta, Loop's
existing minimum: `PregeneratedLocalNonce`, `MUSIG2_VERSION_V100RC2`, Taproot
script/key spend methods and prevouts, `DeriveKey`, `SignPsbt`, and
`FinalizePsbt`. These APIs do not require raising that minimum. This is an API
compatibility check, not end-to-end validation of an old asset stack. Runtime
tests use the LND and tapd versions pinned by the SDK module and integration
harness. Session responses and signatures are verified before use; a peer
that ignores the requested nonce or tweak cannot produce an accepted result.

Open one `Signer` per journal with `Open(path, signerRPC, walletRPC)`. The journal
must be a private mode-0600 file. Keep its directory private too. A live process
holds an exclusive file lock; stop and join callers before `Close`.

For a cooperative input:

1. Call `Nonce(ctx, roundID, locator, package, requestID)`. Use a stable nonzero
   round ID for this operation. The method verifies the package through SDK
   `SigningRequests`, derives the wallet's public key, and stores the complete
   package, request ID, locator, public key, and generated nonce before returning
   the public nonce. LND sorts participants, so the SDK participant list must be
   sorted in the same compressed-key order.
2. Exchange public nonces. Call `Sign(ctx, roundID, peers)` with one `PeerNonce`
   per other participant in SDK order. The first call durably fixes this peer
   list before creating an LND session. Later calls reject substitutions.
3. Store or deliver the returned public nonce and partial. `Sign` verifies LND's
   returned nonce, aggregate keys, and partial, then flushes the partial before
   returning it. The active record no longer needs the secret nonce afterward;
   Bolt's old pages are not a secure-erasure mechanism.
4. The combining party calls `Combine(ctx, roundID, peerPartials)`. It verifies
   every partial against the recorded public nonces, combines them using public
   data, verifies the final signature, and returns SDK-ready signature bytes.
   This step needs no live LND session. Apply them with SDK
   `ApplyKeyPathSignature` and verify the completed package before publication.

An LND session ID is deliberately not the durable identity. After an ambiguous
signing response or process restart, the signer recreates a session with LND's
`PregeneratedLocalNonce` field and the exact committed message, participants,
Taproot tweak, and peer nonces. Repeating this identical signing operation is
safe; the journal prevents using its nonce with another operation. Once a
partial is persisted, retries return those bytes without calling LND.

The journal contains secret nonces before signing completes. Protect it like
the wallet. Never roll it back, fork it into competing processes, or restore an
older backup while its wallet key remains active: erasing a peer binding can
allow nonce reuse against a different message or aggregate nonce. It contains
neither wallet private keys nor public API methods to export secret nonces.

`ScriptPath` signs SDK-derived recovery requests using an explicitly selected
wallet locator, then verifies the returned Schnorr signature.
`AnchorSigner` adapts wallet `SignPsbt`/`FinalizePsbt` for SDK-managed Bitcoin
inputs. The transaction driver must still verify the final SDK package and
persist publication intent; these helpers do not authorize a spend themselves.

Tests exercise LND's real MuSig session manager, public SDK package validation,
both Taproot tweak modes, explicit sighash encoding, concurrent conflicting
requests, invalid wallet responses, lost signing replies, and journal reopen
after both application and wallet state are discarded.
