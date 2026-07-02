package test

import (
	"context"
	"testing"

	"github.com/lightningnetwork/lnd/keychain"
	"github.com/stretchr/testify/require"
)

func TestMockSignerDeriveSharedKeyDependsOnInputs(t *testing.T) {
	t.Parallel()

	signer := NewMockLnd().Signer.(*mockSigner)
	_, remotePubKeyA := CreateKey(7)
	_, remotePubKeyB := CreateKey(8)

	sharedKeyA0, err := signer.DeriveSharedKey(
		context.Background(), remotePubKeyA, &keychain.KeyLocator{
			Index: 0,
		},
	)
	require.NoError(t, err)

	sharedKeyA1, err := signer.DeriveSharedKey(
		context.Background(), remotePubKeyA, &keychain.KeyLocator{
			Index: 1,
		},
	)
	require.NoError(t, err)

	sharedKeyB0, err := signer.DeriveSharedKey(
		context.Background(), remotePubKeyB, &keychain.KeyLocator{
			Index: 0,
		},
	)
	require.NoError(t, err)

	require.NotEqual(t, sharedKeyA0, sharedKeyA1)
	require.NotEqual(t, sharedKeyA0, sharedKeyB0)
}
