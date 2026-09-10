-- Each field records one purchase fact, not an opaque FSM checkpoint.
ALTER TABLE asset_reservations ADD COLUMN quote BYTEA;
ALTER TABLE asset_reservations ADD COLUMN max_route_fee_msat BIGINT NOT NULL DEFAULT 0
    CHECK (max_route_fee_msat >= 0);
ALTER TABLE asset_reservations ADD COLUMN skip_probe BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE asset_reservations ADD COLUMN main_probe INTEGER NOT NULL DEFAULT 0
    CHECK (main_probe BETWEEN 0 AND 4);
ALTER TABLE asset_reservations ADD COLUMN probes_checked_at TIMESTAMP;
-- Approximate fee scaled from the main probe; ignores fixed hop fees.
ALTER TABLE asset_reservations ADD COLUMN prepay_route_fee_msat BIGINT NOT NULL DEFAULT 0
    CHECK (prepay_route_fee_msat >= 0);
ALTER TABLE asset_reservations ADD COLUMN main_route_fee_msat BIGINT NOT NULL DEFAULT 0
    CHECK (main_route_fee_msat >= 0);
ALTER TABLE asset_reservations ADD COLUMN payment_hash BYTEA
    CHECK (payment_hash IS NULL OR length(payment_hash) = 32);
ALTER TABLE asset_reservations ADD COLUMN paying_node_key BYTEA
    CHECK (paying_node_key IS NULL OR length(paying_node_key) = 33);
ALTER TABLE asset_reservations ADD COLUMN payment_request BYTEA;
ALTER TABLE asset_reservations ADD COLUMN payment_result BYTEA;
ALTER TABLE asset_reservations ADD COLUMN funding_outpoint TEXT;
ALTER TABLE asset_reservations ADD COLUMN confirmation_height BIGINT NOT NULL DEFAULT 0
    CHECK (confirmation_height BETWEEN 0 AND 2147483647);
ALTER TABLE asset_reservations ADD COLUMN prepay_credit BIGINT NOT NULL DEFAULT 0
    CHECK (prepay_credit >= 0);
ALTER TABLE asset_reservations ADD COLUMN deposit_proof BYTEA;

CREATE UNIQUE INDEX asset_reservations_outpoint_idx
    ON asset_reservations(funding_outpoint);
