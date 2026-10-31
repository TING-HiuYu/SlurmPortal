// Package jobdoneemail is the compile-time job-done-email plugin. The host only
// supplies verified reports; SMTP retry ownership remains in its private FIFO.
package jobdoneemail

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/TING-HiuYu/SlurmPortal/internal/lifecycle/eventbus"
	"github.com/TING-HiuYu/SlurmPortal/internal/lifecycle/jobnotify"
)

const ID = "job-done-email"

// Plugin sends finished-job events to master, which e-mails the job owner.
type Plugin struct{ sender *jobnotify.Service }

func New(ctx context.Context, cfg jobnotify.Config, reader, sender *http.Client, masterURL string) (*Plugin, error) {
	s, err := jobnotify.New(cfg, reader, sender, masterURL)
	if err != nil {
		return nil, err
	}
	s.RunSender(ctx)
	return &Plugin{s}, nil
}
func (p *Plugin) Handle(ctx context.Context, event eventbus.Event) error {
	var report jobnotify.Event
	if err := json.Unmarshal(event.Payload, &report); err != nil {
		return err
	}
	return p.sender.AcceptReport(ctx, report)
}
func (p *Plugin) Close(ctx context.Context) error { return p.sender.Close(ctx) }
func (p *Plugin) Stats() jobnotify.Stats          { return p.sender.Snapshot() }
