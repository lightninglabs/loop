package sqlstore

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	fsm "github.com/lightninglabs/loop/fsm/v2"
	"github.com/stretchr/testify/require"
)

type intentData struct {
	OperationID string `json:"operation_id"`
	Transaction []byte `json:"transaction"`
	Completed   bool   `json:"completed,omitzero"`
}

type intentEvent struct {
	OperationID string `json:"operation_id"`
	Transaction []byte `json:"transaction"`
	Completed   bool   `json:"completed,omitzero"`
}

// TestMachineRestartReconcilesIntent combines real SQL persistence with engine
// recovery after an external effect succeeded but its RPC response was lost.
func TestMachineRestartReconcilesIntent(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "restart.sqlite")
	db := openDatabase(t, path)
	_, err := db.ExecContext(ctx, Schema)
	require.NoError(t, err)
	store, err := New(db)
	require.NoError(t, err)

	// This represents the remote service: its outcome survives the local
	// process, while callbacks and machine runtime are reconstructed below.
	external := make(map[string][]byte)
	var enterCalls, restoreCalls int
	responseLost := errors.New("external effect applied; RPC response lost")
	codec := fsm.JSONCodec[intentData]()
	validateIntent := func(data intentData) error {
		if data.OperationID == "" || len(data.Transaction) == 0 {
			return errors.New("missing operation identity or transaction")
		}
		return nil
	}
	allow := func(context.Context, fsm.Snapshot[intentData],
		intentEvent) error {

		return nil
	}
	cfg := fsm.Config[intentData, intentEvent]{
		ID:         "restart-swap",
		Version:    1,
		Store:      store,
		DataCodec:  codec,
		EventCodec: fsm.JSONCodec[intentEvent](),
		States: map[fsm.StateID]fsm.State[intentData, intentEvent]{
			"ready": {
				Validate: func(intentData) error { return nil },
				Restore: func(context.Context,
					fsm.Snapshot[intentData]) error {

					return nil
				},
				Transitions: map[fsm.Event]fsm.Transition[intentData, intentEvent]{
					"publish": {
						To:   "intent",
						Gate: allow,
						Update: func(_ context.Context,
							_ fsm.Snapshot[intentData],
							event intentEvent) (intentData, error) {

							return intentData{
								OperationID: event.OperationID,
								Transaction: event.Transaction,
							}, nil
						},
					},
				},
			},
			"intent": {
				Validate: validateIntent,
				Enter: func(ctx context.Context,
					snapshot fsm.Snapshot[intentData]) error {

					// The operation/artifact must already be durable before
					// any external side effect can observe them.
					record, err := store.Load(ctx, snapshot.ID)
					require.NoError(t, err)
					persisted, err := codec.Decode(record.Data)
					require.NoError(t, err)
					require.Equal(t, snapshot.Data, persisted)
					require.Equal(t, uint64(2), record.Revision)
					external[snapshot.Data.OperationID] =
						bytes.Clone(snapshot.Data.Transaction)
					enterCalls++
					return responseLost
				},
				Restore: func(_ context.Context,
					snapshot fsm.Snapshot[intentData]) error {

					// Query the stable remote operation rather than replaying
					// entry or inventing a different transaction on restart.
					if !bytes.Equal(external[snapshot.Data.OperationID],
						snapshot.Data.Transaction) {

						return errors.New("remote operation not reconciled")
					}
					restoreCalls++
					return nil
				},
				Transitions: map[fsm.Event]fsm.Transition[intentData, intentEvent]{
					"confirmed": {
						To: "done",
						Gate: func(_ context.Context,
							snapshot fsm.Snapshot[intentData],
							event intentEvent) error {

							if !event.Completed || event.OperationID !=
								snapshot.Data.OperationID {

								return errors.New("confirmation mismatch")
							}
							return nil
						},
						Update: func(_ context.Context,
							snapshot fsm.Snapshot[intentData],
							_ intentEvent) (intentData, error) {

							data := snapshot.Data
							data.Completed = true
							return data, nil
						},
					},
				},
			},
			"done": {
				Terminal: true,
				Validate: func(data intentData) error {
					if !data.Completed {
						return errors.New("not completed")
					}
					return validateIntent(data)
				},
			},
		},
	}

	machine, err := fsm.Create(ctx, cfg, "ready", intentData{})
	require.NoError(t, err)
	require.NoError(t, machine.Recover(ctx))
	event := intentEvent{
		OperationID: "stable-publication-id",
		Transaction: []byte("original-signed-transaction"),
	}
	committed, err := machine.Send(ctx, "publish", event)
	require.ErrorIs(t, err, responseLost)
	require.ErrorIs(t, err, fsm.ErrRecoveryRequired)
	require.Equal(t, fsm.StateID("intent"), committed.State)
	require.Equal(t, uint64(2), committed.Revision)
	require.NoError(t, db.Close())

	// Reopen the persisted database and construct a new machine. Only the
	// external operation survives besides SQL; Enter must not be replayed.
	cfg.Store, err = New(openDatabase(t, path))
	require.NoError(t, err)
	restarted, err := fsm.Open(ctx, cfg)
	require.NoError(t, err)
	event.Completed = true
	_, err = restarted.Send(ctx, "confirmed", event)
	require.ErrorIs(t, err, fsm.ErrRecoveryRequired)
	require.NoError(t, restarted.Recover(ctx))
	done, err := restarted.Send(ctx, "confirmed", event)
	require.NoError(t, err)
	require.Equal(t, fsm.StateID("done"), done.State)
	require.Equal(t, uint64(3), done.Revision)
	require.Equal(t, 1, enterCalls)
	require.Equal(t, 1, restoreCalls)
	require.Equal(t, event.OperationID, done.Data.OperationID)
	require.Equal(t, event.Transaction, done.Data.Transaction)
	require.True(t, done.Data.Completed)
}
