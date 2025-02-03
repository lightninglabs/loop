-- name: UpsertLiquidityParams :exec
INSERT INTO liquidity_params (
    asset_id, params
) VALUES (
    $1, $2
) ON CONFLICT (asset_id) DO UPDATE SET
    params = $2;

-- name: FetchLiquidityParams :many
SELECT * FROM liquidity_params;