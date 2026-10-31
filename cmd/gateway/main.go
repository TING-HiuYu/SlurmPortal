// web-access-gateway serves the native, user-scoped SSH data plane.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/gateway"
)

const maxTLSFileBytes = 1 << 20

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "web access gateway failed: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("web-access-gateway", flag.ContinueOnError)
	flags.SetOutput(output)
	configPath := flags.String("config", "/etc/go-ldap-admin/web-access-gateway.yaml", "private gateway YAML configuration")
	checkOnly := flags.Bool("check-config", false, "validate configuration and TLS files without starting listeners")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected gateway arguments")
	}
	if ctx.Err() != nil {
		return nil
	}
	if _, err := readRegularFile(*configPath, true); err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	cfg, err := gateway.LoadConfig(*configPath)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	identity, err := gatewayTLS(cfg)
	if err != nil {
		return err
	}
	if *checkOnly {
		_, err = fmt.Fprintln(output, "web access gateway configuration and TLS files are valid; no listeners started")
		return err
	}
	return serve(ctx, cfg, identity, output)
}

// Read through an opened regular file and reject a replacement between Lstat
// and Open. Private configuration and key material must never be symlinks or
// group/world-readable files.
func readRegularFile(path string, private bool) ([]byte, error) {
	if path == "" {
		return nil, errors.New("file path is required")
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, errors.New("file must be a regular file, not a symlink")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() {
		return nil, errors.New("file changed while opening")
	}
	mode := after.Mode().Perm()
	if private && mode != 0600 && mode != 0400 {
		return nil, errors.New("private file mode must be 0600 or 0400")
	}
	if !private && mode&0022 != 0 {
		return nil, errors.New("public certificate must not be group/world writable")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxTLSFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxTLSFileBytes {
		return nil, errors.New("file exceeds 1 MiB")
	}
	return raw, nil
}

func keyPair(certificatePath, keyPath string) (tls.Certificate, error) {
	certificate, err := readRegularFile(certificatePath, false)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("certificate: %w", err)
	}
	key, err := readRegularFile(keyPath, true)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("private key: %w", err)
	}
	identity, err := tls.X509KeyPair(certificate, key)
	if err != nil {
		return tls.Certificate{}, errors.New("certificate/private key pair is invalid")
	}
	return identity, nil
}

func gatewayTLS(cfg gateway.Config) (tls.Certificate, error) {
	identity, err := keyPair(cfg.TLSCertificate, cfg.TLSKey)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("public HTTPS TLS: %w", err)
	}
	if cfg.MasterOrigin == "" {
		if cfg.MasterClientCertificate != "" || cfg.MasterClientKey != "" {
			return tls.Certificate{}, errors.New("master mTLS credentials require master_origin")
		}
	} else if _, err := keyPair(cfg.MasterClientCertificate, cfg.MasterClientKey); err != nil {
		return tls.Certificate{}, fmt.Errorf("master mTLS: %w", err)
	}
	return identity, nil
}

func gatewayServer(ctx context.Context, handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		// A terminal or streamed archive may legitimately outlive an ordinary
		// request. Keep global read/write deadlines unset; handlers own bounds.
	}
}

func serve(parent context.Context, cfg gateway.Config, identity tls.Certificate, output io.Writer) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	gateway, err := gateway.New(ctx, cfg)
	if err != nil {
		return fmt.Errorf("initialize gateway: %w", err)
	}
	defer gateway.Close()
	var listen net.ListenConfig
	publicListener, err := listen.Listen(ctx, "tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("public HTTPS listener: %w", err)
	}
	defer func() { _ = publicListener.Close() }()
	callbackListener, err := listen.Listen(ctx, "tcp", cfg.CallbackListen)
	if err != nil {
		return fmt.Errorf("loopback completion listener: %w", err)
	}
	defer func() { _ = callbackListener.Close() }()
	publicIdentity := newHTTPSIdentity(cfg.TLSCertificate, cfg.TLSKey, identity)
	reloadSignals := make(chan os.Signal, 1)
	signal.Notify(reloadSignals, syscall.SIGHUP)
	defer signal.Stop(reloadSignals)
	reloadsStopped := make(chan struct{})
	go func() {
		defer close(reloadsStopped)
		publicIdentity.reloadOnSignals(ctx, reloadSignals, output)
	}()
	defer func() { cancel(); <-reloadsStopped }()
	publicServer := gatewayServer(ctx, gateway)
	publicServer.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: publicIdentity.getCertificate}
	callbackServer := gatewayServer(ctx, gateway.CallbackHandler())
	serveErrors := make(chan error, 2)
	go func() { serveErrors <- publicServer.ServeTLS(publicListener, "", "") }()
	go func() { serveErrors <- callbackServer.Serve(callbackListener) }()
	_, _ = fmt.Fprintf(output, "web access gateway listening on HTTPS %s; completion callback on loopback HTTP %s\n", publicListener.Addr(), callbackListener.Addr())
	var result error
	select {
	case <-ctx.Done():
	case err = <-serveErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			result = fmt.Errorf("serve gateway: %w", err)
		}
	}
	// Cancel request/SSH contexts first. Shutdown does not own upgraded
	// WebSocket connections, which are closed by the gateway's final Close.
	cancel()
	shutdownContext, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	var shuttingDown sync.WaitGroup
	for _, server := range []*http.Server{publicServer, callbackServer} {
		shuttingDown.Add(1)
		go func(server *http.Server) {
			defer shuttingDown.Done()
			_ = server.Shutdown(shutdownContext)
		}(server)
	}
	shuttingDown.Wait()
	_ = publicServer.Close()
	_ = callbackServer.Close()
	return result
}
