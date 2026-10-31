// Package otel exports traces and metrics with OTLP/HTTP JSON to any OpenTelemetry collector.
// It deliberately avoids the OpenTelemetry SDK to keep the Wasm build small and the dependency surface minimal.
package otel

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/telemetry"
)

const ID = "telemetry-otel"

// Config names the OTLP/HTTP collector endpoint and headers.
type Config struct {
	Endpoint    string            `yaml:"endpoint"`
	Headers     map[string]string `yaml:"headers"`
	ServiceName string            `yaml:"service-name"`
	Timeout     time.Duration     `yaml:"timeout"`
	MaxSpans    int               `yaml:"max-spans"`
}

// Plugin is the telemetry-otel plugin.
type Plugin struct{}

func (Plugin) ID() string { return ID }
func (Plugin) Roles() []plugin.Role {
	return []plugin.Role{plugin.RoleStandalone, plugin.RoleMaster, plugin.RoleSlave, plugin.RoleGateway}
}

func (Plugin) Provider(_ context.Context, role plugin.Role, decode func(any) error) (telemetry.Provider, error) {
	cfg := Config{ServiceName: "go-ldap-admin", Timeout: 5 * time.Second, Endpoint: os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")}
	if err := decode(&cfg); err != nil {
		return nil, err
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && !(u.Scheme == "http" && isLocal(u.Hostname()))) {
		return nil, errors.New("telemetry-otel endpoint must be https (or http on localhost)")
	}
	headers := map[string]string{}
	// OTEL_EXPORTER_OTLP_HEADERS uses the standard "k=v,k2=v2" form
	for _, pair := range strings.Split(os.Getenv("OTEL_EXPORTER_OTLP_HEADERS"), ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(pair), "="); ok && k != "" {
			if dv, err := url.QueryUnescape(v); err == nil {
				v = dv
			}
			headers[k] = v
		}
	}
	for k, v := range cfg.Headers {
		// "env:NAME" keeps collector credentials out of the config file
		if name, ok := strings.CutPrefix(v, "env:"); ok {
			v = os.Getenv(name)
		}
		headers[k] = v
	}
	e := &Exporter{Endpoint: strings.TrimRight(cfg.Endpoint, "/"), Headers: headers, Service: cfg.ServiceName, Role: string(role),
		Client: &http.Client{Timeout: cfg.Timeout}}
	return telemetry.NewBuffered(e, cfg.MaxSpans), nil
}

func isLocal(h string) bool { return h == "localhost" || h == "127.0.0.1" || h == "::1" }

// Exporter posts spans and metrics as OTLP/HTTP JSON.
type Exporter struct {
	Endpoint string
	Headers  map[string]string
	Service  string
	Role     string
	Client   *http.Client
}

type kv struct {
	Key   string         `json:"key"`
	Value map[string]any `json:"value"`
}

func value(v any) map[string]any {
	switch x := v.(type) {
	case string:
		return map[string]any{"stringValue": x}
	case bool:
		return map[string]any{"boolValue": x}
	case int:
		return map[string]any{"intValue": strconv.Itoa(x)}
	case int64:
		return map[string]any{"intValue": strconv.FormatInt(x, 10)}
	case float64:
		return map[string]any{"doubleValue": x}
	default:
		return map[string]any{"stringValue": fmt.Sprint(x)}
	}
}

func attrs(a []telemetry.Attr) []kv {
	out := make([]kv, 0, len(a))
	for _, x := range a {
		out = append(out, kv{x.Key, value(x.Value)})
	}
	return out
}

func (e *Exporter) resource() map[string]any {
	return map[string]any{"attributes": attrs([]telemetry.Attr{{Key: "service.name", Value: e.Service}, {Key: "service.namespace", Value: "go-ldap-admin"}, {Key: "deployment.role", Value: e.Role}})}
}

func nanos(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }

func (e *Exporter) post(ctx context.Context, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.Endpoint+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range e.Headers {
		req.Header.Set(k, v)
	}
	resp, err := e.Client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("OTLP %s: HTTP %d", path, resp.StatusCode)
	}
	return nil
}

func (e *Exporter) ExportSpans(ctx context.Context, spans []telemetry.FinishedSpan) error {
	out := make([]map[string]any, 0, len(spans))
	for _, s := range spans {
		span := map[string]any{
			"traceId": hex.EncodeToString(s.Context.TraceID[:]), "spanId": hex.EncodeToString(s.Context.SpanID[:]),
			"name": s.Name, "kind": 2, "startTimeUnixNano": nanos(s.Start), "endTimeUnixNano": nanos(s.End),
			"attributes": attrs(s.Attrs), "status": map[string]any{"code": 1},
		}
		if s.Parent != ([8]byte{}) {
			span["parentSpanId"] = hex.EncodeToString(s.Parent[:])
			span["kind"] = 1
		}
		if s.Err != "" {
			span["status"] = map[string]any{"code": 2, "message": s.Err}
		}
		out = append(out, span)
	}
	return e.post(ctx, "/v1/traces", map[string]any{"resourceSpans": []any{map[string]any{
		"resource": e.resource(), "scopeSpans": []any{map[string]any{"scope": map[string]any{"name": "go-ldap-admin"}, "spans": out}},
	}}})
}

func (e *Exporter) ExportMetrics(ctx context.Context, points []telemetry.MetricPoint) error {
	byName := map[string][]telemetry.MetricPoint{}
	var names []string
	for _, p := range points {
		if _, ok := byName[p.Name]; !ok {
			names = append(names, p.Name)
		}
		byName[p.Name] = append(byName[p.Name], p)
	}
	var metrics []any
	for _, n := range names {
		ps := byName[n]
		var dps []any
		for _, p := range ps {
			dp := map[string]any{"attributes": attrs(p.Attrs), "startTimeUnixNano": nanos(p.Start), "timeUnixNano": nanos(p.Time)}
			if p.Kind == telemetry.MetricHistogram {
				counts := make([]string, len(p.Buckets))
				for i, c := range p.Buckets {
					counts[i] = strconv.FormatUint(c, 10)
				}
				dp["count"], dp["sum"], dp["bucketCounts"], dp["explicitBounds"] = strconv.FormatUint(p.Count, 10), p.Sum, counts, telemetry.HistogramBounds
			} else {
				dp["asDouble"] = p.Sum
			}
			dps = append(dps, dp)
		}
		m := map[string]any{"name": n}
		if ps[0].Kind == telemetry.MetricHistogram {
			m["histogram"] = map[string]any{"aggregationTemporality": 2, "dataPoints": dps}
		} else {
			m["sum"] = map[string]any{"aggregationTemporality": 2, "isMonotonic": true, "dataPoints": dps}
		}
		metrics = append(metrics, m)
	}
	return e.post(ctx, "/v1/metrics", map[string]any{"resourceMetrics": []any{map[string]any{
		"resource": e.resource(), "scopeMetrics": []any{map[string]any{"scope": map[string]any{"name": "go-ldap-admin"}, "metrics": metrics}},
	}}})
}
