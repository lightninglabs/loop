ALTER TABLE liquidity_params RENAME TO liquidity_params_assets_backup;

CREATE TABLE liquidity_params (
    id INTEGER PRIMARY KEY,
    params BLOB
);

INSERT INTO liquidity_params (id, params)
SELECT 1, params
FROM liquidity_params_assets_backup
WHERE asset_id = 'btc';

DROP TABLE liquidity_params_assets_backup;
