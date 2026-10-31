// Package joblifecycle hosts Slurm lifecycle events and compiled-in plugins.
// Event delivery, resolution, sampling and retry state are process-local only.
package joblifecycle

import (
	"errors"
	"net"
	"net/url"
	"regexp"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/lifecycle/jobnotify"
)

const StartingPath = "/internal/slurm/job-starting"
const EndingPath = "/internal/slurm/job-ending"
const StatsPath = "/internal/events/stats"
const StartingType = "slurm.execution.starting"
const EndingType = "slurm.execution.ending"
const ReportType = "slurm.job.report.ready"

// Config is the lifecycle runtime configuration (job-lifecycle.yaml).
type Config struct {
	ClusterID        string             `yaml:"cluster-id"`
	Listen           string             `yaml:"listen"`
	TLS              jobnotify.TLSFiles `yaml:"callback-tls"`
	SlurmURL         string             `yaml:"slurm-url"`
	SlurmTLS         jobnotify.TLSFiles `yaml:"slurm-tls"`
	TokenFile        string             `yaml:"tokens-file"`
	ClustatSocket    string             `yaml:"clustat-socket"`
	NotificationFile string             `yaml:"notification-file"`
	Plugins          struct {
		JobEmail  bool `yaml:"job-email"`
		Resources struct {
			Enabled               bool `yaml:"enabled"`
			SampleIntervalSeconds int  `yaml:"sample-interval-seconds"` // default 15, allowed 5..300
		} `yaml:"resources"`
	} `yaml:"plugins"`
	// Explicit migration compatibility: preserve the former trusted mTLS-only
	// completion callback while controller hooks are being replaced or rolled back.
	AllowLegacyCompletionHook bool `yaml:"allow-legacy-completion-hook"`
	// This must be explicitly verified for each installation. GRES indices are
	// not inherently NVML ordinals. Explicit UUID maps take precedence.
	GRESIndicesAreNVML     bool                         `yaml:"gres-indices-are-nvml"`
	GPUUUIDMapping         map[string]map[string]string `yaml:"gpu-uuid-mapping"`
	MaxJobs                int                          `yaml:"max-jobs"`
	MaxRetainedReportBytes int64                        `yaml:"max-retained-report-bytes"`
	ResolveWorkers         int                          `yaml:"resolve-workers"`
	ResolveTTL             string                       `yaml:"resolve-ttl"`
	RequestTimeout         string                       `yaml:"request-timeout"`
	// Test-only override of the validated sample interval; never read from YAML.
	sampleInterval time.Duration
}

// LoadConfig reads and validates Config.
func LoadConfig(path string) (Config, error) {
	var c Config
	if err := jobnotify.ReadPrivateYAML(path, &c); err != nil {
		return c, err
	}
	return c, c.defaults()
}
func (c *Config) defaults() error {
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`).MatchString(c.ClusterID) || c.TokenFile == "" {
		return errors.New("lifecycle cluster and local credential path are required")
	}
	if c.Plugins.Resources.SampleIntervalSeconds == 0 {
		c.Plugins.Resources.SampleIntervalSeconds = 15
	}
	if c.Plugins.Resources.SampleIntervalSeconds < 5 || c.Plugins.Resources.SampleIntervalSeconds > 300 {
		return errors.New("invalid resources sample interval")
	}
	if c.sampleInterval <= 0 {
		c.sampleInterval = time.Duration(c.Plugins.Resources.SampleIntervalSeconds) * time.Second
	}
	if host, _, err := net.SplitHostPort(c.Listen); err != nil || net.ParseIP(host) == nil {
		return errors.New("lifecycle listener must use an explicit IP")
	}
	u, err := url.Parse(c.SlurmURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.RawPath != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("lifecycle observer must be an HTTPS origin")
	}
	if c.MaxJobs == 0 {
		c.MaxJobs = 1000
	}
	if c.MaxRetainedReportBytes == 0 {
		c.MaxRetainedReportBytes = 64 << 20
	}
	if c.ResolveWorkers == 0 {
		c.ResolveWorkers = 2
	}
	if c.ResolveTTL == "" {
		c.ResolveTTL = "15m"
	}
	if c.RequestTimeout == "" {
		c.RequestTimeout = "2s"
	}
	if c.MaxJobs < 1 || c.MaxJobs > 10000 || c.ResolveWorkers < 1 || c.ResolveWorkers > 8 || c.MaxRetainedReportBytes < 4096 || c.MaxRetainedReportBytes > 1<<30 {
		return errors.New("invalid lifecycle capacity")
	}
	for _, s := range []string{c.ResolveTTL, c.RequestTimeout} {
		d, e := time.ParseDuration(s)
		if e != nil || d < time.Millisecond || d > 24*time.Hour {
			return errors.New("invalid lifecycle deadline")
		}
	}
	return nil
}
