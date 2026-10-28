// Package sqlstore implements the fsm/v2 checkpoint register for SQLite and
// PostgreSQL through database/sql. Applications install Schema through their
// migrations and supply a database configured for durable commits. SQLite needs
// durable journaling and synchronous=FULL (or stricter); PostgreSQL needs fsync
// and synchronous_commit enabled. This package does not change database settings
// or install itself into loopd.
//
// This adapter atomically stores one machine checkpoint. Business state spanning
// swaps and deposit/reservation ownership needs a Store implementation sharing
// the application's resource transaction, or durable intent and reconciliation.
package sqlstore

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	fsm "github.com/lightninglabs/loop/fsm/v2"
)

// Schema is migration SQL accepted by SQLite and PostgreSQL. BYTEA is the
// PostgreSQL binary type; SQLite accepts the type and stores []byte bindings as
// blobs. IDs must never be deleted and reused: resetting a revision would
// invalidate compare-and-swap's ABA protection.
const Schema = `CREATE TABLE loop_fsm_v2 (
    id TEXT PRIMARY KEY NOT NULL CHECK (length(id) > 0),
    version BIGINT NOT NULL CHECK (version > 0 AND version <= 4294967295),
    revision BIGINT NOT NULL CHECK (revision > 0),
    state TEXT NOT NULL CHECK (length(state) > 0),
    data BYTEA NOT NULL CHECK (length(data) > 0)
)`

// Store is a concurrent-safe SQL-backed checkpoint register. It borrows the
// database; the caller owns its lifetime and durability configuration.
type Store struct {
	db *sql.DB
}

var _ fsm.Store = (*Store)(nil)

// New binds a store to db without creating tables or opening a connection.
func New(db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("sql store requires database: %w", fsm.ErrConfig)
	}

	return &Store{db: db}, nil
}

// Create inserts revision one without overwriting an existing machine.
func (s *Store) Create(ctx context.Context, next fsm.Record) error {
	if err := validate(next); err != nil {
		return err
	}
	if next.Revision != 1 {
		return fmt.Errorf("creation requires revision one: %w",
			fsm.ErrInvalidRecord)
	}

	result, err := s.db.ExecContext(ctx, `
		INSERT INTO loop_fsm_v2 (id, version, revision, state, data)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO NOTHING`, next.ID, int64(next.Version),
		int64(next.Revision), string(next.State), bytes.Clone(next.Data))
	if err != nil {
		return fmt.Errorf("create checkpoint: %w", err)
	}

	return checkWrite(result)
}

// Load reads the complete committed checkpoint or returns ErrNotFound.
func (s *Store) Load(ctx context.Context, id string) (fsm.Record, error) {
	var (
		record   fsm.Record
		version  int64
		revision int64
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT id, version, revision, state, data
		FROM loop_fsm_v2 WHERE id = $1`, id).Scan(
		&record.ID, &version, &revision, &record.State, &record.Data,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return fsm.Record{}, fsm.ErrNotFound
	}
	if err != nil {
		return fsm.Record{}, fmt.Errorf("load checkpoint: %w", err)
	}
	if record.ID != id || version <= 0 || version > math.MaxUint32 ||
		revision <= 0 {

		return fsm.Record{}, fmt.Errorf("invalid checkpoint envelope: %w",
			fsm.ErrInvalidRecord)
	}

	record.Version = uint32(version)
	record.Revision = uint64(revision)
	record.Data = bytes.Clone(record.Data)
	if err := validate(record); err != nil {
		return fsm.Record{}, err
	}

	return record, nil
}

// CompareAndSwap replaces all checkpoint fields in one statement only at the
// expected revision and unchanged definition version. Zero affected rows means a
// missing machine, stale writer or incompatible definition and returns conflict.
func (s *Store) CompareAndSwap(ctx context.Context, next fsm.Record,
	expectedRevision uint64) error {

	if err := validate(next); err != nil {
		return err
	}
	if expectedRevision == 0 || expectedRevision >= fsm.MaxRevision ||
		next.Revision != expectedRevision+1 {

		return fmt.Errorf("invalid compare-and-swap revision: %w",
			fsm.ErrInvalidRecord)
	}

	result, err := s.db.ExecContext(ctx, `
		UPDATE loop_fsm_v2
		SET version = $1, revision = $2, state = $3, data = $4
		WHERE id = $5 AND revision = $6 AND version = $1`,
		int64(next.Version), int64(next.Revision), string(next.State),
		bytes.Clone(next.Data), next.ID, int64(expectedRevision))
	if err != nil {
		return fmt.Errorf("compare-and-swap checkpoint: %w", err)
	}

	return checkWrite(result)
}

func validate(record fsm.Record) error {
	if record.ID == "" || record.Version == 0 || record.Revision == 0 ||
		record.Revision > fsm.MaxRevision || record.State == "" ||
		len(record.Data) == 0 {

		return fmt.Errorf("invalid checkpoint envelope: %w",
			fsm.ErrInvalidRecord)
	}

	return nil
}

func checkWrite(result sql.Result) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checkpoint affected rows: %w", err)
	}
	if affected == 0 {
		return fsm.ErrConflict
	}
	if affected != 1 {
		return fmt.Errorf("checkpoint affected multiple rows: %w",
			fsm.ErrInvalidRecord)
	}

	return nil
}
