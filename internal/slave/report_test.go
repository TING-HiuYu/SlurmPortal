package slave

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestReporterOnce(t *testing.T) {
	var method, body string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		method, body = r.Method, string(b)
		w.WriteHeader(status)
	}))
	defer srv.Close()
	collectErr := errors.New("partial")
	rp := Reporter{Name: "x", Method: http.MethodPut, URL: srv.URL, Client: srv.Client(), Now: time.Now,
		Collect: func(context.Context, time.Time) (any, error) { return map[string]int{"v": 1}, collectErr }}
	if err := rp.Once(context.Background()); !errors.Is(err, collectErr) || method != http.MethodPut || body != `{"v":1}` {
		t.Fatal(err, method, body)
	}
	rp.Method, status = "", http.StatusServiceUnavailable
	if err := rp.Once(context.Background()); err == nil || method != http.MethodPost {
		t.Fatal("rejection not reported", err, method)
	}
	rp.Collect = func(context.Context, time.Time) (any, error) { return nil, collectErr }
	method = ""
	if err := rp.Once(context.Background()); !errors.Is(err, collectErr) || method != "" {
		t.Fatal("nil document must not be sent")
	}
}
