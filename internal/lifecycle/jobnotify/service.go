package jobnotify

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type pending struct {
	callback Callback
	created  time.Time
	due      time.Time
	working  bool
	attempts int
	bytes    int64
	early    json.RawMessage
}
type delivery struct {
	body    []byte
	key     string
	created time.Time
}

// Stats contains only process-local aggregate counters. No identifiers or
// payloads are ever recorded in logs or persistent metrics.
type Stats struct {
	Accepted   uint64 `json:"accepted"`
	Rejected   uint64 `json:"rejected"`
	Duplicates uint64 `json:"duplicates"`
	Filtered   uint64 `json:"filtered"`
	Expired    uint64 `json:"expired"`
	Sent       uint64 `json:"sent"`
	Skipped    uint64 `json:"skipped"`
	Pending    int    `json:"pending"`
	Bytes      int64  `json:"bytes"`
}

// Service receives job callbacks from the Slurm hook, resolves the job's details from slurmrestd
// and sends a completion event to master.
type Service struct {
	cfg            Config
	reader         *http.Client
	sender         *http.Client
	masterEndpoint string
	mu             sync.Mutex
	items          map[string]*pending
	seen           map[string]time.Time
	ready          []delivery
	bytes          int64
	stats          Stats
	work           chan string
	wake           chan struct{}
	ctx            context.Context
	cancel         context.CancelFunc
	wg             sync.WaitGroup
	server         *http.Server
}

// New is also the isolated integration seam: callers supply local HTTPS test
// clients without introducing test switches into production request handlers.
func New(cfg Config, reader, sender *http.Client, masterURL string) (*Service, error) {
	if err := cfg.defaults(); err != nil {
		return nil, err
	}
	u, err := httpsURL(masterURL)
	if err != nil {
		return nil, err
	}
	if u.Path != "" && u.Path != "/" {
		return nil, errors.New("notification master URL must be an origin")
	}
	u.Path = CompletionPath
	if reader == nil || sender == nil {
		return nil, errors.New("notification HTTP clients are required")
	}
	return &Service{cfg: cfg, reader: reader, sender: sender, masterEndpoint: u.String(), items: map[string]*pending{}, seen: map[string]time.Time{}, work: make(chan string, cfg.ResolveWorkers), wake: make(chan struct{}, 1)}, nil
}

// Start loads the configuration, listens for hook callbacks over mTLS and starts the workers.
func Start(ctx context.Context, path, masterURL string, masterTLS *tls.Config) (*Service, error) {
	cfg, err := LoadConfig(path)
	if err != nil {
		return nil, err
	}
	callbackTLS, err := serverTLS(cfg.CallbackTLS)
	if err != nil {
		return nil, err
	}
	restTLS, err := clientTLS(cfg.SlurmTLS)
	if err != nil {
		return nil, err
	}
	if len(restTLS.Certificates) == 0 || masterTLS == nil || masterTLS.InsecureSkipVerify || len(masterTLS.Certificates) == 0 {
		return nil, errors.New("notification clients require verified mTLS identities")
	}
	timeout, _ := time.ParseDuration(cfg.RequestTimeout)
	s, err := New(cfg, newHTTPClient(restTLS, timeout), newHTTPClient(masterTLS, timeout), masterURL)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, errors.New("notification listener unavailable")
	}
	s.Run(ctx)
	s.server = &http.Server{Addr: listener.Addr().String(), Handler: s.Handler(), TLSConfig: callbackTLS, ReadHeaderTimeout: time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second, MaxHeaderBytes: 4096, ErrorLog: log.New(io.Discard, "", 0)}
	s.wg.Add(1)
	go func() { defer s.wg.Done(); _ = s.server.Serve(tls.NewListener(listener, callbackTLS)) }()
	return s, nil
}

// Run starts the resolve and send workers.
func (s *Service) Run(parent context.Context) {
	s.ctx, s.cancel = context.WithCancel(parent)
	for i := 0; i < s.cfg.ResolveWorkers; i++ {
		s.wg.Add(1)
		go s.collector()
	}
	s.wg.Add(2)
	go s.scheduler()
	go s.sendLoop()
}

// Close stops the listener and workers.
func (s *Service) Close(ctx context.Context) error {
	if s.cancel != nil {
		s.cancel()
	}
	if s.server != nil {
		_ = s.server.Close()
	}
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		s.reader.CloseIdleConnections()
		s.sender.CloseIdleConnections()
		s.mu.Lock()
		s.items = map[string]*pending{}
		s.seen = map[string]time.Time{}
		s.ready = nil
		s.bytes = 0
		s.mu.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Handler accepts hook callbacks.
func (s *Service) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path != CallbackPath || r.URL.RawQuery != "" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			w.WriteHeader(405)
			return
		}
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
			w.WriteHeader(401)
			return
		}
		allowed := false
		if len(r.TLS.PeerCertificates) > 0 {
			fingerprint := certificateFingerprint(r.TLS.PeerCertificates[0].Raw)
			for _, f := range s.cfg.CallbackTLS.ClientFingerprints {
				if strings.EqualFold(f, fingerprint) {
					allowed = true
				}
			}
		}
		if !allowed {
			w.WriteHeader(403)
			return
		}
		if strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/json" {
			w.WriteHeader(415)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1025))
		if err != nil || len(body) > 1024 {
			w.WriteHeader(400)
			return
		}
		callback, err := decodeCallback(body)
		if err != nil {
			w.WriteHeader(400)
			return
		}
		// Deliberately do not inherit r.Context(): the one-way helper closes the
		// connection after the complete body. Only process shutdown cancels work.
		if !s.enqueue(callback) {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(202) // Helper deliberately does not read this response.
	})
}

func (s *Service) enqueue(callback Callback) bool {
	callback.SLUID = canonicalSLUID(callback.SLUID)
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if s.ctx == nil || s.ctx.Err() != nil {
		s.stats.Rejected++
		return false
	}
	if callback.Validate() != nil {
		s.stats.Rejected++
		return false
	}
	key := callbackKey(callback)
	for key, expiry := range s.seen {
		if now.After(expiry) {
			delete(s.seen, key)
		}
	}
	if _, ok := s.seen[key]; ok {
		s.stats.Duplicates++
		return true
	}
	if _, ok := s.items[key]; ok {
		s.stats.Duplicates++
		return true
	}
	const reserved = 512
	if len(s.items) >= s.cfg.MaxEvents || s.bytes+reserved > s.cfg.MaxBytes {
		s.stats.Rejected++
		return false
	}
	s.items[key] = &pending{callback: callback, created: now, due: now, bytes: reserved}
	s.bytes += reserved
	s.stats.Accepted++
	return true
}

func callbackKey(callback Callback) string {
	return strconv.FormatUint(uint64(callback.JobID), 10) + "/" + callback.SLUID
}

// Decode the two-key wire contract explicitly. encoding/json's struct decoder
// otherwise accepts duplicate keys and case-insensitive aliases, making an
// apparently different JobID/SLUID pair resolve to its last occurrence.
func decodeCallback(body []byte) (Callback, error) {
	var callback Callback
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return callback, errors.New("invalid completion callback")
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err = decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] {
			return callback, errors.New("invalid completion callback")
		}
		seen[key] = true
		switch key {
		case "job_id":
			err = decoder.Decode(&callback.JobID)
		case "sluid":
			err = decoder.Decode(&callback.SLUID)
		default:
			return callback, errors.New("invalid completion callback")
		}
		if err != nil {
			return callback, errors.New("invalid completion callback")
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') || !seen["job_id"] || !seen["sluid"] {
		return callback, errors.New("invalid completion callback")
	}
	if _, err = decoder.Token(); err != io.EOF || callback.Validate() != nil {
		return callback, errors.New("invalid completion callback")
	}
	return callback, nil
}

// Snapshot returns delivery counters.
func (s *Service) Snapshot() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := s.stats
	result.Pending = len(s.items)
	result.Bytes = s.bytes
	return result
}

func (s *Service) scheduler() {
	defer s.wg.Done()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-tick.C:
			s.mu.Lock()
			for key, p := range s.items {
				if p.due.IsZero() || p.working || now.Before(p.due) {
					continue
				}
				select {
				case s.work <- key:
					p.working = true
				default:
				}
			}
			s.mu.Unlock()
		}
	}
}

func (s *Service) collector() {
	defer s.wg.Done()
	resolveTTL, _ := time.ParseDuration(s.cfg.ResolveTTL)
	for {
		select {
		case <-s.ctx.Done():
			return
		case key := <-s.work:
			s.mu.Lock()
			p := s.items[key]
			if p == nil {
				s.mu.Unlock()
				continue
			}
			callback := p.callback
			created := p.created
			s.mu.Unlock()
			if time.Since(created) > resolveTTL {
				s.finish(key, "expired")
				continue
			}
			queryCtx, cancelQuery := context.WithDeadline(s.ctx, created.Add(resolveTTL))
			body, err := s.readJobContext(queryCtx, callback)
			cancelQuery()
			if s.ctx.Err() != nil {
				return
			}
			if time.Since(created) >= resolveTTL {
				s.finish(key, "expired")
				continue
			}
			var event Event
			if err == nil {
				event, err = Normalize(s.cfg.ClusterID, callback, body, time.Now(), s.cfg.MaxEventBytes)
			}
			if errors.Is(err, ErrSuperseded) || errors.Is(err, ErrFiltered) {
				s.finish(key, "filtered")
				continue
			}
			if err == nil {
				s.mu.Lock()
				p = s.items[key]
				if p != nil && len(p.early) > 0 {
					event.Raw.InitialController = p.early
				}
				s.mu.Unlock()
				_, encoded, err := encodeBoundedEvent(event, s.cfg.MaxEventBytes)
				if err != nil {
					s.finish(key, "filtered")
					continue
				}
				s.mu.Lock()
				p = s.items[key]
				if p != nil {
					delta := int64(len(encoded)) - p.bytes
					if s.bytes+delta > s.cfg.MaxBytes {
						s.stats.Rejected++
						s.removeLocked(key)
					} else {
						s.bytes += delta
						p.bytes = int64(len(encoded))
						p.early = nil
						p.due = time.Time{}
						p.working = false
						s.ready = append(s.ready, delivery{body: encoded, key: key, created: time.Now()})
						select {
						case s.wake <- struct{}{}:
						default:
						}
					}
				}
				s.mu.Unlock()
				continue
			}
			// Preserve only this job's authenticated, matching execution snapshot.
			// It is included only after a later stable terminal verification.
			early := matchingSnapshot(callback, body, s.cfg.MaxEventBytes)
			s.mu.Lock()
			p = s.items[key]
			if p != nil {
				if p.early == nil && len(early) > 0 && s.bytes+int64(len(early)) <= s.cfg.MaxBytes {
					p.early = early
					p.bytes += int64(len(early))
					s.bytes += int64(len(early))
				}
				p.attempts++
				p.working = false
				p.due = time.Now().Add(retryDelay(p.attempts, 10*time.Second))
			}
			s.mu.Unlock()
		}
	}
}

func (s *Service) readJob(callback Callback) ([]byte, error) {
	return s.readJobContext(s.ctx, callback)
}

func (s *Service) readJobContext(ctx context.Context, callback Callback) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.cfg.SlurmURL, "/")+"/slurm/"+SlurmAPIVersion+"/job/"+strconv.FormatUint(uint64(callback.JobID), 10), nil)
	if err != nil {
		return nil, ErrRetry
	}
	request.Header.Set("Accept", "application/json")
	response, err := s.reader.Do(request)
	if err != nil {
		return nil, ErrRetry
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != 200 {
		return nil, ErrRetry
	}
	const maxResponse = 4 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err != nil || len(body) > maxResponse {
		return nil, ErrRetry
	}
	return body, nil
}

func matchingSnapshot(callback Callback, body []byte, max int) json.RawMessage {
	var response struct {
		Jobs []json.RawMessage `json:"jobs"`
	}
	if json.Unmarshal(body, &response) != nil {
		return nil
	}
	for _, raw := range response.Jobs {
		var row struct {
			JobID uint32 `json:"job_id"`
			Step  struct {
				SLUID string `json:"sluid"`
			} `json:"step_id"`
		}
		if json.Unmarshal(raw, &row) == nil && row.JobID == callback.JobID && canonicalSLUID(row.Step.SLUID) == callback.SLUID && len(raw) <= max {
			return raw
		}
	}
	return nil
}

func (s *Service) sendLoop() {
	defer s.wg.Done()
	ttl, _ := time.ParseDuration(s.cfg.SendTTL)
	retryMax, _ := time.ParseDuration(s.cfg.RetryMax)
	for {
		s.mu.Lock()
		if len(s.ready) == 0 {
			s.mu.Unlock()
			select {
			case <-s.ctx.Done():
				return
			case <-s.wake:
				continue
			}
		}
		item := s.ready[0]
		s.mu.Unlock()
		outcome := "expired"
		for attempt := 1; attempt <= s.cfg.MaxAttempts; attempt++ {
			if time.Since(item.created) > ttl {
				break
			}
			sendCtx, cancelSend := context.WithDeadline(s.ctx, item.created.Add(ttl))
			result := s.deliverContext(sendCtx, item.body)
			cancelSend()
			if result != "retry" {
				outcome = result
				break
			}
			if attempt == s.cfg.MaxAttempts {
				break
			}
			delay := retryDelay(attempt, retryMax)
			if remaining := ttl - time.Since(item.created); remaining < delay {
				delay = remaining
			}
			if delay <= 0 {
				break
			}
			timer := time.NewTimer(delay)
			select {
			case <-s.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		s.mu.Lock()
		if len(s.ready) > 0 && s.ready[0].key == item.key {
			s.ready[0] = delivery{}
			s.ready = s.ready[1:]
		}
		s.mu.Unlock()
		s.finish(item.key, outcome)
		select {
		case <-s.ctx.Done():
			return
		default:
		}
	}
}

func (s *Service) deliver(body []byte) string {
	return s.deliverContext(s.ctx, body)
}

func (s *Service) deliverContext(ctx context.Context, body []byte) string {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.masterEndpoint, bytes.NewReader(body))
	if err != nil {
		return "retry"
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := s.sender.Do(request)
	if err != nil {
		return "retry"
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != 200 {
		return "retry"
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(data) > 4096 {
		return "retry"
	}
	outcome, err := decodeOutcome(data)
	if err != nil {
		return "retry"
	}
	switch outcome {
	case "smtp_accepted":
		return "sent"
	case "skipped":
		return "skipped"
	default:
		return "retry"
	}
}

func decodeOutcome(body []byte) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return "", errors.New("invalid notification acknowledgement")
	}
	seen := map[string]bool{}
	var outcome, reason string
	for decoder.More() {
		token, err = decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] {
			return "", errors.New("invalid notification acknowledgement")
		}
		seen[key] = true
		switch key {
		case "outcome":
			err = decoder.Decode(&outcome)
		case "reason":
			err = decoder.Decode(&reason)
		default:
			var extension json.RawMessage
			err = decoder.Decode(&extension)
		}
		if err != nil {
			return "", errors.New("invalid notification acknowledgement")
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') || !seen["outcome"] {
		return "", errors.New("invalid notification acknowledgement")
	}
	if _, err = decoder.Token(); err != io.EOF || (outcome == "skipped" && (reason == "" || len(reason) > 200)) {
		return "", errors.New("invalid notification acknowledgement")
	}
	return outcome, nil
}

func retryDelay(attempt int, max time.Duration) time.Duration {
	if attempt > 20 {
		attempt = 20
	}
	delay := time.Second * time.Duration(1<<uint(attempt-1))
	if delay > max {
		delay = max
	}
	return delay/2 + time.Duration(rand.Int64N(int64(delay/2)+1))
}

func (s *Service) finish(key, outcome string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[key]; !ok {
		return
	}
	s.removeLocked(key)
	switch outcome {
	case "sent":
		s.stats.Sent++
	case "skipped":
		s.stats.Skipped++
	case "filtered":
		s.stats.Filtered++
	default:
		s.stats.Expired++
	}
}
func (s *Service) removeLocked(key string) {
	if p, ok := s.items[key]; ok {
		s.bytes -= p.bytes
		delete(s.items, key)
	}
	if len(s.seen) >= s.cfg.MaxEvents*2 {
		for k := range s.seen {
			delete(s.seen, k)
			break
		}
	}
	s.seen[key] = time.Now().Add(time.Hour)
}
