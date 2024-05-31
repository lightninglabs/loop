CREATE TABLE migration_tracker (
        -- migration_id is the id of the migration.
        migration_id TEXT NOT NULL,

        -- migration_ts is the timestamp at which the migration was run.
        migration_ts TIMESTAMP,

        PRIMARY KEY (migration_id)
);
