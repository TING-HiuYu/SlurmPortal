//go:build !js

package mail

import (
	"context"
	"crypto/x509"
	"net"
	"time"
)

// NativeTransportForTest trusts roots and dials addr instead of host:port.
func NativeTransportForTest(roots *x509.CertPool, addr string) Transport {
	return nativeTransport{roots: roots, dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
}

func BuildMessageForTest(cfg SMTPConfig, to, subject, text string) ([]byte, error) {
	return BuildMessage(cfg, Message{ID: "test", To: to, Subject: subject, Text: text, HTML: "<p>" + text + "</p>"}, time.Now())
}
