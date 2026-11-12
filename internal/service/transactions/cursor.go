package transactions

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"strconv"

	"github.com/wmiha/vyking-3p-integration-service/internal/domain"
)

// cursor is the opaque pagination token handed to partners. It is bound to
// the customer and status filter it was created for, so it cannot be replayed
// against a different query and silently skip or repeat transactions.
type cursor struct {
	Version int    `json:"v"`
	Created int64  `json:"c"`
	ID      uint64 `json:"i,string"`
	Filter  string `json:"f"`
}

const cursorVersion = 1

func encodeCursor(p Position, filter string) string {
	b, _ := json.Marshal(cursor{Version: cursorVersion, Created: p.CreatedUnix, ID: p.ID, Filter: filter})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s, filter string) (*Position, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, domain.ErrInvalidCursor
	}
	var c cursor
	if err := json.Unmarshal(raw, &c); err != nil || c.Version != cursorVersion || c.Filter != filter || c.ID == 0 {
		return nil, domain.ErrInvalidCursor
	}
	return &Position{CreatedUnix: c.Created, ID: c.ID}, nil
}

// filterKey fingerprints the parts of a query a cursor must not outlive.
func filterKey(customer domain.CustomerID, statusIDs []uint32) string {
	h := sha256.New()
	h.Write([]byte(strconv.FormatUint(uint64(customer), 10)))
	for _, id := range statusIDs {
		h.Write(binary.BigEndian.AppendUint32(nil, id))
	}
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil)[:9])
}
