package mail

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

// Sample is the template context of the most recent real message of one kind, kept so that
// administrators can copy real data while writing templates. Only the latest one is stored.
type Sample struct {
	// Source is "latest" for a stored real context, or "example" when none has been recorded yet.
	Source    string         `json:"source"`
	UpdatedAt *time.Time     `json:"updatedAt,omitempty"`
	Context   map[string]any `json:"context"`
	Variables []Variable     `json:"variables"`
}

// SaveSample replaces the stored context for kind. Constants are not stored; they are applied when read.
func SaveSample(ctx context.Context, db store.Store, kind string, data map[string]any, now time.Time) error {
	clean := make(map[string]any, len(data))
	for k, v := range data {
		if k != "constants" {
			clean[k] = v
		}
	}
	raw, err := json.Marshal(clean)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, `INSERT INTO v2_mail_samples (kind, context_json, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(kind) DO UPDATE SET context_json = excluded.context_json, updated_at = excluded.updated_at`, kind, string(raw), now.UTC().Format(time.RFC3339Nano))
	return err
}

// LoadSample returns the stored context for kind, or example when none exists, with constants applied.
func LoadSample(ctx context.Context, db store.Store, kind string, example map[string]any, constants map[string]any, vars []Variable) (Sample, error) {
	var raw, at string
	err := store.QueryRow(ctx, db, "SELECT context_json, updated_at FROM v2_mail_samples WHERE kind = ?", []any{kind}, &raw, &at)
	out := Sample{Source: "example", Context: example, Variables: vars}
	switch {
	case errors.Is(err, store.ErrNoRows):
	case err != nil:
		return Sample{}, err
	default:
		var data map[string]any
		if json.Unmarshal([]byte(raw), &data) == nil {
			t, _ := time.Parse(time.RFC3339Nano, at)
			out = Sample{Source: "latest", UpdatedAt: &t, Context: data, Variables: vars}
		}
	}
	if constants == nil {
		constants = map[string]any{}
	}
	withConstants := make(map[string]any, len(out.Context)+1)
	for k, v := range out.Context {
		withConstants[k] = v
	}
	withConstants["constants"] = constants
	out.Context = withConstants
	return out, nil
}
