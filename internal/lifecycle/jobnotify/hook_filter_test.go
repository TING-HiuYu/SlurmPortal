package jobnotify

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestHookTLSShutdownCannotExtendDeadlineForCloseNotify(t *testing.T) {
	material := testTLS(t)
	left, right := net.Pipe()
	server := tls.Server(right, &tls.Config{Certificates: []tls.Certificate{material.certificate}})
	client := tls.Client(left, &tls.Config{RootCAs: material.pool, ServerName: "127.0.0.1"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ready := make(chan error, 1)
	stop := make(chan struct{})
	go func() { ready <- server.HandshakeContext(ctx); <-stop; _ = right.Close() }()
	if err := client.HandshakeContext(ctx); err != nil {
		close(stop)
		t.Fatal(err)
	}
	if err := <-ready; err != nil {
		close(stop)
		t.Fatal(err)
	}
	// The peer intentionally never reads another TLS record. Sending a
	// close_notify on this unbuffered connection would block until its deadline.
	begin := time.Now()
	if err := closeHookConnection(client); err != nil {
		close(stop)
		t.Fatal(err)
	}
	close(stop)
	if time.Since(begin) > 100*time.Millisecond {
		t.Fatal("TLS shutdown exceeded hook budget")
	}
}

func TestHookRuntimeQueryMatchesExecutionAndExcludesSuspend(t *testing.T) {
	material := testTLS(t)
	var reads atomic.Int64
	var body []byte
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		if r.Method != "GET" || r.URL.Path != "/slurm/v0.0.45/job/7" || r.URL.RawQuery != "" {
			t.Error("unexpected runtime query")
		}
		_, _ = w.Write(body)
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{material.certificate}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: material.pool}
	server.StartTLS()
	defer server.Close()
	cfg := HookConfig{SlurmURL: server.URL, SlurmTLS: material.files, Timeout: "1s"}
	for _, test := range []struct {
		name              string
		end, suspend, pre int64
		sluid             string
		eligible          bool
	}{
		{"sixty_one", 1061, 0, 0, testSLUID, true},
		{"pause_sixty", 1100, 1080, 40, testSLUID, false},
		{"pause_sixty_one", 1100, 1080, 41, testSLUID, true},
		{"other_execution", 1061, 0, 0, "s123456789ABCE", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			row := fixtureJob(7, test.sluid, "COMPLETING")
			row["start_time"], row["end_time"] = wrapped(1000), wrapped(test.end)
			row["suspend_time"], row["pre_sus_time"] = wrapped(test.suspend), wrapped(test.pre)
			body = responseBytes(row)
			before := reads.Load()
			eligible, err := HookEligible(context.Background(), cfg, Callback{7, testSLUID}, 1000, test.end)
			if err != nil || eligible != test.eligible || reads.Load() != before+1 {
				t.Fatalf("runtime gate: eligible=%v err=%v", eligible, err)
			}
		})
	}
	body, _ = json.Marshal(map[string]any{"jobs": []any{fixtureJob(7, testSLUID, "COMPLETED")}, "errors": []any{}})
	if eligible, err := HookEligible(context.Background(), cfg, Callback{7, testSLUID}, 1000, 1062); eligible || err == nil {
		t.Fatal("environment/REST time mismatch accepted")
	}
	before := reads.Load()
	if eligible, err := HookEligible(context.Background(), cfg, Callback{7, testSLUID}, 1000, 1060); eligible || err != nil || reads.Load() != before {
		t.Fatal("short job queried observer")
	}
}

func TestHookRuntimeBudgetAndAPIErrorNeverPermitCallback(t *testing.T) {
	material := testTLS(t)
	for _, slow := range []bool{false, true} {
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if slow {
				<-r.Context().Done()
				return
			}
			_, _ = w.Write([]byte(`{"jobs":[],"errors":[{"description":"private-detail"}]}`))
		}))
		server.TLS = &tls.Config{Certificates: []tls.Certificate{material.certificate}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: material.pool}
		server.StartTLS()
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		begin := time.Now()
		eligible, err := HookEligible(ctx, HookConfig{SlurmURL: server.URL, SlurmTLS: material.files, Timeout: "1s"}, Callback{7, testSLUID}, 1000, 1061)
		cancel()
		server.Close()
		if eligible || err == nil || time.Since(begin) > time.Second {
			t.Fatal("unverifiable runtime permitted callback or exceeded parent budget")
		}
	}
}
