package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"time"
)

// httpsIdentity owns only the public HTTPS leaf. Reloading it must not rebuild
// the gateway, its SSH sessions, or the independent outbound master mTLS client.
type httpsIdentity struct {
	certificatePath string
	keyPath         string
	current         atomic.Pointer[tls.Certificate]
}

func newHTTPSIdentity(certificatePath, keyPath string, initial tls.Certificate) *httpsIdentity {
	identity := &httpsIdentity{certificatePath: certificatePath, keyPath: keyPath}
	identity.current.Store(&initial)
	return identity
}

func (identity *httpsIdentity) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return identity.current.Load(), nil
}

func (identity *httpsIdentity) reload() error {
	// Reuse startup's bounded regular-file, private-key permissions, and matching
	// key-pair validation. Never publish half a pair while files are replaced.
	next, err := keyPair(identity.certificatePath, identity.keyPath)
	if err != nil {
		return err
	}
	leaf := next.Leaf
	if leaf == nil {
		leaf, err = x509.ParseCertificate(next.Certificate[0])
		if err != nil {
			return errors.New("HTTPS leaf certificate is invalid")
		}
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return errors.New("HTTPS leaf certificate is outside its validity period")
	}
	identity.current.Store(&next)
	return nil
}

func (identity *httpsIdentity) reloadOnSignals(ctx context.Context, signals <-chan os.Signal, output io.Writer) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, open := <-signals:
			if !open {
				return
			}
			if err := identity.reload(); err != nil {
				// Parsing errors deliberately omit PEM contents and key material.
				_, _ = fmt.Fprintf(output, "public HTTPS TLS reload rejected: %v; keeping current certificate\n", err)
				continue
			}
			_, _ = fmt.Fprintln(output, "public HTTPS TLS certificate reloaded; existing connections retained")
		}
	}
}
