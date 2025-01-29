-- Create a new table with the desired schema
CREATE TABLE new_liquidity_params (
    asset_id TEXT NOT NULL PRIMARY KEY,
    params BLOB
);

-- Copy data from the old table to the new table
INSERT INTO new_liquidity_params (asset_id, params)
SELECT 'btc', params FROM liquidity_params;

-- Drop the old table
DROP TABLE liquidity_params;

-- Rename the new table to the old table name
ALTER TABLE new_liquidity_params RENAME TO liquidity_params;