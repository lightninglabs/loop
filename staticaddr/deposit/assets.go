package deposit

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/wire"
)

// RegisterAssetDeposit excludes a proof-bearing anchor from Bitcoin-only
// selection and sweeping. Persistence runs while the shared manager lock is
// held; callers restore these registrations before starting the manager.
// Asset reservations and their hash ownership live in the durable asset
// inventory. They must never reinterpret an existing Bitcoin deposit.
func (m *Manager) RegisterAssetDeposit(ctx context.Context, outpoint string,
	persist func() error) error {

	point, err := wire.NewOutPointFromString(outpoint)
	if err != nil || point.String() != outpoint {
		return fmt.Errorf("invalid asset deposit outpoint")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.assetDeposits == nil {
		m.assetDeposits = make(map[wire.OutPoint]struct{})
	}
	if _, exists := m.assetDeposits[*point]; exists {
		return persist()
	}
	if _, exists := m.deposits[*point]; exists {
		return fmt.Errorf("asset anchor is already a Bitcoin deposit")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := persist(); err != nil {
		return err
	}
	m.assetDeposits[*point] = struct{}{}
	return nil
}
