package mail_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	. "github.com/TING-HiuYu/SlurmPortal/internal/mail"
)

type restCall struct {
	method, path, query, auth, ctype string
	body                             []byte
}

func restServer(t *testing.T, status *int) (*httptest.Server, *restCall) {
	got := &restCall{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path, got.query = r.Method, r.URL.EscapedPath(), r.URL.RawQuery
		got.auth, got.ctype = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		got.body, _ = io.ReadAll(r.Body)
		w.WriteHeader(*status)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func restInput(request string, secretNames []string, secrets map[string]string) Input {
	cfg, _ := json.Marshal(RESTConfig{Request: request, FromAddress: "no-reply@example.org", FromName: "Example Lab, HPC", TimeoutMs: 2000, SecretNames: secretNames})
	in := Input{Kind: "rest", Enabled: true, Config: cfg}
	if secrets != nil {
		b, _ := json.Marshal(secrets)
		s := string(b)
		in.Secret = &s
	}
	return in
}

func TestRESTProvider(t *testing.T) {
	status := http.StatusOK
	srv, got := restServer(t, &status)
	h, _ := newHub(t, masterKey)
	ctx := context.Background()
	// The admin's prototype, with the provider's placeholders and a declared secret.
	request := `curl -X POST "` + srv.URL + `/client/v4/accounts/0123456789abcdef0123456789abcdef/email/sending/send" \
  -H "Authorization: Bearer {{ secrets.CLOUDFLARE_API_TOKEN }}" \
  -H "Content-Type: application/json" \
  -d '{
    "to": {{ to }},
    "from": {"address": {{ from.address }}, "name": {{ from.name }}},
    "reply": {{ from }},
    "subject": {{ subject }},
    "html": {{ html }},
    "text": {{ text }}
  }'`
	names := []string{"CLOUDFLARE_API_TOKEN"}
	if _, err := h.Save(ctx, "cf", restInput(request, names, nil)); err == nil {
		t.Fatal("declared secret without a value accepted")
	}
	if _, err := h.Save(ctx, "cf", restInput(request, names, map[string]string{"CLOUDFLARE_API_TOKEN": "tok-1"})); err != nil {
		t.Fatal(err)
	}
	msg := Message{ID: "m1", To: "user@example.org", Subject: `Job "42" done`, Text: "line1\nline2", HTML: `<p class="x">done</p>`}
	if err := h.Send(ctx, "cf", msg); err != nil {
		t.Fatal(err)
	}
	var body map[string]string
	var from struct{ Address, Name string }
	var parts map[string]json.RawMessage
	if err := json.Unmarshal(got.body, &parts); err != nil {
		t.Fatalf("body %s: %v", got.body, err)
	}
	_ = json.Unmarshal(parts["from"], &from)
	delete(parts, "from")
	b, _ := json.Marshal(parts)
	_ = json.Unmarshal(b, &body)
	if got.method != "POST" || got.path != "/client/v4/accounts/0123456789abcdef0123456789abcdef/email/sending/send" || got.auth != "Bearer tok-1" ||
		got.ctype != "application/json" || body["to"] != msg.To || body["reply"] != `"Example Lab, HPC" <no-reply@example.org>` || from.Address != "no-reply@example.org" || from.Name != "Example Lab, HPC" || body["subject"] != msg.Subject ||
		body["text"] != msg.Text || body["html"] != msg.HTML {
		t.Fatalf("request %+v body %v", got, body)
	}

	// Saving again without secret values keeps the stored ones; a new value replaces just that one.
	if _, err := h.Save(ctx, "cf", restInput(request, names, map[string]string{})); err != nil {
		t.Fatal(err)
	}
	if err := h.Send(ctx, "cf", msg); err != nil || got.auth != "Bearer tok-1" {
		t.Fatalf("kept secret: %v %q", err, got.auth)
	}
	if _, err := h.Save(ctx, "cf", restInput(request, names, map[string]string{"CLOUDFLARE_API_TOKEN": "tok-2"})); err != nil {
		t.Fatal(err)
	}
	if err := h.Send(ctx, "cf", msg); err != nil || got.auth != "Bearer tok-2" {
		t.Fatalf("replaced secret: %v %q", err, got.auth)
	}

	var e *Error
	for code, kind := range map[int]string{http.StatusUnauthorized: "rest_auth_failed", http.StatusBadRequest: "rest_rejected", http.StatusTooManyRequests: "rest_unavailable", http.StatusBadGateway: "rest_unavailable"} {
		status = code
		if err := h.Send(ctx, "cf", msg); !errors.As(err, &e) || e.Kind != kind || e.Permanent == (kind == "rest_unavailable") {
			t.Fatalf("%d: %v", code, err)
		}
	}
	status = http.StatusOK

	// Form bodies and query strings are URL-escaped; GET carries values in the query.
	form := `curl ` + srv.URL + `/v3/messages -H "Content-Type: application/x-www-form-urlencoded" -H "X-Key: {{ secrets.KEY }}" -d "to={{ to }}&subject={{ subject }}&text={{ text }}"`
	if _, err := h.Save(ctx, "form", restInput(form, []string{"KEY"}, map[string]string{"KEY": "k"})); err != nil {
		t.Fatal(err)
	}
	if err := h.Send(ctx, "form", msg); err != nil || string(got.body) != "to=user%40example.org&subject=Job+%2242%22+done&text=line1%0Aline2" {
		t.Fatalf("form body %q %v", got.body, err)
	}
	get := `curl "` + srv.URL + `/send/{{ to }}?subject={{ subject }}&id={{ message_id }}"`
	put := `curl -X PUT ` + srv.URL + `/put -d '{{ to }}'`
	if _, err := h.Save(ctx, "put", restInput(put, nil, nil)); err != nil {
		t.Fatal(err)
	}
	if err := h.Send(ctx, "put", msg); err != nil || got.method != "PUT" {
		t.Fatalf("put %+v %v", got, err)
	}
	if _, err := h.Save(ctx, "get", restInput(get, nil, nil)); err != nil {
		t.Fatal(err)
	}
	if err := h.Send(ctx, "get", msg); err != nil || got.method != "GET" || got.path != "/send/user@example.org" || got.query != "subject=Job+%2242%22+done&id=m1" {
		t.Fatalf("get %+v %v", got, err)
	}
}

func TestRESTValidation(t *testing.T) {
	h, _ := newHub(t, masterKey)
	ctx := context.Background()
	ok := `curl https://api.example.com/send -H "Content-Type: application/json" -d '{"to": {{ to }}}'`
	for name, c := range map[string]struct {
		request string
		secrets []string
	}{
		"bad method":          {`curl -X DELETE https://api.example.com/send -d '{{ to }}'`, nil},
		"no to":               {`curl https://api.example.com/send -d 'x'`, nil},
		"plain http":          {`curl http://api.example.com/send -d '{{ to }}'`, nil},
		"unknown placeholder": {`curl https://api.example.com/send -d '{{ to }} {{ from_name }}'`, nil},
		"undeclared secret":   {`curl https://api.example.com/send -H "X: {{ secrets.TOKEN }}" -d '{{ to }}'`, nil},
		"quoted json":         {`curl https://api.example.com/send -H "Content-Type: application/json" -d '{"to": "{{ to }}"}'`, nil},
		"get with body":       {`curl -X GET https://api.example.com/send -d '{{ to }}'`, nil},
		"unknown option":      {`curl -F x=1 https://api.example.com/send -d '{{ to }}'`, nil},
		"unterminated quote":  {`curl https://api.example.com/send -d '{{ to }}`, nil},
		"bad secret name":     {ok, []string{"token"}},
	} {
		if _, err := h.Save(ctx, "p", restInput(c.request, c.secrets, map[string]string{"token": "x"})); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := h.Save(ctx, "p", restInput(ok, nil, nil)); err != nil {
		t.Fatal(err)
	}
	legacy := Input{Kind: "rest", Enabled: true, Config: json.RawMessage(`{"method":"POST","request":` + strconv.Quote(ok) + `,"fromAddress":"a@example.com","timeoutMs":2000,"secretNames":[]}`)}
	if _, err := h.Save(ctx, "legacy", legacy); err != nil {
		t.Fatalf("legacy method field: %v", err)
	}
	unknown := Input{Kind: "rest", Enabled: true, Config: json.RawMessage(`{"request":` + strconv.Quote(ok) + `,"fromAddress":"a@example.com","url":"x"}`)}
	if _, err := h.Save(ctx, "unknown", unknown); err == nil || !strings.Contains(err.Error(), `"url"`) {
		t.Fatalf("unknown field not named: %v", err)
	}
	if _, err := h.Save(ctx, "p", restInput(ok, nil, map[string]string{"EXTRA": "x"})); err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("undeclared secret value: %v", err)
	}
}
