# Fee Rate API

## Status

Accepted.

## Context

Bitcoin fee rates are normally shown to application users as sat/vB. The SDK
should use that unit at the public API boundary.

Several tapd RPC fields still use sat/kWU. Passing that raw wire unit through
the SDK forces application developers to know daemon-specific details and makes
small fee rates awkward to enter correctly.

Whole-number sat/vB fields are also too coarse. Fee rates such as `1.1 sat/vB`
are common, and callers should not need floating point arithmetic to express
them.

## Decision

The root package owns a `tapsdk.FeeRate` value type for on-chain fee rates.

Callers construct and display `FeeRate` values in sat/vB:

- whole sat/vB through `NewFeeRateSatPerVByte`
- decimal sat/vB through `ParseFeeRateSatPerVByte`
- exact fixed-point values through `NewFeeRateSatPerKVByte`

Internally, `FeeRate` stores sat/kvB. That is equivalent to fixed-point sat/vB
with three decimal places, while avoiding Lightning `msat` terminology for
on-chain fees.

Transport adapters convert to sat/kWU only when calling tapd endpoints that
require that wire unit. This conversion rounds up so the SDK never pays a lower
fee rate than the caller requested.

## Consequences

- Public SDK APIs can standardize on `FeeRate` instead of raw numeric units.
- Fractional sat/vB values are exact and do not require `float64`.
- tapd wire-unit conversions remain transport details.
- Future language SDKs can mirror the same value-type model.
