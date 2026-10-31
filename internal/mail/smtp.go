package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"
)

// SMTPConfig is the stored, non-secret part of an SMTP provider; the password is sealed separately.
type SMTPConfig struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Security    string `json:"security"`
	Username    string `json:"username"`
	FromAddress string `json:"fromAddress"`
	FromName    string `json:"fromName"`
	ReplyTo     string `json:"replyTo"`
	TimeoutMs   int    `json:"timeoutMs"`
}

func timeout(c SMTPConfig) time.Duration { return time.Duration(c.TimeoutMs) * time.Millisecond }

// DefaultSMTP is the form a new SMTP provider starts from.
func DefaultSMTP() SMTPConfig { return SMTPConfig{Port: 465, Security: "tls", TimeoutMs: 15000} }

func checkedString(v string, maxLen int, name string) (string, error) {
	if len(v) > maxLen || strings.ContainsAny(v, "\r\n\x00") {
		return "", invalid("invalid " + name)
	}
	return v, nil
}

// Validate normalises c; ready additionally requires everything a delivery needs.
func (c SMTPConfig) Validate(ready bool) (SMTPConfig, error) {
	var err error
	for _, f := range []struct {
		v    *string
		max  int
		name string
	}{
		{&c.Host, 253, "SMTP host"}, {&c.Username, 512, "SMTP username"}, {&c.FromAddress, 254, "sender address"},
		{&c.FromName, 200, "sender name"}, {&c.ReplyTo, 254, "reply address"},
	} {
		if *f.v, err = checkedString(*f.v, f.max, f.name); err != nil {
			return c, err
		}
	}
	c.Host = strings.ToLower(c.Host)
	if !((c.Security == "tls" && c.Port == 465) || (c.Security == "starttls" && c.Port == 587)) {
		return c, invalid("SMTP requires TLS on 465 or STARTTLS on 587")
	}
	if c.TimeoutMs < 1000 || c.TimeoutMs > 15000 {
		return c, invalid("SMTP timeout must be 1000-15000ms")
	}
	if (c.Host != "" && !ValidHost(c.Host)) || (c.FromAddress != "" && !ValidAddress(c.FromAddress)) || (c.ReplyTo != "" && !ValidAddress(c.ReplyTo)) {
		return c, invalid("invalid SMTP endpoint or address")
	}
	if ready && (c.Host == "" || c.Username == "" || c.FromAddress == "") {
		return c, invalid("SMTP host, username and sender are required")
	}
	return c, nil
}

// SMTPPasswordPurpose binds sealed SMTP passwords; v1 and the earlier v2 notification config used the same purpose.
const SMTPPasswordPurpose = "smtp-password"

type smtpKind struct{}

func (smtpKind) ID() string            { return "smtp" }
func (smtpKind) Label() string         { return "SMTP" }
func (smtpKind) SecretPurpose() string { return SMTPPasswordPurpose }

func (smtpKind) Normalize(raw json.RawMessage, enabled, secretSet bool) (json.RawMessage, error) {
	c := DefaultSMTP()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, invalid("invalid SMTP configuration")
	}
	c, err := c.Validate(enabled)
	if err != nil {
		return nil, err
	}
	if enabled && !secretSet {
		return nil, invalid("SMTP password is required")
	}
	return json.Marshal(c)
}

func (smtpKind) Open(_ context.Context, h *Hub, s Stored, secret string) (Provider, error) {
	var c SMTPConfig
	if err := json.Unmarshal(s.Config, &c); err != nil {
		return nil, err
	}
	return &smtpProvider{cfg: c, password: secret, transport: h.transport, now: h.now}, nil
}

type smtpProvider struct {
	cfg       SMTPConfig
	password  string
	transport Transport
	now       func() time.Time
}

func (p *smtpProvider) Send(ctx context.Context, m Message) error {
	if p.cfg.Host == "" || p.cfg.Username == "" || p.cfg.FromAddress == "" || p.password == "" {
		return &Error{Kind: "smtp_credentials_missing", Permanent: true}
	}
	msg, err := BuildMessage(p.cfg, m, p.now())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout(p.cfg))
	defer cancel()
	return p.transport.Send(ctx, p.cfg, p.password, p.cfg.FromAddress, []string{m.To}, msg)
}
