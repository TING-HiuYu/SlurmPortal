package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const validEvent = `{"event":"user.created","data":{"user":{"username":"alice","dn":"uid=alice,ou=people,dc=example","uidNumber":10001,"gidNumber":10000,"homeDirectory":"/HOME/team/alice","loginShell":"/bin/bash","mail":"alice@example.org"}}}`

func TestEventBoundaries(t *testing.T) {
	for name, payload := range map[string]string{
		"oversized":       strings.Repeat(" ", maxEventBytes+1),
		"empty":           "",
		"multiple":        validEvent + "{}",
		"invalid-event":   strings.Replace(validEvent, "user.created", "ca.updated", 1),
		"shell-username":  strings.Replace(validEvent, `"username":"alice"`, `"username":"alice; touch /tmp/injected"`, 1),
		"unknown-command": strings.Replace(validEvent, `"event":`, `"command":"id","event":`, 1),
		"missing-user":    `{"event":"user.updated","data":{}}`,
		"negative-uid":    strings.Replace(validEvent, `"uidNumber":10001`, `"uidNumber":-1`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validateEvent([]byte(payload)); err == nil {
				t.Fatal("unsafe payload accepted")
			}
		})
	}
	if _, err := validateEvent([]byte(validEvent)); err != nil {
		t.Fatal(err)
	}
}

func TestJSONNeverEntersRemoteCommand(t *testing.T) {
	payload := strings.Replace(validEvent, "alice@example.org", `$(touch /tmp/injected);'\\n`, 1)
	canonical, err := validateEvent([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	cfg := clientConfig{Host: "192.0.2.10", Port: 22, User: "root", IdentityFile: "/etc/go-ldap-admin/trigger.key", TimeoutSeconds: 285}
	args := sshArgs(cfg)
	for _, setting := range []string{"StrictHostKeyChecking=no", "UserKnownHostsFile=/dev/null", "GlobalKnownHostsFile=/dev/null", "CheckHostIP=no", "BatchMode=yes", "IdentitiesOnly=yes", "PasswordAuthentication=no", "ForwardAgent=no", "ClearAllForwardings=yes", "PermitLocalCommand=no"} {
		found := 0
		for index, arg := range args {
			if arg == setting && index > 0 && args[index-1] == "-o" {
				found++
			}
		}
		if found != 1 {
			t.Fatalf("SSH setting not explicit and unique: %s", setting)
		}
	}
	if got := args[len(args)-3:]; !reflect.DeepEqual(got, []string{"--", cfg.Host, remoteCommand}) {
		t.Fatalf("unexpected command tail: %q", got)
	}
	for _, arg := range args {
		if strings.Contains(arg, "injected") || strings.Contains(arg, "alice") || strings.Contains(arg, "user.created") {
			t.Fatalf("JSON leaked into remote command: %q", arg)
		}
	}
	var invoked []byte
	if err := dispatch(context.Background(), bytes.NewReader(canonical), remoteCommand, 0, func(_ context.Context, arg []byte) error {
		invoked = append([]byte(nil), arg...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var received eventPayload
	if err := json.Unmarshal(invoked, &received); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(received.Data.User.Mail, "$(touch /tmp/injected)") {
		t.Fatal("opaque JSON content was altered")
	}
}

func TestDispatcherRejectsArbitrarySSHCommandsAndNonRoot(t *testing.T) {
	for _, original := range []string{"", "id", remoteCommand + "; id", remoteCommand + " --config /tmp/custom", "sh -c id"} {
		invoked := false
		err := dispatch(context.Background(), strings.NewReader(validEvent), original, 0, func(context.Context, []byte) error { invoked = true; return nil })
		if err == nil || invoked {
			t.Fatalf("unexpected execution for SSH command %q", original)
		}
	}
	if err := dispatch(context.Background(), strings.NewReader(validEvent), remoteCommand, 900, func(context.Context, []byte) error { t.Fatal("non-root invocation"); return nil }); err == nil {
		t.Fatal("non-root dispatcher accepted")
	}
	if err := run([]string{"dispatch", "id"}); err == nil {
		t.Fatal("dispatcher accepts client arguments")
	}
}

func TestDispatcherRejectsOversizedAndInvalidDataBeforeExecution(t *testing.T) {
	for _, payload := range []string{strings.Repeat("a", maxEventBytes+1), `{"event":"run","data":{"user":{"username":"alice"}}}`, "{}"} {
		invoked := false
		err := dispatch(context.Background(), strings.NewReader(payload), remoteCommand, 0, func(context.Context, []byte) error { invoked = true; return nil })
		if err == nil || invoked {
			t.Fatal("unsafe event executed")
		}
	}
}

func TestReadBoundedCancelsUnterminatedInput(t *testing.T) {
	r, w := io.Pipe()
	defer func() { _ = r.Close() }()
	defer func() { _ = w.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := readBounded(ctx, r); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("read did not honor deadline: %v", err)
	}
}

func TestClientConfigurationRejectsOptionAndPathInjection(t *testing.T) {
	good := clientConfig{Host: "nas.example.org", User: "root", IdentityFile: "/etc/bridge/key"}
	if err := good.validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*clientConfig){
		func(c *clientConfig) { c.Host = "-oProxyCommand=id" },
		func(c *clientConfig) { c.Host = "nas.example.org;id" },
		func(c *clientConfig) { c.User = "root -oProxyCommand=id" },
		func(c *clientConfig) { c.IdentityFile = "relative-key" },
		func(c *clientConfig) { c.IdentityFile = "/etc/key\nProxyCommand=id" },
		func(c *clientConfig) { c.Port = 65536 },
	} {
		cfg := good
		change(&cfg)
		if err := cfg.validate(); err == nil {
			t.Fatalf("unsafe config accepted: %+v", cfg)
		}
	}
}

func TestClientConfigurationRequiresPrivateOwnedFiles(t *testing.T) {
	dir := t.TempDir()
	key, config := filepath.Join(dir, "key"), filepath.Join(dir, "bridge.json")
	if err := os.WriteFile(key, []byte("test private key"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := clientConfig{Host: "127.0.0.1", User: "root", IdentityFile: key}
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(config, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadClientConfig(config); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(config, 0640); err != nil {
		t.Fatal(err)
	}
	if _, err := loadClientConfig(config); err == nil {
		t.Fatal("group-readable config accepted")
	}
	if err := os.Chmod(config, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(key, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadClientConfig(config); err == nil {
		t.Fatal("world-readable private key accepted")
	}
	if err := os.Chmod(key, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "key-link")
	if err := os.Symlink(key, link); err != nil {
		t.Fatal(err)
	}
	cfg.IdentityFile = link
	raw, _ = json.Marshal(cfg)
	if err := os.WriteFile(config, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadClientConfig(config); err == nil {
		t.Fatal("symlinked private key accepted")
	}
}

func TestLegacyKnownHostsConfigIsDiscardedWithoutReadingOrUsingPath(t *testing.T) {
	dir := t.TempDir()
	key, known, config := filepath.Join(dir, "key"), filepath.Join(dir, "known_hosts"), filepath.Join(dir, "bridge.json")
	if err := os.WriteFile(key, []byte("synthetic private key"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(known, []byte("unused legacy host key"), 0666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(known, 0666); err != nil {
		t.Fatal(err)
	}
	for _, legacy := range []string{"", filepath.Join(dir, "nonexistent-known-hosts"), known, "/ignored\nProxyCommand=id"} {
		cfg := clientConfig{Host: "127.0.0.1", User: "root", IdentityFile: key, KnownHostsFile: legacy}
		raw, _ := json.Marshal(cfg)
		if err := os.WriteFile(config, raw, 0600); err != nil {
			t.Fatal(err)
		}
		loaded, err := loadClientConfig(config)
		if err != nil {
			t.Fatalf("deprecated host-key file remained a load dependency: %v", err)
		}
		if loaded.KnownHostsFile != "" {
			t.Fatal("deprecated knownHostsFile retained in active config")
		}
		if legacy != "" && strings.Contains(strings.Join(sshArgs(cfg), "\n"), legacy) {
			t.Fatal("legacy host-key configuration entered SSH arguments")
		}
	}
}

func TestExistingQueuePayloadShapes(t *testing.T) {
	// These are the publicUser/publicGroup keys serialized by TriggerRunner,
	// including an existing user's previous state and membership group metadata.
	data := `{"user":{"username":"alice","dn":"uid=alice,ou=people,dc=slurm,dc=lan","uidNumber":10001,"gidNumber":10000,"homeDirectory":"/HOME/sshusers/alice","loginShell":"/bin/zsh","mail":"alice@example.org"},"previous":{"username":"alice","dn":"uid=alice,ou=people,dc=slurm,dc=lan","uidNumber":10001,"gidNumber":10000,"homeDirectory":"/HOME/sshusers/alice","loginShell":"/bin/bash","mail":"alice@example.org"},"group":{"name":"sshusers","dn":"cn=sshusers,ou=groups,dc=slurm,dc=lan","gidNumber":10000,"class":"posixGroup"}}`
	for _, event := range []string{"user.created", "user.updated", "user.deleted", "membership.added", "membership.removed"} {
		payload := `{"event":"` + event + `","data":` + data + `}`
		if _, err := validateEvent([]byte(payload)); err != nil {
			t.Fatalf("old queue shape rejected for %s: %v", event, err)
		}
	}
	admin := `{"event":"user.updated","data":{"user":{"username":"admin","dn":"cn=admin,dc=slurm,dc=lan","uidNumber":0,"gidNumber":0,"homeDirectory":"","loginShell":"","mail":""}}}`
	if _, err := validateEvent([]byte(admin)); err != nil {
		t.Fatalf("application admin no-op rejected: %v", err)
	}
}

func TestPrivateFileRejectsDifferentOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := secureFile(path, true, uint32(os.Geteuid()+1)); err == nil {
		t.Fatal("foreign-owner private file accepted")
	}
}
