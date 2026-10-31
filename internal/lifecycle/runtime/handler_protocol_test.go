package joblifecycle

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestLifecycleHandlerRequiresExplicitBearerAndRejectsForcedEmptyQuery(t *testing.T) {
	f := newLifecycleFixture(t)
	r, tokens := f.runtime(t, nil)
	server := f.mtls.serve(t, r.Handler())
	client := f.mtls.httpClient(t)
	callback := `{"job_id":7,"sluid":"` + lifecycleSLUID + `"}`
	tests := []struct {
		name, method, path, authorization string
		status                            int
	}{
		{"raw-read-token", http.MethodGet, StatsPath, tokens.ReadOnly, http.StatusForbidden},
		{"raw-write-token-on-stats", http.MethodGet, StatsPath, tokens.ReadWrite, http.StatusForbidden},
		{"wrong-auth-scheme", http.MethodGet, StatsPath, "Basic " + tokens.ReadOnly, http.StatusForbidden},
		{"extra-bearer-space", http.MethodGet, StatsPath, "Bearer  " + tokens.ReadOnly, http.StatusForbidden},
		{"raw-write-token", http.MethodPost, StartingPath, tokens.ReadWrite, http.StatusForbidden},
		{"forced-empty-query-stats", http.MethodGet, StatsPath + "?", "Bearer " + tokens.ReadOnly, http.StatusNotFound},
		{"forced-empty-query-start", http.MethodPost, StartingPath + "?", "Bearer " + tokens.ReadWrite, http.StatusNotFound},
		{"forced-empty-query-end", http.MethodPost, EndingPath + "?", "Bearer " + tokens.ReadWrite, http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			request, err := http.NewRequest(tc.method, server.URL+tc.path, strings.NewReader(callback))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", tc.authorization)
			request.Header.Set("Content-Type", "application/json")
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode != tc.status {
				t.Fatalf("status=%d want=%d", response.StatusCode, tc.status)
			}
		})
	}
	if r.Snapshot().Published != 0 {
		t.Fatal("rejected protocol request published an event")
	}
	request, err := http.NewRequest(http.MethodGet, server.URL+StatsPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+tokens.ReadOnly)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal("canonical Bearer read token no longer works")
	}
	if status := postLifecycle(t, client, server.URL, StartingPath, tokens.ReadWrite, callback); status != http.StatusAccepted {
		t.Fatalf("canonical Bearer write token no longer works: %d", status)
	}
}
