#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COMPOSE_FILE="${SCRIPT_DIR}/docker-compose.yml"
START_TIMEOUT_SECONDS="${START_TIMEOUT_SECONDS:-180}"

if docker compose version >/dev/null 2>&1; then
  COMPOSE=(docker compose -p regtest -f "${COMPOSE_FILE}")
else
  COMPOSE=(docker-compose -p regtest -f "${COMPOSE_FILE}")
fi

bitcoin() {
  docker exec -i bitcoind bitcoin-cli -regtest "$@"
}

lndserver() {
  docker exec -i lndserver lncli --network regtest "$@"
}

lndclient() {
  docker exec -i lndclient lncli --network regtest "$@"
}

loop() {
  docker exec -i loopclient loop --network regtest "$@"
}

wait_for() {
  local description="$1"
  shift

  local deadline=$((SECONDS + START_TIMEOUT_SECONDS))
  until "$@" >/dev/null 2>&1; do
    if [ "${SECONDS}" -ge "${deadline}" ]; then
      echo "Timed out waiting for ${description}" >&2
      return 1
    fi
    sleep 1
  done
}

lnd_rpc_ready() {
  local node="$1"
  "${node}" getinfo | jq -e '.identity_pubkey != ""'
}

lnd_synced() {
  local node="$1"
  "${node}" getinfo | jq -e \
    '.identity_pubkey != "" and .synced_to_chain == true'
}

channels_active() {
  local node="$1"
  local expected="$2"

  [ "$("${node}" getinfo | jq -r '.num_active_channels')" \
    -ge "${expected}" ]
}

loop_ready() {
  loop getinfo
}

payment_route_ready() {
  local invoice="$1"
  local decoded destination amount

  decoded="$(lndclient decodepayreq --pay_req "${invoice}")" || return 1
  destination="$(jq -r '.destination' <<<"${decoded}")"
  amount="$(jq -r '.num_satoshis' <<<"${decoded}")"

  [ -n "${destination}" ] && [ "${destination}" != "null" ] && \
    [ "${amount}" -gt 0 ] && \
    lndclient queryroutes "${destination}" "${amount}" | \
      jq -e '.routes | length > 0'
}

mine() {
  local blocks="${1:-6}"
  local address
  address="$(bitcoin getnewaddress "" legacy)"
  bitcoin generatetoaddress "${blocks}" "${address}" >/dev/null
  wait_for_nodes
}

wait_for_rpc() {
  echo "Waiting for both lnd RPC servers"
  wait_for "server lnd RPC" lnd_rpc_ready lndserver
  wait_for "client lnd RPC" lnd_rpc_ready lndclient
}

wait_for_nodes() {
  echo "Waiting for both lnd nodes to synchronize"
  wait_for "server lnd chain sync" lnd_synced lndserver
  wait_for "client lnd chain sync" lnd_synced lndclient
}

wait_for_channels() {
  local expected="${1}"
  echo "Waiting for ${expected} active channel(s) on both nodes"
  wait_for "${expected} server channel(s)" \
    channels_active lndserver "${expected}"
  wait_for "${expected} client channel(s)" \
    channels_active lndclient "${expected}"
}

bootstrap_l402() {
  echo "Fetching the regtest L402 token"

  local before_index fetch_error invoice latest_index
  before_index="$(lndserver listinvoices --max_invoices 1 | \
    jq -r '.last_index_offset // 0')"

  # The current lndclient compatibility PayInvoice helper omits lnd's
  # mandatory payment timeout. The first call still persists the pending
  # challenge, so pay that exact newly-created invoice with lncli and let the
  # interceptor resume it on the second call.
  if fetch_error="$(loop fetchl402 2>&1)"; then
    return
  fi

  latest_index="$(lndserver listinvoices --max_invoices 1 | \
    jq -r '.last_index_offset // 0')"
  if [ "${latest_index}" -le "${before_index}" ]; then
    echo "${fetch_error}" >&2
    echo "Aperture did not create an L402 invoice" >&2
    return 1
  fi

  invoice="$(lndserver listinvoices --max_invoices 1 | \
    jq -r '.invoices[-1].payment_request')"
  if [ -z "${invoice}" ] || [ "${invoice}" = "null" ]; then
    echo "Aperture returned an empty L402 invoice" >&2
    return 1
  fi

  wait_for "a route to the Aperture L402 invoice" \
    payment_route_ready "${invoice}"
  lndclient payinvoice --pay_req "${invoice}" --fee_limit 10 \
    --timeout 60s --force >/dev/null
  loop fetchl402 >/dev/null
}

setup() {
  echo "Creating and funding the regtest topology"
  if ! bitcoin listwallets | jq -e '.[] | select(. == "miner")' >/dev/null; then
    bitcoin createwallet miner >/dev/null
  fi

  local miner_address server_address client_address
  miner_address="$(bitcoin getnewaddress "" legacy)"
  bitcoin generatetoaddress 106 "${miner_address}" >/dev/null
  wait_for_nodes

  server_address="$(lndserver newaddress p2wkh | jq -r '.address')"
  client_address="$(lndclient newaddress p2wkh | jq -r '.address')"
  bitcoin sendtoaddress "${server_address}" 5 >/dev/null
  bitcoin sendtoaddress "${client_address}" 5 >/dev/null
  mine 6

  local server_pubkey client_pubkey
  server_pubkey="$(lndserver getinfo | jq -r '.identity_pubkey')"
  client_pubkey="$(lndclient getinfo | jq -r '.identity_pubkey')"

  lndserver openchannel --node_key "${client_pubkey}" \
    --connect lndclient:9735 --local_amt 16000000 >/dev/null
  mine 6
  wait_for_channels 1

  lndclient openchannel --node_key "${server_pubkey}" \
    --local_amt 16000000 >/dev/null
  mine 6
  wait_for_channels 2

  echo "Waiting for loopd and the source-built regtest server"
  wait_for "loopd and regtest server readiness" loop_ready
  bootstrap_l402
}

start() {
  # The server is intentionally in-memory, so keeping client or Aperture state
  # across a server recreation would leave orphaned swaps and invalid tokens.
  "${COMPOSE[@]}" down --volumes --remove-orphans
  "${COMPOSE[@]}" up --build --force-recreate -d
  wait_for_rpc
  setup
  info
}

stop() {
  "${COMPOSE[@]}" down --volumes --remove-orphans
}

restart() {
  start
}

info() {
  local server_info client_info
  server_info="$(lndserver getinfo | jq -c \
    '{pubkey: .identity_pubkey, channels: .num_active_channels, peers: .num_peers}')"
  client_info="$(lndclient getinfo | jq -c \
    '{pubkey: .identity_pubkey, channels: .num_active_channels, peers: .num_peers}')"
  echo "lnd server:   ${server_info}"
  echo "lnd client:   ${client_info}"
}

logs() {
  if [ "$#" -eq 0 ]; then
    "${COMPOSE[@]}" logs -f loopserver loopclient
  else
    "${COMPOSE[@]}" logs -f "$@"
  fi
}

usage() {
  echo "Usage: $0 start|stop|restart|info|mine|bitcoin|lndserver|lndclient|loop|logs"
}

if [ "$#" -lt 1 ]; then
  usage
  exit 1
fi

command_name="$1"
shift
case "${command_name}" in
  start|stop|restart|info|mine|bitcoin|lndserver|lndclient|loop|logs)
    "${command_name}" "$@"
    ;;
  *)
    usage
    exit 1
    ;;
esac
