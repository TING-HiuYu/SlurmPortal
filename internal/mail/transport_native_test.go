//go:build !js

package mail_test

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	. "github.com/TING-HiuYu/SlurmPortal/internal/mail"
)

const smtpHost = "smtp.example.com"

func testCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: smtpHost}, DNSNames: []string{smtpHost},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

type fakeServer struct {
	addr     string
	implicit bool
	starttls bool
	authMech string
	mu       sync.Mutex
	log      []string
	data     string
}

func (s *fakeServer) record(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log = append(s.log, line)
}

func (s *fakeServer) commands() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.log, "|")
}

func startServer(t *testing.T, implicit, starttls bool, authMech string) (*fakeServer, *x509.CertPool) {
	t.Helper()
	cert, pool := testCert(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &fakeServer{addr: ln.Addr().String(), implicit: implicit, starttls: starttls, authMech: authMech}
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c, tlsCfg)
		}
	}()
	return s, pool
}

func (s *fakeServer) serve(c net.Conn, tlsCfg *tls.Config) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	secure := s.implicit
	if s.implicit {
		c = tls.Server(c, tlsCfg)
	}
	r, w := bufio.NewReader(c), bufio.NewWriter(c)
	send := func(lines ...string) {
		for _, l := range lines {
			w.WriteString(l + "\r\n")
		}
		w.Flush()
	}
	send("220 " + smtpHost + " ESMTP")
	loginStep := 0
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if loginStep > 0 {
			loginStep++
			if loginStep == 2 {
				send("334 UGFzc3dvcmQ6")
			} else {
				pw, _ := base64.StdEncoding.DecodeString(line)
				loginStep = 0
				if string(pw) == "pw" {
					send("235 ok")
				} else {
					send("535 no")
				}
			}
			continue
		}
		cmd := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
		s.record(cmd)
		switch {
		case cmd == "EHLO":
			ext := []string{"250-" + smtpHost}
			if !secure && s.starttls {
				ext = append(ext, "250-STARTTLS")
			}
			send(append(ext, "250 AUTH "+s.authMech)...)
		case cmd == "STARTTLS":
			send("220 go ahead")
			c = tls.Server(c, tlsCfg)
			r, w = bufio.NewReader(c), bufio.NewWriter(c)
			secure = true
		case cmd == "AUTH" && strings.HasPrefix(strings.ToUpper(line), "AUTH PLAIN "):
			tok, _ := base64.StdEncoding.DecodeString(line[len("AUTH PLAIN "):])
			if string(tok) == "\x00user\x00pw" {
				send("235 ok")
			} else {
				send("535 bad credentials")
			}
		case cmd == "AUTH":
			loginStep = 1
			send("334 VXNlcm5hbWU6")
		case cmd == "MAIL":
			send("250 ok")
		case cmd == "RCPT":
			if strings.Contains(line, "reject@") {
				send("550 no such user")
			} else {
				send("250 ok")
			}
		case cmd == "DATA":
			send("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			s.mu.Lock()
			s.data = b.String()
			s.mu.Unlock()
			send("250 queued")
		case cmd == "QUIT":
			send("221 bye")
			return
		default:
			send("502 unknown")
		}
	}
}

func cfgFor(implicit bool) SMTPConfig {
	if implicit {
		return SMTPConfig{Host: smtpHost, Port: 465, Security: "tls", Username: "user", FromAddress: "bot@example.com", TimeoutMs: 3000}
	}
	return SMTPConfig{Host: smtpHost, Port: 587, Security: "starttls", Username: "user", FromAddress: "bot@example.com", TimeoutMs: 3000}
}

func TestNativeTransport(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		implicit bool
		mech     string
	}{{"implicit tls plain", true, "PLAIN LOGIN"}, {"starttls plain", false, "PLAIN"}, {"starttls login", false, "LOGIN"}} {
		t.Run(tc.name, func(t *testing.T) {
			srv, roots := startServer(t, tc.implicit, true, tc.mech)
			cfg := cfgFor(tc.implicit)
			msg, err := BuildMessageForTest(cfg, "u@example.com", "subject", "body")
			if err != nil {
				t.Fatal(err)
			}
			msg = append(msg, []byte(".leading dot\r\n")...)
			if err := NativeTransportForTest(roots, srv.addr).Send(ctx, cfg, "pw", cfg.FromAddress, []string{"u@example.com"}, msg); err != nil {
				t.Fatalf("send: %v (%s)", err, srv.commands())
			}
			srv.mu.Lock()
			data := srv.data
			srv.mu.Unlock()
			if !strings.Contains(data, "To: <u@example.com>") || !strings.Contains(data, "\r\n..leading dot\r\n") {
				t.Fatalf("data %q", data)
			}
			if !tc.implicit && !strings.HasPrefix(srv.commands(), "EHLO|STARTTLS|EHLO|AUTH") {
				t.Fatalf("dialogue %s", srv.commands())
			}
		})
	}

	t.Run("starttls not offered", func(t *testing.T) {
		srv, roots := startServer(t, false, false, "PLAIN")
		cfg := cfgFor(false)
		err := NativeTransportForTest(roots, srv.addr).Send(ctx, cfg, "pw", cfg.FromAddress, []string{"u@example.com"}, []byte("x\r\n"))
		var se *Error
		if !errors.As(err, &se) || se.Kind != "smtp_starttls_required" || strings.Contains(srv.commands(), "AUTH") {
			t.Fatalf("%v %s", err, srv.commands())
		}
	})
	t.Run("untrusted certificate", func(t *testing.T) {
		srv, _ := startServer(t, true, false, "PLAIN")
		_, other := testCert(t)
		cfg := cfgFor(true)
		err := NativeTransportForTest(other, srv.addr).Send(ctx, cfg, "pw", cfg.FromAddress, []string{"u@example.com"}, []byte("x\r\n"))
		var se *Error
		if !errors.As(err, &se) || se.Kind != "smtp_tls_failed" {
			t.Fatalf("%v", err)
		}
	})
	t.Run("auth failure", func(t *testing.T) {
		srv, roots := startServer(t, true, false, "PLAIN")
		cfg := cfgFor(true)
		err := NativeTransportForTest(roots, srv.addr).Send(ctx, cfg, "wrong", cfg.FromAddress, []string{"u@example.com"}, []byte("x\r\n"))
		var se *Error
		if !errors.As(err, &se) || se.Kind != "smtp_authentication_failed" || strings.Contains(srv.commands(), "MAIL") {
			t.Fatalf("%v %s", err, srv.commands())
		}
	})
	t.Run("recipient rejected", func(t *testing.T) {
		srv, roots := startServer(t, true, false, "PLAIN")
		cfg := cfgFor(true)
		err := NativeTransportForTest(roots, srv.addr).Send(ctx, cfg, "pw", cfg.FromAddress, []string{"reject@example.com"}, []byte("x\r\n"))
		var se *Error
		if !errors.As(err, &se) || se.Kind != "smtp_recipient_rejected" || !se.Permanent {
			t.Fatalf("%v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		go func() {
			c, err := ln.Accept()
			if err == nil {
				time.Sleep(3 * time.Second)
				c.Close()
			}
		}()
		cfg := cfgFor(true)
		cfg.TimeoutMs = 1000
		start := time.Now()
		err = NativeTransportForTest(nil, ln.Addr().String()).Send(ctx, cfg, "pw", cfg.FromAddress, []string{"u@example.com"}, []byte("x\r\n"))
		var se *Error
		if !errors.As(err, &se) || se.Kind != "smtp_timeout" || time.Since(start) > 2*time.Second {
			t.Fatalf("%v after %v", err, time.Since(start))
		}
	})
	t.Run("endpoint policy", func(t *testing.T) {
		cfg := cfgFor(true)
		cfg.Port = 25
		err := NativeTransportForTest(nil, "127.0.0.1:1").Send(ctx, cfg, "pw", cfg.FromAddress, []string{"u@example.com"}, []byte("x\r\n"))
		var se *Error
		if !errors.As(err, &se) || se.Kind != "invalid_smtp_endpoint" {
			t.Fatalf("%v", err)
		}
	})
}
