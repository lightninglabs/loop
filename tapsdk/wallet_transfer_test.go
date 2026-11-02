package tapsdk

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestListTransfersUsesGroupKeyForAssetRef(t *testing.T) {
	mc := new(mockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	issuanceID := testAssetID()
	groupKey := testKey(t, 91)
	groupRef := AssetRefFromGroupKey(groupKey)
	req := &ListTransfersRequest{AnchorTxid: testAbc}
	raw := []*AssetTransfer{{
		TransferTimestamp:   123,
		AnchorTxid:          testAbc,
		AnchorTxBlockHeight: 42,
		AnchorTxChainFees:   250,
		Label:               "label",
		Inputs: []TransferInput{{
			IssuanceID: issuanceID,
			AssetType:  AssetTypeNormal,
			Amount:     300,
			ScriptKey:  testKey(t, 11),
			GroupKey:   &groupKey,
		}},
		Outputs: []TransferOutput{{
			IssuanceID: issuanceID,
			AssetType:  AssetTypeNormal,
			Amount:     200,
			ScriptKey:  testKey(t, 12),
			GroupKey:   &groupKey,
		}},
	}}

	mc.On("ListTransfers", ctx, req).Return(raw, nil)

	transfers, err := w.ListTransfers(ctx, req)
	require.NoError(t, err)
	require.Len(t, transfers, 1)

	transfer := transfers[0]
	require.Equal(t, raw[0].TransferTimestamp,
		transfer.TransferTimestamp)
	require.Equal(t, raw[0].AnchorTxid, transfer.AnchorTxid)
	require.Equal(t, raw[0].AnchorTxBlockHeight,
		transfer.AnchorTxBlockHeight)
	require.Equal(t, raw[0].AnchorTxChainFees,
		transfer.AnchorTxChainFees)
	require.Equal(t, raw[0].Label, transfer.Label)

	require.Len(t, transfer.Inputs, 1)
	require.True(t, transfer.Inputs[0].AssetRef.Equivalent(groupRef))
	require.Equal(t, issuanceID, transfer.Inputs[0].IssuanceID)
	require.Equal(t, uint64(300), transfer.Inputs[0].Amount)

	require.Len(t, transfer.Outputs, 1)
	require.True(t, transfer.Outputs[0].AssetRef.Equivalent(groupRef))
	require.Equal(t, issuanceID, transfer.Outputs[0].IssuanceID)
	require.Equal(t, uint64(200), transfer.Outputs[0].Amount)

	mc.AssertExpectations(t)
}

func TestListTransfersUsesAssetIDRefWhenNoGroup(t *testing.T) {
	mc := new(mockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	issuanceID := testAssetID()
	raw := []*AssetTransfer{{
		Inputs: []TransferInput{{
			IssuanceID: issuanceID,
			Amount:     1,
		}},
	}}

	mc.On("ListTransfers", ctx, (*ListTransfersRequest)(nil)).
		Return(raw, nil)

	transfers, err := w.ListTransfers(ctx, nil)
	require.NoError(t, err)
	require.Len(t, transfers, 1)
	require.Len(t, transfers[0].Inputs, 1)
	require.True(t, transfers[0].Inputs[0].AssetRef.IsAssetIDRef())
	require.True(t, transfers[0].Inputs[0].AssetRef.Equivalent(
		AssetRefFromAssetID(issuanceID),
	))

	mc.AssertExpectations(t)
}

func TestListTransfersUsesAssetIDRefForCollectionItem(t *testing.T) {
	mc := new(mockClient)
	w := NewWallet(mc, NetworkRegtest)
	ctx := context.Background()

	issuanceID := testAssetID()
	groupKey := testKey(t, 97)
	itemRef := AssetRefFromAssetID(issuanceID)
	raw := []*AssetTransfer{{
		Inputs: []TransferInput{{
			IssuanceID: issuanceID,
			AssetType:  AssetTypeCollectible,
			Amount:     1,
			GroupKey:   &groupKey,
		}},
		Outputs: []TransferOutput{{
			IssuanceID: issuanceID,
			AssetType:  AssetTypeCollectible,
			Amount:     1,
			GroupKey:   &groupKey,
		}},
	}}

	mc.On("ListTransfers", ctx, (*ListTransfersRequest)(nil)).
		Return(raw, nil)

	transfers, err := w.ListTransfers(ctx, nil)
	require.NoError(t, err)
	require.Len(t, transfers, 1)

	require.Len(t, transfers[0].Inputs, 1)
	require.True(t, transfers[0].Inputs[0].AssetRef.Equivalent(
		itemRef,
	))
	require.Equal(t, AssetTypeCollectible,
		transfers[0].Inputs[0].Type)

	require.Len(t, transfers[0].Outputs, 1)
	require.True(t, transfers[0].Outputs[0].AssetRef.Equivalent(
		itemRef,
	))
	require.Equal(t, AssetTypeCollectible,
		transfers[0].Outputs[0].Type)

	mc.AssertExpectations(t)
}

// TestNewSendEvent verifies that the entities-level projection of a raw send
// event record into a high-level SendEvent populates AssetRefs from the
// recipient addresses, falls back to the embedded transfer's inputs/outputs
// when no addresses are present, and rebuilds the Transfer from the raw
// input/output asset identity.
func TestNewSendEvent(t *testing.T) {
	issuanceID := testAssetID()
	groupKey := testKey(t, 93)
	ref := AssetRefFromGroupKey(groupKey)
	record := &SendEventRecord{
		Timestamp:     456,
		SendState:     SendStateComplete,
		NextSendState: SendStateComplete,
		TransferLabel: "send-label",
		Addresses: []*Address{
			{AssetRef: ref},
			{AssetRef: ref},
		},
		Transfer: &AssetTransfer{
			Outputs: []TransferOutput{{
				IssuanceID: issuanceID,
				Amount:     10,
				GroupKey:   &groupKey,
			}},
		},
	}

	event := NewSendEvent(record)
	require.NotNil(t, event)
	require.Equal(t, record.Timestamp, event.Timestamp)
	require.Equal(t, record.SendState, event.SendState)
	require.Equal(t, record.TransferLabel, event.TransferLabel)
	require.Len(t, event.AssetRefs, 1)
	require.True(t, event.AssetRefs[0].Equivalent(ref))
	require.NotNil(t, event.Transfer)
	require.Len(t, event.Transfer.Outputs, 1)
	require.True(t, event.Transfer.Outputs[0].AssetRef.Equivalent(ref))
}

func TestNewSendEventPrefersTransferAssetRefs(t *testing.T) {
	issuanceID := testAssetID()
	groupKey := testKey(t, 95)
	collectionRef := AssetRefFromGroupKey(groupKey)
	itemRef := AssetRefFromAssetID(issuanceID)
	record := &SendEventRecord{
		Addresses: []*Address{{
			AssetRef: collectionRef,
		}},
		Transfer: &AssetTransfer{
			Outputs: []TransferOutput{{
				IssuanceID: issuanceID,
				AssetType:  AssetTypeCollectible,
				Amount:     1,
				GroupKey:   &groupKey,
			}},
		},
	}

	event := NewSendEvent(record)
	require.NotNil(t, event)
	require.Len(t, event.AssetRefs, 1)
	require.True(t, event.AssetRefs[0].Equivalent(itemRef))
	require.NotNil(t, event.Transfer)
	require.True(t, event.Transfer.Outputs[0].AssetRef.Equivalent(
		itemRef,
	))
}

func TestNewSendEventFallsBackToAddressesForEmptyTransfer(t *testing.T) {
	ref := AssetRefFromAssetID(testAssetID())
	record := &SendEventRecord{
		Addresses: []*Address{{
			AssetRef: ref,
		}},
		Transfer: &AssetTransfer{},
	}

	event := NewSendEvent(record)
	require.NotNil(t, event)
	require.NotNil(t, event.Transfer)
	require.Len(t, event.AssetRefs, 1)
	require.True(t, event.AssetRefs[0].Equivalent(ref))
}

// TestNewReceiveEvent verifies that the entities-level projection of a raw
// receive event record copies AssetRef and Amount from the embedded address.
func TestNewReceiveEvent(t *testing.T) {
	ref := AssetRefFromAssetID(testAssetID())
	record := &ReceiveEventRecord{
		Timestamp:          789,
		Status:             AddressEventStatusCompleted,
		Outpoint:           "abc:0",
		ConfirmationHeight: 100,
		Address: &Address{
			AssetRef: ref,
			Amount:   25,
		},
	}

	event := NewReceiveEvent(record)
	require.NotNil(t, event)
	require.Equal(t, record.Timestamp, event.Timestamp)
	require.True(t, event.AssetRef.Equivalent(ref))
	require.Equal(t, uint64(25), event.Amount)
	require.Equal(t, record.Status, event.Status)
	require.Equal(t, record.Outpoint, event.Outpoint)
	require.Equal(t, record.ConfirmationHeight, event.ConfirmationHeight)
}
