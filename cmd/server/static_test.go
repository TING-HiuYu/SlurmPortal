//go:build !js

package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestWithStaticServesAppAndKeepsAPI(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"index.html": "<app>", "assets/app-1.js": "js", "logo.svg": "svg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("api")) })
	h := withStatic(dir, api)
	for _, c := range []struct {
		method, path, want string
		status             int
	}{
		{"GET", "/api/v2/me", "api", 200},
		{"POST", "/sync/v2/current-status", "api", 200},
		{"GET", "/healthz", "api", 200},
		{"GET", "/", "<app>", 200},
		{"GET", "/admin/users", "<app>", 200},
		{"GET", "/logo.svg", "svg", 200},
		{"GET", "/assets/app-1.js", "js", 200},
		{"GET", "/assets/missing.js", "", 404},
		{"POST", "/admin/users", "", 405},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != c.status || (c.want != "" && rec.Body.String() != c.want) {
			t.Errorf("%s %s: %d %q", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
}

func TestBehindProxyMarksForwardedHTTPS(t *testing.T) {
	var secure []bool
	h := behindProxy(true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { secure = append(secure, r.TLS != nil) }))
	for _, proto := range []string{"https", "http", ""} {
		r := httptest.NewRequest("GET", "/api/v2/me", nil)
		if proto != "" {
			r.Header.Set("X-Forwarded-Proto", proto)
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	if len(secure) != 3 || !secure[0] || secure[1] || secure[2] {
		t.Fatalf("secure = %v", secure)
	}
	off := behindProxy(false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil {
			t.Fatal("trusted X-Forwarded-Proto with the option off")
		}
	}))
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	off.ServeHTTP(httptest.NewRecorder(), r)
}
