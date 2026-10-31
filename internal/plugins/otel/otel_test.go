package otel_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/app"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/config"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugins/otel"
	"github.com/TING-HiuYu/SlurmPortal/internal/telemetry"
	"github.com/TING-HiuYu/SlurmPortal/internal/testutil"
)

func init() { plugin.RegisterTelemetry(otel.Plugin{}) }

type collector struct {
	mu      sync.Mutex
	traces  []map[string]any
	metrics []map[string]any
	auth    []string
}

func (c *collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var v map[string]any
	_ = json.Unmarshal(b, &v)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.auth = append(c.auth, r.Header.Get("Authorization"))
	if strings.HasSuffix(r.URL.Path, "/v1/traces") {
		c.traces = append(c.traces, v)
	} else {
		c.metrics = append(c.metrics, v)
	}
}

type span struct {
	TraceID, SpanID, ParentSpanID, Name string
	Attributes                          []struct {
		Key   string
		Value map[string]any
	}
}

func (c *collector) spans() []span {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []span
	for _, t := range c.traces {
		b, _ := json.Marshal(t)
		var doc struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []span
				}
			}
		}
		_ = json.Unmarshal(b, &doc)
		for _, rs := range doc.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				out = append(out, ss.Spans...)
			}
		}
	}
	return out
}

func newApp(t *testing.T, endpoint string, ratio float64) *app.App {
	t.Helper()
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "Authorization=Bearer%20collector-token")
	db := testutil.DB(t)
	testutil.User(t, db, "root", 1, "admin")
	cfg := &config.Config{Role: "master", HTTP: config.HTTP{RatePerSecond: 1e6, RateBurst: 1e6},
		Auth:    config.Auth{Issuer: "i", Audience: "a", TokenTTL: time.Hour},
		JWTKeys: authn.Keyring{ActiveKID: "k", Keys: map[string][]byte{"k": []byte("0123456789abcdef0123456789abcdef")}},
		Plugins: config.Plugins{Enabled: []string{otel.ID}}}
	cfg.Telemetry.SampleRatio = ratio
	if err := config.Parse([]byte("plugins:\n  enabled: [telemetry-otel]\n  config:\n    telemetry-otel:\n      endpoint: "+endpoint+"\n"), cfg); err != nil {
		t.Fatal(err)
	}
	a, err := app.New(context.Background(), cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { telemetry.SetSampleRatio(1) })
	return a
}

func TestOTLPExportContinuesTraceAndNestsDBSpans(t *testing.T) {
	c := &collector{}
	srv := httptest.NewServer(c)
	defer srv.Close()
	a := newApp(t, srv.URL, 1)
	tok, _, _, _ := a.JWT.Issue(1, authn.SourcePassword)
	req := httptest.NewRequest(http.MethodGet, "/api/v2/users", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	rec := httptest.NewRecorder()
	a.Handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if err := a.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	all := c.spans()
	var spans []span
	for _, s := range all {
		// startup work (policy load, migrations) forms its own root traces
		if s.TraceID == "4bf92f3577b34da6a3ce929d0e0e4736" {
			spans = append(spans, s)
		}
	}
	var server *span
	db := 0
	for i, s := range spans {
		if s.Name == "http.server" {
			server = &spans[i]
		}
	}
	if server == nil || server.ParentSpanID != "00f067aa0ba902b7" {
		t.Fatalf("server span missing or not parented to caller: %+v", server)
	}
	for _, s := range spans {
		if strings.HasPrefix(s.Name, "db.") {
			db++
			if s.ParentSpanID != server.SpanID {
				t.Fatalf("db span %s not a child of the server span", s.Name)
			}
			for _, a := range s.Attributes {
				if a.Key == "db.statement" && strings.Contains(a.Value["stringValue"].(string), "0123456789") {
					t.Fatal("bound values leaked into spans")
				}
			}
		}
	}
	if db == 0 {
		t.Fatal("no database spans exported")
	}
	if len(c.metrics) == 0 || c.auth[0] != "Bearer collector-token" {
		t.Fatalf("metrics=%d auth=%v", len(c.metrics), c.auth)
	}
}

func TestZeroSampleRatioExportsMetricsOnly(t *testing.T) {
	c := &collector{}
	srv := httptest.NewServer(c)
	defer srv.Close()
	a := newApp(t, srv.URL, 0)
	a.Handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if err := a.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(c.spans()) != 0 || len(c.metrics) == 0 {
		t.Fatalf("spans=%d metrics=%d", len(c.spans()), len(c.metrics))
	}
}

func TestRejectsPlainHTTPEndpoint(t *testing.T) {
	_, err := otel.Plugin{}.Provider(context.Background(), plugin.RoleMaster, func(dst any) error {
		dst.(*otel.Config).Endpoint = "http://collector.example.com"
		return nil
	})
	if err == nil {
		t.Fatal("plaintext remote endpoint accepted")
	}
}
