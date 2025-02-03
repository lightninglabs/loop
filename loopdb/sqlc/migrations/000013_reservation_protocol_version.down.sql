-- protocol_version is used to determine the version of the reservation protocol
-- that was used to create the reservation.
ALTER TABLE reservations DROP COLUMN protocol_Version;
