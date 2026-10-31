package resources

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"sync"
	"time"
)

var (
	ErrSenderClosed    = errors.New("resource summary sender is closed")
	ErrSenderFull      = errors.New("resource summary queue capacity exceeded")
	ErrSummaryConflict = errors.New("resource summary ID conflicts with previous content")
)

// SenderConfig tunes retries and batching of summary delivery.
type SenderConfig struct {
	MaxAttempts    int
	TTL            time.Duration
	RetryMin       time.Duration
	RetryMax       time.Duration
	RequestTimeout time.Duration
}

func (c SenderConfig) defaults() SenderConfig {
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 20
	}
	if c.TTL == 0 {
		c.TTL = 15 * time.Minute
	}
	if c.RetryMin == 0 {
		c.RetryMin = 250 * time.Millisecond
	}
	if c.RetryMax == 0 {
		c.RetryMax = 30 * time.Second
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = 5 * time.Second
	}
	return c
}

// SenderStats counts summary deliveries.
type SenderStats struct {
	Accepted         uint64 `json:"accepted"`
	Duplicates       uint64 `json:"duplicates"`
	Rejected         uint64 `json:"rejected"`
	Stored           uint64 `json:"stored"`
	RemoteDuplicates uint64 `json:"remote_duplicates"`
	Skipped          uint64 `json:"skipped"`
	Failed           uint64 `json:"failed"`
	Retries          uint64 `json:"retries"`
	Cancelled        uint64 `json:"cancelled"`
	Pending          int    `json:"pending"`
	Bytes            int64  `json:"bytes"`
}

type summaryDelivery struct {
	id      string
	body    []byte
	hash    [32]byte
	created time.Time
}
type summarySeen struct {
	hash    [32]byte
	until   time.Time
	pending bool
}

// Sender owns a separate bounded RAM FIFO. Accept means ownership transfer to
// this queue, never durable storage or successful master receipt. Only master's
// stored/duplicate acknowledgement completes an accepted delivery successfully.
type Sender struct {
	mu        sync.Mutex
	client    *http.Client
	endpoint  string
	maxEvents int
	maxBytes  int64
	bytes     int64
	queue     []*summaryDelivery
	seen      map[string]summarySeen
	maxSeen   int
	stats     SenderStats
	cfg       SenderConfig
	ctx       context.Context
	cancel    context.CancelFunc
	wake      chan struct{}
	done      chan struct{}
	closed    bool
}

// NewSender returns a Sender with default settings.
func NewSender(ctx context.Context, client *http.Client, masterOrigin string, maxEvents int, maxBytes int64) (*Sender, error) {
	return NewSenderWithConfig(ctx, client, masterOrigin, maxEvents, maxBytes, SenderConfig{})
}

// NewSenderWithConfig returns a Sender that posts job summaries to master over mTLS, holding at most
// maxEvents summaries or maxBytes in memory.
func NewSenderWithConfig(ctx context.Context, client *http.Client, masterOrigin string, maxEvents int, maxBytes int64, cfg SenderConfig) (*Sender, error) {
	u, err := url.Parse(masterOrigin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || client == nil {
		return nil, errors.New("resource sender requires a verified HTTPS origin and client")
	}
	if transport, ok := client.Transport.(*http.Transport); ok && transport.TLSClientConfig != nil && transport.TLSClientConfig.InsecureSkipVerify {
		return nil, errors.New("resource sender TLS verification must be enabled")
	}
	if maxEvents == 0 {
		maxEvents = 1000
	}
	if maxBytes == 0 {
		maxBytes = 64 << 20
	}
	cfg = cfg.defaults()
	if maxEvents < 1 || maxEvents > 1_000_000 || maxBytes < 1024 || maxBytes > 4<<30 || cfg.MaxAttempts < 1 || cfg.MaxAttempts > 1000 || cfg.TTL <= 0 || cfg.TTL > 24*time.Hour || cfg.RetryMin <= 0 || cfg.RetryMax < cfg.RetryMin || cfg.RetryMax > 5*time.Minute || cfg.RequestTimeout <= 0 || cfg.RequestTimeout > time.Minute {
		return nil, errors.New("invalid resource sender limits")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	owned, cancel := context.WithCancel(ctx)
	clone := *client
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	u.Path = SummaryPath
	maxSeen := maxEvents * 2
	if maxSeen < 128 {
		maxSeen = 128
	}
	if maxSeen > 100_000 {
		maxSeen = 100_000
	}
	s := &Sender{client: &clone, endpoint: u.String(), maxEvents: maxEvents, maxBytes: maxBytes, seen: map[string]summarySeen{}, maxSeen: maxSeen, cfg: cfg, ctx: owned, cancel: cancel, wake: make(chan struct{}, 1), done: make(chan struct{})}
	go s.run()
	return s, nil
}

func finiteNonnegative(n float64) bool { return !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 }

func validSummary(summary Summary) bool {
	if summary.Version != 1 || summary.EventID == "" || len(summary.EventID) > 512 || summary.ClusterID == "" || len(summary.ClusterID) > 64 || summary.Job.JobID == 0 || summary.Job.SLUID == "" || summary.Job.UID == 0 || len(summary.Job.Name) > 4096 || len(summary.Job.Nodes) > 4096 || summary.Job.CPUs < 0 || summary.Job.GPUs < 0 || summary.Job.MemoryMiB < 0 || summary.Duration.ActiveSeconds < 0 || summary.Duration.Scope != "last_attempt" || summary.EndedAt.IsZero() {
		return false
	}
	g := summary.Usage.GPU
	for _, n := range []float64{g.AllocatedGPUSeconds, g.ObservedGPUSeconds, g.UtilizationPercentSeconds, g.MemoryUtilizationPercentSeconds} {
		if !finiteNonnegative(n) {
			return false
		}
	}
	for _, p := range []*float64{g.MeanUtilizationPercent, g.MeanMemoryUtilizationPercent} {
		if p != nil && !validPercent(*p) {
			return false
		}
	}
	if g.ObservedGPUSeconds > g.AllocatedGPUSeconds+1e-5 || g.UtilizationPercentSeconds > 100*g.ObservedGPUSeconds+1e-5 || g.MemoryUtilizationPercentSeconds > 100*g.ObservedGPUSeconds+1e-5 {
		return false
	}
	return true
}

// Accept queues one job summary for delivery.
func (s *Sender) Accept(ctx context.Context, summary Summary) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if !validSummary(summary) {
		return errors.New("invalid resource summary")
	}
	body, err := json.Marshal(summary)
	if err != nil || len(body) > 256<<10 {
		return errors.New("resource summary exceeds payload budget")
	}
	hash := sha256.Sum256(body)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ctx.Err() != nil {
		return ErrSenderClosed
	}
	for id, entry := range s.seen {
		if !entry.pending && now.After(entry.until) {
			delete(s.seen, id)
		}
	}
	if existing, ok := s.seen[summary.EventID]; ok {
		if existing.hash != hash {
			s.stats.Rejected++
			return ErrSummaryConflict
		}
		s.stats.Duplicates++
		return nil
	}
	charge := int64(len(body)) + 256
	if len(s.queue) >= s.maxEvents || charge > s.maxBytes-s.bytes {
		s.stats.Rejected++
		return ErrSenderFull
	}
	if len(s.seen) >= s.maxSeen {
		oldest := ""
		var until time.Time
		for id, entry := range s.seen {
			if entry.pending {
				continue
			}
			if oldest == "" || entry.until.Before(until) {
				oldest = id
				until = entry.until
			}
		}
		if oldest == "" {
			s.stats.Rejected++
			return ErrSenderFull
		}
		delete(s.seen, oldest)
	}
	s.queue = append(s.queue, &summaryDelivery{id: summary.EventID, body: body, hash: hash, created: now})
	s.bytes += charge
	s.seen[summary.EventID] = summarySeen{hash: hash, until: now.Add(s.cfg.TTL), pending: true}
	s.stats.Accepted++
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}

func (s *Sender) run() {
	defer close(s.done)
	defer func() {
		s.mu.Lock()
		s.closed = true
		s.stats.Cancelled += uint64(len(s.queue))
		s.queue = nil
		s.bytes = 0
		s.seen = map[string]summarySeen{}
		s.mu.Unlock()
	}()
	for {
		if s.ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		var item *summaryDelivery
		if len(s.queue) > 0 {
			item = s.queue[0]
		}
		s.mu.Unlock()
		if item == nil {
			select {
			case <-s.ctx.Done():
				return
			case <-s.wake:
				continue
			}
		}
		outcome := "failed"
		for attempt := 1; attempt <= s.cfg.MaxAttempts; attempt++ {
			if s.ctx.Err() != nil {
				return
			}
			remaining := s.cfg.TTL - time.Since(item.created)
			if remaining <= 0 {
				break
			}
			timeout := s.cfg.RequestTimeout
			if remaining < timeout {
				timeout = remaining
			}
			ctx, cancel := context.WithTimeout(s.ctx, timeout)
			result := s.deliver(ctx, item.body)
			cancel()
			if result == "stored" || result == "duplicate" || result == "skipped" || result == "permanent" {
				outcome = result
				break
			}
			if attempt == s.cfg.MaxAttempts {
				break
			}
			delay := s.cfg.RetryMin
			for i := 1; i < attempt && delay < s.cfg.RetryMax; i++ {
				if delay > s.cfg.RetryMax/2 {
					delay = s.cfg.RetryMax
					break
				}
				delay *= 2
			}
			if delay > s.cfg.RetryMax {
				delay = s.cfg.RetryMax
			}
			remaining = s.cfg.TTL - time.Since(item.created)
			if remaining <= 0 {
				break
			}
			if delay > remaining {
				delay = remaining
			}
			s.mu.Lock()
			s.stats.Retries++
			s.mu.Unlock()
			timer := time.NewTimer(delay)
			select {
			case <-s.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		s.mu.Lock()
		if len(s.queue) > 0 && s.queue[0] == item {
			s.queue[0] = nil
			s.queue = s.queue[1:]
			s.bytes -= int64(len(item.body)) + 256
		}
		entry := s.seen[item.id]
		entry.pending = false
		entry.until = time.Now().Add(s.cfg.TTL)
		s.seen[item.id] = entry
		switch outcome {
		case "stored":
			s.stats.Stored++
		case "duplicate":
			s.stats.RemoteDuplicates++
		case "skipped":
			s.stats.Skipped++
		default:
			s.stats.Failed++
		}
		s.mu.Unlock()
	}
}

func (s *Sender) deliver(ctx context.Context, body []byte) string {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return "permanent"
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		return "retry"
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode >= 400 && response.StatusCode < 500 && response.StatusCode != 408 && response.StatusCode != 425 && response.StatusCode != 429 {
		return "permanent"
	}
	if response.StatusCode != http.StatusOK {
		return "retry"
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(raw) > 4096 {
		return "retry"
	}
	var result struct {
		Outcome string `json:"outcome"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if decoder.Decode(&result) != nil {
		return "retry"
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return "retry"
	}
	if result.Outcome == "stored" || result.Outcome == "duplicate" || result.Outcome == "skipped" {
		return result.Outcome
	}
	return "retry"
}

func (s *Sender) SnapshotStats() SenderStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := s.stats
	result.Pending = len(s.queue)
	result.Bytes = s.bytes
	return result
}

// Close stops delivery.
func (s *Sender) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	s.closed = true
	s.cancel()
	s.mu.Unlock()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
