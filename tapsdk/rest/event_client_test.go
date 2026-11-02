package rest

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWebsocketURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		base    string
		path    string
		want    string
		wantErr string
	}{{
		name: "https becomes wss and method query set",
		base: "https://localhost:8089",
		path: "/v1/taproot-assets/events/asset-receive",
		want: "wss://localhost:8089/v1/taproot-assets/events/" +
			"asset-receive?method=POST",
	}, {
		name:    "http is rejected",
		base:    "http://host:8089",
		path:    "/v1/taproot-assets/events/asset-send",
		wantErr: "unsupported base URL scheme",
	}, {
		name: "trailing slash on base url is trimmed",
		base: "https://host/",
		path: "/v1/taproot-assets/events/asset-mint",
		want: "wss://host/v1/taproot-assets/events/" +
			"asset-mint?method=POST",
	}, {
		name:    "unsupported scheme is rejected",
		base:    "ftp://host",
		path:    "/whatever",
		wantErr: "unsupported base URL scheme",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := websocketURL(tc.base, tc.path)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}
