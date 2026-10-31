//go:build !js

package mail

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
)

const maxSessionRead = 256 << 10

func defaultTransport() Transport { return nativeTransport{} }

type nativeTransport struct {
	// nil roots = system roots; dial is overridden only by tests.
	roots *x509.CertPool
	dial  func(ctx context.Context, network, addr string) (net.Conn, error)
}

// limitedConn bounds everything the server may send in one session.
type limitedConn struct {
	net.Conn
	left int
}

func (c *limitedConn) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, &Error{Kind: "smtp_reply_too_large"}
	}
	if len(p) > c.left {
		p = p[:c.left]
	}
	n, err := c.Conn.Read(p)
	c.left -= n
	return n, err
}

type loginAuth struct {
	user, pass string
	step       int
}

func (a *loginAuth) Start(s *smtp.ServerInfo) (string, []byte, error) {
	if !s.TLS {
		return "", nil, errors.New("unencrypted connection")
	}
	return "LOGIN", nil, nil
}

func (a *loginAuth) Next(_ []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	a.step++
	switch a.step {
	case 1:
		return []byte(a.user), nil
	case 2:
		return []byte(a.pass), nil
	}
	return nil, errors.New("unexpected LOGIN challenge")
}

func hasMechanism(list, mech string) bool {
	for _, f := range strings.Fields(strings.ToUpper(list)) {
		if f == mech {
			return true
		}
	}
	return false
}

func fail(kind string, err error) error {
	var se *Error
	if errors.As(err, &se) {
		return se
	}
	var tp *textproto.Error
	return &Error{Kind: kind, Permanent: errors.As(err, &tp) && tp.Code >= 500}
}

func (m nativeTransport) Send(ctx context.Context, cfg SMTPConfig, password, from string, to []string, msg []byte) error {
	if !ValidHost(cfg.Host) || !((cfg.Security == "tls" && cfg.Port == 465) || (cfg.Security == "starttls" && cfg.Port == 587)) {
		return &Error{Kind: "invalid_smtp_endpoint", Permanent: true}
	}
	if cfg.Username == "" || password == "" || strings.ContainsAny(cfg.Username, "\x00\r\n") || strings.Contains(password, "\x00") {
		return &Error{Kind: "smtp_credentials_missing", Permanent: true}
	}
	if cfg.TimeoutMs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout(cfg))
		defer cancel()
	}
	err := m.send(ctx, cfg, password, from, to, msg)
	if err != nil && ctx.Err() != nil {
		return &Error{Kind: "smtp_timeout"}
	}
	return err
}

func (m nativeTransport) send(ctx context.Context, cfg SMTPConfig, password, from string, to []string, msg []byte) error {
	dial := m.dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	raw, err := dial(ctx, "tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)))
	if err != nil {
		return &Error{Kind: "smtp_unavailable"}
	}
	defer raw.Close()
	defer context.AfterFunc(ctx, func() { _ = raw.Close() })()
	if dl, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(dl)
	}
	tlsCfg := &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12, RootCAs: m.roots}
	var conn net.Conn = &limitedConn{Conn: raw, left: maxSessionRead}
	if cfg.Security == "tls" {
		tc := tls.Client(conn, tlsCfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			return fail("smtp_tls_failed", err)
		}
		conn = tc
	}
	c, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		return fail("smtp_rejected", err)
	}
	defer c.Close()
	if err := c.Hello("go-ldap-admin.invalid"); err != nil {
		return fail("smtp_rejected", err)
	}
	if cfg.Security == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return &Error{Kind: "smtp_starttls_required"}
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return fail("smtp_tls_failed", err)
		}
	}
	_, mechs := c.Extension("AUTH")
	var auth smtp.Auth
	switch {
	case hasMechanism(mechs, "PLAIN"):
		auth = smtp.PlainAuth("", cfg.Username, password, cfg.Host)
	case hasMechanism(mechs, "LOGIN"):
		auth = &loginAuth{user: cfg.Username, pass: password}
	default:
		return &Error{Kind: "smtp_authentication_unsupported"}
	}
	if err := c.Auth(auth); err != nil {
		return fail("smtp_authentication_failed", err)
	}
	if err := c.Mail(from); err != nil {
		return fail("smtp_sender_rejected", err)
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return fail("smtp_recipient_rejected", err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fail("smtp_rejected", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fail("smtp_data_rejected", err)
	}
	if err := w.Close(); err != nil {
		return fail("smtp_data_rejected", err)
	}
	// DATA acceptance is the delivery boundary; a failed QUIT must not cause a resend.
	_ = c.Quit()
	return nil
}
