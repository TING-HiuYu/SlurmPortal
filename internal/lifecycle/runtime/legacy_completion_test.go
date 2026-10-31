package joblifecycle

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TING-HiuYu/SlurmPortal/internal/lifecycle/eventbus"
)

func legacyTestPost(t *testing.T, client *http.Client, origin, path, body, authorization string) (int, eventbus.Receipt) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, origin+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var receipt eventbus.Receipt
	if response.StatusCode == http.StatusAccepted {
		if err := json.NewDecoder(response.Body).Decode(&receipt); err != nil {
			t.Fatal(err)
		}
	} else {
		_, _ = io.Copy(io.Discard, response.Body)
	}
	return response.StatusCode, receipt
}

func TestLegacyCompletionRequiresExplicitFlagAndControllerCertificate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		allowed bool
		want    int
	}{
		{"default-disabled", false, true, http.StatusNotFound},
		{"explicit-enabled-controller", true, true, http.StatusAccepted},
		{"CA-trusted-peer-outside-controller-allowlist", true, false, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLifecycleFixture(t)
			r, _ := f.runtime(t, func(cfg *Config) {
				cfg.AllowLegacyCompletionHook = tc.enabled
				if tc.allowed {
					sum := sha256.Sum256(f.mtls.client.Certificate[0])
					cfg.TLS.ClientFingerprints = []string{hex.EncodeToString(sum[:])}
				}
			})
			server := f.mtls.serve(t, r.Handler())
			body := `{"job_id":7,"sluid":"` + lifecycleSLUID + `"}`
			status, _ := legacyTestPost(t, f.mtls.httpClient(t), server.URL, legacyCompletionPath, body, "")
			if status != tc.want {
				t.Fatalf("status=%d want=%d", status, tc.want)
			}
			wantPublished := uint64(0)
			if tc.want == http.StatusAccepted {
				wantPublished = 1
			}
			if r.Snapshot().Published != wantPublished {
				t.Fatal("unauthorized legacy path published an event")
			}
		})
	}
}

func TestLegacyCompletionUsesSameEndingEventAndStrictDecoder(t *testing.T) {
	for _, legacyFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "legacy-first", false: "new-first"}[legacyFirst], func(t *testing.T) {
			f := newLifecycleFixture(t)
			r, tokens := f.runtime(t, func(cfg *Config) {
				cfg.AllowLegacyCompletionHook = true
				sum := sha256.Sum256(f.mtls.client.Certificate[0])
				cfg.TLS.ClientFingerprints = []string{hex.EncodeToString(sum[:])}
			})
			server := f.mtls.serve(t, r.Handler())
			client := f.mtls.httpClient(t)
			body := `{"job_id":7,"sluid":"` + lifecycleSLUID + `"}`
			for _, path := range []string{StartingPath, EndingPath} {
				for _, authorization := range []string{"", "Bearer " + tokens.ReadOnly} {
					status, _ := legacyTestPost(t, client, server.URL, path, body, authorization)
					if status != http.StatusForbidden {
						t.Fatalf("legacy flag broadened %s: status=%d", path, status)
					}
				}
			}
			for _, malformed := range []string{
				`{"job_id":7,"job_id":8,"sluid":"` + lifecycleSLUID + `"}`,
				`{"job_id":7,"sluid":"` + lifecycleSLUID + `","unknown":true}`,
				`{"job_id":7,"sluid":"` + lifecycleSLUID + `"} {}`,
				`{"job_id":7,"sluid":null}`,
			} {
				if status, _ := legacyTestPost(t, client, server.URL, legacyCompletionPath, malformed, ""); status != http.StatusBadRequest {
					t.Fatalf("legacy malformed callback accepted: status=%d", status)
				}
			}
			for _, path := range []string{legacyCompletionPath + "?", legacyCompletionPath + "?x=1", legacyCompletionPath + "/"} {
				if status, _ := legacyTestPost(t, client, server.URL, path, body, ""); status != http.StatusNotFound {
					t.Fatalf("legacy noncanonical path accepted: status=%d", status)
				}
			}
			paths := []string{legacyCompletionPath, EndingPath}
			if !legacyFirst {
				paths[0], paths[1] = paths[1], paths[0]
			}
			var first eventbus.Receipt
			for i, path := range paths {
				authorization := ""
				if path == EndingPath {
					authorization = "Bearer " + tokens.ReadWrite
				}
				status, receipt := legacyTestPost(t, client, server.URL, path, body, authorization)
				if status != http.StatusAccepted || receipt.Duplicate != (i == 1) {
					t.Fatalf("cross-path receipt=%+v status=%d", receipt, status)
				}
				if i == 0 {
					first = receipt
				} else if first.EventID != receipt.EventID {
					t.Fatal("legacy callback created an independent ending event")
				}
			}
			if got := r.Snapshot(); got.Published != 1 || got.Duplicates != 1 {
				t.Fatalf("cross-path event was not deduplicated: %+v", got)
			}
			withoutCertificate := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: f.mtls.pool}}}
			defer withoutCertificate.CloseIdleConnections()
			response, err := withoutCertificate.Post(server.URL+legacyCompletionPath, "application/json", strings.NewReader(body))
			if err == nil {
				_ = response.Body.Close()
				t.Fatal("legacy route accepted a connection without client certificate")
			}
		})
	}
}

func TestLegacyCompletionConfigDefaultsOffAndAcceptsExplicitOptIn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifecycle.yaml")
	body := "cluster-id: fixture\nlisten: 127.0.0.1:19444\nslurm-url: https://127.0.0.1:19445\ntokens-file: /private/unused\n"
	for _, enabled := range []bool{false, true} {
		data := body
		if enabled {
			data += "allow-legacy-completion-hook: true\n"
		}
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(path)
		if err != nil || cfg.AllowLegacyCompletionHook != enabled {
			t.Fatalf("legacy compatibility opt-in=%v config=%v err=%v", enabled, cfg.AllowLegacyCompletionHook, err)
		}
	}
}
