package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// The rest kind delivers through any HTTP e-mail API (Cloudflare Email Service, Brevo, Mailgun …).
// The admin writes the request as a cURL command line (-X method, URL, -H headers, -d body) with
// {{ placeholders }} for the message and {{ secrets.NAME }} for sealed secrets.

// RESTConfig is the stored, non-secret part of a rest provider; secret values are sealed separately.
type RESTConfig struct {
	Request     string   `json:"request"`
	FromAddress string   `json:"fromAddress"`
	FromName    string   `json:"fromName"`
	TimeoutMs   int      `json:"timeoutMs"`
	SecretNames []string `json:"secretNames"`
}

// RESTSecretsPurpose binds the sealed JSON object of a rest provider's secret values.
const RESTSecretsPurpose = "rest-mail-secrets"

// Placeholder documents one value a request template can use.
type Placeholder struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// RESTPlaceholders are the message values; secrets are {{ secrets.NAME }}.
var RESTPlaceholders = []Placeholder{
	{"to", "收件人地址"},
	{"from", "发件人：名称 <地址>，未设置名称时只有地址"},
	{"from.address", "发件地址"},
	{"from.name", "发件人名称，可能为空"},
	{"subject", "邮件主题"},
	{"html", "HTML 正文"},
	{"text", "纯文本正文"},
	{"message_id", "本封邮件的稳定 ID，重发时不变，可用作幂等键"},
}

var (
	placeholderRE = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z0-9_]+)?)\s*\}\}`)
	secretNameRE  = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	headerNameRE  = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
	restMethods   = []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodGet}
)

const maxRESTRequest = 16 << 10

// restTemplate is a parsed request: every part may still hold placeholders.
type restTemplate struct {
	Method  string
	URL     string
	Headers [][2]string
	Body    string
	HasBody bool
}

// shellWords splits a cURL command line like a POSIX shell would for quoting: '…' is literal,
// "…" honours \" \\ \$ \`, a backslash-newline continues the line. Nothing is expanded.
func shellWords(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && (s[i+1] == '\n' || s[i+1] == '\r'):
			i++
			if s[i] == '\r' && i+1 < len(s) && s[i+1] == '\n' {
				i++
			}
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			inWord = true
		case c == '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return nil, errors.New("unterminated single quote")
			}
			cur.WriteString(s[i+1 : i+1+end])
			i += end + 1
			inWord = true
		case c == '"':
			i++
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`\n", s[i+1]) >= 0 {
					i++
					if s[i] == '\n' {
						continue
					}
				}
				cur.WriteByte(s[i])
			}
			if i >= len(s) {
				return nil, errors.New("unterminated double quote")
			}
			inWord = true
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if inWord {
				words, inWord = append(words, cur.String()), false
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

func parseCurl(s string) (restTemplate, error) {
	var t restTemplate
	words, err := shellWords(s)
	if err != nil {
		return t, err
	}
	if len(words) > 0 && words[0] == "curl" {
		words = words[1:]
	}
	var bodies []string
	for i := 0; i < len(words); i++ {
		w := words[i]
		arg := func() (string, error) {
			if i+1 >= len(words) {
				return "", fmt.Errorf("%s needs a value", w)
			}
			i++
			return words[i], nil
		}
		switch w {
		case "-H", "--header":
			v, err := arg()
			if err != nil {
				return t, err
			}
			name, value, ok := strings.Cut(v, ":")
			name = strings.TrimSpace(name)
			if !ok || !headerNameRE.MatchString(name) {
				return t, fmt.Errorf("invalid header %q", v)
			}
			t.Headers = append(t.Headers, [2]string{name, strings.TrimSpace(value)})
		case "-d", "--data", "--data-raw", "--data-binary":
			v, err := arg()
			if err != nil {
				return t, err
			}
			bodies = append(bodies, v)
		case "--url":
			v, err := arg()
			if err != nil {
				return t, err
			}
			if t.URL != "" {
				return t, errors.New("more than one URL")
			}
			t.URL = v
		case "-X", "--request":
			v, err := arg()
			if err != nil {
				return t, err
			}
			t.Method = strings.ToUpper(v)
		case "-s", "--silent", "-S", "--show-error", "-f", "--fail", "-L", "--location", "-v", "--verbose", "-i", "--include":
			// cosmetic curl switches in pasted examples
		default:
			if strings.HasPrefix(w, "-") {
				return t, fmt.Errorf("unsupported curl option %s (use -H, -d and the URL)", w)
			}
			if t.URL != "" {
				return t, errors.New("more than one URL")
			}
			t.URL = w
		}
	}
	if t.URL == "" {
		return t, errors.New("the request needs a URL")
	}
	t.Body, t.HasBody = strings.Join(bodies, "&"), len(bodies) > 0
	if t.Method == "" {
		// curl's default: POST with -d, GET without.
		t.Method = http.MethodGet
		if t.HasBody {
			t.Method = http.MethodPost
		}
	}
	if !slices.Contains(restMethods, t.Method) {
		return t, errors.New("method must be POST, PUT, PATCH or GET")
	}
	if t.Method == http.MethodGet && t.HasBody {
		return t, errors.New("a GET request cannot have a body (-d)")
	}
	return t, nil
}

func (t restTemplate) header(name string) string {
	for _, h := range t.Headers {
		if strings.EqualFold(h[0], name) {
			return h[1]
		}
	}
	return ""
}

// bodyMode decides how placeholder values are encoded in the body, from the Content-Type header.
func (t restTemplate) bodyMode() string {
	ct := strings.ToLower(t.header("Content-Type"))
	switch {
	case strings.Contains(ct, "json"):
		return "json"
	case strings.Contains(ct, "x-www-form-urlencoded"):
		return "form"
	}
	return "raw"
}

// render substitutes placeholders in s. mode: "json" (complete JSON strings), "form"/"query"
// (query escaping), "path" (path escaping), "header" (as is, no line breaks).
func render(s, mode string, vals map[string]string) (string, error) {
	var err error
	out := placeholderRE.ReplaceAllStringFunc(s, func(m string) string {
		name := placeholderRE.FindStringSubmatch(m)[1]
		v, ok := vals[name]
		if !ok {
			err = fmt.Errorf("unknown placeholder {{ %s }}", name)
			return ""
		}
		switch mode {
		case "json":
			b, _ := json.Marshal(v)
			return string(b)
		case "form", "query":
			return url.QueryEscape(v)
		case "path":
			return url.PathEscape(v)
		case "header":
			if strings.ContainsAny(v, "\r\n\x00") {
				err = errors.New("a header value contains a line break")
			}
		}
		return v
	})
	return out, err
}

// renderURL escapes values as path segments before '?' and as query values after it.
func renderURL(s string, vals map[string]string) (string, error) {
	path, query, hasQuery := strings.Cut(s, "?")
	p, err := render(path, "path", vals)
	if err != nil || !hasQuery {
		return p, err
	}
	q, err := render(query, "query", vals)
	return p + "?" + q, err
}

type restRequest struct {
	URL     string
	Headers [][2]string
	Body    []byte
}

func (t restTemplate) build(vals map[string]string) (restRequest, error) {
	var r restRequest
	var err error
	if r.URL, err = renderURL(t.URL, vals); err != nil {
		return r, err
	}
	for _, h := range t.Headers {
		v, err := render(h[1], "header", vals)
		if err != nil {
			return r, err
		}
		r.Headers = append(r.Headers, [2]string{h[0], v})
	}
	if t.HasBody {
		b, err := render(t.Body, t.bodyMode(), vals)
		if err != nil {
			return r, err
		}
		r.Body = []byte(b)
	}
	return r, nil
}

// sender formats "Name <address>", quoting the name when it holds RFC 5322 specials.
func sender(name, address string) string {
	if name == "" {
		return address
	}
	if strings.ContainsAny(name, "()<>[]:;@\\,.\"") {
		name = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(name) + `"`
	}
	return name + " <" + address + ">"
}

func messageValues(c RESTConfig, m Message, secrets map[string]string) map[string]string {
	v := map[string]string{"to": m.To, "from": sender(c.FromName, c.FromAddress), "from.address": c.FromAddress, "from.name": c.FromName, "subject": m.Subject,
		"html": m.HTML, "text": m.Text, "message_id": m.ID}
	for k, s := range secrets {
		v["secrets."+k] = s
	}
	return v
}

func validEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("the request URL is not a valid absolute URL")
	}
	host := u.Hostname()
	loopback := host == "localhost" || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return errors.New("the request URL must use https")
	}
	return nil
}

func (c RESTConfig) validate(enabled bool) (RESTConfig, restTemplate, error) {
	var t restTemplate
	if len(c.Request) > maxRESTRequest || containsNUL(c.Request) {
		return c, t, invalid("the request is too long")
	}
	var err error
	if c.FromAddress, err = checkedString(strings.TrimSpace(c.FromAddress), 254, "sender address"); err != nil {
		return c, t, err
	}
	if c.FromName, err = checkedString(c.FromName, 200, "sender name"); err != nil {
		return c, t, err
	}
	if c.FromAddress != "" && !ValidAddress(c.FromAddress) {
		return c, t, invalid("invalid sender address")
	}
	if c.TimeoutMs < 1000 || c.TimeoutMs > 15000 {
		return c, t, invalid("timeout must be 1000-15000ms")
	}
	if len(c.SecretNames) > 20 {
		return c, t, invalid("at most 20 secrets")
	}
	if c.SecretNames == nil {
		c.SecretNames = []string{}
	}
	for i, n := range c.SecretNames {
		if !secretNameRE.MatchString(n) || slices.Contains(c.SecretNames[:i], n) {
			return c, t, invalid("secret names must be unique UPPER_SNAKE_CASE, e.g. API_TOKEN")
		}
	}
	if strings.TrimSpace(c.Request) == "" {
		if enabled {
			return c, t, invalid("the request is required")
		}
		return c, t, nil
	}
	if t, err = parseCurl(c.Request); err != nil {
		return c, t, invalid("request: " + err.Error())
	}
	if !strings.Contains(c.Request, "{{") || !slices.ContainsFunc(placeholderRE.FindAllStringSubmatch(c.Request, -1), func(m []string) bool { return m[1] == "to" }) {
		return c, t, invalid("the request must use {{ to }}")
	}
	// Render with sample values: unknown placeholders and undeclared secrets fail here, and a JSON body must parse.
	secrets := map[string]string{}
	for _, n := range c.SecretNames {
		secrets[n] = "secret"
	}
	sample := messageValues(c, Message{ID: "id", To: "user@example.org", Subject: "s", HTML: "<p>\"h\"</p>", Text: "t\n"}, secrets)
	r, err := t.build(sample)
	if err != nil {
		return c, t, invalid("request: " + err.Error())
	}
	if err := validEndpoint(r.URL); err != nil {
		return c, t, invalid(err.Error())
	}
	if t.bodyMode() == "json" && !json.Valid(r.Body) {
		return c, t, invalid("the JSON body is not valid JSON once filled in; placeholders become complete JSON strings, so do not put quotes around them")
	}
	if enabled && c.FromAddress == "" {
		return c, t, invalid("sender address is required")
	}
	return c, t, nil
}

type restKind struct{}

func (restKind) ID() string            { return "rest" }
func (restKind) Label() string         { return "REST" }
func (restKind) SecretPurpose() string { return RESTSecretsPurpose }

func (restKind) Normalize(raw json.RawMessage, enabled, _ bool) (json.RawMessage, error) {
	c := RESTConfig{TimeoutMs: 15000}
	// A page loaded before the method moved into the request still sends "method"; ignore it.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, invalid("invalid REST configuration")
	}
	delete(fields, "method")
	raw, _ = json.Marshal(fields)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, invalid("invalid REST configuration: " + err.Error())
	}
	c, _, err := c.validate(enabled)
	if err != nil {
		return nil, err
	}
	return json.Marshal(c)
}

// MergeSecret applies an update {"NAME": "value"} to the stored values: a missing or empty value keeps
// the stored one, and names no longer declared in config are dropped. Every declared name needs a value.
func (restKind) MergeSecret(current, update string, config json.RawMessage) (string, error) {
	var c RESTConfig
	if err := json.Unmarshal(config, &c); err != nil {
		return "", err
	}
	cur, upd := map[string]string{}, map[string]string{}
	if current != "" {
		if err := json.Unmarshal([]byte(current), &cur); err != nil {
			return "", invalid("stored secrets cannot be read; set them again")
		}
	}
	if update != "" {
		if err := json.Unmarshal([]byte(update), &upd); err != nil {
			return "", invalid("secrets must be a JSON object of NAME: value")
		}
	}
	out := map[string]string{}
	for _, n := range c.SecretNames {
		v := upd[n]
		if v == "" {
			v = cur[n]
		}
		if v == "" {
			return "", invalid("secret " + n + " has no value")
		}
		if len(v) > 4096 || strings.ContainsAny(v, "\x00") {
			return "", invalid("invalid value for secret " + n)
		}
		out[n] = v
	}
	for n := range upd {
		if !slices.Contains(c.SecretNames, n) {
			return "", invalid("secret " + n + " is not declared")
		}
	}
	if len(out) == 0 {
		return "", nil
	}
	b, err := json.Marshal(out)
	return string(b), err
}

// Placeholders lists the message values a request can use.
func (restKind) Placeholders() []Placeholder { return RESTPlaceholders }

func (restKind) Open(_ context.Context, h *Hub, s Stored, secret string) (Provider, error) {
	var c RESTConfig
	if err := json.Unmarshal(s.Config, &c); err != nil {
		return nil, err
	}
	secrets := map[string]string{}
	if secret != "" {
		if err := json.Unmarshal([]byte(secret), &secrets); err != nil {
			return nil, err
		}
	}
	return &restProvider{cfg: c, secrets: secrets, client: h.httpClient}, nil
}

type restProvider struct {
	cfg     RESTConfig
	secrets map[string]string
	client  *http.Client
}

func (p *restProvider) Send(ctx context.Context, m Message) error {
	if !ValidAddress(m.To) {
		return &Error{Kind: "invalid_mail_address", Permanent: true}
	}
	if len(m.Subject)+len(m.Text)+len(m.HTML) > maxMessageBytes {
		return &Error{Kind: "message_too_large", Permanent: true}
	}
	c, t, err := p.cfg.validate(true)
	if err != nil {
		return &Error{Kind: "rest_config_invalid", Permanent: true}
	}
	for _, n := range c.SecretNames {
		if p.secrets[n] == "" {
			return &Error{Kind: "rest_credentials_missing", Permanent: true}
		}
	}
	r, err := t.build(messageValues(c, m, p.secrets))
	if err != nil {
		return &Error{Kind: "rest_config_invalid", Permanent: true}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(c.TimeoutMs)*time.Millisecond)
	defer cancel()
	var body io.Reader
	if t.HasBody {
		body = bytes.NewReader(r.Body)
	}
	req, err := http.NewRequestWithContext(ctx, t.Method, r.URL, body)
	if err != nil {
		return &Error{Kind: "rest_config_invalid", Permanent: true}
	}
	for _, h := range r.Headers {
		req.Header.Add(h[0], h[1])
	}
	res, err := p.client.Do(req)
	if err != nil {
		return &Error{Kind: "rest_unavailable"}
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	switch {
	case res.StatusCode >= 200 && res.StatusCode < 300:
		return nil
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return &Error{Kind: "rest_auth_failed", Permanent: true}
	case res.StatusCode == http.StatusRequestTimeout || res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500:
		return &Error{Kind: "rest_unavailable"}
	}
	return &Error{Kind: "rest_rejected", Permanent: true}
}
