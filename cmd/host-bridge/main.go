// host-bridge runs inside a containerised slave and hands a trigger event to the host:
// it opens SSH to the controller and runs slurm_trigger.py there, which creates or removes the
// user's Slurm account and storage (home ACLs, quotas).
//
// The bridge deliberately sends event data on stdin. It never constructs a
// remote shell command from a username, JSON value, or client configuration.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const (
	remoteCommand  = "go-ldap-admin-trigger-v1"
	maxEventBytes  = 64 * 1024
	maxConfigBytes = 16 * 1024
	hostScript     = "/usr/local/libexec/go-ldap-admin/slurm_trigger.py"
	hostConfig     = "/etc/go-ldap-admin/slurm-trigger.json"
)

var allowedEvents = map[string]bool{
	"user.created": true, "user.updated": true, "user.deleted": true,
	"membership.added": true, "membership.removed": true,
}
var usernamePattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
var hostnamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)

type eventUser struct {
	Username      string `json:"username"`
	DN            string `json:"dn"`
	UIDNumber     uint   `json:"uidNumber"`
	GIDNumber     uint   `json:"gidNumber"`
	HomeDirectory string `json:"homeDirectory"`
	LoginShell    string `json:"loginShell"`
	Mail          string `json:"mail"`
}

type eventGroup struct {
	Name      string `json:"name"`
	DN        string `json:"dn"`
	GIDNumber uint   `json:"gidNumber"`
	Class     string `json:"class"`
}

type eventPayload struct {
	Event string `json:"event"`
	Data  struct {
		User     *eventUser  `json:"user"`
		Previous *eventUser  `json:"previous,omitempty"`
		Group    *eventGroup `json:"group,omitempty"`
	} `json:"data"`
}

type clientConfig struct {
	Host         string `json:"host"`
	Port         int    `json:"port"`
	User         string `json:"user"`
	IdentityFile string `json:"identityFile"`
	// Deprecated JSON compatibility only. The project does not verify SSH
	// server host keys; this value is discarded without opening the path.
	KnownHostsFile string `json:"knownHostsFile,omitempty"`
	TimeoutSeconds int    `json:"timeoutSeconds"`
}

func decodeOne(data []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("expected exactly one JSON document")
	}
	return nil
}

func validateEvent(raw []byte) ([]byte, error) {
	if len(raw) > maxEventBytes {
		return nil, errors.New("trigger event exceeds 64 KiB")
	}
	var payload eventPayload
	if err := decodeOne(raw, &payload); err != nil {
		return nil, fmt.Errorf("invalid trigger JSON: %w", err)
	}
	if !allowedEvents[payload.Event] {
		return nil, errors.New("trigger event is not allowed")
	}
	if payload.Data.User == nil || !usernamePattern.MatchString(payload.Data.User.Username) {
		return nil, errors.New("trigger requires a valid POSIX username")
	}
	// Canonical JSON ensures client and dispatcher interpret the same data.
	return json.Marshal(payload)
}

func validPath(path string) bool {
	return filepath.IsAbs(path) && !strings.ContainsAny(path, "\x00\r\n\t ")
}

func secureFile(path string, private bool, expectedOwner uint32) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file without symlinks", path)
	}
	perm := info.Mode().Perm()
	if (private && perm&0077 != 0) || perm&0022 != 0 {
		return fmt.Errorf("unsafe permissions on %s", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != expectedOwner {
		return fmt.Errorf("unsafe owner on %s", path)
	}
	return nil
}

func loadClientConfig(path string) (clientConfig, error) {
	var cfg clientConfig
	if err := secureFile(path, true, uint32(os.Geteuid())); err != nil {
		return cfg, err
	}
	f, err := os.Open(path)
	if err != nil {
		return cfg, err
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return cfg, err
	}
	if len(raw) > maxConfigBytes {
		return cfg, errors.New("bridge config exceeds 16 KiB")
	}
	if err := decodeOne(raw, &cfg); err != nil {
		return cfg, err
	}
	cfg.KnownHostsFile = ""
	if err := cfg.validate(); err != nil {
		return cfg, err
	}
	if err := secureFile(cfg.IdentityFile, true, uint32(os.Geteuid())); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func (c *clientConfig) validate() error {
	if (!hostnamePattern.MatchString(c.Host) && net.ParseIP(c.Host) == nil) || strings.HasPrefix(c.Host, "-") {
		return errors.New("bridge host must be a hostname or IP address")
	}
	if !usernamePattern.MatchString(c.User) {
		return errors.New("invalid SSH user")
	}
	if c.Port == 0 {
		c.Port = 22
	}
	if c.Port < 1 || c.Port > 65535 {
		return errors.New("invalid SSH port")
	}
	if c.TimeoutSeconds == 0 {
		c.TimeoutSeconds = 300
	}
	if c.TimeoutSeconds < 1 || c.TimeoutSeconds > 3600 {
		return errors.New("invalid SSH timeout")
	}
	if !validPath(c.IdentityFile) {
		return errors.New("SSH key must be an absolute path without whitespace")
	}
	return nil
}

func sshArgs(c clientConfig) []string {
	return []string{
		"-F", "/dev/null", "-T", "-i", c.IdentityFile,
		"-p", fmt.Sprint(c.Port), "-l", c.User,
		"-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		"-o", "GlobalKnownHostsFile=/dev/null", "-o", "CheckHostIP=no", "-o", "UpdateHostKeys=no", "-o", "PasswordAuthentication=no",
		"-o", "KbdInteractiveAuthentication=no", "-o", "NumberOfPasswordPrompts=0",
		"-o", "ForwardAgent=no", "-o", "ClearAllForwardings=yes",
		"-o", "PermitLocalCommand=no", "-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3",
		"--", c.Host, remoteCommand,
	}
}

func client(ctx context.Context, cfg clientConfig, payload []byte) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/ssh", sshArgs(cfg)...)
	cmd.Stdin = bytes.NewReader(append(payload, '\n'))
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	return cmd.Run()
}

func readBounded(ctx context.Context, input io.Reader) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(input, maxEventBytes+1))
		ch <- result{data, err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		if len(r.data) > maxEventBytes {
			return nil, errors.New("trigger event exceeds 64 KiB")
		}
		return r.data, nil
	}
}

func dispatch(ctx context.Context, input io.Reader, original string, euid int, invoke func(context.Context, []byte) error) error {
	if euid != 0 {
		return errors.New("host dispatcher must run as root")
	}
	if original != remoteCommand {
		return errors.New("SSH command is not allowed")
	}
	readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	raw, err := readBounded(readCtx, input)
	cancel()
	if err != nil {
		return err
	}
	payload, err := validateEvent(raw)
	if err != nil {
		return err
	}
	return invoke(ctx, payload)
}

func rootPath(path string, private bool) error {
	if err := secureFile(path, private, 0); err != nil {
		return err
	}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !info.IsDir() || !ok || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 {
			return fmt.Errorf("unsafe dispatcher parent directory %s", dir)
		}
		if dir == "/" {
			return nil
		}
	}
}

func invokeHost(ctx context.Context, payload []byte) error {
	if err := rootPath(hostScript, false); err != nil {
		return err
	}
	if err := rootPath(hostConfig, true); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/python3", hostScript, "--config", hostConfig, string(payload))
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	return cmd.Run()
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("expected client or dispatch subcommand")
	}
	switch args[0] {
	case "client":
		flags := flag.NewFlagSet("client", flag.ContinueOnError)
		configPath := flags.String("config", "/etc/go-ldap-admin/host-bridge.json", "secure bridge JSON config")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 {
			return errors.New("client requires one JSON event argument")
		}
		payload, err := validateEvent([]byte(flags.Arg(0)))
		if err != nil {
			return err
		}
		cfg, err := loadClientConfig(*configPath)
		if err != nil {
			return err
		}
		return client(context.Background(), cfg, payload)
	case "dispatch":
		if len(args) != 1 {
			return errors.New("dispatch accepts no command arguments")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 280*time.Second)
		defer cancel()
		return dispatch(ctx, os.Stdin, os.Getenv("SSH_ORIGINAL_COMMAND"), os.Geteuid(), invokeHost)
	default:
		return errors.New("unknown bridge subcommand")
	}
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "slave host bridge:", err)
		os.Exit(1)
	}
}
