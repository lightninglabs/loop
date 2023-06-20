-- liquidity_params stores the liquidity parameters for autoloop as a single row
-- with a blob column, which is the serialized proto request.
CREATE TABLE liquidity_params (
    id INTEGER PRIMARY KEY,
    params BLOB
);