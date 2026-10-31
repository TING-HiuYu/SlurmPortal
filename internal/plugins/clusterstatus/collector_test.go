package clusterstatus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

type runFunc func(context.Context, string, ...string) ([]byte, error)

func (f runFunc) Run(ctx context.Context, path string, args ...string) ([]byte, error) {
	return f(ctx, path, args...)
}

type resolveFunc func(context.Context, string) ([]net.IPAddr, error)

func (f resolveFunc) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return f(ctx, host)
}

func TestNodeStatusPrecedenceAndSafeProjection(t *testing.T) {
	states := map[string]string{"IDLE": "idle", "ALLOCATED": "full", "MIXED": "running", "IDLE+MAINT": "maintenance", "MIXED+DRAIN": "drain", "IDLE+DRAIN+NO_RESPOND": "offline", "DOWN+MAINT": "offline", "IDLE+POWERED_DOWN": "offline", "IDLE+NEW_FLAG": "unknown", "IDLE+COMPLETING+NEW_FLAG": "unknown", "IDLE+NOT_RESPONDING": "offline", "MIXED+NOT_RESPOND": "offline", "IDLE+RESERVED": "idle", "IDLE+PLANNED": "idle", "ALLOCATED+DYNAMIC_FUTURE": "full", "MIXED+DYNAMIC": "running", "IDLE+COMPLETING": "running", "IDLE+COMPLETING+DRAIN": "drain"}
	for state, want := range states {
		t.Run(state, func(t *testing.T) {
			nodes, counts, err := parseNodes([]byte("NodeName=gpu01 State=" + state + " NodeAddr=10.99.88.77 NodeHostName=secret.internal Reason=private reason"))
			if err != nil || len(nodes) != 1 || nodes[0].Status != want || counts[want] != 1 {
				t.Fatalf("bad node status: %+v %+v %v", nodes, counts, err)
			}
			body, _ := json.Marshal(nodes)
			for _, secret := range []string{"10.99.88.77", "secret.internal", "Reason", "private"} {
				if strings.Contains(string(body), secret) {
					t.Fatal("private node metadata leaked")
				}
			}
		})
	}
	for _, body := range []string{"", "No nodes", "NodeName=gpu01 State=IDLE\nNodeName=gpu01 State=DOWN", "NodeName=gpu01 State=IDLE State=DOWN", "NodeName=10.1.1.1 State=IDLE", "NodeName=gpu01"} {
		if _, _, err := parseNodes([]byte(body)); err == nil {
			t.Fatalf("accepted ambiguous nodes %q", body)
		}
	}
}
func TestCollectorOnlyCallsScontrolAndResolvesConfiguredAddress(t *testing.T) {
	var calls []string
	collector, err := NewCollector("cluster1", Config{Address: "LOGIN.Example.invalid", SlurmConf: "/etc/slurm/custom.conf"}, runFunc(func(ctx context.Context, path string, args ...string) ([]byte, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("missing command deadline")
		}
		calls = append(calls, path+" "+strings.Join(args, " "))
		return []byte("NodeName=gpu01 State=MIXED NodeAddr=10.99.1.1"), nil
	}), resolveFunc(func(ctx context.Context, host string) ([]net.IPAddr, error) {
		if host != "login.example.invalid" {
			t.Fatal("resolved an inferred internal address")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("missing DNS deadline")
		}
		return []net.IPAddr{{IP: net.ParseIP("2001:db8::2")}, {IP: net.ParseIP("192.0.2.2")}, {IP: net.ParseIP("192.0.2.2")}}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	report, err := collector.Collect(context.Background(), time.Unix(1000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"/usr/bin/scontrol --oneliner show nodes"}) || report.Health != "ok" || report.AddressHealth != "ok" || !reflect.DeepEqual(report.CurrentIPs, []string{"192.0.2.2", "2001:db8::2"}) {
		t.Fatalf("unsafe collection %+v calls%v", report, calls)
	}
	local, _ := NewCollector("cluster1", Config{SlurmConf: "/etc/slurm/custom.conf"}, nil, nil)
	if local.cfg.Enabled || !reflect.DeepEqual(local.runner.(commandRunner).env, []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "SLURM_CONF=/etc/slurm/custom.conf"}) {
		t.Fatal("collection enabled or inherited environment")
	}
}
func TestDNSFailureIsIndependentOfNodesAndLiteralDoesNotResolve(t *testing.T) {
	runner := runFunc(func(context.Context, string, ...string) ([]byte, error) {
		return []byte("NodeName=cpu01 State=IDLE"), nil
	})
	collector, _ := NewCollector("cluster1", Config{Address: "login.example.invalid"}, runner, resolveFunc(func(context.Context, string) ([]net.IPAddr, error) {
		return nil, errors.New("private resolver details")
	}))
	report, err := collector.Collect(context.Background(), time.Now())
	if err != nil || report.Health != "ok" || report.AddressHealth != "unresolved" || len(report.CurrentIPs) != 0 {
		t.Fatalf("DNS failure fabricated offline nodes %+v %v", report, err)
	}
	collector, _ = NewCollector("cluster1", Config{Address: "10.2.3.4"}, runner, resolveFunc(func(context.Context, string) ([]net.IPAddr, error) {
		t.Fatal("literal address performed DNS")
		return nil, nil
	}))
	report, err = collector.Collect(context.Background(), time.Now())
	if err != nil || !reflect.DeepEqual(report.CurrentIPs, []string{"10.2.3.4"}) {
		t.Fatalf("configured private literal lost: %+v %v", report, err)
	}
}
func TestAddressValidationAndResolverCancellation(t *testing.T) {
	for _, address := range []string{"localhost", "node.localhost", "https://login.example.invalid", "user@login.example.invalid", "login.example.invalid:22", "login.example.invalid/path", "127.0.0.1", "0:0:0:0:0:0:0:1", "0.0.0.0", "0.0.0.1", "240.0.0.1", "255.255.255.255", "fe80::1"} {
		if _, err := (Config{Address: address}).WithDefaults(); err == nil {
			t.Fatalf("unsafe address accepted %s", address)
		}
	}
	collector, _ := NewCollector("cluster1", Config{Address: "login.example.invalid", ResolveTimeout: "10ms"}, runFunc(func(context.Context, string, ...string) ([]byte, error) {
		return []byte("NodeName=cpu01 State=IDLE"), nil
	}), resolveFunc(func(ctx context.Context, _ string) ([]net.IPAddr, error) { <-ctx.Done(); return nil, ctx.Err() }))
	started := time.Now()
	report, err := collector.Collect(context.Background(), time.Now())
	if err != nil || report.AddressHealth != "unresolved" || time.Since(started) > time.Second {
		t.Fatal("DNS deadline blocked node status")
	}
}
func TestNodeTruncationRetainsFullInventoryCountsAndFailureIsSafe(t *testing.T) {
	var body strings.Builder
	for i := 0; i < MaxNodes+1; i++ {
		state := "IDLE"
		if i == MaxNodes {
			state = "DOWN"
		}
		fmt.Fprintf(&body, "NodeName=n%05d State=%s\n", i, state)
	}
	collector, _ := NewCollector("cluster1", Config{}, runFunc(func(context.Context, string, ...string) ([]byte, error) { return []byte(body.String()), nil }), nil)
	report, err := collector.Collect(context.Background(), time.Now())
	if err != nil || len(report.Nodes) != MaxNodes || report.NodesTotal != MaxNodes+1 || !report.NodesTruncated || report.NodeCounts["offline"] != 1 {
		t.Fatalf("hidden offline node lost: %+v %v", report, err)
	}
	encoded, _ := json.Marshal(report)
	if len(encoded) > MaxPayloadBytes {
		t.Fatal("unbounded report")
	}
	collector.runner = runFunc(func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("secret command stderr")
	})
	report, err = collector.Collect(context.Background(), time.Now())
	encoded, _ = json.Marshal(report)
	if err == nil || report.Health != "error" || report.Nodes == nil || report.NodesTotal != 0 || report.Error == nil || *report.Error != "collection_failed" || strings.Contains(string(encoded), "secret") {
		t.Fatal("invalid safe failure report")
	}
}

func TestTrustedTransportLimitsPreserveCompleteLargeInventory(t *testing.T) {
	var body strings.Builder
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(&body, "NodeName=n%05d State=IDLE\n", i)
	}
	runner := runFunc(func(context.Context, string, ...string) ([]byte, error) { return []byte(body.String()), nil })
	collector, err := NewCollectorWithLimits("cluster1", Config{}, runner, nil, CollectionLimits{MaxItems: 20000, MaxPayloadBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	report, err := collector.Collect(context.Background(), time.Now())
	if err != nil || report.NodesTruncated || len(report.Nodes) != 20000 || report.NodesTotal != 20000 || report.NodeCounts["idle"] != 20000 {
		t.Fatalf("complete inventory rejected: kept%d total%d err%v", len(report.Nodes), report.NodesTotal, err)
	}
	for _, limits := range []CollectionLimits{{0, 4 << 20}, {20001, 4 << 20}, {20000, 4095}, {20000, 4<<20 + 1}} {
		if _, err := NewCollectorWithLimits("cluster1", Config{}, runner, nil, limits); err == nil {
			t.Fatal("unbounded transport limits accepted")
		}
	}
}

func TestNativeCommandCancellationAndOutputBudget(t *testing.T) {
	var buffer boundedBuffer
	if _, err := buffer.Write(make([]byte, maxCommandBytes+1)); err == nil {
		t.Fatal("unbounded node stdout accepted")
	}
	path, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := (commandRunner{env: []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}}).Run(ctx, path, "10"); err == nil {
		t.Fatal("native command ignored cancellation")
	}
}
