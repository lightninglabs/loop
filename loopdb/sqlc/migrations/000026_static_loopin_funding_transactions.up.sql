-- Preserve unconfirmed funding parents before authorizing the swap so their
-- confirmed replacements can be identified after wallet history is pruned.
ALTER TABLE static_address_swaps ADD funding_txs BLOB;
