-- protocol_version is used to determine the version of the reservation protocol
-- that was used to create the reservation.
ALTER TABLE reservations ADD COLUMN protocol_Version INTEGER NOT NULL DEFAULT 0;