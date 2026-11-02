package grpc

import (
	"testing"

	tapsdk "github.com/lightninglabs/loop/tapsdk"
	"github.com/lightninglabs/taproot-assets/taprpc"
	"github.com/stretchr/testify/require"
)

var (
	// Test data: valid 33-byte compressed public key.
	testPubKey = []byte{
		0x02,
		0x79, 0xbe, 0x66, 0x7e, 0xf9, 0xdc, 0xbb, 0xac,
		0x55, 0xa0, 0x62, 0x95, 0xce, 0x87, 0x0b, 0x07,
		0x02, 0x9b, 0xfc, 0xdb, 0x2d, 0xce, 0x28, 0xd9,
		0x59, 0xf2, 0x81, 0x5b, 0x16, 0xf8, 0x17, 0x98,
	}

	// Test data: valid 32-byte x-only public key.
	testXOnlyPubKey = []byte{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
		0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
		0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20,
	}

	// Test data: valid 32-byte asset ID.
	testAssetID = []byte{
		0xa1, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xa7, 0xa8,
		0xa9, 0xaa, 0xab, 0xac, 0xad, 0xae, 0xaf, 0xb0,
		0xb1, 0xb2, 0xb3, 0xb4, 0xb5, 0xb6, 0xb7, 0xb8,
		0xb9, 0xba, 0xbb, 0xbc, 0xbd, 0xbe, 0xbf, 0xc0,
	}
)

func TestUnmarshalAddr(t *testing.T) {
	tests := []struct {
		name    string
		rpcAddr *taprpc.Addr
		wantRef func() tapsdk.AssetRef
		wantErr string
	}{
		{
			name:    "nil address",
			rpcAddr: nil,
			wantErr: "nil address",
		},
		{
			name: "invalid script key length",
			rpcAddr: &taprpc.Addr{
				Encoded:          testTap1test,
				AssetId:          testAssetID,
				ScriptKey:        []byte{0x01, 0x02}, // Too short
				InternalKey:      testPubKey,
				TaprootOutputKey: testXOnlyPubKey,
			},
			wantErr: "invalid script key length",
		},
		{
			name: "invalid internal key length",
			rpcAddr: &taprpc.Addr{
				Encoded:          testTap1test,
				AssetId:          testAssetID,
				ScriptKey:        testPubKey,
				InternalKey:      []byte{0x01, 0x02}, // Too short
				TaprootOutputKey: testXOnlyPubKey,
			},
			wantErr: "invalid internal key length",
		},
		{
			name: "invalid taproot output key length",
			rpcAddr: &taprpc.Addr{
				Encoded:          testTap1test,
				AssetId:          testAssetID,
				ScriptKey:        testPubKey,
				InternalKey:      testPubKey,
				TaprootOutputKey: []byte{0x01, 0x02}, // Too short
			},
			wantErr: "invalid taproot output key length",
		},
		{
			name: testUnknownAssetType,
			rpcAddr: &taprpc.Addr{
				Encoded:          "tap1testunknown",
				AssetId:          testAssetID,
				AssetType:        taprpc.AssetType(99),
				Amount:           1,
				ScriptKey:        testPubKey,
				InternalKey:      testPubKey,
				TaprootOutputKey: testXOnlyPubKey,
			},
			wantErr: testUnknownAssetType,
		},
		{
			name: "valid V0 address",
			rpcAddr: &taprpc.Addr{
				Encoded:          "tap1testaddr",
				AssetId:          testAssetID,
				AssetType:        taprpc.AssetType_NORMAL,
				Amount:           1000,
				ScriptKey:        testPubKey,
				InternalKey:      testPubKey,
				TaprootOutputKey: testXOnlyPubKey,
				ProofCourierAddr: "universe://localhost:10029",
				AssetVersion:     taprpc.AssetVersion_ASSET_VERSION_V0,
				AddressVersion:   taprpc.AddrVersion_ADDR_VERSION_V0,
			},
			wantRef: func() tapsdk.AssetRef {
				var expectedAssetID tapsdk.AssetID
				copy(expectedAssetID[:], testAssetID)
				return tapsdk.AssetRefFromAssetID(expectedAssetID)
			},
			wantErr: "",
		},
		{
			name: "valid V2 address with group key",
			rpcAddr: &taprpc.Addr{
				Encoded:          "tap1testaddrv2",
				AssetType:        taprpc.AssetType_NORMAL,
				Amount:           0, // V2 can have zero amount
				GroupKey:         testPubKey,
				ScriptKey:        testPubKey,
				InternalKey:      testPubKey,
				TaprootOutputKey: testXOnlyPubKey,
				ProofCourierAddr: "authmailbox+universerpc://localhost:10029",
				AssetVersion:     taprpc.AssetVersion_ASSET_VERSION_V1,
				AddressVersion:   taprpc.AddrVersion_ADDR_VERSION_V2,
			},
			wantRef: func() tapsdk.AssetRef {
				var expectedGroupKey tapsdk.PubKey
				copy(expectedGroupKey[:], testPubKey)
				return tapsdk.AssetRefFromGroupKey(expectedGroupKey)
			},
			wantErr: "",
		},
		{
			name: "collection item address with group key",
			rpcAddr: &taprpc.Addr{
				Encoded:          "tap1testcollection",
				AssetId:          testAssetID,
				AssetType:        taprpc.AssetType_COLLECTIBLE,
				Amount:           1,
				GroupKey:         testPubKey,
				ScriptKey:        testPubKey,
				InternalKey:      testPubKey,
				TaprootOutputKey: testXOnlyPubKey,
				ProofCourierAddr: "authmailbox+universerpc://localhost:10029",
				AssetVersion:     taprpc.AssetVersion_ASSET_VERSION_V1,
				AddressVersion:   taprpc.AddrVersion_ADDR_VERSION_V2,
			},
			wantRef: func() tapsdk.AssetRef {
				var expectedAssetID tapsdk.AssetID
				copy(expectedAssetID[:], testAssetID)
				return tapsdk.AssetRefFromAssetID(expectedAssetID)
			},
			wantErr: "",
		},
		{
			name: "collection address with group key",
			rpcAddr: &taprpc.Addr{
				Encoded:          "tap1testcollectiongroup",
				AssetId:          make([]byte, 32),
				AssetType:        taprpc.AssetType_COLLECTIBLE,
				Amount:           1,
				GroupKey:         testPubKey,
				ScriptKey:        testPubKey,
				InternalKey:      testPubKey,
				TaprootOutputKey: testXOnlyPubKey,
				AssetVersion:     taprpc.AssetVersion_ASSET_VERSION_V1,
				AddressVersion:   taprpc.AddrVersion_ADDR_VERSION_V2,
			},
			wantRef: func() tapsdk.AssetRef {
				var expectedGroupKey tapsdk.PubKey
				copy(expectedGroupKey[:], testPubKey)
				return tapsdk.AssetRefFromGroupKey(expectedGroupKey)
			},
			wantErr: "",
		},
		{
			name: "zero asset ID without group key",
			rpcAddr: &taprpc.Addr{
				Encoded:          "tap1testzeroasset",
				AssetId:          make([]byte, 32),
				AssetType:        taprpc.AssetType_COLLECTIBLE,
				Amount:           1,
				ScriptKey:        testPubKey,
				InternalKey:      testPubKey,
				TaprootOutputKey: testXOnlyPubKey,
				AssetVersion:     taprpc.AssetVersion_ASSET_VERSION_V1,
				AddressVersion:   taprpc.AddrVersion_ADDR_VERSION_V2,
			},
			wantErr: "zero with no group key",
		},
		{
			name: "missing asset reference",
			rpcAddr: &taprpc.Addr{
				Encoded:          "tap1testmissingref",
				AssetType:        taprpc.AssetType_NORMAL,
				Amount:           1,
				ScriptKey:        testPubKey,
				InternalKey:      testPubKey,
				TaprootOutputKey: testXOnlyPubKey,
				AssetVersion:     taprpc.AssetVersion_ASSET_VERSION_V1,
				AddressVersion:   taprpc.AddrVersion_ADDR_VERSION_V2,
			},
			wantErr: "missing asset ID or group key",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			addr, err := unmarshalAddr(tc.rpcAddr)

			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, addr)
			require.Equal(t, tc.rpcAddr.Encoded, addr.Encoded)
			require.Equal(t, tc.rpcAddr.Amount, addr.Amount)
			require.Equal(t,
				tapsdk.AssetType(tc.rpcAddr.AssetType),
				addr.AssetType,
			)
			require.Equal(t,
				tapsdk.AddressVersion(tc.rpcAddr.AddressVersion),
				addr.AddressVersion,
			)

			if tc.wantRef != nil {
				require.True(t,
					addr.AssetRef.Equivalent(tc.wantRef()))
			}
		})
	}
}

func TestMarshalNewAddrRequest(t *testing.T) {
	tests := []struct {
		name     string
		req      *tapsdk.NewAddressRequest
		validate func(*testing.T, *taprpc.NewAddrRequest)
	}{
		{
			name: testNilRequest,
			req:  nil,
			validate: func(t *testing.T, rpcReq *taprpc.NewAddrRequest) {
				require.NotNil(t, rpcReq)
				require.Empty(t, rpcReq.AssetId)
				require.Zero(t, rpcReq.Amt)
			},
		},
		{
			name: "V0 address with asset ID and amount",
			req: &tapsdk.NewAddressRequest{
				AssetRef: func() tapsdk.AssetRef {
					var id tapsdk.AssetID
					copy(id[:], testAssetID)
					return tapsdk.AssetRefFromAssetID(id)
				}(),
				Amount: 1000,
			},
			validate: func(t *testing.T, rpcReq *taprpc.NewAddrRequest) {
				require.Equal(t, testAssetID, rpcReq.AssetId)
				require.Equal(t, uint64(1000), rpcReq.Amt)
			},
		},
		{
			name: "V2 address with group key",
			req: &tapsdk.NewAddressRequest{
				AssetRef: func() tapsdk.AssetRef {
					var gk tapsdk.PubKey
					copy(gk[:], testPubKey)
					return tapsdk.AssetRefFromGroupKey(gk)
				}(),
				AddressVersion: func() *tapsdk.AddressVersion {
					v := tapsdk.AddressVersionV2
					return &v
				}(),
			},
			validate: func(t *testing.T, rpcReq *taprpc.NewAddrRequest) {
				require.Equal(t, testPubKey, rpcReq.GroupKey)
				require.Equal(t,
					taprpc.AddrVersion_ADDR_VERSION_V2,
					rpcReq.AddressVersion,
				)
			},
		},
		{
			name: "address with proof courier and skip check",
			req: &tapsdk.NewAddressRequest{
				AssetRef: func() tapsdk.AssetRef {
					var id tapsdk.AssetID
					copy(id[:], testAssetID)
					return tapsdk.AssetRefFromAssetID(id)
				}(),
				Amount:                    500,
				ProofCourierAddr:          "universe://localhost:10029",
				SkipProofCourierConnCheck: true,
			},
			validate: func(t *testing.T, rpcReq *taprpc.NewAddrRequest) {
				require.Equal(t,
					"universe://localhost:10029",
					rpcReq.ProofCourierAddr,
				)
				require.True(t, rpcReq.SkipProofCourierConnCheck)
			},
		},
		{
			name: "address with custom script key and internal key",
			req: func() *tapsdk.NewAddressRequest {
				var scriptPubKey tapsdk.PubKey
				copy(scriptPubKey[:], testPubKey)

				var rawKeyBytes tapsdk.PubKey
				copy(rawKeyBytes[:], testPubKey)

				var internalRawKey tapsdk.PubKey
				copy(internalRawKey[:], testPubKey)

				return &tapsdk.NewAddressRequest{
					AssetRef: func() tapsdk.AssetRef {
						var id tapsdk.AssetID
						copy(id[:], testAssetID)
						return tapsdk.AssetRefFromAssetID(id)
					}(),
					Amount: 1000,
					ScriptKey: &tapsdk.ScriptKey{
						PubKey: scriptPubKey,
						KeyDesc: tapsdk.KeyDescriptor{
							RawKeyBytes: rawKeyBytes,
							KeyLocator: tapsdk.KeyLocator{
								Family: 212,
								Index:  7,
							},
						},
						TapTweak: []byte{0xaa, 0xbb},
					},
					InternalKey: &tapsdk.KeyDescriptor{
						RawKeyBytes: internalRawKey,
						KeyLocator: tapsdk.KeyLocator{
							Family: 212,
							Index:  3,
						},
					},
				}
			}(),
			validate: func(t *testing.T, rpcReq *taprpc.NewAddrRequest) {
				require.NotNil(t, rpcReq.ScriptKey)
				require.Equal(t,
					testPubKey,
					rpcReq.ScriptKey.PubKey,
				)
				require.Equal(t,
					[]byte{0xaa, 0xbb},
					rpcReq.ScriptKey.TapTweak,
				)

				require.NotNil(t, rpcReq.ScriptKey.KeyDesc)
				require.Equal(t,
					testPubKey,
					rpcReq.ScriptKey.KeyDesc.RawKeyBytes,
				)
				require.Equal(t,
					int32(212),
					rpcReq.ScriptKey.KeyDesc.KeyLoc.KeyFamily,
				)
				require.Equal(t,
					int32(7),
					rpcReq.ScriptKey.KeyDesc.KeyLoc.KeyIndex,
				)

				require.NotNil(t, rpcReq.InternalKey)
				require.Equal(t,
					testPubKey,
					rpcReq.InternalKey.RawKeyBytes,
				)
				require.Equal(t,
					int32(212),
					rpcReq.InternalKey.KeyLoc.KeyFamily,
				)
				require.Equal(t,
					int32(3),
					rpcReq.InternalKey.KeyLoc.KeyIndex,
				)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rpcReq, err := marshalNewAddrRequest(tc.req)
			require.NoError(t, err)
			require.NotNil(t, rpcReq)
			tc.validate(t, rpcReq)
		})
	}
}

func TestUnmarshalAddrEvent(t *testing.T) {
	tests := []struct {
		name     string
		rpcEvent *taprpc.AddrEvent
		wantErr  string
	}{
		{
			name:     testNilEvent,
			rpcEvent: nil,
			wantErr:  "nil address event",
		},
		{
			name: "event with invalid address",
			rpcEvent: &taprpc.AddrEvent{
				CreationTimeUnixSeconds: 1234567890,
				Status:                  taprpc.AddrEventStatus_ADDR_EVENT_STATUS_COMPLETED,
				Addr: &taprpc.Addr{
					ScriptKey: []byte{0x01}, // Invalid
				},
			},
			wantErr: "invalid address in event",
		},
		{
			name: "valid event without address",
			rpcEvent: &taprpc.AddrEvent{
				CreationTimeUnixSeconds: 1234567890,
				Status: taprpc.
					AddrEventStatus_ADDR_EVENT_STATUS_TRANSACTION_DETECTED,
				Outpoint:           "abc123:0",
				UtxoAmtSat:         10000,
				ConfirmationHeight: 0,
				HasProof:           false,
			},
			wantErr: "",
		},
		{
			name: "valid completed event with address",
			rpcEvent: &taprpc.AddrEvent{
				CreationTimeUnixSeconds: 1234567890,
				Status:                  taprpc.AddrEventStatus_ADDR_EVENT_STATUS_COMPLETED,
				Outpoint:                "abc123:1",
				UtxoAmtSat:              50000,
				ConfirmationHeight:      800000,
				HasProof:                true,
				Addr: &taprpc.Addr{
					Encoded:          testTap1test,
					AssetId:          testAssetID,
					ScriptKey:        testPubKey,
					InternalKey:      testPubKey,
					TaprootOutputKey: testXOnlyPubKey,
				},
			},
			wantErr: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			event, err := unmarshalAddrEvent(tc.rpcEvent)

			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, event)
			require.Equal(t,
				tc.rpcEvent.CreationTimeUnixSeconds,
				event.CreationTime,
			)
			require.Equal(t,
				tapsdk.AddressEventStatus(tc.rpcEvent.Status),
				event.Status,
			)
			require.Equal(t, tc.rpcEvent.Outpoint, event.Outpoint)
			require.Equal(t, tc.rpcEvent.UtxoAmtSat, event.UTXOAmountSat)
			require.Equal(t,
				tc.rpcEvent.ConfirmationHeight,
				event.ConfirmationHeight,
			)
			require.Equal(t, tc.rpcEvent.HasProof, event.HasProof)

			if tc.rpcEvent.Addr != nil {
				require.NotNil(t, event.Address)
				require.Equal(t,
					tc.rpcEvent.Addr.Encoded,
					event.Address.Encoded,
				)
			} else {
				require.Nil(t, event.Address)
			}
		})
	}
}

func TestAddressVersionConstants(t *testing.T) {
	// Verify SDK constants match taprpc constants.
	require.Equal(t,
		int(taprpc.AddrVersion_ADDR_VERSION_V0),
		int(tapsdk.AddressVersionV0),
	)
	require.Equal(t,
		int(taprpc.AddrVersion_ADDR_VERSION_V1),
		int(tapsdk.AddressVersionV1),
	)
	require.Equal(t,
		int(taprpc.AddrVersion_ADDR_VERSION_V2),
		int(tapsdk.AddressVersionV2),
	)
}

func TestAddressEventStatusConstants(t *testing.T) {
	// Verify SDK constants match taprpc constants.
	require.Equal(t,
		int(taprpc.AddrEventStatus_ADDR_EVENT_STATUS_UNKNOWN),
		int(tapsdk.AddressEventStatusUnknown),
	)
	require.Equal(t,
		int(taprpc.AddrEventStatus_ADDR_EVENT_STATUS_TRANSACTION_DETECTED),
		int(tapsdk.AddressEventStatusTransactionDetected),
	)
	require.Equal(t,
		int(taprpc.AddrEventStatus_ADDR_EVENT_STATUS_TRANSACTION_CONFIRMED),
		int(tapsdk.AddressEventStatusTransactionConfirmed),
	)
	require.Equal(t,
		int(taprpc.AddrEventStatus_ADDR_EVENT_STATUS_PROOF_RECEIVED),
		int(tapsdk.AddressEventStatusProofReceived),
	)
	require.Equal(t,
		int(taprpc.AddrEventStatus_ADDR_EVENT_STATUS_COMPLETED),
		int(tapsdk.AddressEventStatusCompleted),
	)
}

func TestSortDirectionConstants(t *testing.T) {
	// Verify SDK constants match taprpc constants.
	require.Equal(t,
		int(taprpc.SortDirection_SORT_DIRECTION_ASC),
		int(tapsdk.SortAscending),
	)
	require.Equal(t,
		int(taprpc.SortDirection_SORT_DIRECTION_DESC),
		int(tapsdk.SortDescending),
	)
}
