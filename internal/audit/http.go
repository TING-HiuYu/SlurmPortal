package audit

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
	"github.com/TING-HiuYu/SlurmPortal/internal/authz"
	"github.com/TING-HiuYu/SlurmPortal/internal/httpx"
)

// List returns up to limit records with seq > after, oldest first.
func (s SQLStore) List(ctx context.Context, after int64, limit int) ([]Record, error) {
	return s.list(ctx, `WHERE seq > ? ORDER BY seq LIMIT ?`, after, limit)
}

// ListBefore returns up to limit records with seq < before (0: from the newest), newest first.
func (s SQLStore) ListBefore(ctx context.Context, before int64, limit int) ([]Record, error) {
	if before <= 0 {
		// No sentinel bound: int64 max does not survive the Worker's JS number bridge to D1.
		return s.list(ctx, `ORDER BY seq DESC LIMIT ?`, limit)
	}
	return s.list(ctx, `WHERE seq < ? ORDER BY seq DESC LIMIT ?`, before, limit)
}

func (s SQLStore) list(ctx context.Context, where string, args ...any) ([]Record, error) {
	rows, err := s.DB.Query(ctx, `SELECT seq, time, actor_id, actor, action, target, outcome, status, request_id, trace_id, detail, prev_hash, hash
		FROM v2_audit_log `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		var rec Record
		var ts, detail string
		if err := rows.Scan(&rec.Seq, &ts, &rec.ActorID, &rec.Actor, &rec.Action, &rec.Target, &rec.Outcome,
			&rec.Status, &rec.RequestID, &rec.TraceID, &detail, &rec.PrevHash, &rec.Hash); err != nil {
			return nil, err
		}
		if rec.Time, err = time.Parse(time.RFC3339Nano, ts); err != nil {
			return nil, err
		}
		if detail != "" {
			if err := json.Unmarshal([]byte(detail), &rec.Detail); err != nil {
				return nil, err
			}
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

type listedRecord struct {
	Record
	Hash string `json:"hash"`
}

type verifyResult struct {
	Enabled         bool   `json:"enabled"`
	VerifiedThrough int64  `json:"verifiedThrough"`
	Intact          bool   `json:"intact"`
	BrokenAt        int64  `json:"brokenAt,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

// Register exposes read-only audit endpoints; there is intentionally no delete or clean operation.
func Register(rt *httpx.Router, rec *Recorder, s SQLStore) {
	rt.Handle(httpx.Route{Method: http.MethodGet, Path: "/api/v2/audit", Permission: authz.Policy("audit", authz.ActionRead),
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			q := r.URL.Query()
			cursor, newestFirst := "after", q.Has("before")
			if newestFirst {
				cursor = "before"
			}
			after, err := strconv.ParseInt(q.Get(cursor), 10, 64)
			if q.Get(cursor) == "" {
				after, err = 0, nil
			}
			limit, lerr := strconv.Atoi(q.Get("limit"))
			if q.Get("limit") == "" {
				limit, lerr = 100, nil
			}
			if err != nil || lerr != nil || after < 0 || limit < 1 || limit > 500 || q.Has("after") && newestFirst {
				return apperr.BadRequest("after/before must be >= 0 (not both) and limit within [1, 500]")
			}
			list := s.List
			if newestFirst {
				list = s.ListBefore
			}
			recs, err := list(r.Context(), after, limit)
			if err != nil {
				return err
			}
			out := make([]listedRecord, len(recs))
			for i, x := range recs {
				out[i] = listedRecord{x, x.Hash}
			}
			httpx.WriteJSON(w, http.StatusOK, out)
			return nil
		}})
	rt.Handle(httpx.Route{Method: http.MethodGet, Path: "/api/v2/audit/verify", Permission: authz.Policy("audit", authz.ActionRead),
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			res := verifyResult{Enabled: rec.Enabled(), Intact: true}
			n, err := Verify(r.Context(), s)
			res.VerifiedThrough = n
			var broken *BrokenChainError
			switch {
			case errors.As(err, &broken):
				res.Intact, res.BrokenAt, res.Reason = false, broken.Seq, broken.Reason
			case err != nil:
				return err
			}
			httpx.WriteJSON(w, http.StatusOK, res)
			return nil
		}})
}
