-- name: InsertMigration :exec
INSERT INTO migration_tracker (
  migration_id,
  migration_ts
) VALUES ($1, $2);

-- name: GetMigration :one
SELECT
  migration_id,
  migration_ts
FROM
  migration_tracker
WHERE
  migration_id = $1;
