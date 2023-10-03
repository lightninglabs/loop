package loopdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

var (
	ErrLoopOutsNotEqual        = errors.New("loop outs not equal")
	ErrLoopInsNotEqual         = errors.New("loop ins not equal")
	ErrLiquidityParamsNotEqual = errors.New("liquidity params not equal")
)

// MigratorManager is a struct that handles migrating data from one SwapStore
// to another.
type MigratorManager struct {
	fromStore SwapStore
	toStore   SwapStore
}

// NewMigratorManager creates a new MigratorManager.
func NewMigratorManager(fromStore SwapStore,
	toStore SwapStore) *MigratorManager {

	return &MigratorManager{
		fromStore: fromStore,
		toStore:   toStore,
	}
}

// RunMigrations runs the migrations from the fromStore to the toStore.
func (m *MigratorManager) RunMigrations(ctx context.Context) error {
	log.Infof("Migrating loop outs...")

	// Migrate loop outs.
	err := m.migrateLoopOuts(ctx)
	if err != nil {
		return err
	}

	log.Infof("Checking loop outs...")

	// Check that the loop outs are equal.
	err = m.checkLoopOuts(ctx)
	if err != nil {
		return err
	}

	log.Infof("Migrating loop ins...")

	// Migrate loop ins.
	err = m.migrateLoopIns(ctx)
	if err != nil {
		return err
	}

	log.Infof("Checking loop ins...")

	// Check that the loop ins are equal.
	err = m.checkLoopIns(ctx)
	if err != nil {
		return err
	}

	log.Infof("Migrating liquidity parameters...")

	// Migrate liquidity parameters.
	err = m.migrateLiquidityParams(ctx)
	if err != nil {
		return err
	}

	log.Infof("Checking liquidity parameters...")

	// Check that the liquidity parameters are equal.
	err = m.checkLiquidityParams(ctx)
	if err != nil {
		return err
	}

	log.Infof("Migrations complete!")

	return nil
}

func (m *MigratorManager) migrateLoopOuts(ctx context.Context) error {
	// Fetch all loop outs from the fromStore.
	loopOuts, err := m.fromStore.FetchLoopOutSwaps(ctx)
	if err != nil {
		return err
	}

	swapMap := make(map[lntypes.Hash]*LoopOutContract)
	updateMap := make(map[lntypes.Hash][]BatchInsertUpdateData)

	// For each loop out, create a new loop out in the toStore.
	for _, loopOut := range loopOuts {
		swapMap[loopOut.Hash] = loopOut.Contract

		for _, event := range loopOut.Events {
			updateMap[loopOut.Hash] = append(
				updateMap[loopOut.Hash],
				BatchInsertUpdateData{
					Time:  event.Time,
					State: event.SwapStateData,
				},
			)
		}
	}

	// Create the loop outs in the toStore.
	err = m.toStore.BatchCreateLoopOut(ctx, swapMap)
	if err != nil {
		return err
	}

	// Update the loop outs in the toStore.
	err = m.toStore.BatchInsertUpdate(
		ctx, updateMap,
	)
	if err != nil {
		return err
	}

	return nil
}

// migrateLoopIns migrates all loop ins from the fromStore to the toStore.
func (m *MigratorManager) migrateLoopIns(ctx context.Context) error {
	// Fetch all loop ins from the fromStore.
	loopIns, err := m.fromStore.FetchLoopInSwaps(ctx)
	if err != nil {
		return err
	}

	swapMap := make(map[lntypes.Hash]*LoopInContract)
	updateMap := make(map[lntypes.Hash][]BatchInsertUpdateData)

	// For each loop in, create a new loop in the toStore.
	for _, loopIn := range loopIns {
		swapMap[loopIn.Hash] = loopIn.Contract

		for _, event := range loopIn.Events {
			updateMap[loopIn.Hash] = append(
				updateMap[loopIn.Hash],
				BatchInsertUpdateData{
					Time:  event.Time,
					State: event.SwapStateData,
				},
			)
		}
	}

	// Create the loop outs in the toStore.
	err = m.toStore.BatchCreateLoopIn(ctx, swapMap)
	if err != nil {
		return err
	}

	// Update the loop outs in the toStore.
	err = m.toStore.BatchInsertUpdate(
		ctx, updateMap,
	)
	if err != nil {
		return err
	}

	return nil
}

// migrateLiquidityParams migrates the liquidity parameters from the fromStore
// to the toStore.
func (m *MigratorManager) migrateLiquidityParams(ctx context.Context) error {
	// Fetch the liquidity parameters from the fromStore.
	params, err := m.fromStore.FetchLiquidityParams(ctx)
	if err != nil {
		return err
	}

	// Put the liquidity parameters in the toStore.
	err = m.toStore.PutLiquidityParams(ctx, params)
	if err != nil {
		return err
	}

	return nil
}

// checkLoopOuts checks that all loop outs in the toStore are the exact same as
// the loop outs in the fromStore.
func (m *MigratorManager) checkLoopOuts(ctx context.Context) error {
	// Fetch all loop outs from the fromStore.
	fromLoopOuts, err := m.fromStore.FetchLoopOutSwaps(ctx)
	if err != nil {
		return err
	}

	// Fetch all loop outs from the toStore.
	toLoopOuts, err := m.toStore.FetchLoopOutSwaps(ctx)
	if err != nil {
		return err
	}

	// Check that the number of loop outs is the same.
	if len(fromLoopOuts) != len(toLoopOuts) {
		return NewMigrationError(
			fmt.Errorf("from: %d, to: %d", len(fromLoopOuts), len(toLoopOuts)),
		)
	}

	// Sort both list of loop outs by hash.
	sortLoopOuts(fromLoopOuts)
	sortLoopOuts(toLoopOuts)

	// Check that each loop out is the same.
	for i, fromLoopOut := range fromLoopOuts {
		toLoopOut := toLoopOuts[i]

		err := equalizeLoopOut(fromLoopOut, toLoopOut)
		if err != nil {
			return NewMigrationError(err)
		}

		err = equalValues(fromLoopOut, toLoopOut)
		if err != nil {
			return NewMigrationError(err)
		}
	}

	return nil
}

// checkLoopIns checks that all loop ins in the toStore are the exact same as
// the loop ins in the fromStore.
func (m *MigratorManager) checkLoopIns(ctx context.Context) error {
	// Fetch all loop ins from the fromStore.
	fromLoopIns, err := m.fromStore.FetchLoopInSwaps(ctx)
	if err != nil {
		return err
	}

	// Fetch all loop ins from the toStore.
	toLoopIns, err := m.toStore.FetchLoopInSwaps(ctx)
	if err != nil {
		return err
	}

	// Check that the number of loop ins is the same.
	if len(fromLoopIns) != len(toLoopIns) {
		return NewMigrationError(
			fmt.Errorf("from: %d, to: %d", len(fromLoopIns), len(toLoopIns)),
		)
	}

	// Sort both list of loop ins by hash.
	sortLoopIns(fromLoopIns)
	sortLoopIns(toLoopIns)

	// Check that each loop in is the same.
	for i, fromLoopIn := range fromLoopIns {
		toLoopIn := toLoopIns[i]

		err := equalizeLoopIns(fromLoopIn, toLoopIn)
		if err != nil {
			return NewMigrationError(err)
		}

		err = equalValues(fromLoopIn, toLoopIn)
		if err != nil {
			return NewMigrationError(err)
		}
	}

	return nil
}

// checkLiquidityParams checks that the liquidity parameters in the toStore are
// the exact same as the liquidity parameters in the fromStore.
func (m *MigratorManager) checkLiquidityParams(ctx context.Context) error {
	// Fetch the liquidity parameters from the fromStore.
	fromParams, err := m.fromStore.FetchLiquidityParams(ctx)
	if err != nil {
		return err
	}

	// Fetch the liquidity parameters from the toStore.
	toParams, err := m.toStore.FetchLiquidityParams(ctx)
	if err != nil {
		return err
	}

	// Check that the liquidity parameters are the same.
	if !bytes.Equal(fromParams, toParams) {
		return NewMigrationError(
			fmt.Errorf("from: %v, to: %v", fromParams, toParams),
		)
	}

	return nil
}

// equalizeLoopOut checks that the loop outs have the same time stored.
// Due to some weirdness with timezones between boltdb and sqlite we then
// set the times to the same value.
func equalizeLoopOut(fromLoopOut, toLoopOut *LoopOut) error {
	if fromLoopOut.Contract.InitiationTime.Unix() !=
		toLoopOut.Contract.InitiationTime.Unix() {

		return fmt.Errorf("initiation time mismatch")
	}

	toLoopOut.Contract.InitiationTime = fromLoopOut.Contract.InitiationTime

	if fromLoopOut.Contract.SwapPublicationDeadline.Unix() !=
		toLoopOut.Contract.SwapPublicationDeadline.Unix() {

		return fmt.Errorf("swap publication deadline mismatch")
	}

	toLoopOut.Contract.
		SwapPublicationDeadline = fromLoopOut.Contract.SwapPublicationDeadline

	for i, event := range fromLoopOut.Events {
		if event.Time.Unix() != toLoopOut.Events[i].Time.Unix() {
			return fmt.Errorf("event time mismatch")
		}
		toLoopOut.Events[i].Time = event.Time
	}

	return nil
}

func equalizeLoopIns(fromLoopIn, toLoopIn *LoopIn) error {
	if fromLoopIn.Contract.InitiationTime.Unix() !=
		toLoopIn.Contract.InitiationTime.Unix() {

		return fmt.Errorf("initiation time mismatch")
	}

	toLoopIn.Contract.InitiationTime = fromLoopIn.Contract.InitiationTime

	for i, event := range fromLoopIn.Events {
		if event.Time.Unix() != toLoopIn.Events[i].Time.Unix() {
			return fmt.Errorf("event time mismatch")
		}
		toLoopIn.Events[i].Time = event.Time
	}

	return nil
}

// sortLoopOuts sorts a list of loop outs by hash.
func sortLoopOuts(loopOuts []*LoopOut) {
	sort.Slice(loopOuts, func(i, j int) bool {
		return bytes.Compare(loopOuts[i].Hash[:], loopOuts[j].Hash[:]) < 0
	})
}

// sortLoopIns sorts a list of loop ins by hash.
func sortLoopIns(loopIns []*LoopIn) {
	sort.Slice(loopIns, func(i, j int) bool {
		return bytes.Compare(loopIns[i].Hash[:], loopIns[j].Hash[:]) < 0
	})
}

type migrationError struct {
	Err error
}

func (e *migrationError) Error() string {
	return fmt.Sprintf("migrator error: %v", e.Err)
}

func (e *migrationError) Unwrap() error {
	return e.Err
}

func (e *migrationError) Is(target error) bool {
	_, ok := target.(*migrationError)
	return ok
}

func NewMigrationError(err error) *migrationError {
	return &migrationError{Err: err}
}

func equalValues(src interface{}, dst interface{}) error {
	mt := &mockTesting{}

	require.EqualValues(mt, src, dst)
	if mt.fail || mt.failNow {
		return fmt.Errorf(mt.format, mt.args)
	}

	return nil
}

type mockTesting struct {
	failNow bool
	fail    bool
	format  string
	args    []interface{}
}

func (m *mockTesting) FailNow() {
	m.failNow = true
}

func (m *mockTesting) Errorf(format string, args ...interface{}) {
	m.format = format
	m.args = args
}
