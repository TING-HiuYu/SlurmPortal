package jobnotify

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type isolatedRoundTripper func(*http.Request) (*http.Response, error)

func (f isolatedRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMaliciousCallbackJSONNeverEntersQueue(t *testing.T) {
	material := testTLS(t)
	cfg := testConfig()
	cfg.CallbackTLS.ClientFingerprints = []string{material.fingerprint}
	client := &http.Client{Transport: isolatedRoundTripper(func(*http.Request) (*http.Response, error) { return nil, errors.New("isolated transport") })}
	s, err := New(cfg, client, client, "https://fixture.invalid")
	if err != nil {
		t.Fatal(err)
	}
	s.Run(context.Background())
	defer func() {
		if err := s.Close(context.Background()); err != nil {
			t.Errorf("close notification service: %v", err)
		}
	}()
	leaf, _ := x509.ParseCertificate(material.certificate.Certificate[0])
	for _, body := range []string{
		`{"job_id":7,"job_id":8,"sluid":"` + testSLUID + `"}`,
		`{"job_id":7,"sluid":"` + testSLUID + `","sluid":"s0123456789ABC"}`,
		`{"job_id":7,"job_\u0069d":8,"sluid":"` + testSLUID + `"}`,
		`{"Job_ID":7,"sluid":"` + testSLUID + `"}`,
		`{"job_id":"7","sluid":"` + testSLUID + `"}`,
		`{"job_id":7.0,"sluid":"` + testSLUID + `"}`,
		`{"job_id":1e3,"sluid":"` + testSLUID + `"}`,
		`{"job_id":-1,"sluid":"` + testSLUID + `"}`,
		`{"job_id":4294967294,"sluid":"` + testSLUID + `"}`,
		`{"job_id":4294967295,"sluid":"` + testSLUID + `"}`,
		`{"job_id":7,"sluid":null}`,
		`{"job_id":7,"sluid":"` + testSLUID + `","recipient":"injected@example.invalid"}`,
		`{"job_id":{},"sluid":"` + testSLUID + `"}`,
		`{"job_id":7,"sluid":"` + testSLUID + `"} {}`,
		`[{"job_id":7,"sluid":"` + testSLUID + `"}]`,
		"\xef\xbb\xbf" + `{"job_id":7,"sluid":"` + testSLUID + `"}`,
		strings.Repeat(" ", 1025),
	} {
		r := httptest.NewRequest(http.MethodPost, "https://fixture"+CallbackPath, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}, VerifiedChains: [][]*x509.Certificate{{leaf}}}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 400 {
			t.Errorf("ambiguous callback accepted, status %d for %q", w.Code, body)
		}
	}
	if s.Snapshot().Accepted != 0 || s.Snapshot().Pending != 0 {
		t.Fatal("invalid callback reached process queue")
	}
}

func TestAcknowledgementAmbiguityFailsClosed(t *testing.T) {
	for _, body := range []string{
		`{"outcome":"received","outcome":"smtp_accepted"}`,
		`{"outcome":"smtp_accepted","outcome":"received"}`,
		`{"Outcome":"smtp_accepted"}`,
		`{"outcome":"skipped"}`,
		`{"outcome":"skipped","reason":null}`,
		`{"outcome":"skipped","reason":"x","reason":"y"}`,
		`{"outcome":"smtp_accepted"} {}`,
	} {
		if _, err := decodeOutcome([]byte(body)); err == nil {
			t.Fatalf("ambiguous acknowledgement accepted: %s", body)
		}
	}
	if value, err := decodeOutcome([]byte(`{"outcome":"smtp_accepted","duplicate":true}`)); err != nil || value != "smtp_accepted" {
		t.Fatal("valid deduplication acknowledgement rejected")
	}
}

func TestMalformedNumbersAndOverflowCannotCreateNotification(t *testing.T) {
	for _, mutate := range []func(map[string]any){
		func(row map[string]any) { row["user_id"] = "10019" },
		func(row map[string]any) { row["start_time"] = "1000" },
		func(row map[string]any) { row["pre_sus_time"] = map[string]any{"set": true, "number": "0"} },
		func(row map[string]any) { row["end_time"] = int64(1 << 62) },
		func(row map[string]any) {
			row["end_time"] = wrapped(2000)
			row["suspend_time"] = wrapped(1500)
			row["pre_sus_time"] = wrapped(1000)
		},
	} {
		row := fixtureJob(7, testSLUID, "COMPLETED")
		mutate(row)
		if _, err := Normalize("cluster1", Callback{7, testSLUID}, responseBytes(row), time.Now(), 256<<10); err == nil {
			t.Fatalf("invalid numeric metadata became a notification: %+v", row)
		}
	}
}

func TestResponseSizeBounds(t *testing.T) {
	rest := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, strings.Repeat(" ", (4<<20)+1)) }))
	defer rest.Close()
	master := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"outcome":"smtp_accepted"}`+strings.Repeat(" ", 4096))
	}))
	defer master.Close()
	s := startTestService(t, testConfig(), rest, master)
	if _, err := s.readJob(Callback{7, testSLUID}); err == nil {
		t.Fatal("oversized REST response accepted")
	}
	if s.deliver([]byte(`{}`)) != "retry" {
		t.Fatal("oversized acknowledgement was trusted")
	}
}

func FuzzCallbackWireContract(f *testing.F) {
	for _, seed := range []string{`{"job_id":7,"sluid":"s0123456789ABC"}`, `{"job_id":7,"job_id":8,"sluid":"s0123456789ABC"}`, `null`, `{}`, `[]`, `{"job_id":4294967295,"sluid":"s0123456789ABC"}`} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) > 1024 {
			return
		}
		callback, err := decodeCallback(body)
		if err != nil {
			return
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(body, &object) != nil || len(object) != 2 || object["job_id"] == nil || object["sluid"] == nil || callback.JobID < 1 || callback.JobID > 4294967293 {
			t.Fatal("non-contract object passed decoder")
		}
		var inputID uint32
		var inputSLUID string
		if json.Unmarshal(object["job_id"], &inputID) != nil || json.Unmarshal(object["sluid"], &inputSLUID) != nil || callback.JobID != inputID || callback.SLUID != inputSLUID {
			t.Fatal("decoder changed wire identity")
		}
	})
}

func FuzzNormalizeControllerResponse(f *testing.F) {
	f.Add(responseBytes(fixtureJob(7, testSLUID, "COMPLETED")))
	f.Add(responseBytes(fixtureJob(7, testSLUID, "COMPLETED", "COMPLETING")))
	f.Add([]byte(`{"jobs":null}`))
	f.Add([]byte(`{"jobs":[null]}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) > 128<<10 {
			return
		}
		event, err := Normalize("cluster1", Callback{7, testSLUID}, body, time.Unix(2000, 0), 2048)
		if err != nil {
			return
		}
		encoded, err := json.Marshal(event)
		if err != nil || len(encoded) > 2048 || event.Job.JobID != 7 || event.Job.SLUID != testSLUID || event.Job.UID == 0 || event.Duration.ActiveSeconds <= MinActiveSeconds || event.Duration.ActiveSeconds > (1<<53)-1 {
			t.Fatal("invalid event escaped normalization")
		}
	})
}
