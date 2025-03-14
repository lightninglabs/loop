CREATE TABLE IF NOT EXISTS loopout_swaps_asset_info (
        -- swap_hash points to the parent loop out swap hash.
        swap_hash BLOB PRIMARY KEY REFERENCES loopout_swaps(swap_hash),

        -- asset_id is the asset that is used to pay the swap invoice.
        asset_id BYTEA NOT NULL,

        -- swap_rfq_id is the RFQ id that will be used to pay the swap invoice.
        swap_rfq_id BYTEA NOT NULL,

        -- prepay_rfq_id is the RFQ id that will be used to pay the prepay
        -- invoice.
        prepay_rfq_id BYTEA NOT NULL,

        -- asset_amt_paid_swap is the actual asset amt that has been paid for
        -- the swap invoice.
        asset_amt_paid_swap BIGINT NOT NULL DEFAULT 0,

        -- asset_amt_paid_prepay is the actual asset amt that has been paid for
        -- the prepay invoice.
        asset_amt_paid_prepay BIGINT NOT NULL DEFAULT 0
)