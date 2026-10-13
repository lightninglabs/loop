-- Keep uint64 asset quantities lossless on both SQLite and PostgreSQL.
ALTER TABLE loopin_swaps ADD COLUMN asset_info BLOB;
