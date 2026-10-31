package jobnotify

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testAuthority struct {
	certificate *x509.Certificate
	key         *ecdsa.PrivateKey
	der         []byte
}

func faultAuthority(t *testing.T) testAuthority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "isolated rollover authority"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return testAuthority{certificate, key, der}
}

func faultIdentity(t *testing.T, ca testAuthority, serial int64, expired bool) tlsMaterial {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	notBefore, notAfter := time.Now().Add(-time.Minute), time.Now().Add(time.Hour)
	if expired {
		notBefore, notAfter = time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "isolated rollover endpoint"}, NotBefore: notBefore, NotAfter: notAfter, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, certificate, ca.certificate, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	files := TLSFiles{CertFile: filepath.Join(dir, "identity.crt"), KeyFile: filepath.Join(dir, "identity.key"), CAFile: filepath.Join(dir, "ca.crt"), ServerName: "127.0.0.1"}
	for path, value := range map[string][]byte{files.CertFile: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), files.KeyFile: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), files.CAFile: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.der})} {
		if os.WriteFile(path, value, 0600) != nil {
			t.Fatal("write isolated TLS material")
		}
	}
	pair, err := tls.LoadX509KeyPair(files.CertFile, files.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.der}))
	return tlsMaterial{files: files, certificate: pair, pool: pool, fingerprint: certificateFingerprint(der)}
}

func waitFault(t *testing.T, timeout time.Duration, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("isolated fault scenario timed out")
}

func TestControllerCertificateRolloverAndRevocationPins(t *testing.T) {
	ca := faultAuthority(t)
	serverIdentity := faultIdentity(t, ca, 2, false)
	old := faultIdentity(t, ca, 3, false)
	next := faultIdentity(t, ca, 4, false)
	unlisted := faultIdentity(t, ca, 5, false)
	expired := faultIdentity(t, ca, 6, true)
	rest := httptest.NewTLSServer(http.NotFoundHandler())
	defer rest.Close()
	master := httptest.NewTLSServer(http.NotFoundHandler())
	defer master.Close()
	cfg := testConfig()
	cfg.CallbackTLS = serverIdentity.files
	cfg.CallbackTLS.ClientFingerprints = []string{old.fingerprint, next.fingerprint, expired.fingerprint}
	s := startTestService(t, cfg, rest, master)
	newCallbackServer := func(pins []string) *httptest.Server {
		files := serverIdentity.files
		files.ClientFingerprints = pins
		tlsConfig, err := serverTLS(files)
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewUnstartedServer(s.Handler())
		server.TLS = tlsConfig
		server.Config.ErrorLog = log.New(io.Discard, "", 0)
		server.StartTLS()
		t.Cleanup(server.Close)
		return server
	}
	overlap := newCallbackServer([]string{old.fingerprint, next.fingerprint, expired.fingerprint})
	send := func(server *httptest.Server, identity tlsMaterial, id uint32) {
		// A one-way write may succeed before the peer's client-cert rejection
		// arrives. Acceptance is asserted at the actual receiver below.
		_ = SendHook(context.Background(), HookConfig{URL: server.URL + CallbackPath, TLS: identity.files, Timeout: "500ms"}, Callback{id, fmt.Sprintf("s%013X", id)})
	}
	send(overlap, old, 7)
	send(overlap, next, 8)
	eventually(t, func() bool { return s.Snapshot().Accepted == 2 })
	send(overlap, unlisted, 9)
	send(overlap, expired, 10)
	time.Sleep(100 * time.Millisecond)
	if s.Snapshot().Accepted != 2 {
		t.Fatal("unlisted or expired controller identity was accepted")
	}
	rotated := newCallbackServer([]string{next.fingerprint})
	send(rotated, old, 11)
	send(rotated, next, 12)
	eventually(t, func() bool { return s.Snapshot().Accepted == 3 })
	time.Sleep(50 * time.Millisecond)
	if s.Snapshot().Accepted != 3 {
		t.Fatal("retired controller identity survived pin removal")
	}
}

func TestHookTLSFailuresAndHandshakeDeadline(t *testing.T) {
	material := testTLS(t)
	var requests atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{material.certificate}, MinVersion: tls.VersionTLS12}
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	defer server.Close()
	wrongName := material.files
	wrongName.ServerName = "wrong-name.invalid"
	other := testTLS(t)
	wrongCA := material.files
	wrongCA.CAFile = other.files.CAFile
	for _, files := range []TLSFiles{wrongName, wrongCA} {
		if SendHook(context.Background(), HookConfig{URL: server.URL + CallbackPath, TLS: files, Timeout: "100ms"}, Callback{7, testSLUID}) == nil {
			t.Fatal("invalid server TLS identity accepted")
		}
	}
	if requests.Load() != 0 {
		t.Fatal("request sent before server identity verified")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			accepted <- connection
		}
	}()
	started := time.Now()
	err = SendHook(context.Background(), HookConfig{URL: "https://" + listener.Addr().String() + CallbackPath, TLS: material.files, Timeout: "50ms"}, Callback{7, testSLUID})
	if err == nil || time.Since(started) > 500*time.Millisecond {
		t.Fatal("stalled TLS handshake exceeded hook bound")
	}
	select {
	case connection := <-accepted:
		_ = connection.Close()
	case <-time.After(time.Second):
		t.Fatal("isolated handshake peer not connected")
	}
}

func TestSMTPAcceptedButResponseLostRetriesSameEvent(t *testing.T) {
	rest := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(responseBytes(fixtureJob(7, testSLUID, "COMPLETED")))
	}))
	defer rest.Close()
	var attempts atomic.Int64
	var accepted atomic.Int64
	var identities sync.Map
	secondEntered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	master := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event Event
		if json.NewDecoder(r.Body).Decode(&event) != nil {
			t.Error("invalid event")
		}
		identities.Store(event.EventID, true)
		if attempts.Add(1) == 1 {
			accepted.Add(1) // Model upstream SMTP DATA accepted before HTTP loss.
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			// Even a complete JSON fragment is not a valid acknowledgement
			// when the declared HTTP body is truncated after SMTP acceptance.
			_, _ = io.WriteString(connection, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 500\r\n\r\n{\"outcome\":\"smtp_accepted\"}")
			_ = connection.Close()
			return
		}
		select {
		case <-secondEntered:
		default:
			close(secondEntered)
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		accepted.Add(1)
		_, _ = io.WriteString(w, `{"outcome":"smtp_accepted"}`)
	}))
	defer func() { releaseOnce.Do(func() { close(release) }); master.Close() }()
	s := startTestService(t, testConfig(), rest, master)
	s.enqueue(Callback{7, testSLUID})
	select {
	case <-secondEntered:
	case <-time.After(4 * time.Second):
		t.Fatal("lost response was not retried")
	}
	if accepted.Load() != 1 || s.Snapshot().Pending != 1 || s.Snapshot().Sent != 0 {
		t.Fatal("SMTP acceptance alone prematurely removed unacknowledged event")
	}
	releaseOnce.Do(func() { close(release) })
	eventually(t, func() bool { return s.Snapshot().Sent == 1 })
	keys := 0
	identities.Range(func(_, _ any) bool { keys++; return true })
	if attempts.Load() != 2 || accepted.Load() != 2 || keys != 1 {
		t.Fatal("retry changed event identity or unexpected attempt count")
	}
}

func TestWrongJobIDCannotPoisonCorrectSLUIDCandidate(t *testing.T) {
	rest := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/7") {
			_, _ = w.Write(responseBytes(fixtureJob(7, testSLUID, "COMPLETED")))
			return
		}
		w.WriteHeader(404)
	}))
	defer rest.Close()
	master := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{"outcome":"smtp_accepted"}`) }))
	defer master.Close()
	s := startTestService(t, testConfig(), rest, master)
	if !s.enqueue(Callback{8, testSLUID}) || !s.enqueue(Callback{7, testSLUID}) {
		t.Fatal("paired callbacks not accepted")
	}
	eventually(t, func() bool { return s.Snapshot().Sent == 1 })
	if s.Snapshot().Accepted != 2 || s.Snapshot().Pending != 1 {
		t.Fatal("wrong JobID suppressed the correct execution")
	}
	if !s.enqueue(Callback{7, testSLUID}) || s.Snapshot().Duplicates != 1 {
		t.Fatal("same JobID and SLUID were not deduplicated")
	}
}

func TestEarlySnapshotDisappearsWithoutInventingFinalState(t *testing.T) {
	var reads atomic.Int64
	var sends atomic.Int64
	rest := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reads.Add(1) == 1 {
			_, _ = w.Write(responseBytes(fixtureJob(7, testSLUID, "FAILED", "COMPLETING")))
			return
		}
		w.WriteHeader(404)
	}))
	defer rest.Close()
	master := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		_, _ = io.WriteString(w, `{"outcome":"smtp_accepted"}`)
	}))
	defer master.Close()
	cfg := testConfig()
	cfg.ResolveTTL = "1500ms"
	s := startTestService(t, cfg, rest, master)
	s.enqueue(Callback{7, testSLUID})
	eventually(t, func() bool { return s.Snapshot().Expired == 1 })
	if reads.Load() < 2 || sends.Load() != 0 || s.Snapshot().Bytes != 0 {
		t.Fatal("disappeared record was mailed from an unverified snapshot")
	}
}

func TestBurst1000CandidatesWithFourReadersAndFailedSender(t *testing.T) {
	var reads, active, peak atomic.Int64
	rest := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := active.Add(1)
		defer active.Add(-1)
		for {
			old := peak.Load()
			if now <= old || peak.CompareAndSwap(old, now) {
				break
			}
		}
		id, err := strconv.ParseUint(strings.TrimPrefix(r.URL.Path, "/slurm/"+SlurmAPIVersion+"/job/"), 10, 32)
		if err != nil {
			t.Error("invalid REST query")
			w.WriteHeader(400)
			return
		}
		time.Sleep(time.Millisecond)
		row := fixtureJob(uint32(id), fmt.Sprintf("s%013X", id), "COMPLETED")
		row["load_metadata"] = strings.Repeat("x", 256)
		_, _ = w.Write(responseBytes(row))
		reads.Add(1)
	}))
	defer rest.Close()
	var sends atomic.Int64
	master := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(503)
	}))
	defer master.Close()
	cfg := testConfig()
	cfg.MaxBytes = 64 << 20
	cfg.ResolveWorkers = 4
	cfg.ResolveTTL = "1m"
	cfg.SendTTL = "1m"
	cfg.RetryMax = "100ms"
	cfg.MaxAttempts = 1000
	s := startTestService(t, cfg, rest, master)
	for id := uint32(1); id <= 1000; id++ {
		if !s.enqueue(Callback{id, fmt.Sprintf("s%013X", id)}) {
			t.Fatalf("burst event %d unexpectedly rejected", id)
		}
	}
	if s.enqueue(Callback{1001, fmt.Sprintf("s%013X", 1001)}) {
		t.Fatal("count capacity was exceeded")
	}
	var highWater atomic.Int64
	var boundViolation atomic.Bool
	stop := make(chan struct{})
	var monitor sync.WaitGroup
	monitor.Add(1)
	var stopOnce sync.Once
	stopMonitor := func() { stopOnce.Do(func() { close(stop) }); monitor.Wait() }
	defer stopMonitor()
	go func() {
		defer monitor.Done()
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				stats := s.Snapshot()
				if stats.Pending > 1000 || stats.Bytes < 0 || stats.Bytes > cfg.MaxBytes && cfg.MaxBytes != 0 {
					boundViolation.Store(true)
				}
				for {
					old := highWater.Load()
					if stats.Bytes <= old || highWater.CompareAndSwap(old, stats.Bytes) {
						break
					}
				}
			}
		}
	}()
	waitFault(t, 30*time.Second, func() bool {
		s.mu.Lock()
		ready := len(s.ready)
		s.mu.Unlock()
		return reads.Load() == 1000 && ready == 1000
	})
	stopMonitor()
	stats := s.Snapshot()
	if boundViolation.Load() || peak.Load() > 4 || peak.Load() < 2 || sends.Load() == 0 || stats.Sent != 0 || stats.Pending != 1000 || stats.Rejected != 1 || highWater.Load() > 64<<20 {
		t.Fatalf("load bounds: peak=%d sends=%d highWater=%d stats=%+v", peak.Load(), sends.Load(), highWater.Load(), stats)
	}
	t.Logf("1000 candidates; REST peak %d; queued-byte high-water %d; failed sends %d", peak.Load(), highWater.Load(), sends.Load())
}

func TestSmallByteCapacityAndConcurrentShutdown(t *testing.T) {
	rest := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(5 * time.Second):
			w.WriteHeader(503)
		}
	}))
	defer rest.Close()
	master := httptest.NewTLSServer(http.NotFoundHandler())
	defer master.Close()
	cfg := testConfig()
	cfg.MaxBytes = 4096
	cfg.MaxEventBytes = 2048
	s := startTestService(t, cfg, rest, master)
	var producers sync.WaitGroup
	for producer := 0; producer < 8; producer++ {
		producers.Add(1)
		go func(offset int) {
			defer producers.Done()
			for n := 1; n <= 100; n++ {
				id := uint32(offset*100 + n)
				s.enqueue(Callback{id, fmt.Sprintf("s%013X", id)})
				_ = s.Snapshot()
			}
		}(producer)
	}
	producers.Wait()
	stats := s.Snapshot()
	if stats.Pending > 8 || stats.Bytes > 4096 || stats.Rejected == 0 {
		t.Fatalf("byte admission bound: %+v", stats)
	}
	var closers sync.WaitGroup
	for n := 0; n < 4; n++ {
		closers.Add(1)
		go func() {
			defer closers.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := s.Close(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	closers.Wait()
	if s.enqueue(Callback{900, testSLUID}) || s.Snapshot().Pending != 0 || s.Snapshot().Bytes != 0 {
		t.Fatal("shutdown admitted or retained work")
	}
}

func TestRetryDelayJitterIsCapped(t *testing.T) {
	for _, tc := range []struct {
		attempt        int
		max, low, high time.Duration
	}{
		{1, 5 * time.Minute, 500 * time.Millisecond, time.Second},
		{2, 5 * time.Minute, time.Second, 2 * time.Second},
		{1000, 5 * time.Minute, 150 * time.Second, 5 * time.Minute},
		{20, 100 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond},
		{1, time.Millisecond, 500 * time.Microsecond, time.Millisecond},
	} {
		values := map[time.Duration]bool{}
		for n := 0; n < 100; n++ {
			delay := retryDelay(tc.attempt, tc.max)
			values[delay] = true
			if delay < tc.low || delay > tc.high {
				t.Fatalf("backoff exceeded specified range: attempt=%d max=%s delay=%s", tc.attempt, tc.max, delay)
			}
		}
		if len(values) < 2 {
			t.Fatal("retry jitter did not vary")
		}
	}
}

func TestItemTTLInterruptsBlockedRESTAndMasterRequests(t *testing.T) {
	for _, stage := range []string{"REST", "master"} {
		t.Run(stage, func(t *testing.T) {
			var restStarted, masterStarted atomic.Bool
			rest := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				restStarted.Store(true)
				if stage == "REST" {
					<-r.Context().Done()
					return
				}
				_, _ = w.Write(responseBytes(fixtureJob(7, testSLUID, "COMPLETED")))
			}))
			defer rest.Close()
			master := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				masterStarted.Store(true)
				_, _ = io.Copy(io.Discard, r.Body)
				<-r.Context().Done()
			}))
			defer master.Close()
			cfg := testConfig()
			cfg.ResolveTTL = "150ms"
			cfg.SendTTL = "150ms"
			s := startTestService(t, cfg, rest, master)
			started := time.Now()
			s.enqueue(Callback{7, testSLUID})
			waitFault(t, time.Second, func() bool { return s.Snapshot().Expired == 1 })
			if time.Since(started) > 700*time.Millisecond || !restStarted.Load() || (stage == "master" && !masterStarted.Load()) || s.Snapshot().Bytes != 0 {
				t.Fatalf("item TTL did not interrupt %s request", stage)
			}
		})
	}
}

func TestFinalFailedAttemptDoesNotWaitForAnotherBackoff(t *testing.T) {
	rest := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(responseBytes(fixtureJob(7, testSLUID, "COMPLETED")))
	}))
	defer rest.Close()
	var attempts atomic.Int64
	master := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { attempts.Add(1); w.WriteHeader(503) }))
	defer master.Close()
	cfg := testConfig()
	cfg.MaxAttempts = 1
	cfg.RetryMax = "5m"
	s := startTestService(t, cfg, rest, master)
	s.enqueue(Callback{7, testSLUID})
	waitFault(t, 500*time.Millisecond, func() bool { return s.Snapshot().Expired == 1 })
	if attempts.Load() != 1 || s.Snapshot().Pending != 0 {
		t.Fatal("final failed attempt was retained for a nonexistent retry")
	}
}

func TestJoiningEarlyAndFinalSnapshotsCannotExceedWireLimit(t *testing.T) {
	const limit = 2048
	row := fixtureJob(7, testSLUID, "COMPLETED")
	row["padding"] = ""
	baseline, err := Normalize("cluster1", Callback{7, testSLUID}, responseBytes(row), time.Now(), limit)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(baseline)
	padding := limit - len(encoded) - 10
	if padding < 0 {
		t.Fatal("wire fixture exceeds baseline limit")
	}
	row["padding"] = strings.Repeat("x", padding)
	qualified, err := Normalize("cluster1", Callback{7, testSLUID}, responseBytes(row), time.Now(), limit)
	if err != nil || !qualified.Raw.Complete {
		t.Fatal("near-limit final snapshot was not complete")
	}
	var reads atomic.Int64
	rest := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		copyRow := fixtureJob(7, testSLUID, "COMPLETED")
		copyRow["padding"] = row["padding"]
		if reads.Add(1) == 1 {
			copyRow["job_state"] = []string{"COMPLETED", "COMPLETING"}
		}
		_, _ = w.Write(responseBytes(copyRow))
	}))
	defer rest.Close()
	var wireBytes atomic.Int64
	var omissions atomic.Bool
	master := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		wireBytes.Store(int64(len(body)))
		if len(body) > limit {
			w.WriteHeader(413)
			return
		}
		var event Event
		_ = json.Unmarshal(body, &event)
		omissions.Store(!event.Raw.Complete && event.Raw.OmittedReason != "")
		_, _ = io.WriteString(w, `{"outcome":"smtp_accepted"}`)
	}))
	defer master.Close()
	cfg := testConfig()
	cfg.MaxEventBytes = limit
	cfg.MaxAttempts = 1
	s := startTestService(t, cfg, rest, master)
	s.enqueue(Callback{7, testSLUID})
	eventually(t, func() bool { return s.Snapshot().Sent == 1 || s.Snapshot().Expired > 0 })
	if s.Snapshot().Sent != 1 || wireBytes.Load() > limit || !omissions.Load() {
		t.Fatalf("joined metadata violated wire limit: bytes=%d stats=%+v", wireBytes.Load(), s.Snapshot())
	}
}
