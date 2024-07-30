#!/bin/bash

# Define the reg_alice function
reg_alice() {
    docker exec -ti alice lncli --network regtest "$@"
}

# Request a new static loop-in address from the server
NEW_ADDRESS=$(loop --network=regtest s n | grep "Received a new static loop-in address from the server:" | awk '{print $10}')

echo "New static loop-in address: $NEW_ADDRESS"

# Send coins to the new address
AMOUNT=250000
SEND_COINS=$(reg_alice sendcoins --addr "$NEW_ADDRESS" --amt "$AMOUNT" --min_confs 0 --force)

echo "$SEND_COINS"

# Extract the txid
TXID=$(echo "$SEND_COINS" | grep "txid" | awk '{print $2}' | tr -d '",')

echo "Transaction ID: $TXID"

# Mine 3 blocks to confirm the transaction
/usr/local/bin/regtest mine 3

echo "Mined 3 blocks to confirm the transaction."

echo "Script completed successfully."
