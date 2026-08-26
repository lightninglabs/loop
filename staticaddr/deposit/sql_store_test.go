package deposit

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/jackc/pgx/v5"
	"github.com/lightninglabs/loop/fsm"
	"github.com/lightninglabs/loop/loopdb/sqlc"
	"github.com/lightninglabs/loop/staticaddr/script"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
)

func TestCreateDepositRejectsUnpersistedAddress(t *testing.T) {
	store := NewSqlStore(nil)
	tests := []struct {
		name    string
		params  *script.Parameters
		wantErr string
	}{
		{
			name:    "missing parameters",
			wantErr: "static address parameters must be set",
		},
		{
			name:    "missing database ID",
			params:  &script.Parameters{},
			wantErr: "static address ID must be set",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			deposit := &Deposit{AddressParams: testCase.params}
			err := store.CreateDeposit(context.Background(), deposit)
			require.ErrorContains(t, err, testCase.wantErr)
		})
	}
}

func TestToDeposit(t *testing.T) {
	depositID, err := GetRandomDepositID()
	require.NoError(t, err)

	swapHash, err := lntypes.MakeHash(dummyHashBytes())
	require.NoError(t, err)

	tx := wire.NewMsgTx(2)
	txHash := tx.TxHash()

	tests := []struct {
		name       string
		row        sqlc.Deposit
		lastUpdate sqlc.DepositUpdate
		expectErr  bool
	}{
		{
			name: "fully valid data",
			row: sqlc.Deposit{
				DepositID:          depositID[:],
				TxHash:             txHash[:],
				Amount:             100000000,
				ConfirmationHeight: 123456,
				SwapHash:           swapHash[:],
			},
			lastUpdate: sqlc.DepositUpdate{
				UpdateState: "completed",
			},
			expectErr: false,
		},
		{
			name: "fully valid data",
			row: sqlc.Deposit{
				DepositID:          depositID[:],
				TxHash:             txHash[:],
				Amount:             100000000,
				ConfirmationHeight: 123456,
			},
			lastUpdate: sqlc.DepositUpdate{
				UpdateState: "completed",
			},
			expectErr: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := ToDeposit(test.row, test.lastUpdate)
			if test.expectErr {
				require.Error(t, err)
				require.Nil(t, result)
			} else {
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, fsm.StateType(test.lastUpdate.UpdateState), result.state)
			}
		})
	}
}

func TestToDepositWithAddress(t *testing.T) {
	depositID, err := GetRandomDepositID()
	require.NoError(t, err)

	txHash := wire.NewMsgTx(2).TxHash()
	params := testAddressParameters(t, 7)
	row := sqlc.AllDepositsWithAddressRow{
		DepositID:          depositID[:],
		TxHash:             txHash[:],
		Amount:             100_000,
		ConfirmationHeight: 123,
		StaticAddressID: sql.NullInt32{
			Int32: params.ID,
			Valid: true,
		},
		ClientPubkey: params.ClientPubkey.SerializeCompressed(),
		ServerPubkey: params.ServerPubkey.SerializeCompressed(),
		Expiry: sql.NullInt32{
			Int32: int32(params.Expiry),
			Valid: true,
		},
		ClientKeyFamily: sql.NullInt32{
			Int32: int32(params.KeyLocator.Family),
			Valid: true,
		},
		ClientKeyIndex: sql.NullInt32{
			Int32: int32(params.KeyLocator.Index),
			Valid: true,
		},
		Pkscript: params.PkScript,
		ProtocolVersion: sql.NullInt32{
			Int32: int32(params.ProtocolVersion),
			Valid: true,
		},
		InitiationHeight: sql.NullInt32{
			Int32: params.InitiationHeight,
			Valid: true,
		},
	}
	lastUpdate := sqlc.DepositUpdate{UpdateState: "completed"}

	result, err := ToDepositWithAddress(row, lastUpdate)
	require.NoError(t, err)
	require.NotNil(t, result.AddressParams)
	require.Equal(t, params.ID, result.AddressParams.ID)
	require.True(t, params.ClientPubkey.IsEqual(
		result.AddressParams.ClientPubkey,
	))
	require.True(t, params.ServerPubkey.IsEqual(
		result.AddressParams.ServerPubkey,
	))
	require.Equal(t, params.Expiry, result.AddressParams.Expiry)
	require.Equal(t, params.PkScript, result.AddressParams.PkScript)
	require.Equal(t, params.KeyLocator, result.AddressParams.KeyLocator)
	require.Equal(t, params.ProtocolVersion,
		result.AddressParams.ProtocolVersion)
	require.Equal(t, params.InitiationHeight,
		result.AddressParams.InitiationHeight)

	row.ClientPubkey = []byte{0x01}
	result, err = ToDepositWithAddress(row, lastUpdate)
	require.Error(t, err)
	require.Nil(t, result)
}

func TestDepositRowConvertersStayInSync(t *testing.T) {
	t.Parallel()

	all := populatedSQLRow[sqlc.AllDepositsWithAddressRow](t)
	require.Equal(t, expectedDepositRow(t, all), depositRowFromAll(all))

	get := populatedSQLRow[sqlc.GetDepositWithAddressRow](t)
	require.Equal(t, expectedDepositRow(t, get), depositRowFromGet(get))

	outpoint := populatedSQLRow[sqlc.DepositForOutpointWithAddressRow](t)
	require.Equal(t, expectedDepositRow(t, outpoint),
		depositRowFromOutpoint(outpoint))

	swapHash := populatedSQLRow[sqlc.DepositsForSwapHashRow](t)
	require.Equal(t,
		expectedDepositRow(t, swapHash, "UpdateState", "UpdateTimestamp"),
		depositRowFromSwapHash(swapHash))
}

func populatedSQLRow[T any](t *testing.T) T {
	t.Helper()

	var row T
	value := reflect.ValueOf(&row).Elem()
	for i := 0; i < value.NumField(); i++ {
		field := value.Field(i)
		n := int64(i + 1)
		switch field.Type() {
		case reflect.TypeOf([]byte{}):
			field.SetBytes([]byte{byte(n)})

		case reflect.TypeOf(int32(0)), reflect.TypeOf(int64(0)):
			field.SetInt(n)

		case reflect.TypeOf(sql.NullString{}):
			field.Set(reflect.ValueOf(sql.NullString{
				String: "set",
				Valid:  true,
			}))

		case reflect.TypeOf(sql.NullInt32{}):
			field.Set(reflect.ValueOf(sql.NullInt32{
				Int32: int32(n),
				Valid: true,
			}))

		case reflect.TypeOf(sql.NullTime{}):
			field.Set(reflect.ValueOf(sql.NullTime{
				Time:  time.Unix(n, 0).UTC(),
				Valid: true,
			}))

		default:
			t.Fatalf("unsupported SQL row field %s", field.Type())
		}
	}

	return row
}

func expectedDepositRow(t *testing.T, sqlRow any,
	additionalFields ...string) depositRow {

	t.Helper()

	source := reflect.ValueOf(sqlRow)
	target := reflect.ValueOf(&depositRow{}).Elem()
	require.Equal(t, target.NumField()+1+len(additionalFields),
		source.NumField())
	require.Equal(t, "ID", source.Type().Field(0).Name)
	for _, fieldName := range additionalFields {
		require.True(t, source.FieldByName(fieldName).IsValid(), fieldName)
	}

	for i := 0; i < target.NumField(); i++ {
		targetField := target.Type().Field(i)
		sourceField := source.FieldByName(targetField.Name)
		require.True(t, sourceField.IsValid(), targetField.Name)
		require.Equal(t, targetField.Type, sourceField.Type())
		target.Field(i).Set(sourceField)
	}

	return target.Interface().(depositRow)
}

func dummyHashBytes() []byte {
	return []byte{0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b,
		0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12, 0x13, 0x14, 0x15,
		0x16, 0x17, 0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f,
		0x20, 0x21, 0x22, 0x23}
}

// TestErrNoRows ensures that pgx.ErrNoRows is a wrapped sql.ErrNoRows, so we
// don't have to check against both of them.
func TestErrNoRows(t *testing.T) {
	require.ErrorIs(t, pgx.ErrNoRows, sql.ErrNoRows)
}
