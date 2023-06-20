-- name: UpsertLiquidityParams :exec
INSERT INTO liquidity_params (
    id, params
) VALUES (
    1, $1
) ON CONFLICT (id) DO UPDATE SET
    params = excluded.params;

-- name: FetchLiquidityParams :one
SELECT params FROM liquidity_params WHERE id = 1;