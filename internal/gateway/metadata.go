package gateway

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/pkg/sftp"
)

// ReadDir accumulates records in the maintained SFTP library. Bound the total
// response bytes on a separate metadata channel; large file streams use the
// original channel and are not buffered or subject to this metadata budget.
type metadataReader struct {
	source    io.Reader
	remaining int64
}

func (r *metadataReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, errors.New("metadata budget exceeded")
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, e := r.source.Read(p)
	r.remaining -= int64(n)
	return n, e
}
func (g *Gateway) metadata(ctx context.Context, s *session) (*session, func(), error) {
	select {
	case g.metadataSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	release := func() { <-g.metadataSlots }
	client, closeChannel, e := fileChannel(ctx, s, 16<<20)
	if e != nil {
		release()
		return nil, nil, e
	}
	meta := &session{home: s.home, root: s.root, files: client, client: s.client}
	return meta, func() { closeChannel(); release() }, nil
}

// One subsystem channel per operation allows cancellation without closing the
// file browser's base subsystem. A noncooperating peer gets one second to
// acknowledge CHANNEL_CLOSE before the transport is closed as a last resort.
func fileChannel(ctx context.Context, s *session, budget int64) (*sftp.Client, func(), error) {
	if e := ctx.Err(); e != nil {
		return nil, nil, e
	}
	stopOpening := context.AfterFunc(ctx, func() { _ = s.client.Close() })
	remote, e := s.client.NewSession()
	stopOpening()
	if e != nil {
		return nil, nil, e
	}
	setupDone := make(chan struct{})
	stopSetup := context.AfterFunc(ctx, func() {
		killer := time.AfterFunc(time.Second, func() { _ = s.client.Close() })
		defer killer.Stop()
		_ = remote.Close()
		<-setupDone
	})
	defer func() { stopSetup(); close(setupDone) }()
	stdout, e := remote.StdoutPipe()
	if e != nil {
		_ = remote.Close()
		return nil, nil, e
	}
	stdin, e := remote.StdinPipe()
	if e != nil {
		_ = remote.Close()
		return nil, nil, e
	}
	if e = remote.RequestSubsystem("sftp"); e != nil {
		_ = remote.Close()
		return nil, nil, e
	}
	var reader = stdout
	if budget > 0 {
		reader = &metadataReader{stdout, budget}
	}
	client, e := sftp.NewClientPipe(reader, stdin)
	if e != nil {
		_ = remote.Close()
		return nil, nil, e
	}
	var once sync.Once
	closeChannel := func() {
		once.Do(func() {
			killer := time.AfterFunc(time.Second, func() { _ = s.client.Close() })
			defer killer.Stop()
			_ = remote.Close()
			_ = client.Close()
		})
	}
	stopActive := context.AfterFunc(ctx, closeChannel)
	return client, func() { stopActive(); closeChannel() }, nil
}
