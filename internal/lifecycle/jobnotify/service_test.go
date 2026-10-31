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
	"gopkg.in/yaml.v3"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type tlsMaterial struct {
	files       TLSFiles
	certificate tls.Certificate
	pool        *x509.CertPool
	fingerprint string
}

func testTLS(t *testing.T) tlsMaterial {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "isolated test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "isolated endpoint"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(leafKey)
	dir := t.TempDir()
	files := TLSFiles{CertFile: filepath.Join(dir, "peer.crt"), KeyFile: filepath.Join(dir, "peer.key"), CAFile: filepath.Join(dir, "ca.crt"), ServerName: "127.0.0.1"}
	for path, data := range map[string][]byte{files.CertFile: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), files.KeyFile: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), files.CAFile: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})} {
		if os.WriteFile(path, data, 0600) != nil {
			t.Fatal("write test certificate")
		}
	}
	cert, err := tls.LoadX509KeyPair(files.CertFile, files.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	return tlsMaterial{files, cert, pool, certificateFingerprint(leafDER)}
}
func testConfig() Config {
	return Config{ClusterID: "cluster1", Listen: "127.0.0.1:0", SlurmURL: "https://127.0.0.1", CallbackTLS: TLSFiles{ClientFingerprints: []string{strings.Repeat("0", 64)}}, RetryMax: "2ms", RequestTimeout: "1s", ResolveTTL: "5s", SendTTL: "10s", MaxAttempts: 100}
}
func startTestService(t *testing.T, cfg Config, rest, master *httptest.Server) *Service {
	t.Helper()
	cfg.SlurmURL = rest.URL
	s, err := New(cfg, rest.Client(), master.Client(), master.URL)
	if err != nil {
		t.Fatal(err)
	}
	s.Run(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return s
}
func eventually(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("isolated notification condition timed out")
}

func TestHookDoesNotReadResponseAndDisconnectDoesNotCancelWork(t *testing.T) {
	material := testTLS(t)
	cfg := testConfig()
	cfg.CallbackTLS.ClientFingerprints = []string{material.fingerprint}
	var mail atomic.Int64
	rest := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/slurm/v0.0.45/job/7" {
			t.Errorf("unexpected REST path %s", r.URL.Path)
		}
		time.Sleep(60 * time.Millisecond)
		_, _ = w.Write(responseBytes(fixtureJob(7, testSLUID, "COMPLETED")))
	}))
	defer rest.Close()
	master := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != CompletionPath {
			t.Error("wrong master endpoint")
		}
		var event Event
		if json.NewDecoder(r.Body).Decode(&event) != nil || event.Job.UID != 10019 || event.Job.SLUID != testSLUID {
			t.Error("wrong event")
		}
		mail.Add(1)
		_, _ = io.WriteString(w, `{"outcome":"smtp_accepted"}`)
	}))
	defer master.Close()
	s := startTestService(t, cfg, rest, master)
	callback := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.Handler().ServeHTTP(w, r)
		time.Sleep(750 * time.Millisecond)
	}))
	callback.TLS = &tls.Config{Certificates: []tls.Certificate{material.certificate}, ClientCAs: material.pool, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12}
	callback.StartTLS()
	defer callback.Close()
	started := time.Now()
	err := SendHook(context.Background(), HookConfig{URL: callback.URL + CallbackPath, TLS: material.files, Timeout: "500ms"}, Callback{7, testSLUID})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > 400*time.Millisecond {
		t.Fatal("hook waited for HTTP response")
	}
	eventually(t, func() bool { return mail.Load() == 1 && s.Snapshot().Sent == 1 })
	// An incomplete body is never accepted even when its headers parse.
	clientTLSConfig, _ := clientTLS(material.files)
	connection, err := tls.Dial("tcp", strings.TrimPrefix(callback.URL, "https://"), clientTLSConfig)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(connection, "POST "+CallbackPath+" HTTP/1.1\r\nHost: fixture\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"job_id\":8,")
	_ = connection.Close()
	time.Sleep(100 * time.Millisecond)
	if s.Snapshot().Accepted != 1 {
		t.Fatal("partial request became a notification")
	}
}

func TestMasterFailureDoesNotBlockCollectionAndSenderIsFIFO(t *testing.T) {
	var queried sync.Map
	var available atomic.Bool
	var attempts atomic.Int64
	var mu sync.Mutex
	var delivered []uint32
	rest := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := uint32(7)
		sluid := testSLUID
		if strings.HasSuffix(r.URL.Path, "/8") {
			id = 8
			sluid = "s123456789abce"
		}
		queried.Store(id, true)
		_, _ = w.Write(responseBytes(fixtureJob(id, sluid, "FAILED")))
	}))
	defer rest.Close()
	master := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if !available.Load() {
			w.WriteHeader(503)
			return
		}
		var event Event
		_ = json.NewDecoder(r.Body).Decode(&event)
		mu.Lock()
		delivered = append(delivered, event.Job.JobID)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"outcome":"smtp_accepted"}`)
	}))
	defer master.Close()
	cfg := testConfig()
	cfg.ResolveWorkers = 1
	s := startTestService(t, cfg, rest, master)
	if !s.enqueue(Callback{7, testSLUID}) {
		t.Fatal("enqueue first")
	}
	eventually(t, func() bool { return attempts.Load() > 0 })
	if !s.enqueue(Callback{8, "s123456789abce"}) {
		t.Fatal("enqueue second")
	}
	eventually(t, func() bool { _, ok := queried.Load(uint32(8)); return ok })
	if s.Snapshot().Pending != 2 {
		t.Fatal("events discarded before master acknowledgement")
	}
	available.Store(true)
	eventually(t, func() bool { return s.Snapshot().Sent == 2 })
	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 2 || delivered[0] != 7 || delivered[1] != 8 {
		t.Fatalf("FIFO changed: %v", delivered)
	}
}

func TestCompletingSnapshotCannotSendAfterSLUIDChanges(t *testing.T) {
	var reads atomic.Int64
	var sends atomic.Int64
	rest := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		row := fixtureJob(7, testSLUID, "FAILED", "COMPLETING")
		if reads.Add(1) > 1 {
			row = fixtureJob(7, "s123456789abce", "RUNNING")
		}
		_, _ = w.Write(responseBytes(row))
	}))
	defer rest.Close()
	master := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		_, _ = io.WriteString(w, `{"outcome":"smtp_accepted"}`)
	}))
	defer master.Close()
	s := startTestService(t, testConfig(), rest, master)
	s.enqueue(Callback{7, testSLUID})
	eventually(t, func() bool { return s.Snapshot().Filtered == 1 })
	if sends.Load() != 0 {
		t.Fatal("stale completing snapshot was mailed")
	}
}

func TestCapacityDedupAndVolatileShutdown(t *testing.T) {
	rest := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer rest.Close()
	master := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer master.Close()
	cfg := testConfig()
	cfg.MaxEvents = 1
	s := startTestService(t, cfg, rest, master)
	if !s.enqueue(Callback{7, testSLUID}) {
		t.Fatal("first completion rejected")
	}
	if !s.enqueue(Callback{7, testSLUID}) {
		t.Fatal("duplicate completion rejected")
	}
	if s.enqueue(Callback{8, "s123456789abce"}) {
		t.Fatal("completion accepted beyond queue capacity")
	}
	stats := s.Snapshot()
	if stats.Accepted != 1 || stats.Duplicates != 1 || stats.Rejected != 1 || stats.Pending != 1 {
		t.Fatalf("wrong counters %+v", stats)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().Pending != 0 || s.Snapshot().Bytes != 0 {
		t.Fatal("RAM queue survived shutdown")
	}
}

func TestReceiverAuthenticationAndPayloadValidation(t *testing.T) {
	material := testTLS(t)
	cfg := testConfig()
	cfg.CallbackTLS.ClientFingerprints = []string{material.fingerprint}
	rest := httptest.NewTLSServer(http.NotFoundHandler())
	defer rest.Close()
	master := httptest.NewTLSServer(http.NotFoundHandler())
	defer master.Close()
	s := startTestService(t, cfg, rest, master)
	for _, tc := range []struct {
		method, path, body string
		auth               bool
		want               int
	}{
		{"POST", CallbackPath, `{"job_id":7,"sluid":"` + testSLUID + `"}`, false, 401},
		{"POST", CallbackPath, `{"job_id":7,"sluid":"` + testSLUID + `","uid":1}`, true, 400},
		{"POST", CallbackPath, `{"job_id":7,"sluid":"bad"}`, true, 400},
		{"POST", CallbackPath + "?job_id=7", `{}`, true, 404},
		{"GET", CallbackPath, `{}`, true, 405},
	} {
		r := httptest.NewRequest(tc.method, "https://fixture"+tc.path, strings.NewReader(tc.body))
		r.Header.Set("Content-Type", "application/json")
		if tc.auth {
			leaf, _ := x509.ParseCertificate(material.certificate.Certificate[0])
			r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}, VerifiedChains: [][]*x509.Certificate{{leaf}}}
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("%s expected %d got %d", tc.body, tc.want, w.Code)
		}
	}
}

func TestStartUsesIndependentMTLSListenerAndPrivateConfiguration(t *testing.T) {
	material := testTLS(t)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == CompletionPath {
			_, _ = io.WriteString(w, `{"outcome":"smtp_accepted"}`)
			return
		}
		_, _ = w.Write(responseBytes(fixtureJob(7, testSLUID, "COMPLETED")))
	}))
	upstream.TLS = &tls.Config{Certificates: []tls.Certificate{material.certificate}, ClientCAs: material.pool, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12}
	upstream.StartTLS()
	defer upstream.Close()
	cfg := testConfig()
	cfg.CallbackTLS = material.files
	cfg.CallbackTLS.ClientFingerprints = []string{material.fingerprint}
	cfg.SlurmTLS = material.files
	cfg.SlurmURL = upstream.URL
	data, _ := yaml.Marshal(cfg)
	path := filepath.Join(t.TempDir(), "notify.yaml")
	if os.WriteFile(path, data, 0600) != nil {
		t.Fatal("config fixture")
	}
	masterTLS, err := clientTLS(material.files)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Start(context.Background(), path, upstream.URL, masterTLS)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if s.Close(ctx) != nil {
			t.Error("shutdown failed")
		}
	}()
	if err := SendHook(context.Background(), HookConfig{URL: "https://" + s.server.Addr + CallbackPath, TLS: material.files, Timeout: "500ms"}, Callback{7, testSLUID}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return s.Snapshot().Sent == 1 })
	// A valid certificate signed by another CA cannot enter this listener.
	untrusted := testTLS(t)
	untrusted.files.CAFile = material.files.CAFile
	// TLS 1.3 may report local-write success before the server's client-cert
	// rejection is read. Verify rejection at the receiver below instead.
	_ = SendHook(context.Background(), HookConfig{URL: "https://" + s.server.Addr + CallbackPath, TLS: untrusted.files, Timeout: "500ms"}, Callback{8, "s123456789ABCE"})
	time.Sleep(100 * time.Millisecond)
	if s.Snapshot().Accepted != 1 {
		t.Fatal("untrusted mTLS callback accepted")
	}
}

func TestOnlyFinalMasterAcknowledgementsRemoveAsDelivered(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		status                 int
		body                   string
		sent, skipped, expired uint64
	}{
		{"SMTP accepted", 200, `{"outcome":"smtp_accepted"}`, 1, 0, 0},
		{"Explicit skip", 200, `{"outcome":"skipped","reason":"missing_mail"}`, 0, 1, 0},
		{"202 is not delivery", 202, `{"outcome":"smtp_accepted"}`, 0, 0, 1},
		{"Received is not delivery", 200, `{"outcome":"received"}`, 0, 0, 1},
		{"Malformed response", 200, `not json`, 0, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rest := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write(responseBytes(fixtureJob(7, testSLUID, "COMPLETED")))
			}))
			defer rest.Close()
			master := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer master.Close()
			cfg := testConfig()
			cfg.MaxAttempts = 1
			s := startTestService(t, cfg, rest, master)
			s.enqueue(Callback{7, testSLUID})
			eventually(t, func() bool { return s.Snapshot().Pending == 0 })
			stats := s.Snapshot()
			if stats.Sent != tc.sent || stats.Skipped != tc.skipped || stats.Expired != tc.expired {
				t.Fatalf("wrong acknowledgement semantics: %+v", stats)
			}
		})
	}
}

func TestEarlySnapshotIsAttachedOnlyToVerifiedSameExecution(t *testing.T) {
	var reads atomic.Int64
	var earlyPreserved atomic.Bool
	rest := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		row := fixtureJob(7, testSLUID, "COMPLETED")
		if reads.Add(1) == 1 {
			row["job_state"] = []string{"COMPLETED", "COMPLETING"}
			row["early_only_detail"] = "allocation detail"
		}
		_, _ = w.Write(responseBytes(row))
	}))
	defer rest.Close()
	master := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event Event
		_ = json.NewDecoder(r.Body).Decode(&event)
		earlyPreserved.Store(strings.Contains(string(event.Raw.InitialController), "early_only_detail"))
		_, _ = io.WriteString(w, `{"outcome":"smtp_accepted"}`)
	}))
	defer master.Close()
	s := startTestService(t, testConfig(), rest, master)
	s.enqueue(Callback{7, testSLUID})
	eventually(t, func() bool { return s.Snapshot().Sent == 1 })
	if !earlyPreserved.Load() {
		t.Fatal("early matching allocation fields lost")
	}
}

func TestCandidateExpiryIsBounded(t *testing.T) {
	var queries atomic.Int64
	rest := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { queries.Add(1); w.WriteHeader(503) }))
	defer rest.Close()
	master := httptest.NewTLSServer(http.NotFoundHandler())
	defer master.Close()
	cfg := testConfig()
	cfg.ResolveTTL = "1ms"
	s := startTestService(t, cfg, rest, master)
	s.enqueue(Callback{7, testSLUID})
	eventually(t, func() bool { return s.Snapshot().Expired == 1 })
	if s.Snapshot().Bytes != 0 {
		t.Fatal("expired candidate bytes retained")
	}
}
