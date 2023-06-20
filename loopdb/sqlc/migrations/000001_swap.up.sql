-- swaps stores all base data that is shared between loop-outs and loop-ins,
-- as well as the updates.
CREATE TABLE swaps (
	-- id is the autoincrementing primary key.
	id INTEGER PRIMARY KEY,

	-- swap_hash is the randomly generated hash of the swap, which is used
	-- as the swap identifier for the clients.
	swap_hash BLOB NOT NULL UNIQUE,

	-- preimage is the preimage for swap htlc.
	preimage BLOB NOT NULL UNIQUE, 

	-- initiation_time is the creation time (when stored) of the contract.
	initiation_time TIMESTAMP NOT NULL,
	
	-- amount_requested is the requested swap amount in sats.
	amount_requested BIGINT NOT NULL,

	-- cltv_expiry defines the on-chain HTLC's CLTV. In specific,
	--  * For loop in swap, this value must be greater than the off-chain
	--    payment's CLTV.
	--  * For loop out swap, this value must be smaller than the off-chain
	--    payment's CLTV.
	cltv_expiry INTEGER NOT NULL,

	-- max_miner_fee is the maximum in on-chain fees that we are willing to
	-- spend.
	max_miner_fee BIGINT NOT NULL,

	-- max_swap_fee is the maximum we are willing to pay the server for the
	-- swap.
	max_swap_fee BIGINT NOT NULL,

	-- initiation_height is the block height at which the swap was initiated.
	initiation_height INTEGER NOT NULL,

	-- protocol_version is the protocol version that the swap was created with.
	-- Note that this version is not upgraded if the client upgrades or
	-- downgrades their protocol version mid-swap.
	protocol_version INTEGER NOT NULL,

	-- label contains an optional label for the swap.
	label TEXT NOT NULL
);


-- swap_updates stores timestamps and states of swap updates.
CREATE TABLE swap_updates (
	-- id is the autoincrementing primary key.
	id INTEGER PRIMARY KEY,

	-- swap_id is the foreign key referencing the swap in the swaps table.
	swap_hash BLOB NOT NULL,

	-- update_timestamp is the timestamp the swap was updated at.
	update_timestamp TIMESTAMP NOT NULL,

	-- update_state is the state the swap was in at a given timestamp.
	update_state INTEGER NOT NULL,

	-- htlc_txhash is the hash of the transaction that creates the htlc.
	htlc_txhash TEXT NOT NULL, 

	-- server_cost is the amount paid to the server.
	server_cost BIGINT NOT NULL DEFAULT 0,
	
	-- onchain_cost is the amount paid to miners for the onchain tx.
	onchain_cost BIGINT NOT NULL DEFAULT 0,

	-- offchain_cost is the amount paid in routing fees.
	offchain_cost BIGINT NOT NULL DEFAULT 0,

	-- Foreign key constraint to ensure that swap_id references an existing swap.
	FOREIGN KEY (swap_hash) REFERENCES swaps (swap_hash)
);


-- loopin_swaps stores the loop-in specific data.
CREATE TABLE loopin_swaps (
	-- swap_hash points to the parent swap hash.
	swap_hash BLOB PRIMARY KEY REFERENCES swaps(swap_hash),

	-- htlc_conf_target specifies the targeted confirmation target for the
	-- sweep transaction.
	htlc_conf_target INTEGER NOT NULL,

	-- last_hop is an optional parameter that specifies the last hop to be
	-- used for a loop in swap.
	last_hop BLOB,

	-- external_htlc specifies whether the htlc is published by an external
	-- source.
	external_htlc BOOLEAN NOT NULL
);

-- loopout_swaps stores the loop-out specific data.
CREATE TABLE loopout_swaps (
	-- swap_hash points to the parent swap hash.
	swap_hash BLOB PRIMARY KEY REFERENCES swaps(swap_hash),

	-- dest_address is the destination address of the loop out swap.
	dest_address TEXT NOT NULL, 

	-- SwapInvoice is the invoice that is to be paid by the client to
	-- initiate the loop out swap.
	swap_invoice TEXT NOT NULL,
	
	-- MaxSwapRoutingFee is the maximum off-chain fee in msat that may be
	-- paid for the swap payment to the server.
	max_swap_routing_fee BIGINT NOT NULL,

	-- SweepConfTarget specifies the targeted confirmation target for the
	-- client sweep tx.
	sweep_conf_target INTEGER NOT NULL,

	-- HtlcConfirmations is the number of confirmations we require the on
	-- chain htlc to have before proceeding with the swap.
	htlc_confirmations INTEGER NOT NULL,

	-- OutgoingChanSet is the set of short ids of channels that may be used.
	-- If empty, any channel may be used.
	outgoing_chan_set TEXT NOT NULL,

	-- PrepayInvoice is the invoice that the client should pay to the
	-- server that will be returned if the swap is complete.
	prepay_invoice TEXT NOT NULL, 
	
	-- MaxPrepayRoutingFee is the maximum off-chain fee in msat that may be
	-- paid for the prepayment to the server.
	max_prepay_routing_fee BIGINT NOT NULL,

	-- SwapPublicationDeadline is a timestamp that the server commits to
	-- have the on-chain swap published by. It is set by the client to
	-- allow the server to delay the publication in exchange for possibly
	-- lower fees.
	publication_deadline TIMESTAMP NOT NULL
);


-- htlc_keys stores public and private keys used when construcing swap HTLCs.
CREATE TABLE htlc_keys (
	-- swap_hash points to the parent swap hash.
	swap_hash BLOB PRIMARY KEY REFERENCES swaps(swap_hash),

	-- sender_script_pubkey is the sender's script pubkey used in the HTLC.
	sender_script_pubkey BLOB NOT NULL,

	-- receiver_script_pubkey is the receivers's script pubkey used in the HTLC.
	receiver_script_pubkey BLOB NOT NULL,

	-- sender_internal_pubkey is the public key for the sender_internal_key.
	sender_internal_pubkey BLOB,

	-- receiver_internal_pubkey is the public key for the receiver_internal_key.
	receiver_internal_pubkey BLOB,

	-- client_key_family is the family of key being identified.
	client_key_family INTEGER NOT NULL,

	-- client_key_index is the precise index of the key being identified.
	client_key_index INTEGER NOT NULL
);


CREATE INDEX IF NOT EXISTS updates_swap_hash_idx ON swap_updates(swap_hash);