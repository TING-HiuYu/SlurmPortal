package jobnotify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrivateConfigStrictnessAndSecretProjection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	valid := "cluster-id: cluster1\nlisten: 127.0.0.1:9444\nslurm-url: https://observer.invalid\ncallback-tls:\n  client-fingerprints: [" + strings.Repeat("a", 64) + "]\n"
	if os.WriteFile(path, []byte(valid), 0600) != nil {
		t.Fatal("write fixture")
	}
	cfg, err := LoadConfig(path)
	if err != nil || cfg.MaxEvents != 1000 || cfg.MaxEventBytes != 256<<10 {
		t.Fatalf("defaults: %+v %v", cfg, err)
	}
	if os.Chmod(path, 0644) != nil {
		t.Fatal("chmod fixture")
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("public notification configuration accepted")
	}
	_ = os.Chmod(path, 0600)
	projection := filepath.Join(filepath.Dir(path), "secret-link.yaml")
	if os.Symlink(path, projection) != nil {
		t.Fatal("link fixture")
	}
	if _, err := LoadConfig(projection); err != nil {
		t.Fatal("0600 projected secret rejected")
	}
	for _, suffix := range []string{"unknown-option: true\n", "---\ncluster-id: extra\n", "max-event-bytes: 262145\n", "request-timeout: 31s\n"} {
		_ = os.WriteFile(path, []byte(valid+suffix), 0600)
		if _, err := LoadConfig(path); err == nil {
			t.Fatalf("invalid config accepted: %s", suffix)
		}
	}
}

func TestNoRedirectOrCredentialsInEndpoints(t *testing.T) {
	for _, endpoint := range []string{"http://observer.invalid", "https://user:password@observer.invalid", "https://observer.invalid?token=secret", "https://observer.invalid#fragment"} {
		cfg := testConfig()
		cfg.SlurmURL = endpoint
		if cfg.defaults() == nil {
			t.Fatalf("unsafe endpoint allowed: %s", endpoint)
		}
	}
	for _, cluster := range []string{"Uppercase", "a.b", "-bad"} {
		cfg := testConfig()
		cfg.ClusterID = cluster
		if cfg.defaults() == nil {
			t.Fatal("master-incompatible cluster identity accepted")
		}
	}
}

func TestCanonicalSluidMatchesSlurm2605PrintFormat(t *testing.T) {
	for _, value := range []string{"s0123456789ABC", "s0123456789abc", "S0123456789aBc"} {
		callback := Callback{JobID: 7, SLUID: value}
		if callback.Validate() != nil {
			t.Fatalf("valid SLUID rejected %s", value)
		}
		event, err := Normalize("cluster1", callback, responseBytes(fixtureJob(7, "s0123456789ABC", "COMPLETED")), time.Now(), 256<<10)
		if err != nil || event.Job.SLUID != "s0123456789ABC" || event.EventID != "cluster1/s0123456789ABC/ended" {
			t.Fatalf("canonical identity mismatch: %+v %v", event, err)
		}
	}
	for _, value := range []string{"s0123456789ABU", "sG123456789ABC", "s0123456789AB"} {
		if (Callback{JobID: 7, SLUID: value}).Validate() == nil {
			t.Fatalf("invalid SLUID accepted %s", value)
		}
	}
}
