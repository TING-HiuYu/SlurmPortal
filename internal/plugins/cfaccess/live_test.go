package cfaccess

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestLiveCerts checks the real team key set parses: CFACCESS_LIVE_TEAM=<team>.cloudflareaccess.com go test -run Live
func TestLiveCerts(t *testing.T) {
	team := os.Getenv("CFACCESS_LIVE_TEAM")
	if team == "" {
		t.Skip("CFACCESS_LIVE_TEAM not set")
	}
	v := &verifier{cfg: Config{TeamDomain: team}, http: &http.Client{Timeout: 10 * time.Second}, now: time.Now}
	keys, err := v.fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for kid, k := range keys {
		t.Logf("kid %s: RSA-%d e=%d", kid, k.N.BitLen(), k.E)
	}
}
