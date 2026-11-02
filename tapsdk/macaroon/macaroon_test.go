package macaroon

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
)

func TestWithMacaroonAuth(t *testing.T) {
	t.Parallel()

	const credential = "existing"

	tests := []struct {
		name string
		mac  SerializedMacaroon
		want []string
	}{
		{name: "empty preserves existing", want: []string{credential}},
		{
			name: "nonempty appends", mac: "service",
			want: []string{credential, "service"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			ctx := metadata.NewOutgoingContext(t.Context(), metadata.Pairs(
				"macaroon", credential, "trace", "transfer",
			))
			result := test.mac.WithMacaroonAuth(ctx)
			md, _ := metadata.FromOutgoingContext(result)
			require.Equal(t, test.want, md.Get("macaroon"))
			require.Equal(t, []string{"transfer"}, md.Get("trace"))
			original, _ := metadata.FromOutgoingContext(ctx)
			require.Equal(t, []string{credential}, original.Get("macaroon"))
		})
	}

	ctx := t.Context()
	result := SerializedMacaroon("").WithMacaroonAuth(ctx)
	_, ok := metadata.FromOutgoingContext(result)
	require.False(t, ok, "empty macaroons must not add metadata")
}
