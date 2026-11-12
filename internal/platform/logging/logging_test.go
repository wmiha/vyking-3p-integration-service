package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wmiha/vyking-3p-integration-service/internal/platform/requestid"
)

func TestContextAttributes(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, slog.LevelInfo, "json").With("service", "svc")

	ctx := requestid.NewContext(context.Background(), "req-1")
	ctx = WithAttrs(ctx, slog.String("run_id", "run-1"))
	ctx = WithAttrs(ctx, slog.Int("brand_id", 7))
	logger.InfoContext(ctx, "hello", "k", "v")

	var rec map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rec))
	assert.Equal(t, "hello", rec["msg"])
	assert.Equal(t, "svc", rec["service"])
	assert.Equal(t, "v", rec["k"])
	assert.Equal(t, "req-1", rec["request_id"])
	assert.Equal(t, "run-1", rec["run_id"])
	assert.EqualValues(t, 7, rec["brand_id"])
}

func TestLevelAndFormat(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, slog.LevelWarn, "text")

	logger.Info("dropped")
	assert.Empty(t, buf.String())

	logger.Warn("kept")
	assert.Contains(t, buf.String(), "level=WARN msg=kept")
}

func TestMaskEmail(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{in: "jane.doe@example.com", want: "j***@example.com"},
		{in: "j@x.io", want: "j***@x.io"},
		{in: "", want: ""},
		{in: "not-an-email", want: "***"},
		{in: "@example.com", want: "***"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			assert.Equal(t, tt.want, MaskEmail(tt.in))
		})
	}
}
