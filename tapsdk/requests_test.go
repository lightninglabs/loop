package tapsdk

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScriptKeyTypeQueryValidate(t *testing.T) {
	burn := ScriptKeyTypeBurn
	unknown := ScriptKeyType(99)

	tests := []struct {
		name    string
		query   *ScriptKeyTypeQuery
		wantErr string
	}{
		{
			name:  "nil",
			query: nil,
		},
		{
			name: "explicit type",
			query: &ScriptKeyTypeQuery{
				ExplicitType: &burn,
			},
		},
		{
			name: "all types",
			query: &ScriptKeyTypeQuery{
				AllTypes: true,
			},
		},
		{
			name: "conflicting query",
			query: &ScriptKeyTypeQuery{
				ExplicitType: &burn,
				AllTypes:     true,
			},
			wantErr: "cannot set both explicit type and all types",
		},
		{
			name: "unknown explicit type",
			query: &ScriptKeyTypeQuery{
				ExplicitType: &unknown,
			},
			wantErr: "unknown script key type",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.query.Validate()
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}

			require.NoError(t, err)
		})
	}
}
