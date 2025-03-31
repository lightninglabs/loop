#!/bin/bash

# The absolute directory this file is located in.
COMPOSE="docker-compose -p regtest"

function bitcoin() {
  docker exec -ti bitcoind bitcoin-cli -regtest "$@"
}

function lndserver() {
  docker exec -ti lndserver lncli --network regtest "$@"
}

function lndclient() {
  docker exec -ti lndclient lncli --network regtest "$@"
}

function loop() {
  docker exec -ti loopclient loop --network regtest "$@"
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
  echo "Copying loopserver files"
  copy_loopserver_files

  echo "Creating wallet"
  bitcoin createwallet miner

  ADDR_BTC=$(bitcoin getnewaddress "" legacy)
  echo "Generating blocks to $ADDR_BTC"
  bitcoin generatetoaddress 106 "$ADDR_BTC" > /dev/null

  echo "Getting pubkeys"
  LNDSERVER=$(lndserver getinfo | jq .identity_pubkey -r)
  LNDCLIENT=$(lndclient getinfo | jq .identity_pubkey -r)
  echo "Getting addresses"
  
  
  echo "Sending funds"
  ADDR_SERVER=$(lndserver newaddress p2wkh | jq .address -r)
  ADDR_CLIENT=$(lndclient newaddress p2wkh | jq .address -r)
  bitcoin sendtoaddress "$ADDR_SERVER" 5
  bitcoin sendtoaddress "$ADDR_CLIENT" 5
  mine 6

  sleep 30
  
  lndserver openchannel --node_key $LNDCLIENT --connect lndclient:9735 --local_amt 16000000
  mine 6
  
  sleep 10

  lndclient openchannel --node_key $LNDSERVER --local_amt 16000000
  mine 6

  docker cp aperture:/root/.aperture/tls.cert /tmp/aperture-tls.cert
  chmod 644 /tmp/aperture-tls.cert
  docker cp -a /tmp/aperture-tls.cert loopclient:/root/.loop/aperture-tls.cert

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

function copy_loopserver_files() {
  # copy cert to loopserver
  docker cp lndserver:/root/.lnd/tls.cert /tmp/loopserver-tls.cert
  chmod 644 /tmp/loopserver-tls.cert
  docker cp -a /tmp/loopserver-tls.cert loopserver:/home/loopserver/tls.cert
  
  #copy readonly macaroon to loopserver
  docker cp lndserver:/root/.lnd/data/chain/bitcoin/regtest/readonly.macaroon /tmp/loopserver-read.macaroon
  chmod 644 /tmp/loopserver-read.macaroon
  docker cp -a /tmp/loopserver-read.macaroon loopserver:/home/loopserver/readonly.macaroon

  # copy admin macaroon to loopserver
  docker cp lndserver:/root/.lnd/data/chain/bitcoin/regtest/admin.macaroon /tmp/loopserver-admin.macaroon
  chmod 644 /tmp/loopserver-admin.macaroon
  docker cp -a /tmp/loopserver-admin.macaroon loopserver:/home/loopserver/admin.macaroon

  # copy invoices macaroon to loopserver
  docker cp lndserver:/root/.lnd/data/chain/bitcoin/regtest/invoices.macaroon /tmp/loopserver-invoices.macaroon
  chmod 644 /tmp/loopserver-invoices.macaroon
  docker cp -a /tmp/loopserver-invoices.macaroon loopserver:/home/loopserver/invoices.macaroon

  # copy chainnotifier macaroon to loopserver
  docker cp lndserver:/root/.lnd/data/chain/bitcoin/regtest/chainnotifier.macaroon /tmp/loopserver-chainnotifier.macaroon
  chmod 644 /tmp/loopserver-chainnotifier.macaroon
  docker cp -a /tmp/loopserver-chainnotifier.macaroon loopserver:/home/loopserver/chainnotifier.macaroon

  # copy router macaroon to loopserver
  docker cp lndserver:/root/.lnd/data/chain/bitcoin/regtest/router.macaroon /tmp/loopserver-router.macaroon
  chmod 644 /tmp/loopserver-router.macaroon
  docker cp -a /tmp/loopserver-router.macaroon loopserver:/home/loopserver/router.macaroon

  # copy signer macaroon to loopserver
  docker cp lndserver:/root/.lnd/data/chain/bitcoin/regtest/signer.macaroon /tmp/loopserver-signer.macaroon
  chmod 644 /tmp/loopserver-signer.macaroon
  docker cp -a /tmp/loopserver-signer.macaroon loopserver:/home/loopserver/signer.macaroon

  # copy walletkit macaroon to loopserver
  docker cp lndserver:/root/.lnd/data/chain/bitcoin/regtest/walletkit.macaroon /tmp/loopserver-walletkit.macaroon
  chmod 644 /tmp/loopserver-walletkit.macaroon
  docker cp -a /tmp/loopserver-walletkit.macaroon loopserver:/home/loopserver/walletkit.macaroon

  docker cp loopserver:/home/loopserver/tls.cert /tmp/loopserver-tls.cert
  chmod 644 /tmp/loopserver-tls.cert
  docker cp -a /tmp/loopserver-tls.cert aperture:/root/.aperture/loopserver-tls.cert

  # create the aperture config and copy it to the aperture container.
  write_aperture_config

  docker cp /tmp/aperture.yaml aperture:/root/.aperture/aperture.yaml
}


function write_aperture_config() {
 rm -rf /tmp/aperture.yaml
 touch /tmp/aperture.yaml && cat > /tmp/aperture.yaml <<EOF
listenaddr: '0.0.0.0:11018'
staticroot: '/root/.aperture/static'
servestatic: true
debuglevel: trace
insecure: false
writetimeout: 0s

servername: aperture
autocert: false

authenticator:
 lndhost: lndserver:10009
 tlspath: /root/.lnd/tls.cert
 macdir:  /root/.lnd/data/chain/bitcoin/regtest
 network: regtest

etcd:
 host: 'etcd:2379'
 user:
 password:

services:
 - name: loop
   hostregexp: '^.*$'
   pathregexp: '^/looprpc.*$'
   address: 'loopserver:11009'
   protocol: https
   tlscertpath: /root/.aperture/loopserver-tls.cert
   price: 1000
   authwhitelistpaths:
     - '^/looprpc.SwapServer/LoopOutTerms.*$'
     - '^/looprpc.SwapServer/LoopOutQuote.*$'
     - '^/looprpc.SwapServer/LoopInTerms.*$'
     - '^/looprpc.SwapServer/LoopInQuote.*$'
EOF
}


if [[ $# -lt 1 ]]; then
  echo "Usage: $0 start|stop|restart|info|loop"
fi

CMD=$1
shift
$CMD "$@"
