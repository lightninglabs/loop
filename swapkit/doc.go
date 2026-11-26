// Package swapkit owns the Bitcoin policies and flow records shared by asset
// Loop In and Loop Out. It uses Kon's static asset Loop In contracts directly;
// swapkit/tapsdk adapts those contracts to the native Tap SDK transfer, signing
// request, proof and recovery APIs. This package itself has no SDK, wallet,
// tapd or lnd dependency.
//
// NewDepositKit constructs the cooperative MuSig2 deposit and sender CSV refund
// used by static Loop In. Its original ClientKey/ServerKey form remains valid;
// callers needing separate cooperative and script keys supply all four roles.
// NewReservationKit constructs a server-funded reservation with an absolute
// RefundHeight and sender CLTV refund. It never reinterprets a CSV delay.
//
// NewSwapKit requires an explicit HTLC policy. SwapHTLCLegacyCSV retains the
// original relative refund; SwapHTLCStaticLoopInV3 and its direction-neutral
// SwapHTLCCLTV alias select the absolute refund. Both retain the receiver's
// signature, 32-byte preimage and one-block CSV on success. Sender and receiver
// follow the on-chain asset direction: client/server for Loop In and
// server/client for Loop Out. Their cooperative internal and script signing
// roles remain separate.
//
// AnchorPlan supplies the cooperative key and Bitcoin policy tree. Spend
// information requires a verified complete asset commitment root, which is a
// sibling of that tree. Before signing any input, compare the returned script
// with the authenticated prevout and apply its sequence and locktime. CLTV
// spends use a non-final sequence with BIP68 disabled. These helpers do not
// establish chain maturity, Lightning deadlines, provenance or authorization
// to expose a preimage or a signature. The calling FSM owns those decisions.
//
// Asset script keys are selected by the SDK adapter, independently of these
// Bitcoin policies. The adapter retains the fixed NUMS OP_TRUE construction
// for existing contracts and must preserve the exact descriptor of any funded
// output throughout signing, publication, proof delivery and recovery.
package swapkit
