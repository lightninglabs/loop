package tapsdk

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

type fundingPrevouts map[wire.OutPoint]*wire.TxOut

func (p fundingPrevouts) Prevout(
	_ context.Context,
	point wire.OutPoint,
) (*wire.TxOut, error) {

	return p[point], nil
}

func TestFundingInputsRequireAuthenticatedPrevouts(t *testing.T) {
	f := newPathFixture(t)
	plan, err := f.wallet.NewCustomAnchorTxBuilder().
		Build(t.Context(), f.request)
	require.NoError(t, err)
	source := fundingPrevouts(f.chain.utxos)
	require.NoError(
		t,
		CheckFundingInputs(t.Context(), source, plan.AnchorPSBT()),
	)
	for _, test := range []struct {
		name   string
		change func(*wire.TxOut)
	}{
		{"fee amount", func(output *wire.TxOut) { output.Value++ }},
		{"fee script", func(output *wire.TxOut) { output.PkScript = []byte{0x51} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			point := requestPoint(t, f.request, 0)
			original := source[point]
			fake := wire.NewTxOut(original.Value, original.PkScript)
			test.change(fake)
			source[point] = fake
			require.Error(
				t,
				CheckFundingInputs(
					t.Context(),
					source,
					plan.AnchorPSBT(),
				),
			)
			source[point] = original
		})
	}
	delete(source, requestPoint(t, f.request, 0))
	require.Error(
		t,
		CheckFundingInputs(t.Context(), source, plan.AnchorPSBT()),
	)
}
