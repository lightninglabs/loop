-- sweep_tx_hash is the hash of the transaction that sweeps the htlc.
ALTER TABLE swap_updates ADD sweep_tx_hash TEXT NOT NULL;
