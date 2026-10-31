package currentstatus

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/objstore"
)

// TestLiveR2 exercises the real bucket: CAS writes with the write key, a presigned GET with the
// read key fetched the way a browser on the site origin would. It cleans up its object.
// Run with CURRENT_STATUS_LIVE_ACCOUNT, CURRENT_STATUS_LIVE_BUCKET and the four PLUGIN_CURRENT_STATUS_R2_* variables.
// For another S3-compatible store (e.g. RustFS in the container deployment) set CURRENT_STATUS_LIVE_ENDPOINT
// instead of the account and only the write key; the R2-only checks (read-only key, CORS) are skipped.
func TestLiveR2(t *testing.T) {
	account, bucket, endpoint := os.Getenv("CURRENT_STATUS_LIVE_ACCOUNT"), os.Getenv("CURRENT_STATUS_LIVE_BUCKET"), os.Getenv("CURRENT_STATUS_LIVE_ENDPOINT")
	origin := os.Getenv("CURRENT_STATUS_LIVE_ORIGIN")
	if (account == "" && endpoint == "") || bucket == "" {
		t.Skip("CURRENT_STATUS_LIVE_ACCOUNT / CURRENT_STATUS_LIVE_BUCKET not set")
	}
	creds := func(id, secret string) objstore.Credentials {
		return objstore.Credentials{AccessKeyID: os.Getenv(id), SecretAccessKey: os.Getenv(secret)}
	}
	cfg := MasterConfig{AccountID: account, Bucket: bucket, Endpoint: endpoint}
	writeKey := creds("PLUGIN_CURRENT_STATUS_R2_ACCESS_KEY_ID", "PLUGIN_CURRENT_STATUS_R2_SECRET_ACCESS_KEY")
	readKey := creds("PLUGIN_CURRENT_STATUS_R2_READ_ACCESS_KEY_ID", "PLUGIN_CURRENT_STATUS_R2_READ_SECRET_ACCESS_KEY")
	if endpoint != "" {
		readKey = writeKey
	}
	writer, err := newClient(cfg, writeKey)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := newClient(cfg, readKey)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const cluster = "live-test"
	t.Cleanup(func() { _ = writer.Delete(ctx, Key(cluster)) })
	s := &service{writer: writer, reader: reader, now: time.Now, stale: 3 * time.Minute, link: time.Minute}
	hash := strings.Repeat("a", 64)
	body := `{"version":1,"cluster_id":"live-test"}`
	for i, want := range []struct {
		observed int64
		hash     string
		outcome  string
	}{{2000, hash, "accepted"}, {2000, hash, "duplicate"}, {1000, hash, "ignored"}, {3000, strings.Repeat("b", 64), "accepted"}} {
		got, err := s.put(ctx, cluster, want.observed, want.hash, []byte(body))
		if err != nil || got != want.outcome {
			t.Fatalf("step %d: %q %v, want %q", i, got, err, want.outcome)
		}
	}
	if endpoint == "" {
		if err := reader.Put(ctx, Key(cluster), []byte("{}"), objstore.PutOptions{}); err == nil {
			t.Fatal("read-only key could write")
		}
	}
	link, _, err := reader.PresignGET(Key(cluster), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(got) != body || (endpoint == "" && (resp.Header.Get("Access-Control-Allow-Origin") != origin ||
		resp.Header.Get("Cache-Control") != "no-store, max-age=0")) {
		t.Fatalf("presigned GET %d %q headers %v", resp.StatusCode, got, resp.Header)
	}
}
