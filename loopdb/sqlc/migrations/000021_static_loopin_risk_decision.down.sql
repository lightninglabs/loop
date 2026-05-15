-- Drop confirmation-risk decision fields from static address loop-ins.
ALTER TABLE static_address_swaps DROP COLUMN confirmation_risk_decision;
ALTER TABLE static_address_swaps DROP COLUMN confirmation_risk_decision_time;
