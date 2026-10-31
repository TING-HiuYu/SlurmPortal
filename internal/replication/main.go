package replication

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/authz"
	"github.com/TING-HiuYu/SlurmPortal/internal/httpx"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

const SnapshotPath = "/sync/v2/snapshot"

// HashPath returns only the current snapshot hash, for slaves of a master that publishes no DNS record.
const HashPath = "/sync/v2/snapshot-hash"

// TXTPrefix distinguishes v2 hashes from the v1 "v1:" records so both can coexist during cutover.
const TXTPrefix = "v2:"

// Register adds the peer-only snapshot endpoint that slaves pull from.
func Register(rt *httpx.Router, db store.Querier, baseDN string) {
	rt.Handle(httpx.Route{Method: http.MethodGet, Path: SnapshotPath, Permission: authz.Peer(),
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			s, hash, err := Load(r.Context(), db, baseDN)
			if err != nil {
				return err
			}
			body, err := json.Marshal(s)
			if err != nil {
				return err
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Data-Hash", hash)
			_, err = w.Write(body)
			return err
		}})
	rt.Handle(httpx.Route{Method: http.MethodGet, Path: HashPath, Permission: authz.Peer(), Routine: true,
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			_, hash, err := Load(r.Context(), db, baseDN)
			if err != nil {
				return err
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, err = io.WriteString(w, TXTPrefix+hash)
			return err
		}})
}

// DNSPublisher writes "v2:<sha256>" to one Cloudflare TXT record. Only the digest ever enters DNS.
type DNSPublisher struct {
	DB       store.Store
	BaseDN   string
	Name     string
	ZoneID   string
	RecordID string
	Token    string
	TTL      int
	Client   *http.Client
	APIBase  string
}

const publishedKey = "dns_published_hash"

func (p *DNSPublisher) lastPublished(ctx context.Context) (string, error) {
	var v string
	err := store.QueryRow(ctx, p.DB, "SELECT value FROM v2_sync_state WHERE name = ?", []any{publishedKey}, &v)
	if errors.Is(err, store.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// Publish is idempotent and safe to call after every write and from a periodic trigger.
func (p *DNSPublisher) Publish(ctx context.Context) (string, error) {
	if p.Name == "" || p.ZoneID == "" || p.Token == "" {
		return "", errors.New("DNS publisher is not configured")
	}
	if p.TTL == 0 {
		p.TTL = 60
	}
	_, hash, err := Load(ctx, p.DB, p.BaseDN)
	if err != nil {
		return "", err
	}
	last, err := p.lastPublished(ctx)
	if err != nil {
		return "", err
	}
	if last == hash {
		return hash, nil
	}
	if p.RecordID == "" {
		if p.RecordID, err = p.findOrCreate(ctx, hash); err != nil {
			return "", err
		}
	}
	if err := p.call(ctx, http.MethodPatch, "/zones/"+url.PathEscape(p.ZoneID)+"/dns_records/"+url.PathEscape(p.RecordID), p.record(hash), nil); err != nil {
		return "", err
	}
	_, err = p.DB.Exec(ctx, `INSERT INTO v2_sync_state(name, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, publishedKey, hash, time.Now().UTC())
	return hash, err
}

func (p *DNSPublisher) record(hash string) map[string]any {
	return map[string]any{"type": "TXT", "name": p.Name, "content": `"` + TXTPrefix + hash + `"`, "ttl": p.TTL}
}

func (p *DNSPublisher) findOrCreate(ctx context.Context, hash string) (string, error) {
	var listed []struct {
		ID string `json:"id"`
	}
	if err := p.call(ctx, http.MethodGet, "/zones/"+url.PathEscape(p.ZoneID)+"/dns_records?type=TXT&name="+url.QueryEscape(p.Name), nil, &listed); err != nil {
		return "", err
	}
	switch len(listed) {
	case 1:
		return listed[0].ID, nil
	case 0:
		var created struct {
			ID string `json:"id"`
		}
		if err := p.call(ctx, http.MethodPost, "/zones/"+url.PathEscape(p.ZoneID)+"/dns_records", p.record(hash), &created); err != nil {
			return "", err
		}
		if created.ID == "" {
			return "", errors.New("DNS create returned no record id")
		}
		return created.ID, nil
	default:
		return "", errors.New("several TXT records share the configured DNS name")
	}
}

func (p *DNSPublisher) call(ctx context.Context, method, path string, body any, result any) error {
	base := p.APIBase
	if base == "" {
		base = "https://api.cloudflare.com/client/v4"
	}
	var rd *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.Token)
	req.Header.Set("Content-Type", "application/json")
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var env struct {
		Success bool            `json:"success"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&env); err != nil {
		return fmt.Errorf("DNS API %s: HTTP %d: %w", method, resp.StatusCode, err)
	}
	if resp.StatusCode/100 != 2 || !env.Success {
		return fmt.Errorf("DNS API %s failed: HTTP %d", method, resp.StatusCode)
	}
	if result != nil {
		return json.Unmarshal(env.Result, result)
	}
	return nil
}
