package requestid

import (
	"context"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew(t *testing.T) {
	a, b := New(), New()
	assert.NotEqual(t, a, b)

	parsed, err := uuid.Parse(a)
	require.NoError(t, err)
	assert.Equal(t, byte(7), parsed[6]>>4, "expected a version 7 UUID")
	assert.True(t, Valid(a), "generated ids must pass validation")
}

func TestValid(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want bool
	}{
		{name: "uuid", id: "0195f3a2-7c1e-7d3a-9b1e-1f2e3d4c5b6a", want: true},
		{name: "partner token style", id: "acme_req.42-a", want: true},
		{name: "max length", id: strings.Repeat("a", maxLen), want: true},
		{name: "empty", id: "", want: false},
		{name: "too long", id: strings.Repeat("a", maxLen+1), want: false},
		{name: "space", id: "a b", want: false},
		{name: "newline injection", id: "abc\r\nSet-Cookie: x", want: false},
		{name: "non ascii", id: "zahtev-č", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Valid(tt.id))
		})
	}
}

func TestContext(t *testing.T) {
	ctx := context.Background()
	assert.Empty(t, FromContext(ctx))
	assert.Equal(t, "abc", FromContext(NewContext(ctx, "abc")))
}
