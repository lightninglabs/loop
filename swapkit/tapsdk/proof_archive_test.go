package tapsdk

import (
	"context"
	"errors"
	"testing"

	"github.com/lightninglabs/loop/swapkit"
	sdk "github.com/lightninglabs/tap-sdk"
	"github.com/stretchr/testify/require"
)

type archiveSDK struct {
	sdk.Client

	file      *sdk.ProofFile
	exportErr error
	importErr error
	imports   int
	ref       sdk.AssetRef
	key       sdk.PubKey
	point     sdk.Outpoint
}

func (a *archiveSDK) ExportProof(_ context.Context, ref sdk.AssetRef,
	key sdk.PubKey, point *sdk.Outpoint) (*sdk.ProofFile, error) {

	a.ref, a.key, a.point = ref, key, *point
	return a.file, a.exportErr
}

func (a *archiveSDK) UnpackProofFile(context.Context, []byte) (
	[][]byte, error) {

	a.imports++
	return nil, a.importErr
}

func TestProofArchiveRetryUsesExactOutput(t *testing.T) {
	p := bindingProof(t, bindingGenesis(), bindingKey(2).PubKey(),
		bindingKey(3).PubKey(), nil, bindingGenesis().FirstPrevOut,
		100, false)
	raw := bindingRaw(t, p)
	client := &archiveSDK{file: &sdk.ProofFile{RawProofFile: raw}}
	cfg := DriverConfig{Wallet: sdk.NewWallet(client, sdk.NetworkRegtest)}
	for range 2 {
		driver := &Driver{cfg: cfg}
		require.NoError(t, driver.importProof(t.Context(), raw))
	}
	require.Zero(t, client.imports)
	require.Equal(t, sdk.AssetRefFromAssetID(sdk.AssetID(p.Asset.ID())),
		client.ref)
	require.Equal(t, sdk.Outpoint{Txid: sdk.Hash(p.OutPoint().Hash),
		Index: p.OutPoint().Index}, client.point)
	require.Equal(t, bindingPub(2), [33]byte(client.key))

	// A returned proof for another output must not authorize a skipped
	// import, even when the backend reused the same asset script key.
	p.AnchorTx.TxIn[0].PreviousOutPoint.Index++
	client.file = &sdk.ProofFile{RawProofFile: bindingRaw(t, p)}
	driver := &Driver{cfg: cfg}
	require.ErrorIs(t, driver.importProof(t.Context(), raw), swapkit.ErrInvalid)
	require.Zero(t, client.imports)

	// A missing archive entry still requires a successful import; an
	// ambiguous import response is retried through the archive next time.
	lost := errors.New("lost import response")
	client.exportErr, client.importErr = errors.New("not found"), lost
	require.ErrorIs(t, driver.importProof(t.Context(), raw), lost)
	require.Equal(t, 1, client.imports)
	client.exportErr = nil
	client.file = &sdk.ProofFile{RawProofFile: raw}
	require.NoError(t, (&Driver{cfg: cfg}).importProof(t.Context(), raw))
	require.Equal(t, 1, client.imports)
}
