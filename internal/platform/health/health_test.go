package health

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
)

func ok() Pinger { return PingFunc(func(context.Context) error { return nil }) }

func failing(msg string) Pinger {
	return PingFunc(func(context.Context) error { return errors.New(msg) })
}

func hanging() Pinger {
	return PingFunc(func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
}

func TestCheck(t *testing.T) {
	tests := []struct {
		name   string
		checks map[string]Pinger
		down   bool
		want   Report
	}{
		{
			name: "no dependencies",
			want: Report{Status: StatusOK, Checks: map[string]string{}},
		},
		{
			name:   "all healthy",
			checks: map[string]Pinger{"database": ok(), "cache": ok()},
			want:   Report{Status: StatusOK, Checks: map[string]string{"database": "ok", "cache": "ok"}},
		},
		{
			name:   "one failing",
			checks: map[string]Pinger{"database": failing("connection refused"), "cache": ok()},
			want: Report{Status: StatusUnavailable, Checks: map[string]string{
				"database": "connection refused", "cache": "ok",
			}},
		},
		{
			name:   "timeout",
			checks: map[string]Pinger{"database": hanging()},
			want: Report{Status: StatusUnavailable, Checks: map[string]string{
				"database": context.DeadlineExceeded.Error(),
			}},
		},
		{
			name:   "shutting down skips checks",
			checks: map[string]Pinger{"database": ok()},
			down:   true,
			want:   Report{Status: StatusShuttingDown},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := NewChecker(time.Second)
				for name, p := range tt.checks {
					c.Register(name, p)
				}
				if tt.down {
					c.SetShuttingDown()
				}
				got := c.Check(context.Background())
				assert.Equal(t, tt.want, got)
				assert.Equal(t, tt.want.Status == StatusOK, got.Ready())
			})
		})
	}
}
