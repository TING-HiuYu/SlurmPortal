package clusterstatus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const maxCommandBytes = 16 << 20

var hostname = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$`)
var nodeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
var stateText = regexp.MustCompile(`^[A-Z][A-Z0-9_+~#*!%@$^.-]{0,127}$`)

// Config is the slave-side collector configuration: how to run scontrol for node states.
type Config struct {
	Enabled        bool   `yaml:"enabled"`
	ScontrolPath   string `yaml:"scontrol-path"`
	SlurmConf      string `yaml:"slurm-conf"`
	Address        string `yaml:"address"`
	CommandTimeout string `yaml:"command-timeout"`
	ResolveTimeout string `yaml:"resolve-timeout"`
}

func acceptableIP(ip net.IP) bool {
	if ipv4 := ip.To4(); ipv4 != nil && (ipv4[0] == 0 || ipv4[0] >= 224) {
		return false
	}
	return ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() && !ip.IsMulticast() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast()
}

// NormalizeAddress validates a host or host:port and returns it in canonical form.
func NormalizeAddress(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if ip := net.ParseIP(value); ip != nil {
		if acceptableIP(ip) {
			return ip.String(), nil
		}
		return "", errors.New("cluster address must be an access hostname or nonlocal IP")
	}
	value = strings.ToLower(strings.TrimSuffix(value, "."))
	if len(value) > 253 || !hostname.MatchString(value) || value == "localhost" || strings.HasSuffix(value, ".localhost") {
		return "", errors.New("cluster address must be a hostname or IP without URL, credentials, path or port")
	}
	return value, nil
}

// WithDefaults fills unset paths and timeouts.
func (c Config) WithDefaults() (Config, error) {
	if c.ScontrolPath == "" {
		c.ScontrolPath = "/usr/bin/scontrol"
	}
	if c.CommandTimeout == "" {
		c.CommandTimeout = "5s"
	}
	if c.ResolveTimeout == "" {
		c.ResolveTimeout = "2s"
	}
	if !filepath.IsAbs(c.ScontrolPath) || filepath.Clean(c.ScontrolPath) != c.ScontrolPath || strings.ContainsAny(c.ScontrolPath, "\x00\r\n") {
		return c, errors.New("cluster-status command must be an absolute local path")
	}
	if c.SlurmConf != "" && (!filepath.IsAbs(c.SlurmConf) || filepath.Clean(c.SlurmConf) != c.SlurmConf || strings.ContainsAny(c.SlurmConf, "\x00\r\n")) {
		return c, errors.New("cluster-status Slurm config must be an absolute local path")
	}
	var err error
	c.Address, err = NormalizeAddress(c.Address)
	if err != nil {
		return c, err
	}
	for _, pair := range []struct {
		text    string
		maximum time.Duration
	}{{c.CommandTimeout, 20 * time.Second}, {c.ResolveTimeout, 5 * time.Second}} {
		d, e := time.ParseDuration(pair.text)
		if e != nil || d < time.Millisecond || d > pair.maximum {
			return c, errors.New("invalid cluster-status timeout")
		}
	}
	return c, nil
}

// Runner executes a command (scontrol) and returns its output.
type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

// Resolver looks up host addresses.
type Resolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}
type commandRunner struct{ env []string }
type boundedBuffer struct{ bytes.Buffer }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > maxCommandBytes-b.Len() {
		return 0, errors.New("cluster-status output exceeds budget")
	}
	return b.Buffer.Write(p)
}
func (r commandRunner) Run(ctx context.Context, path string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = append([]string{}, r.env...)
	cmd.WaitDelay = 200 * time.Millisecond
	var output boundedBuffer
	cmd.Stdout, cmd.Stderr = &output, io.Discard
	if cmd.Run() != nil {
		return nil, errors.New("cluster-status command failed")
	}
	return output.Bytes(), nil
}

// Collector builds a node status Report on the slave.
type Collector struct {
	cluster  string
	cfg      Config
	runner   Runner
	resolver Resolver
	limits   CollectionLimits
}

// CollectionLimits are selected only by a trusted transport constructor.
type CollectionLimits struct {
	MaxItems        int
	MaxPayloadBytes int
}

// NewCollector returns a Collector with default limits.
func NewCollector(cluster string, cfg Config, runner Runner, resolver Resolver) (*Collector, error) {
	return NewCollectorWithLimits(cluster, cfg, runner, resolver, CollectionLimits{MaxItems: MaxNodes, MaxPayloadBytes: MaxPayloadBytes})
}

// NewCollectorWithLimits returns a Collector with explicit size limits.
func NewCollectorWithLimits(cluster string, cfg Config, runner Runner, resolver Resolver, limits CollectionLimits) (*Collector, error) {
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`).MatchString(cluster) {
		return nil, errors.New("invalid cluster-status identity")
	}
	if limits.MaxItems < 1 || limits.MaxItems > 20000 || limits.MaxPayloadBytes < 4096 || limits.MaxPayloadBytes > 4<<20 {
		return nil, errors.New("invalid cluster-status transport limits")
	}
	cfg, err := cfg.WithDefaults()
	if err != nil {
		return nil, err
	}
	if runner == nil {
		env := []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
		if cfg.SlurmConf != "" {
			env = append(env, "SLURM_CONF="+cfg.SlurmConf)
		}
		runner = commandRunner{env: env}
	}
	if resolver == nil {
		resolver = &net.Resolver{PreferGo: true, StrictErrors: true}
	}
	return &Collector{cluster: cluster, cfg: cfg, runner: runner, resolver: resolver, limits: limits}, nil
}

func classify(state string) string {
	parts := strings.Split(state, "+")
	for _, part := range parts {
		if strings.Contains(part, "NO_RESPOND") || strings.Contains(part, "NOT_RESPOND") || part == "DOWN" || part == "UNKNOWN" || part == "FUTURE" || strings.HasPrefix(part, "POWER") || strings.ContainsAny(part, "*~#%") {
			return "offline"
		}
	}
	for _, part := range parts {
		if part == "MAINT" || part == "MAINTENANCE" {
			return "maintenance"
		}
	}
	for _, part := range parts {
		if strings.HasPrefix(part, "DRAIN") {
			return "drain"
		}
	}
	for _, part := range parts[1:] {
		if part != "CLOUD" && part != "PLANNED" && part != "RESERVED" && part != "DYNAMIC" && part != "DYNAMIC_FUTURE" && part != "DYNAMIC_NORM" && part != "COMPLETING" {
			return "unknown"
		}
	}
	for _, part := range parts {
		if part == "COMPLETING" {
			return "running"
		}
	}
	switch parts[0] {
	case "IDLE":
		return "idle"
	case "ALLOCATED":
		return "full"
	case "MIXED":
		return "running"
	}
	return "unknown"
}
func parseNodes(body []byte) ([]Node, map[string]int, error) {
	return parseNodesWithLimit(body, 10000)
}

func parseNodesWithLimit(body []byte, maximum int) ([]Node, map[string]int, error) {
	nodes, counts, seen := []Node{}, EmptyCounts(), map[string]bool{}
	lines := bytes.Split(bytes.TrimSpace(body), []byte("\n"))
	if len(lines) > maximum {
		return nil, nil, errors.New("cluster-status node capacity exceeded")
	}
	for _, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		fields := map[string]string{}
		for _, field := range strings.Fields(string(line)) {
			key, value, ok := strings.Cut(field, "=")
			if !ok || (key != "NodeName" && key != "State") {
				continue
			}
			if _, exists := fields[key]; exists {
				return nil, nil, errors.New("ambiguous cluster-status node")
			}
			fields[key] = value
		}
		name, state := fields["NodeName"], fields["State"]
		if !nodeName.MatchString(name) || net.ParseIP(name) != nil || !stateText.MatchString(state) || seen[name] {
			return nil, nil, errors.New("invalid cluster-status node")
		}
		seen[name] = true
		status := classify(state)
		counts[status]++
		nodes = append(nodes, Node{Name: name, Status: status, SlurmState: state})
	}
	if len(nodes) == 0 {
		return nil, nil, errors.New("empty cluster-status nodes")
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	return nodes, counts, nil
}
func (c *Collector) resolve(ctx context.Context) (*string, []string, string) {
	if c.cfg.Address == "" {
		return nil, []string{}, "not_configured"
	}
	address := c.cfg.Address
	if ip := net.ParseIP(address); ip != nil {
		return &address, []string{ip.String()}, "ok"
	}
	timeout, _ := time.ParseDuration(c.cfg.ResolveTimeout)
	lookup, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	values, err := c.resolver.LookupIPAddr(lookup, address)
	if err != nil || lookup.Err() != nil {
		return &address, []string{}, "unresolved"
	}
	unique := map[string]bool{}
	for _, value := range values {
		if acceptableIP(value.IP) {
			unique[value.IP.String()] = true
		}
	}
	ips := make([]string, 0, len(unique))
	for value := range unique {
		ips = append(ips, value)
	}
	sort.Strings(ips)
	if len(ips) == 0 {
		return &address, []string{}, "unresolved"
	}
	if len(ips) > 16 {
		ips = ips[:16]
	}
	return &address, ips, "ok"
}

// Collect reads node states and returns one Report.
func (c *Collector) Collect(ctx context.Context, at time.Time) (Report, error) {
	code := "collection_failed"
	report := Report{Version: 1, ClusterID: c.cluster, ObservedAt: at.UTC(), Health: "error", Error: &code, Nodes: []Node{}, NodeCounts: EmptyCounts()}
	report.Address, report.CurrentIPs, report.AddressHealth = c.resolve(ctx)
	timeout, _ := time.ParseDuration(c.cfg.CommandTimeout)
	command, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, err := c.runner.Run(command, c.cfg.ScontrolPath, "--oneliner", "show", "nodes")
	if err != nil || command.Err() != nil || len(body) > maxCommandBytes {
		return report, errors.New("cluster-status collection failed")
	}
	nodes, counts, err := parseNodesWithLimit(body, max(10000, c.limits.MaxItems))
	if err != nil {
		return report, err
	}
	report.Health, report.Error, report.NodeCounts, report.NodesTotal = "ok", nil, counts, len(nodes)
	if len(nodes) > c.limits.MaxItems {
		nodes = nodes[:c.limits.MaxItems]
	}
	report.Nodes = nodes
	report.NodesTruncated = len(nodes) < report.NodesTotal
	encoded, err := json.Marshal(report)
	if err != nil || len(encoded) > c.limits.MaxPayloadBytes {
		report.Health, report.Error, report.Nodes, report.NodeCounts, report.NodesTotal, report.NodesTruncated = "error", &code, []Node{}, EmptyCounts(), 0, false
		return report, errors.New("cluster-status report exceeds budget")
	}
	return report, nil
}
