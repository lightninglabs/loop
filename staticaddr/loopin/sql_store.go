package loopin

import (
	"context"
	"github.com/lightninglabs/loop/loopdb"
	"github.com/lightningnetwork/lnd/clock"
)

// SqlStore is the backing store for static address deposits.
type SqlStore struct {
	baseDB *loopdb.BaseDB

	clock clock.Clock
}

// NewSqlStore constructs a new SQLStore from a BaseDB. The BaseDB is agnostic
// to the underlying driver which can be postgres or sqlite.
func NewSqlStore(db *loopdb.BaseDB) *SqlStore {
	return &SqlStore{
		baseDB: db,

		clock: clock.NewDefaultClock(),
	}
}

// CreateLoopIn ...
func (s *SqlStore) CreateLoopIn(ctx context.Context, loopIn *StaticAddressLoopIn) error {
	return nil
}

// UpdateLoopIn updates the loop-in in the database.
func (s *SqlStore) UpdateLoopIn(ctx context.Context, loopIn *StaticAddressLoopIn) error {
	return nil
}

// Close closes the database connection.
func (s *SqlStore) Close() {
	s.baseDB.DB.Close()
}
