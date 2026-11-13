package repository

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/wmiha/vyking-3p-integration-service/internal/domain"
)

func TestJSONIDs(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"nil list is an empty array, never null", jsonIDs[uint32](nil), "[]"},
		{"empty list", jsonIDs([]uint64{}), "[]"},
		{"one id", jsonIDs([]domain.BrandID{1}), "[1]"},
		{"several ids keep their order", jsonIDs([]domain.CustomerID{1002, 7, 1001}), "[1002,7,1001]"},
		{"largest uint32", jsonIDs([]uint32{math.MaxUint32}), "[4294967295]"},
		{"largest uint64", jsonIDs([]uint64{math.MaxUint64}), "[18446744073709551615]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.got)
		})
	}
}
