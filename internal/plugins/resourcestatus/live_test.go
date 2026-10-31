package resourcestatus

import (
	"context"
	"os"
	"testing"
	"time"
)

type fileRunner map[string]string

func (f fileRunner) Run(_ context.Context, path string, _ ...string) ([]byte, error) {
	return os.ReadFile(f[path])
}

// TestLiveCapture parses real captured output: STATUS_LIVE_PREFIX=/tmp/<host> (files -nodes.txt, -queue.json).
func TestLiveCapture(t *testing.T) {
	p := os.Getenv("STATUS_LIVE_PREFIX")
	if p == "" {
		t.Skip("STATUS_LIVE_PREFIX not set")
	}
	c, err := NewCollector("live", Config{Enabled: true}, fileRunner{"/usr/bin/scontrol": p + "-nodes.txt", "/usr/bin/squeue": p + "-queue.json"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Collect(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Check("live", time.Now(), MaxJobs); err != nil {
		t.Fatal(err)
	}
	t.Logf("cpu %+v gpu %+v jobs %d", r.Resources.CPU, r.Resources.GPU, r.JobsTotal)
}
