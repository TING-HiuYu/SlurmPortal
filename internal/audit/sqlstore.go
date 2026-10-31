package audit

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

// SQLStore relies on seq being the primary key: concurrent appenders racing on the same head lose and retry.
type SQLStore struct {
	DB      store.Store
	Retries int
	// MaxRecords bounds the log (0 = unbounded): each append past it removes the oldest records.
	MaxRecords int64
}

// trimBatch caps how many old records one append removes, so lowering MaxRecords on a large log
// spreads the cleanup over later appends instead of one huge write.
const trimBatch = 500

func (s SQLStore) head(ctx context.Context) (Head, error) {
	var h Head
	err := store.QueryRow(ctx, s.DB, "SELECT seq, hash FROM v2_audit_log ORDER BY seq DESC LIMIT 1", nil, &h.Seq, &h.Hash)
	if errors.Is(err, store.ErrNoRows) {
		return Head{}, nil
	}
	return h, err
}

// Append inserts the record built on the current head, retrying when a concurrent writer took the
// same sequence number, and trims the oldest records past MaxRecords.
func (s SQLStore) Append(ctx context.Context, build func(Head) (Record, error)) (Record, error) {
	retries := max(s.Retries, 3)
	var lastErr error
	for range retries {
		h, err := s.head(ctx)
		if err != nil {
			return Record{}, err
		}
		rec, err := build(h)
		if err != nil {
			return Record{}, err
		}
		detail := ""
		if len(rec.Detail) > 0 {
			b, err := json.Marshal(rec.Detail)
			if err != nil {
				return Record{}, err
			}
			detail = string(b)
		}
		stmts := []store.Statement{{Query: `INSERT INTO v2_audit_log
			(seq, time, actor_id, actor, action, target, outcome, status, request_id, trace_id, detail, prev_hash, hash)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			Args: []any{rec.Seq, rec.Time.Format(time.RFC3339Nano), rec.ActorID, rec.Actor, rec.Action, rec.Target, rec.Outcome,
				rec.Status, rec.RequestID, rec.TraceID, detail, rec.PrevHash, rec.Hash}}}
		if s.MaxRecords > 0 && rec.Seq > s.MaxRecords {
			// Same batch as the insert: a lost race drops neither. Oldest first (the delete trigger
			// refuses anything else), at most trimBatch rows per append.
			stmts = append(stmts, store.Statement{
				Query: "DELETE FROM v2_audit_log WHERE seq <= min(?, (SELECT min(seq) FROM v2_audit_log) + ?)",
				Args:  []any{rec.Seq - s.MaxRecords, trimBatch - 1},
			})
		}
		_, err = s.DB.Batch(ctx, stmts)
		if err == nil {
			return rec, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return Record{}, lastErr
}

// Scan calls fn for every stored record in sequence order.
func (s SQLStore) Scan(ctx context.Context, fn func(Record) error) error {
	rows, err := s.DB.Query(ctx, `SELECT seq, time, actor_id, actor, action, target, outcome, status, request_id, trace_id, detail, prev_hash, hash
		FROM v2_audit_log ORDER BY seq`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var rec Record
		var ts, detail string
		if err := rows.Scan(&rec.Seq, &ts, &rec.ActorID, &rec.Actor, &rec.Action, &rec.Target, &rec.Outcome,
			&rec.Status, &rec.RequestID, &rec.TraceID, &detail, &rec.PrevHash, &rec.Hash); err != nil {
			return err
		}
		if rec.Time, err = time.Parse(time.RFC3339Nano, ts); err != nil {
			return err
		}
		if detail != "" {
			if err := json.Unmarshal([]byte(detail), &rec.Detail); err != nil {
				return err
			}
		}
		if err := fn(rec); err != nil {
			return err
		}
	}
	return rows.Err()
}
