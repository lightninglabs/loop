# Loop Client Release Notes

#### New Features

* `SuggestSwaps` (`loop suggestswaps`) now works as a dry run for easy
  autoloop. When easy autoloop is enabled it returns the Loop Out that easy
  autoloop would make next, or sets the new `easy_autoloop_reason` field to the
  reason that it would not make one. Two reasons were added for it:
  `AUTO_REASON_BELOW_MINIMUM` and `AUTO_REASON_NO_ELIGIBLE_CHANNEL`. Before,
  the call ignored easy autoloop and failed when no rules were set. A budget
  that is not spent yet, but that the worst-case fees held for swaps in flight
  fill, is reported as `AUTO_REASON_BUDGET_INSUFFICIENT` rather than
  `AUTO_REASON_BUDGET_ELAPSED`, since there may be room again once those swaps
  finish.

#### Breaking Changes

#### Bug Fixes

#### Maintenance

* Update the OpenTelemetry Go SDK from v1.45.0 to v1.47.0 in Loop and
  `looprpc`.

#### Contributors (Alphabetical Order)
