package debugui

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"strings"
)

// row is one result row that keeps its column order when encoded as JSON
// (a plain map would sort the keys, burying "id" in the middle).
type row struct {
	cols []string
	vals []any
}

func (r row) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, c := range r.cols {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(c)
		v, err := json.Marshal(r.vals[i])
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// get returns a column's value (nil when absent).
func (r row) get(col string) any {
	for i, c := range r.cols {
		if c == col {
			return r.vals[i]
		}
	}
	return nil
}

// jsonColumn reports whether a TEXT column holds JSON that should be shown
// decoded rather than as an escaped string.
func jsonColumn(name string) bool {
	switch name {
	case "raw_payload", "raw_json", "counters", "before_json", "after_json", "options_json",
		"preview_json", "result_json":
		return true
	}
	return strings.HasSuffix(name, "_json")
}

// query runs q and returns every row. Never returns nil, so JSON shows [].
func query(ctx context.Context, db *sql.DB, q string, args ...any) ([]row, error) {
	rs, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	cols, err := rs.Columns()
	if err != nil {
		return nil, err
	}
	out := []row{}
	for rs.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rs.Scan(ptrs...); err != nil {
			return nil, err
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			if s, ok := v.(string); ok && jsonColumn(cols[i]) {
				var raw json.RawMessage
				if json.Unmarshal([]byte(s), &raw) == nil {
					v = raw
				}
			}
			vals[i] = v
		}
		out = append(out, row{cols: cols, vals: vals})
	}
	return out, rs.Err()
}

// one returns the first row of q, or nil when there is none.
func one(ctx context.Context, db *sql.DB, q string, args ...any) (*row, error) {
	rs, err := query(ctx, db, q, args...)
	if err != nil || len(rs) == 0 {
		return nil, err
	}
	return &rs[0], nil
}

// count runs a single-integer query.
func count(ctx context.Context, db *sql.DB, q string, args ...any) (int64, error) {
	var n sql.NullInt64
	if err := db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n.Int64, nil
}
