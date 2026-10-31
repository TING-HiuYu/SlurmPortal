package slave

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Reporter periodically pushes one collected document to master over the slave's mTLS client.
type Reporter struct {
	Name string
	// Method defaults to POST.
	Method   string
	URL      string
	Client   *http.Client
	Interval time.Duration
	// Collect may return a document together with an error (e.g. a report that says collection failed).
	Collect func(ctx context.Context, at time.Time) (any, error)
	Now     func() time.Time
}

// Once collects and sends one report. A document collected with an error is still sent, then the
// error is returned.
func (rp Reporter) Once(ctx context.Context) error {
	doc, cerr := rp.Collect(ctx, rp.Now())
	if doc == nil {
		return cerr
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	method := rp.Method
	if method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, rp.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := rp.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s report rejected: status %d", rp.Name, resp.StatusCode)
	}
	return cerr
}

// Run reports at once and then every Interval until ctx ends.
func (rp Reporter) Run(ctx context.Context) {
	if rp.Now == nil {
		rp.Now = time.Now
	}
	t := time.NewTicker(rp.Interval)
	defer t.Stop()
	for {
		if err := rp.Once(ctx); err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "status report failed", "reporter", rp.Name, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
