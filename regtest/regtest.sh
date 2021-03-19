#!/bin/bash

# The absolute directory this file is located in.
COMPOSE="docker-compose -p regtest"

function bitcoin() {
  docker exec -ti regtest_bitcoind_1 bitcoin-cli -regtest "$@"
}

function lndserver() {
  docker exec -ti regtest_lndserver_1 lncli --network regtest "$@"
}

function lndclient() {
  docker exec -ti regtest_lndclient_1 lncli --network regtest "$@"
}

function loop() {
  docker exec -ti regtest_loopclient_1 loop --network regtest "$@"
}

function start() {
  $COMPOSE up --force-recreate -d
  echo "Waiting for nodes to start"
  waitnodestart
  setup
}

function waitnodestart() {
  while ! lndserver getinfo | grep -q identity_pubkey; do
    sleep 1
  done
  while ! lndclient getinfo | grep -q identity_pubkey; do
    sleep 1
  done
}

function mine() {
  NUMBLOCKS=6
  if [ ! -z "$1" ]
  then
    NUMBLOCKS=$1
  fi
  bitcoin generatetoaddress $NUMBLOCKS $(bitcoin getnewaddress "" legacy) > /dev/null
}

function setup() {
  echo "Getting pubkeys"
  LNDSERVER=$(lndserver getinfo | jq .identity_pubkey -r)
  LNDCLIENT=$(lndclient getinfo | jq .identity_pubkey -r)
  echo "Getting addresses"
  
  echo "Creating wallet"
  bitcoin createwallet miner
  
  ADDR_BTC=$(bitcoin getnewaddress "" legacy)
  echo "Generating blocks to $ADDR_BTC"
  bitcoin generatetoaddress 106 "$ADDR_BTC" > /dev/null
  
  echo "Sending funds"
  ADDR_SERVER=$(lndserver newaddress p2wkh | jq .address -r)
  ADDR_CLIENT=$(lndclient newaddress p2wkh | jq .address -r)
  bitcoin sendtoaddress "$ADDR_SERVER" 5
  bitcoin sendtoaddress "$ADDR_CLIENT" 5
  mine 6
  
  lndserver openchannel --node_key $LNDCLIENT --connect regtest_lndclient_1:9735 --local_amt 16000000
  mine 6
  
  lndclient openchannel --node_key $LNDSERVER --local_amt 16000000
  mine 6
}

function stop() {
  $COMPOSE down --volumes
}

function restart() {
  stop
  start
}

function info() {
  LNDSERVER=$(lndserver getinfo | jq -c '{pubkey: .identity_pubkey, channels: .num_active_channels, peers: .num_peers}')
  LNDCLIENT=$(lndclient getinfo | jq -c '{pubkey: .identity_pubkey, channels: .num_active_channels, peers: .num_peers}')
  echo "lnd server:   $LNDSERVER"
  echo "lnd client:   $LNDCLIENT"
}

if [[ $# -lt 1 ]]; then
  echo "Usage: $0 start|stop|restart|info|loop"
fi

CMD=$1
shift
$CMD "$@"
