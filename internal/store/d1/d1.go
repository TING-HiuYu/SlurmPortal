// Package d1 implements store.Store on top of a Cloudflare D1 binding reached through a Bridge.
// The bridge is platform-neutral so the same code is exercised natively in tests.
package d1

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

// D1 batch limits; exceeding them fails instead of silently splitting an atomic operation.
const (
	MaxBatchStatements = 100
	MaxPayloadBytes    = 950 << 10
	maxSafeInteger     = 1<<53 - 1
)

// Bridge sends one JSON request to the D1 binding and returns its JSON response.
type Bridge interface {
	Call(ctx context.Context, op string, payload []byte) ([]byte, error)
}

type wireStatement struct {
	SQL  string `json:"sql"`
	Args []any  `json:"args"`
}

type wireResult struct {
	Columns      []string            `json:"columns,omitempty"`
	Rows         [][]json.RawMessage `json:"rows,omitempty"`
	RowsAffected int64               `json:"rowsAffected"`
	LastInsertID int64               `json:"lastInsertId"`
}

type wireResponse struct {
	Results []wireResult `json:"results"`
	Error   string       `json:"error"`
}

// Store is store.Store over a Cloudflare D1 database.
type Store struct {
	store.SQL
	bridge Bridge
}

// New returns a Store that sends queries through b.
func New(b Bridge) *Store {
	return &Store{SQL: store.SQL{DB: sql.OpenDB(connector{b})}, bridge: b}
}

// Batch runs the statements as one D1 batch, which commits all or none.
func (s *Store) Batch(ctx context.Context, stmts []store.Statement) ([]store.Result, error) {
	if len(stmts) == 0 {
		return nil, nil
	}
	if len(stmts) > MaxBatchStatements {
		return nil, fmt.Errorf("d1 batch has %d statements, limit %d", len(stmts), MaxBatchStatements)
	}
	wire := make([]wireStatement, len(stmts))
	for i, st := range stmts {
		args, err := encodeArgs(st.Args)
		if err != nil {
			return nil, err
		}
		wire[i] = wireStatement{SQL: st.Query, Args: args}
	}
	res, err := call(ctx, s.bridge, "batch", wire)
	if err != nil {
		return nil, err
	}
	if len(res) != len(stmts) {
		return nil, errors.New("d1 batch result count mismatch")
	}
	out := make([]store.Result, len(res))
	for i, r := range res {
		out[i] = store.Result{RowsAffected: r.RowsAffected, LastInsertID: r.LastInsertID}
	}
	return out, nil
}

func call(ctx context.Context, b Bridge, op string, stmts []wireStatement) ([]wireResult, error) {
	payload, err := json.Marshal(map[string]any{"statements": stmts})
	if err != nil {
		return nil, err
	}
	if len(payload) > MaxPayloadBytes {
		return nil, fmt.Errorf("d1 request of %d bytes exceeds %d", len(payload), MaxPayloadBytes)
	}
	raw, err := b.Call(ctx, op, payload)
	if err != nil {
		return nil, err
	}
	var resp wireResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("d1 response: %w", err)
	}
	if resp.Error != "" {
		return nil, errors.New("d1: " + resp.Error)
	}
	return resp.Results, nil
}

func encodeArgs(args []any) ([]any, error) {
	out := make([]any, len(args))
	for i, a := range args {
		v, err := encodeValue(a)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func encodeValue(value any) (any, error) {
	if v, ok := value.(driver.Valuer); ok {
		dv, err := v.Value()
		if err != nil {
			return nil, err
		}
		value = dv
	}
	switch v := value.(type) {
	case nil, string, bool, float64:
		return v, nil
	case int:
		return encodeValue(int64(v))
	case int32:
		return encodeValue(int64(v))
	case uint:
		if uint64(v) > maxSafeInteger {
			return nil, errors.New("d1 integer exceeds JavaScript safe range")
		}
		return int64(v), nil
	case int64:
		if v > maxSafeInteger || v < -maxSafeInteger {
			return nil, errors.New("d1 integer exceeds JavaScript safe range")
		}
		return v, nil
	case []byte:
		return map[string]string{"blob": base64.StdEncoding.EncodeToString(v)}, nil
	case time.Time:
		return v.UTC().Format(time.RFC3339Nano), nil
	default:
		return nil, fmt.Errorf("unsupported d1 parameter type %T", value)
	}
}

func decodeValue(raw json.RawMessage) (driver.Value, error) {
	switch {
	case string(raw) == "null":
		return nil, nil
	case len(raw) > 0 && raw[0] == '{':
		var blob struct {
			Blob string `json:"blob"`
		}
		if err := json.Unmarshal(raw, &blob); err != nil {
			return nil, err
		}
		return base64.StdEncoding.DecodeString(blob.Blob)
	case len(raw) > 0 && raw[0] == '"':
		var s string
		return s, json.Unmarshal(raw, &s)
	case string(raw) == "true" || string(raw) == "false":
		return string(raw) == "true", nil
	}
	n := json.Number(raw)
	if i, err := n.Int64(); err == nil {
		return i, nil
	}
	if f, err := n.Float64(); err == nil {
		return f, nil
	}
	return nil, errors.New("unsupported d1 result value")
}

// database/sql driver used only for Query/Exec; transactions are rejected because D1 has none.
type connector struct{ b Bridge }

func (c connector) Connect(context.Context) (driver.Conn, error) { return conn(c), nil }
func (c connector) Driver() driver.Driver                        { return drv(c) }

type drv struct{ b Bridge }

func (d drv) Open(string) (driver.Conn, error) { return conn(d), nil }

type conn struct{ b Bridge }

var errNoTx = errors.New("d1 has no interactive transactions; use Store.Batch")

func (conn) Close() error                            { return nil }
func (conn) Begin() (driver.Tx, error)               { return nil, errNoTx }
func (c conn) Prepare(q string) (driver.Stmt, error) { return stmt{c, q}, nil }
func (conn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return nil, errNoTx
}

func (c conn) single(ctx context.Context, op, q string, named []driver.NamedValue) (wireResult, error) {
	args := make([]any, len(named))
	for i, n := range named {
		if n.Name != "" {
			return wireResult{}, errors.New("named parameters are not supported")
		}
		args[i] = n.Value
	}
	enc, err := encodeArgs(args)
	if err != nil {
		return wireResult{}, err
	}
	res, err := call(ctx, c.b, op, []wireStatement{{SQL: q, Args: enc}})
	if err != nil {
		return wireResult{}, err
	}
	if len(res) != 1 {
		return wireResult{}, errors.New("d1 result count mismatch")
	}
	return res[0], nil
}

func (c conn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	r, err := c.single(ctx, "exec", q, args)
	if err != nil {
		return nil, err
	}
	return result{id: r.LastInsertID, affected: r.RowsAffected}, nil
}

func (c conn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	r, err := c.single(ctx, "query", q, args)
	if err != nil {
		return nil, err
	}
	return &rows{columns: r.Columns, data: r.Rows}, nil
}

type stmt struct {
	c conn
	q string
}

func (stmt) Close() error  { return nil }
func (stmt) NumInput() int { return -1 }
func (s stmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.c.ExecContext(context.Background(), s.q, named(args))
}
func (s stmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.c.QueryContext(context.Background(), s.q, named(args))
}

func named(args []driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(args))
	for i, v := range args {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return out
}

type result struct{ id, affected int64 }

func (r result) LastInsertId() (int64, error) { return r.id, nil }
func (r result) RowsAffected() (int64, error) { return r.affected, nil }

type rows struct {
	columns []string
	data    [][]json.RawMessage
	i       int
}

func (r *rows) Columns() []string { return r.columns }
func (*rows) Close() error        { return nil }
func (r *rows) Next(dest []driver.Value) error {
	if r.i >= len(r.data) {
		return io.EOF
	}
	row := r.data[r.i]
	r.i++
	if len(row) != len(dest) {
		return errors.New("d1 row width mismatch")
	}
	for i, raw := range row {
		v, err := decodeValue(raw)
		if err != nil {
			return fmt.Errorf("column %s: %w", strings.ToLower(r.columns[i]), err)
		}
		dest[i] = v
	}
	return nil
}

var (
	_ store.Store           = (*Store)(nil)
	_ driver.ConnBeginTx    = conn{}
	_ driver.ExecerContext  = conn{}
	_ driver.QueryerContext = conn{}
)
