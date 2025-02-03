-- prepay_invoice is a field that will store the invoice of the prepay payment
-- that pays for the reservation.
ALTER TABLE reservations ADD COLUMN prepay_invoice TEXT NOT NULL DEFAULT '';
