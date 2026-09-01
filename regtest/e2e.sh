#!/usr/bin/env bash

# Exercises the source-built regtest server through the real loopd/loop CLI.
# Run regtest.sh start first. This script intentionally mines the confirmations
# needed by each protocol and fails if any client state machine reports failure.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REGTEST="${SCRIPT_DIR}/regtest.sh"
TIMEOUT_SECONDS="${TIMEOUT_SECONDS:-180}"

wait_for() {
  local description="$1"
  shift

  local deadline=$((SECONDS + TIMEOUT_SECONDS))
  while true; do
    local status
    if "$@"; then
      return
    else
      status=$?
    fi

    # State predicates use status 1 for "not there yet" and status 2 for a
    # terminal swap failure. Do not hide a real failure behind the timeout.
    if [ "${status}" -gt 1 ]; then
      return "${status}"
    fi

    if [ "${SECONDS}" -ge "${deadline}" ]; then
      echo "Timed out waiting for ${description}" >&2
      return 1
    fi
    sleep 1
  done
}

mempool_nonempty() {
  [ "$("${REGTEST}" bitcoin getrawmempool | jq 'length')" -gt 0 ]
}

traditional_state_is() {
  local hash="$1"
  local expected="$2"
  local state

  state="$("${REGTEST}" loop swapinfo "${hash}" | jq -r '.state')"
  if [ "${state}" = "FAILED" ]; then
    echo "Swap ${hash} failed" >&2
    "${REGTEST}" loop swapinfo "${hash}" >&2
    return 2
  fi

  [ "${state}" = "${expected}" ]
}

deposit_available() {
  [ "$("${REGTEST}" loop static listdeposits --filter deposited | \
    jq '.filtered_deposits | length')" -gt 0 ]
}

static_state_is() {
  local encoded_hash="$1"
  local expected="$2"
  local state

  state="$("${REGTEST}" loop static listswaps | jq -r \
    --arg hash "${encoded_hash}" \
    '.swaps[] | select(.swap_hash == $hash) | .state')"
  if [ "${state}" = "FAILED_STATIC_ADDRESS_SWAP" ]; then
    echo "Static-address swap ${encoded_hash} failed" >&2
    "${REGTEST}" loop static listswaps >&2
    return 2
  fi

  [ "${state}" = "${expected}" ]
}

echo "Running a real regtest Loop Out"
loop_out_output="$("${REGTEST}" loop out --amt 500000 --fast --force)"
loop_out_hash="$(printf '%s\n' "${loop_out_output}" | \
  awk '/^ID:/ {print $2}')"
test -n "${loop_out_hash}"

wait_for "Loop Out HTLC publication" mempool_nonempty
"${REGTEST}" mine 1
wait_for "Loop Out client sweep" mempool_nonempty
"${REGTEST}" mine 3
wait_for "Loop Out success" traditional_state_is \
  "${loop_out_hash}" SUCCESS

echo "Running a real regtest Loop In"
loop_in_output="$("${REGTEST}" loop in --amt 500000 --force)"
loop_in_hash="$(printf '%s\n' "${loop_in_output}" | \
  awk '/^ID:/ {print $2}')"
test -n "${loop_in_hash}"

wait_for "Loop In HTLC publication" mempool_nonempty
"${REGTEST}" mine 1
wait_for "Loop In server claim" mempool_nonempty
"${REGTEST}" mine 1
wait_for "Loop In success" traditional_state_is \
  "${loop_in_hash}" SUCCESS

echo "Running a real static-address deposit Loop In"
static_output="$(printf 'y\n' | "${REGTEST}" loop static new)"
static_address="$(printf '%s\n' "${static_output}" | sed -n \
  's/.*"address":[[:space:]]*"\([^"]*\)".*/\1/p')"
test -n "${static_address}"

"${REGTEST}" lndclient sendcoins --addr "${static_address}" \
  --amt 500000 --min_confs 0 --force >/dev/null
"${REGTEST}" mine 6
wait_for "static-address deposit discovery" deposit_available

static_swap_output="$("${REGTEST}" loop static in --all --fast --force)"
static_swap_hash="$(printf '%s\n' "${static_swap_output}" | \
  jq -r '.swap_hash')"
test -n "${static_swap_hash}" && test "${static_swap_hash}" != "null"

wait_for "static-address Loop In success" static_state_is \
  "${static_swap_hash}" SUCCEEDED
wait_for "static-address settlement transaction" mempool_nonempty
"${REGTEST}" mine 1

# A fallback HTLC sweep can become valid one block after its parent. If the
# server chose that path, confirm it too. The direct sweepless path is already
# complete and leaves the mempool empty here.
sleep 2
if mempool_nonempty; then
  "${REGTEST}" mine 1
fi

echo "All three real regtest swap flows succeeded"
