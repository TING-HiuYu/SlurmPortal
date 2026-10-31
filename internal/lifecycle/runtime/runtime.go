package joblifecycle

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/lifecycle/eventbus"
	"github.com/TING-HiuYu/SlurmPortal/internal/lifecycle/jobdoneemail"
	"github.com/TING-HiuYu/SlurmPortal/internal/lifecycle/jobnotify"
	"github.com/TING-HiuYu/SlurmPortal/internal/lifecycle/resources"
)

type execution struct {
	callback                  jobnotify.Callback
	created, due, endingAt    time.Time
	ending, working, watching bool
	attempts                  int
	// A finalized report is frozen before publication so retries cannot alter
	// an event already accepted by a listener.
	report        []byte
	summary       *resources.Summary
	queuedSummary bool
}

// Runtime runs inside the slave. It receives Slurm job events from the lifecycle hook, publishes
// them on the event bus and runs the compiled-in plugins (job e-mail, resource usage).
type Runtime struct {
	cfg            Config
	bus            *eventbus.Bus
	tokens         eventbus.LocalTokens
	reader, sender *http.Client
	masterURL      string
	backend        resources.Backend
	collector      *resources.Collector
	mail           *jobdoneemail.Plugin
	metrics        *resources.Sender
	mu             sync.Mutex
	jobs           map[string]*execution
	seen           map[string]time.Time
	eventTimes     map[string]time.Time
	reportBytes    int64
	work           chan string
	ctx            context.Context
	cancel         context.CancelFunc
	wg             sync.WaitGroup
	server         *http.Server
}

// Start loads the configuration, starts the plugins and listens for hook calls.
func Start(parent context.Context, path, masterURL string, masterTLS *tls.Config) (*Runtime, error) {
	cfg, err := LoadConfig(path)
	if err != nil {
		return nil, err
	}
	if masterTLS == nil || masterTLS.InsecureSkipVerify || len(masterTLS.Certificates) == 0 {
		return nil, errors.New("lifecycle verified master mTLS identity is required")
	}
	readerTLS, err := jobnotify.ClientTLS(cfg.SlurmTLS)
	if err != nil || len(readerTLS.Certificates) == 0 {
		return nil, errors.New("lifecycle observer mTLS identity is required")
	}
	serverTLS, err := jobnotify.ServerTLS(cfg.TLS)
	if err != nil {
		return nil, err
	}
	tokens, err := eventbus.LoadOrCreateLocalTokens(cfg.TokenFile)
	if err != nil {
		return nil, err
	}
	timeout, _ := time.ParseDuration(cfg.RequestTimeout)
	r, err := New(parent, cfg, tokens, jobnotify.HTTPClient(readerTLS, timeout), jobnotify.HTTPClient(masterTLS, 20*time.Second), masterURL)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		_ = r.Close(context.Background())
		return nil, err
	}
	r.server = &http.Server{Addr: listener.Addr().String(), Handler: r.Handler(), TLSConfig: serverTLS, ReadHeaderTimeout: time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 3 * time.Second, MaxHeaderBytes: 8192, ErrorLog: log.New(io.Discard, "", 0)}
	r.wg.Add(1)
	go func() { defer r.wg.Done(); _ = r.server.Serve(tls.NewListener(listener, serverTLS)) }()
	return r, nil
}

// New injects transports for isolated end-to-end tests. Real startup always
// constructs verified mTLS clients and a private authenticated listener.
func New(parent context.Context, cfg Config, tokens eventbus.LocalTokens, reader, sender *http.Client, masterURL string) (r *Runtime, err error) {
	if err = cfg.defaults(); err != nil {
		return nil, err
	}
	if err = tokens.Validate(); err != nil {
		return nil, err
	}
	if reader == nil || sender == nil {
		return nil, errors.New("lifecycle HTTP clients required")
	}
	u, parseErr := url.Parse(masterURL)
	if parseErr != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.RawPath != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("lifecycle master must be an HTTPS origin")
	}
	ctx, cancel := context.WithCancel(parent)
	r = &Runtime{cfg: cfg, tokens: tokens, reader: reader, sender: sender, masterURL: strings.TrimRight(masterURL, "/"), ctx: ctx, cancel: cancel, jobs: map[string]*execution{}, seen: map[string]time.Time{}, eventTimes: map[string]time.Time{}, work: make(chan string, cfg.ResolveWorkers), backend: resources.Backend{Socket: cfg.ClustatSocket}, collector: resources.NewCollector(cfg.MaxJobs, time.Second)}
	if err = r.collector.SetInterval(cfg.sampleInterval); err != nil {
		cancel()
		return nil, err
	}
	r.bus, err = eventbus.New(eventbus.Config{MaxEvents: cfg.MaxJobs})
	if err != nil {
		cancel()
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = r.Close(context.Background())
		}
	}()
	for _, t := range []eventbus.Type{{ID: StartingType, SchemaVersion: 1, Sources: []string{"slurm-controller"}, Validate: validCallback}, {ID: EndingType, SchemaVersion: 1, Sources: []string{"slurm-controller", "reconciler"}, Validate: validCallback}, {ID: resources.ReadyType, SchemaVersion: 1, Sources: []string{"lifecycle"}}, {ID: ReportType, SchemaVersion: 1, Sources: []string{"lifecycle"}}} {
		if err = r.bus.RegisterType(t); err != nil {
			return nil, err
		}
	}
	if cfg.Plugins.JobEmail {
		mailCfg := jobnotify.Config{ClusterID: cfg.ClusterID, Listen: "127.0.0.1:0", SlurmURL: cfg.SlurmURL, CallbackTLS: cfg.TLS}
		if cfg.NotificationFile != "" {
			mailCfg, err = jobnotify.LoadConfig(cfg.NotificationFile)
			if err != nil {
				return nil, err
			}
		}
		mailCfg.ClusterID = cfg.ClusterID
		r.mail, err = jobdoneemail.New(ctx, mailCfg, reader, sender, r.masterURL)
		if err != nil {
			return nil, err
		}
		err = r.bus.Subscribe(eventbus.Listener{ID: jobdoneemail.ID, Types: []string{ReportType}, Handle: func(c context.Context, e eventbus.Event) error {
			if !r.enabled(jobdoneemail.ID) {
				return nil
			}
			return r.mail.Handle(c, e)
		}})
		if err != nil {
			return nil, err
		}
	}
	if cfg.Plugins.Resources.Enabled {
		r.metrics, err = resources.NewSender(ctx, sender, r.masterURL, cfg.MaxJobs, 64<<20)
		if err != nil {
			return nil, err
		}
		listener := resources.NewListener(r.collector, cfg.MaxJobs*4)
		if err = r.bus.Subscribe(eventbus.Listener{ID: resources.ID, Types: []string{resources.ReadyType, EndingType}, Handle: listener.Handle}); err != nil {
			return nil, err
		}
	}
	if err = r.bus.Subscribe(eventbus.Listener{ID: "lifecycle.resolve", Types: []string{StartingType, EndingType}, Concurrency: 2, Handle: r.accept}); err != nil {
		return nil, err
	}
	for i := 0; i < cfg.ResolveWorkers; i++ {
		r.wg.Add(1)
		go r.worker()
	}
	r.wg.Add(2)
	go r.scheduler()
	go r.sampleLoop()
	return r, nil
}

func validCallback(payload json.RawMessage) error { _, err := decodeCallback(payload); return err }
func decodeCallback(payload []byte) (jobnotify.Callback, error) {
	c, err := jobnotify.DecodeCallback(payload)
	if err != nil {
		return c, err
	}
	c.SLUID = jobnotify.CanonicalSLUID(c.SLUID)
	return c, nil
}
func (r *Runtime) key(c jobnotify.Callback) string {
	return r.cfg.ClusterID + "/" + jobnotify.CanonicalSLUID(c.SLUID)
}
func (r *Runtime) enabled(id string) bool {
	switch id {
	case jobdoneemail.ID:
		return r.cfg.Plugins.JobEmail
	case resources.ID:
		return r.cfg.Plugins.Resources.Enabled
	}
	return false
}

func (r *Runtime) accept(ctx context.Context, event eventbus.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c, err := decodeCallback(event.Payload)
	if err != nil {
		return err
	}
	key := r.key(c)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if expiry, ok := r.seen[key]; ok && time.Now().Before(expiry) {
		return nil
	}
	s := r.jobs[key]
	if s == nil {
		if len(r.jobs) >= r.cfg.MaxJobs {
			return eventbus.ErrFull
		}
		s = &execution{callback: c, created: time.Now(), due: time.Now()}
		r.jobs[key] = s
	}
	if event.Type == EndingType {
		if !s.ending {
			s.endingAt = time.Now()
		}
		s.ending = true
		s.due = time.Now()
		r.collector.Pause(key, true, time.Now())
	}
	return nil
}

// Publish puts one job event on the bus.
func (r *Runtime) Publish(ctx context.Context, eventType string, c jobnotify.Callback, at time.Time) (eventbus.Receipt, error) {
	if r.ctx.Err() != nil {
		return eventbus.Receipt{}, eventbus.ErrClosed
	}
	if c.Validate() != nil {
		return eventbus.Receipt{}, errors.New("invalid lifecycle callback")
	}
	c.SLUID = jobnotify.CanonicalSLUID(c.SLUID)
	if at.IsZero() {
		at = time.Now().UTC()
	}
	body, _ := json.Marshal(c)
	// Hook retries carry no volatile fields: identical logical events must have
	// identical content for deduplication even if delivered at different times.
	id := r.key(c) + "/" + eventType
	r.mu.Lock()
	if r.ctx.Err() != nil {
		r.mu.Unlock()
		return eventbus.Receipt{}, eventbus.ErrClosed
	}
	if first, ok := r.eventTimes[id]; ok {
		at = first
	} else {
		if len(r.eventTimes) >= r.cfg.MaxJobs*4 {
			for k := range r.eventTimes {
				delete(r.eventTimes, k)
				break
			}
		}
		r.eventTimes[id] = at
	}
	r.mu.Unlock()
	return r.bus.Publish(ctx, eventbus.Event{ID: id, Type: eventType, SchemaVersion: 1, Subject: r.key(c), Source: "slurm-controller", OccurredAt: at, Payload: body})
}

func (r *Runtime) scheduler() {
	defer r.wg.Done()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case now := <-tick.C:
			r.mu.Lock()
			for key, expiry := range r.seen {
				if !now.Before(expiry) {
					delete(r.seen, key)
				}
			}
			for key, s := range r.jobs {
				if s.working || s.due.IsZero() || now.Before(s.due) {
					continue
				}
				select {
				case r.work <- key:
					s.working = true
				default:
				}
			}
			r.mu.Unlock()
		}
	}
}

func (r *Runtime) worker() {
	defer r.wg.Done()
	ttl, _ := time.ParseDuration(r.cfg.ResolveTTL)
	timeout, _ := time.ParseDuration(r.cfg.RequestTimeout)
	for {
		select {
		case <-r.ctx.Done():
			return
		case key := <-r.work:
			r.mu.Lock()
			s := r.jobs[key]
			if s == nil {
				r.mu.Unlock()
				continue
			}
			ending := s.ending
			callback := s.callback
			created := s.created
			if ending && !s.endingAt.IsZero() {
				created = s.endingAt
			}
			watching := s.watching
			frozen := append([]byte(nil), s.report...)
			r.mu.Unlock()
			if time.Since(created) > ttl && (!watching || ending) {
				r.discard(key)
				continue
			}
			ctx, cancel := context.WithTimeout(r.ctx, timeout)
			var err error
			if len(frozen) > 0 {
				err = r.publishReport(ctx, key, frozen)
			} else if ending {
				err = r.resolveEnd(ctx, key, callback)
			} else {
				err = r.resolveStart(ctx, key, callback)
			}
			cancel()
			if errors.Is(err, jobnotify.ErrSuperseded) {
				r.discard(key)
				continue
			}
			r.mu.Lock()
			if current := r.jobs[key]; current != nil {
				current.working = false
				if err != nil {
					current.attempts++
					delay := time.Duration(min(current.attempts, 5)) * time.Second
					current.due = time.Now().Add(delay)
				} else if !current.ending {
					current.due = time.Now().Add(30 * time.Second)
				} else if len(current.report) == 0 {
					current.due = time.Now().Add(time.Second)
				}
			}
			r.mu.Unlock()
		}
	}
}

func (r *Runtime) resolveStart(ctx context.Context, key string, c jobnotify.Callback) error {
	body, err := r.get(ctx, "/slurm/"+jobnotify.SlurmAPIVersion+"/job/"+strconv.FormatUint(uint64(c.JobID), 10))
	if err != nil {
		return err
	}
	row, err := selectJob(body, c)
	if err != nil {
		return err
	}
	var states []string
	_ = json.Unmarshal(row["job_state"], &states)
	active := false
	paused := false
	for _, state := range states {
		switch state {
		case "RUNNING", "CONFIGURING":
			active = true
		case "SUSPENDED":
			active = true
			paused = true
		case "COMPLETED", "FAILED", "CANCELLED", "TIMEOUT", "OUT_OF_MEMORY", "NODE_FAIL", "PREEMPTED", "BOOT_FAIL", "DEADLINE":
			r.mu.Lock()
			if s := r.jobs[key]; s != nil {
				s.ending = true
				s.endingAt = time.Now()
				s.due = time.Now()
				r.collector.Pause(key, true, time.Now())
			}
			r.mu.Unlock()
			return nil
		}
	}
	if !active {
		return jobnotify.ErrRetry
	}
	r.mu.Lock()
	s := r.jobs[key]
	watching := s != nil && s.watching
	ended := s == nil || s.ending
	r.mu.Unlock()
	if ended {
		return nil
	}
	if !r.enabled(resources.ID) {
		return nil
	}
	if !watching {
		// A newly discovered suspended execution has no live measurement window.
		// Wait for resume rather than racing an asynchronous Ready with Pause.
		if paused {
			return jobnotify.ErrRetry
		}
		a, e := r.allocation(ctx, c, row)
		if e != nil {
			return e
		}
		r.mu.Lock()
		s = r.jobs[key]
		if s != nil && !s.ending {
			payload, encodeErr := json.Marshal(a)
			if encodeErr != nil {
				r.mu.Unlock()
				return encodeErr
			}
			_, err = r.bus.Publish(ctx, eventbus.Event{ID: key + "/resources-ready", Type: resources.ReadyType, SchemaVersion: 1, Subject: key, Source: "lifecycle", OccurredAt: a.StartedAt, Payload: payload})
			if err == nil {
				s.watching = true
			}
		}
		r.mu.Unlock()
		if err != nil {
			return err
		}
	}
	pausedAt := time.Now()
	if paused {
		if stamp, ok := number(row["suspend_time"]); ok && stamp > 0 {
			pausedAt = time.Unix(stamp, 0)
		}
	}
	r.mu.Lock()
	if current := r.jobs[key]; current != nil && !current.ending {
		r.collector.Pause(key, paused, pausedAt)
	}
	r.mu.Unlock()
	return nil
}

func (r *Runtime) resolveEnd(ctx context.Context, key string, c jobnotify.Callback) error {
	body, err := r.get(ctx, "/slurm/"+jobnotify.SlurmAPIVersion+"/job/"+strconv.FormatUint(uint64(c.JobID), 10))
	if err != nil {
		return err
	}
	report, err := jobnotify.NormalizeLifecycle(r.cfg.ClusterID, c, body, time.Now(), 256<<10)
	if err != nil {
		return err
	}
	end := report.ObservedAt
	if report.Job.EndTime != nil {
		end = time.Unix(*report.Job.EndTime, 0)
	}
	u := r.collector.Preview(key, end, report.Duration.ActiveSeconds)
	// Controller hooks do not report each suspension/resume. Sparse state reads
	// cannot reconstruct every suspended interval; preserve this uncertainty.
	if row, e := selectJob(body, c); e == nil {
		if stamp, ok := number(row["suspend_time"]); ok && stamp > 0 {
			u.Complete = false
			u.MissingReasons = append(u.MissingReasons, "suspension_history_not_fully_observed")
		}
	}
	summary := summaryFor(report, u)
	if summary.Job.GPUs == 0 {
		summary.Usage.GPU = resources.GPUUsage{Complete: true, MissingReasons: []string{}}
	}
	if r.enabled(resources.ID) {
		report.Usage = summary.Usage
	} else {
		report.Usage = nil
	}
	_, encoded, err := jobnotify.EncodeEvent(report, 256<<10)
	if err != nil {
		return err
	}
	r.mu.Lock()
	if s := r.jobs[key]; s != nil && r.ctx.Err() == nil {
		if r.reportBytes+int64(len(encoded))-int64(len(s.report)) > r.cfg.MaxRetainedReportBytes {
			r.mu.Unlock()
			return eventbus.ErrFull
		}
		r.reportBytes += int64(len(encoded)) - int64(len(s.report))
		s.report = encoded
		s.summary = &summary
	} else {
		r.mu.Unlock()
		return eventbus.ErrClosed
	}
	r.mu.Unlock()
	r.collector.Remove(key)
	return r.publishReport(ctx, key, encoded)
}

func (r *Runtime) publishReport(ctx context.Context, key string, payload []byte) error {
	r.mu.Lock()
	s := r.jobs[key]
	var summary *resources.Summary
	queued := false
	if s != nil {
		summary = s.summary
		queued = s.queuedSummary
	}
	r.mu.Unlock()
	if summary != nil && !queued && r.enabled(resources.ID) && r.metrics != nil {
		if err := r.metrics.Accept(ctx, *summary); err != nil {
			return err
		}
		r.mu.Lock()
		if s := r.jobs[key]; s != nil {
			s.queuedSummary = true
		}
		r.mu.Unlock()
	}
	var report jobnotify.Event
	if json.Unmarshal(payload, &report) != nil {
		return errors.New("invalid frozen report")
	}
	_, err := r.bus.Publish(ctx, eventbus.Event{ID: key + "/report", Type: ReportType, SchemaVersion: 1, Subject: key, Source: "lifecycle", OccurredAt: report.ObservedAt, Payload: payload})
	if err == nil {
		r.mu.Lock()
		if s := r.jobs[key]; s != nil {
			r.reportBytes -= int64(len(s.report))
		}
		delete(r.jobs, key)
		if len(r.seen) >= r.cfg.MaxJobs*4 {
			for k := range r.seen {
				delete(r.seen, k)
				break
			}
		}
		r.seen[key] = time.Now().Add(24 * time.Hour)
		r.mu.Unlock()
	}
	return err
}
func (r *Runtime) discard(key string) {
	r.collector.Finish(key, time.Now(), 0)
	r.mu.Lock()
	if s := r.jobs[key]; s != nil {
		r.reportBytes -= int64(len(s.report))
	}
	delete(r.jobs, key)
	r.mu.Unlock()
}

func (r *Runtime) sampleLoop() {
	defer r.wg.Done()
	interval := r.cfg.sampleInterval
	for {
		timer := time.NewTimer(interval)
		select {
		case <-r.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if !r.enabled(resources.ID) {
			continue
		}
		nodes := r.collector.Nodes()
		if len(nodes) == 0 {
			continue
		}
		samples, err := r.backend.Read(r.ctx, nodes)
		if err != nil {
			samples = nil
		}
		r.collector.Observe(time.Now(), samples)
	}
}

// Handler accepts events from the lifecycle hook.
func (r *Runtime) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if req.URL.RawQuery != "" || req.URL.ForceQuery || req.URL.RawPath != "" {
			http.NotFound(w, req)
			return
		}
		if req.TLS == nil || len(req.TLS.VerifiedChains) == 0 {
			http.Error(w, "verified client required", http.StatusUnauthorized)
			return
		}
		// A raw token is not an Authorization scheme. Keep the same scope
		// checks below, but only extract credentials from an explicit Bearer
		// header so malformed schemes cannot become valid token proofs.
		token := ""
		if authorization := req.Header.Get("Authorization"); strings.HasPrefix(authorization, "Bearer ") {
			token = strings.TrimPrefix(authorization, "Bearer ")
		}
		if req.URL.Path == StatsPath && req.Method == http.MethodGet {
			if !r.tokens.CanRead(token) {
				http.Error(w, "credential required", http.StatusForbidden)
				return
			}
			r.mu.Lock()
			jobs := len(r.jobs)
			r.mu.Unlock()
			var mail *jobnotify.Stats
			if r.mail != nil {
				snapshot := r.mail.Stats()
				mail = &snapshot
			}
			var summaries *resources.SenderStats
			if r.metrics != nil {
				snapshot := r.metrics.SnapshotStats()
				summaries = &snapshot
			}
			_ = json.NewEncoder(w).Encode(struct {
				Bus               eventbus.Stats         `json:"bus"`
				ActiveJobs        int                    `json:"active_jobs"`
				Mail              *jobnotify.Stats       `json:"mail,omitempty"`
				ResourceSummaries *resources.SenderStats `json:"resource_summaries,omitempty"`
			}{r.bus.Snapshot(), jobs, mail, summaries})
			return
		}
		legacyCompletion := r.cfg.AllowLegacyCompletionHook && req.URL.Path == legacyCompletionPath
		if req.URL.Path != StartingPath && req.URL.Path != EndingPath && !legacyCompletion {
			http.NotFound(w, req)
			return
		}
		if req.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		if legacyCompletion && !r.legacyControllerAuthorized(req) {
			http.Error(w, "authorized controller required", http.StatusForbidden)
			return
		}
		if !legacyCompletion && !r.tokens.CanWrite(token) {
			http.Error(w, "write credential required", http.StatusForbidden)
			return
		}
		if strings.Split(req.Header.Get("Content-Type"), ";")[0] != "application/json" {
			w.WriteHeader(415)
			return
		}
		b, err := io.ReadAll(io.LimitReader(req.Body, 1025))
		if err != nil || len(b) > 1024 {
			w.WriteHeader(413)
			return
		}
		c, err := decodeCallback(b)
		if err != nil {
			w.WriteHeader(400)
			return
		}
		kind := StartingType
		if req.URL.Path == EndingPath || legacyCompletion {
			kind = EndingType
		}
		receipt, err := r.Publish(r.ctx, kind, c, time.Time{})
		if err != nil {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(202)
		_ = json.NewEncoder(w).Encode(receipt)
	})
}

const legacyCompletionPath = "/internal/slurm/job-ended"

// The compatibility endpoint preserves the previous controller certificate
// authority only. It never broadens the token-protected starting/ending API.
// Check the allowlist here as well as in the production TLS listener so an
// injected HTTP server cannot turn any other CA-trusted peer into a controller.
func (r *Runtime) legacyControllerAuthorized(req *http.Request) bool {
	if req.TLS == nil || len(req.TLS.VerifiedChains) == 0 || len(req.TLS.PeerCertificates) == 0 {
		return false
	}
	sum := sha256.Sum256(req.TLS.PeerCertificates[0].Raw)
	fingerprint := hex.EncodeToString(sum[:])
	for _, allowed := range r.cfg.TLS.ClientFingerprints {
		if strings.EqualFold(fingerprint, allowed) {
			return true
		}
	}
	return false
}

func (r *Runtime) Snapshot() eventbus.Stats { return r.bus.Snapshot() }

// Close stops the listener and plugins.
func (r *Runtime) Close(ctx context.Context) error {
	r.cancel()
	if r.server != nil {
		_ = r.server.Close()
	}
	err := r.bus.Close(ctx)
	if r.mail != nil {
		if e := r.mail.Close(ctx); err == nil {
			err = e
		}
	}
	if r.metrics != nil {
		if e := r.metrics.Close(ctx); err == nil {
			err = e
		}
	}
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		if err == nil {
			err = ctx.Err()
		}
	}
	r.mu.Lock()
	r.jobs = map[string]*execution{}
	r.reportBytes = 0
	r.seen = map[string]time.Time{}
	r.eventTimes = map[string]time.Time{}
	r.mu.Unlock()
	r.collector.Clear()
	return err
}
