package fsm_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	fsm "github.com/lightninglabs/loop/fsm/v2"
	"github.com/stretchr/testify/require"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// inputClaimStore is a test domain adapter for two flows sharing outpoints.
// The Operation field identifies the input. The same SQL statement advances
// the checkpoint and runs the claim or owner-checked release trigger.
type inputClaimStore struct{ db *sql.DB }

func (s inputClaimStore) Create(ctx context.Context, r fsm.Record) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workflows (id, version, revision, state, data)
		VALUES (?, ?, ?, ?, ?)`, r.ID, r.Version, r.Revision, r.State, r.Data)
	return claimWriteError(err)
}

func (s inputClaimStore) Load(ctx context.Context, id string) (fsm.Record, error) {
	r := fsm.Record{ID: id}
	err := s.db.QueryRowContext(ctx, `
		SELECT version, revision, state, data FROM workflows WHERE id = ?`,
		id).Scan(&r.Version, &r.Revision, &r.State, &r.Data)
	if errors.Is(err, sql.ErrNoRows) {
		return r, fsm.ErrNotFound
	}
	return r, err
}

func (s inputClaimStore) CompareAndSwap(ctx context.Context, r fsm.Record,
	expected uint64) error {

	if r.Revision != expected+1 {
		return fsm.ErrConflict
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE workflows SET version = ?, revision = ?, state = ?, data = ?
		WHERE id = ? AND revision = ?`,
		r.Version, r.Revision, r.State, r.Data, r.ID, expected)
	if err != nil {
		return claimWriteError(err)
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return fsm.ErrConflict
	}
	return err
}

func claimWriteError(err error) error {
	if e, ok := errors.AsType[*sqlite.Error](err); ok &&
		e.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY {

		return errors.Join(fsm.ErrConflict, err)
	}
	return err
}

func TestCrossFlowInputOwnership(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "claims.sqlite")
	openStore := func() inputClaimStore {
		db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, db.Close()) })
		return inputClaimStore{db: db}
	}
	store := openStore()
	_, err := store.db.ExecContext(ctx, `
		CREATE TABLE workflows (id TEXT PRIMARY KEY, version INTEGER,
		    revision INTEGER, state TEXT, data BLOB);
		CREATE TABLE claims (input TEXT PRIMARY KEY, owner TEXT NOT NULL);
		CREATE TRIGGER reserve AFTER UPDATE OF state ON workflows
		    WHEN NEW.state = 'pending' BEGIN
		    INSERT INTO claims VALUES (
		        json_extract(CAST(NEW.data AS TEXT), '$.Operation'), NEW.id
		    );
		END;
		CREATE TRIGGER release AFTER UPDATE OF state ON workflows
		    WHEN NEW.state = 'done' BEGIN
		    DELETE FROM claims WHERE owner = OLD.id AND
		        input = json_extract(CAST(OLD.data AS TEXT), '$.Operation');
		END;`)
	require.NoError(t, err)

	type machine = fsm.Machine[businessData, command]
	configFor := func(id string) fsm.Config[businessData, command] {
		cfg := config(store)
		cfg.ID = id
		ready := cfg.States[stateReady]
		ready.Transitions["claim"] = fsm.Transition[businessData, command]{
			To: statePending, Gate: allow, Update: unchanged,
		}
		pending := cfg.States[statePending]
		ready.Transitions["cleanup"] = pending.Transitions["complete"]
		// Simulate stopping immediately after the claim commits, before
		// the owner performs any external work.
		pending.Enter = func(context.Context, fsm.Snapshot[businessData]) error {
			return errInjected
		}
		cfg.States[statePending] = pending
		return cfg
	}
	const input = "funding-tx:0"
	ids := []string{"withdrawal/1", "asset-swap/1"}
	machines := make([]*machine, len(ids))
	for i, id := range ids {
		machines[i], err = fsm.Create(ctx, configFor(id), stateReady, businessData{
			Values: []int{1}, Operation: input,
		})
		require.NoError(t, err)
		require.NoError(t, machines[i].Recover(ctx))
	}

	type outcome struct {
		index int
		err   error
	}
	start := make(chan struct{})
	results := make(chan outcome, len(ids))
	for i, m := range machines {
		go func() {
			<-start
			_, err := m.Send(ctx, "claim", command{})
			results <- outcome{i, err}
		}()
	}
	close(start)
	winner, loser := <-results, <-results
	if errors.Is(winner.err, fsm.ErrConflict) {
		winner, loser = loser, winner
	}
	require.ErrorIs(t, winner.err, errInjected)
	require.ErrorIs(t, winner.err, fsm.ErrRecoveryRequired)
	require.ErrorIs(t, loser.err, fsm.ErrConflict)
	require.ErrorIs(t, loser.err, fsm.ErrRecoveryRequired)

	// Discard both runtimes and their DB connections after the claim commit.
	// Recover strictly from disk before delivering the losing flow's cleanup.
	require.NoError(t, store.db.Close())
	store = openStore()
	for i, id := range ids {
		machines[i], err = fsm.Open(ctx, configFor(id))
		require.NoError(t, err)
		require.NoError(t, machines[i].Recover(ctx))
	}
	checkpoint, err := machines[loser.index].Snapshot(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(1), checkpoint.Revision)
	require.Equal(t, fsm.StateID(stateReady), checkpoint.State)
	_, err = machines[loser.index].Send(ctx, "cleanup", command{})
	require.NoError(t, err)

	var owner string
	require.NoError(t, store.db.QueryRowContext(ctx,
		"SELECT owner FROM claims WHERE input = ?", input,
	).Scan(&owner))
	require.Equal(t, ids[winner.index], owner)
	checkpoint, err = machines[winner.index].Snapshot(ctx)
	require.NoError(t, err)
	require.Equal(t, fsm.StateID(statePending), checkpoint.State)
	require.Equal(t, uint64(2), checkpoint.Revision)

	_, err = machines[winner.index].Send(ctx, "complete", command{})
	require.NoError(t, err)
	var count int
	require.NoError(t, store.db.QueryRowContext(ctx,
		"SELECT count(*) FROM claims").Scan(&count))
	require.Zero(t, count)
}
