package jobnotify

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// These adapters expose the existing tested Slurm/mTLS and SMTP delivery
// components to the compile-time plugin host without importing LDAP or Gin.
func ClientTLS(files TLSFiles) (*tls.Config, error) { return clientTLS(files) }
func ServerTLS(files TLSFiles) (*tls.Config, error) { return serverTLS(files) }
func HTTPClient(config *tls.Config, timeout time.Duration) *http.Client {
	return newHTTPClient(config, timeout)
}
func (c *Config) ApplyDefaults() error { return c.defaults() }
func CanonicalSLUID(s string) string   { return canonicalSLUID(s) }

// DecodeCallback preserves the strict duplicate-free, two-key hook contract.
func DecodeCallback(body []byte) (Callback, error) { return decodeCallback(body) }

func SlurmNumber(raw json.RawMessage) (int64, bool) { return number(raw) }

func EncodeEvent(event Event, maximum int) (Event, []byte, error) {
	return encodeBoundedEvent(event, maximum)
}

// RunSender starts only the mail FIFO. The lifecycle host owns Slurm resolution
// and event scheduling; compile-time email plugins must not duplicate them.
func (s *Service) RunSender(parent context.Context) {
	s.ctx, s.cancel = context.WithCancel(parent)
	s.wg.Add(1)
	go s.sendLoop()
}

// AcceptReport transfers an already-verified report to this plugin's bounded
// RAM FIFO. A successful return means ownership transfer, never SMTP acceptance.
func (s *Service) AcceptReport(ctx context.Context, event Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if event.Duration.ActiveSeconds <= MinActiveSeconds {
		return nil
	}
	if event.ClusterID != s.cfg.ClusterID || (Callback{JobID: event.Job.JobID, SLUID: event.Job.SLUID}).Validate() != nil {
		return errors.New("invalid report identity")
	}
	_, encoded, err := encodeBoundedEvent(event, s.cfg.MaxEventBytes)
	if err != nil {
		return err
	}
	key := callbackKey(Callback{event.Job.JobID, canonicalSLUID(event.Job.SLUID)})
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.ctx == nil || s.ctx.Err() != nil {
		return errors.New("email plugin is stopped")
	}
	if _, ok := s.items[key]; ok {
		return nil
	}
	if _, ok := s.seen[key]; ok {
		return nil
	}
	if len(s.items) >= s.cfg.MaxEvents || s.bytes+int64(len(encoded)) > s.cfg.MaxBytes {
		return errors.New("email queue capacity reached")
	}
	now := time.Now()
	s.items[key] = &pending{callback: Callback{event.Job.JobID, event.Job.SLUID}, created: now, bytes: int64(len(encoded))}
	s.bytes += int64(len(encoded))
	s.stats.Accepted++
	s.ready = append(s.ready, delivery{body: encoded, key: key, created: now})
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}
