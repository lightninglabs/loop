-- confirmation_risk_decision records the server's confirmation-risk decision
-- for a static address loop-in. The empty string means no decision has been
-- received yet.
ALTER TABLE static_address_swaps ADD COLUMN confirmation_risk_decision TEXT NOT NULL DEFAULT '';

-- confirmation_risk_decision_time records when loopd received and persisted
-- the server's decision, so payment deadlines can be reconstructed after
-- restart.
ALTER TABLE static_address_swaps ADD COLUMN confirmation_risk_decision_time TIMESTAMP;
