package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mapLookup(env map[string]string) LookupFunc {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

func TestExpand(t *testing.T) {
	env := mapLookup(map[string]string{
		"HOST":  "db.internal",
		"PORT":  "3306",
		"EMPTY": "",
	})

	tests := []struct {
		name        string
		in          string
		want        string
		wantMissing []string
		wantErr     string
	}{
		{name: "no placeholder", in: "plain value", want: "plain value"},
		{name: "dollar without brace is literal", in: "pa$$word$X", want: "pa$$word$X"},
		{name: "required set", in: "${HOST}", want: "db.internal"},
		{name: "required set but empty", in: "${EMPTY}", want: ""},
		{name: "required unset", in: "${NOPE}", wantMissing: []string{"NOPE"}},
		{name: "default unused", in: "${PORT:-1}", want: "3306"},
		{name: "default when unset", in: "${NOPE:-fallback}", want: "fallback"},
		{name: "default when empty", in: "${EMPTY:-fallback}", want: "fallback"},
		{name: "empty default", in: "${NOPE:-}", want: ""},
		{name: "default containing colon", in: "${NOPE:-:8080}", want: ":8080"},
		{name: "embedded placeholders", in: "tcp(${HOST}:${PORT})/x", want: "tcp(db.internal:3306)/x"},
		{name: "several missing", in: "${A}-${B}", wantMissing: []string{"A", "B"}},
		{name: "unterminated", in: "${HOST", wantErr: "unterminated placeholder"},
		{name: "empty name", in: "${}", wantErr: "invalid variable name"},
		{name: "name starting with digit", in: "${1X}", wantErr: "invalid variable name"},
		{name: "name with dash", in: "${A-B}", wantErr: "invalid variable name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, missing, err := expand(tt.in, env)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantMissing, missing)
			if tt.wantMissing == nil {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}
