-- confirmation_risk_decision records the server's confirmation-risk decision
-- for a static address loop-in. Possible values are:
--   - '': no decision has been received yet;
--   - 'accepted': the server accepted waiting for the low-confirmation
--     deposits, which starts or reconstructs the payment deadline;
--   - 'rejected': the server stopped waiting for the low-confirmation deposits
--     before paying the invoice.
-- Once rejected, a later accepted update is ignored.
ALTER TABLE static_address_swaps ADD COLUMN confirmation_risk_decision TEXT NOT NULL DEFAULT '';

-- confirmation_risk_decision_time records when loopd received and persisted
-- the server's decision, so payment deadlines can be reconstructed after
-- restart. Same-decision replays preserve the original timestamp; changing
-- from accepted to rejected updates it.
ALTER TABLE static_address_swaps ADD COLUMN confirmation_risk_decision_time TIMESTAMP;
