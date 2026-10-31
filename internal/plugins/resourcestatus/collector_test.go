package resourcestatus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

type runFunc func(context.Context, string, ...string) ([]byte, error)

func (f runFunc) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f(ctx, name, args...)
}

const healthyNode = "NodeName=gpu01 CPUAlloc=8 CPUTot=32 CPUEfctv=30 RealMemory=65536 AllocMem=16384 FreeMem=1 State=MIXED Gres=gpu:a100:4 CfgTRES=cpu=32,mem=65536M,gres/gpu=4,gres/gpu:a100=4 AllocTRES=cpu=8,mem=16384M,gres/gpu=1,gres/gpu:a100=1"

func TestCapacityIncludesUnavailableTotalsButNeverTheirIdleResources(t *testing.T) {
	nodes := []string{healthyNode}
	for _, state := range []string{"DOWN", "IDLE+DRAIN", "ALLOCATED+NO_RESPOND", "IDLE+POWERED_DOWN", "UNKNOWN", "IDLE+MAINT", "IDLE+FAIL"} {
		nodes = append(nodes, "NodeName="+strings.ReplaceAll(state, "+", "_")+" CPUAlloc=0 CPUTot=8 RealMemory=1024 AllocMem=0 State="+state+" CfgTRES=cpu=8,mem=1024M,gres/gpu=2 AllocTRES=")
	}
	got, err := parseNodes([]byte(strings.Join(nodes, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if got.CPU.Total != 88 || got.CPU.Available != 22 || got.MemoryMiB.Total != 72704 || got.MemoryMiB.Available != 49152 || got.GPU.Total != 18 || got.GPU.Available != 3 {
		t.Fatalf("wrong scheduler capacity %+v", got)
	}
	if got.CPU.Allocated == nil || *got.CPU.Allocated != 8 || got.MemoryMiB.Allocated == nil || *got.MemoryMiB.Allocated != 16384 || got.GPU.Allocated == nil || *got.GPU.Allocated != 1 {
		t.Fatalf("unavailable or reserved resources were counted as allocations: %+v", got)
	}
}

func TestInvalidNodeResponsesFailInsteadOfReportingAvailableZero(t *testing.T) {
	for _, body := range []string{"", "No nodes in system", strings.Replace(healthyNode, "RealMemory=65536", "RealMemory=unknown", 1), strings.Replace(healthyNode, "CPUAlloc=8 ", "", 1), healthyNode + "\n" + healthyNode, strings.Replace(healthyNode, " AllocTRES=cpu=8,mem=16384M,gres/gpu=1,gres/gpu:a100=1", "", 1), healthyNode + " CPUAlloc=0"} {
		if _, err := parseNodes([]byte(body)); err == nil {
			t.Fatalf("accepted incomplete node response %q", body)
		}
	}
	got, err := parseNodes([]byte("NodeName=cpu01 CPUAlloc=0 CPUTot=8 RealMemory=16384 AllocMem=0 State=IDLE Gres=(null) AllocTRES="))
	if err != nil || got.GPU.Total != 0 || got.CPU.Available != 8 {
		t.Fatalf("valid GPU-free node rejected: %+v %v", got, err)
	}
}

func TestUntrackedGRESTotalsNeverBecomeFalselyIdleGPUs(t *testing.T) {
	base := "NodeName=gpu01 CPUAlloc=0 CPUTot=8 RealMemory=16384 AllocMem=0 State=IDLE Gres=gpu:a100:4 CfgTRES=cpu=8,mem=16384M "
	if _, err := parseNodes([]byte(base + "AllocTRES=")); err == nil {
		t.Fatal("untracked GPU allocations became false zero")
	}
	got, err := parseNodes([]byte(base + "AllocTRES=gres/gpu=2"))
	if err != nil || got.GPU.Total != 4 || got.GPU.Available != 2 {
		t.Fatalf("authoritative GRES fallback failed: %+v %v", got, err)
	}
	tracked := strings.Replace(base, "CfgTRES=cpu=8,mem=16384M", "CfgTRES=cpu=8,mem=16384M,gres/gpu=4", 1)
	got, err = parseNodes([]byte(tracked + "AllocTRES="))
	if err != nil || got.GPU.Available != 4 {
		t.Fatalf("known GPU TRES zero allocation lost: %+v %v", got, err)
	}
}

func queueJob(id int) map[string]any {
	return map[string]any{"job_id": id, "name": "train", "user_name": "alice", "user_id": 1001, "job_state": []string{"RUNNING"}, "partition": "gpu", "tres_req_str": "cpu=4,mem=1.5G,gres/gpu=1,gres/gpu:a100=1", "tres_alloc_str": "cpu=4,mem=2G,gres/gpu=1", "start_time": 1000, "suspend_time": 0, "pre_sus_time": 0, "time_limit": map[string]any{"set": true, "infinite": false, "number": 90}, "node_count": map[string]any{"set": true, "infinite": false, "number": 1}, "state_reason": "None", "command": "private-command", "script": "private-script", "environment": []string{"PASSWORD=secret"}, "current_working_directory": "/private/home/alice"}
}
func queueBody(jobs ...map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{"jobs": jobs, "errors": []any{}})
	return b
}

func TestQueueProjectsOnlySafeSchedulingMetadataAndPreservesUnknown(t *testing.T) {
	running := queueJob(10)
	pending := queueJob(11)
	pending["job_state"] = []string{"PENDING"}
	pending["time_limit"] = map[string]any{"set": false, "infinite": true, "number": 0}
	pending["node_count"] = nil
	pending["tres_req_str"] = ""
	pending["name"] = strings.Repeat("训", 200) + "\nsecret-control\x00"
	jobs, total, err := parseJobs(queueBody(running, pending), time.Unix(1060, 0))
	if err != nil || total != 2 || len(jobs) != 2 {
		t.Fatalf("invalid queue %v %+v", err, jobs)
	}
	if *jobs[0].ElapsedSeconds != 60 || *jobs[0].TimeLimitSeconds != 5400 || *jobs[0].Requested.MemoryMiB != 1536 || *jobs[0].Requested.GPU != 1 {
		t.Fatalf("bad resource/time conversion %+v", jobs[0])
	}
	if jobs[1].TimeLimitSeconds != nil || jobs[1].NodeCount != nil || jobs[1].Requested.CPU != nil || jobs[1].Requested.GPU != nil || *jobs[1].Allocated.GPU != 0 || *jobs[1].ElapsedSeconds != 0 {
		t.Fatalf("unknown or pending values fabricated %+v", jobs[1])
	}
	body, _ := json.Marshal(jobs)
	for _, secret := range []string{"command", "script", "PASSWORD", "private/home", "environment", "current_working_directory", "\\n", "\\u0000"} {
		if bytes.Contains(body, []byte(secret)) {
			t.Fatalf("private/control data leaked: %s", body)
		}
	}
	if len(jobs[1].Name) > 256 {
		t.Fatal("job name exceeds text budget")
	}
	for _, bad := range [][]byte{[]byte(`{"jobs":null}`), []byte(`{"jobs":[],"errors":[{}]}`), queueBody(running, running), []byte(`{"jobs":[{"job_id":0,"job_state":["RUNNING"]}]}`)} {
		if _, _, err := parseJobs(bad, time.Now()); err == nil {
			t.Fatal("accepted ambiguous queue response")
		}
	}
}

func TestQueueRuntimeExcludesSuspensionAndRequiresVerifiedTuple(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state string
		edit  func(map[string]any)
		want  *int64
	}{
		{"runningWithoutSuspension", "RUNNING", func(map[string]any) {}, ptr(300)},
		{"currentlySuspended", "SUSPENDED", func(row map[string]any) { row["suspend_time"], row["pre_sus_time"] = 1100, 80 }, ptr(80)},
		{"resumedRunning", "RUNNING", func(row map[string]any) { row["suspend_time"], row["pre_sus_time"] = 1200, 80 }, ptr(180)},
		{"suspendedMissingPre", "SUSPENDED", func(row map[string]any) { row["suspend_time"] = 1100; delete(row, "pre_sus_time") }, nil},
		{"runningMissingSuspend", "RUNNING", func(row map[string]any) { delete(row, "suspend_time") }, nil},
		{"runningMissingPreIgnoresTotalRunTime", "RUNNING", func(row map[string]any) { delete(row, "pre_sus_time"); row["total_run_time"] = 300 }, nil},
		{"unsetSuspend", "RUNNING", func(row map[string]any) { row["suspend_time"] = map[string]any{"set": false, "number": 0} }, nil},
		{"nullPre", "RUNNING", func(row map[string]any) { row["pre_sus_time"] = nil }, nil},
		{"negativePre", "RUNNING", func(row map[string]any) { row["pre_sus_time"] = -1 }, nil},
		{"suspendBeforeStart", "RUNNING", func(row map[string]any) { row["suspend_time"] = 999 }, nil},
		{"suspendAfterObservation", "SUSPENDED", func(row map[string]any) { row["suspend_time"] = 1301 }, nil},
		{"preExceedsPossibleActiveTime", "RUNNING", func(row map[string]any) { row["suspend_time"], row["pre_sus_time"] = 1100, 101 }, nil},
		{"suspendedWithoutSuspendTimestamp", "SUSPENDED", func(map[string]any) {}, nil},
		{"futureStart", "RUNNING", func(row map[string]any) { row["start_time"] = 1301 }, nil},
		{"otherStateCannotProveRuntime", "CONFIGURING", func(map[string]any) {}, nil},
		{"pendingStillKnownZero", "PENDING", func(row map[string]any) { delete(row, "suspend_time"); delete(row, "pre_sus_time") }, ptr(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := queueJob(1)
			row["job_state"] = []string{tc.state}
			tc.edit(row)
			jobs, _, err := parseJobs(queueBody(row), time.Unix(1300, 0))
			if err != nil || len(jobs) != 1 {
				t.Fatalf("runtime metadata should not discard the queue: %v", err)
			}
			got := jobs[0].ElapsedSeconds
			if (got == nil) != (tc.want == nil) || got != nil && *got != *tc.want {
				t.Fatalf("elapsed=%v, expected %v", got, tc.want)
			}
		})
	}
}

func TestCollectorPinsCommandsAndReportsHonestRowAndByteTruncation(t *testing.T) {
	var calls []string
	jobs := make([]map[string]any, MaxJobs+5)
	for i := range jobs {
		jobs[i] = queueJob(i + 1)
		jobs[i]["name"] = strings.Repeat("n", 256)
		jobs[i]["user_name"] = strings.Repeat("u", 128)
		jobs[i]["partition"] = strings.Repeat("p", 128)
		jobs[i]["state_reason"] = strings.Repeat("r", 256)
	}
	queue := queueBody(jobs...)
	collector, err := NewCollector("cluster1", Config{}, runFunc(func(ctx context.Context, command string, args ...string) ([]byte, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("command has no deadline")
		}
		calls = append(calls, command+" "+strings.Join(args, " "))
		if strings.HasSuffix(command, "scontrol") {
			return []byte(healthyNode), nil
		}
		return queue, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	report, err := collector.Collect(context.Background(), time.Unix(1060, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"/usr/bin/scontrol --oneliner show nodes", "/usr/bin/squeue --json --all --local"}) {
		t.Fatalf("unsafe command invocation %v", calls)
	}
	body, _ := json.Marshal(report)
	if len(body) > MaxPayloadBytes || !report.JobsTruncated || report.JobsTotal != MaxJobs+5 || len(report.Jobs) >= MaxJobs {
		t.Fatalf("bad truncation: bytes%d kept%d/%d flag%v", len(body), len(report.Jobs), report.JobsTotal, report.JobsTruncated)
	}
	if report.Health != "ok" || report.ClusterID != "cluster1" || report.Error != nil || report.Resources == nil {
		t.Fatalf("invalid healthy report %+v", report)
	}
}

func TestTrustedTransportLimitsPreserveCompleteLargeQueueWithoutChangingLegacy(t *testing.T) {
	rows := make([]map[string]any, 2501)
	for i := range rows {
		rows[i] = queueJob(i + 1)
		rows[i]["name"] = strings.Repeat("n", 256)
		rows[i]["state_reason"] = strings.Repeat("r", 256)
	}
	queue := queueBody(rows...)
	runner := runFunc(func(_ context.Context, command string, _ ...string) ([]byte, error) {
		if strings.HasSuffix(command, "scontrol") {
			return []byte(healthyNode), nil
		}
		return queue, nil
	})
	legacy, _ := NewCollector("cluster1", Config{}, runner)
	old, err := legacy.Collect(context.Background(), time.Unix(1060, 0))
	if err != nil || !old.JobsTruncated || len(old.Jobs) > MaxJobs {
		t.Fatal("legacy bounds changed")
	}
	collector, err := NewCollectorWithLimits("cluster1", Config{}, runner, CollectionLimits{MaxItems: 20000, MaxPayloadBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	report, err := collector.Collect(context.Background(), time.Unix(1060, 0))
	encoded, _ := json.Marshal(report)
	if err != nil || report.JobsTruncated || len(report.Jobs) != len(rows) || report.JobsTotal != len(rows) || len(encoded) <= MaxPayloadBytes || len(encoded) > 4<<20 {
		t.Fatalf("complete large queue rejected: kept%d total%d bytes%d err%v", len(report.Jobs), report.JobsTotal, len(encoded), err)
	}
	for _, limits := range []CollectionLimits{{0, 4 << 20}, {20001, 4 << 20}, {20000, 4095}, {20000, 4<<20 + 1}} {
		if _, err := NewCollectorWithLimits("cluster1", Config{}, runner, limits); err == nil {
			t.Fatal("unbounded transport limits accepted")
		}
	}
}

func TestCurrentTransportResourceAggregationAllowsTwentyThousandNodes(t *testing.T) {
	var body strings.Builder
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(&body, "NodeName=n%05d CPUAlloc=0 CPUTot=1 RealMemory=100 AllocMem=0 State=IDLE Gres=(null) CfgTRES=cpu=1,mem=100M AllocTRES=cpu=0,mem=0M\n", i)
	}
	if _, err := parseNodes([]byte(body.String())); err == nil {
		t.Fatal("legacy raw-node capacity changed")
	}
	resources, err := parseNodesWithLimit([]byte(body.String()), 20000)
	if err != nil || resources.CPU.Total != 20000 || resources.CPU.Available != 20000 || resources.MemoryMiB.Total != 2000000 {
		t.Fatalf("current resource aggregation rejected full inventory: %+v %v", resources, err)
	}
}

func TestLocalConfigurationEnvironmentAndCancellationAreBounded(t *testing.T) {
	collector, err := NewCollector("cluster", Config{SlurmConf: "/etc/slurm/custom.conf"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if collector.cfg.Enabled {
		t.Fatal("new compiled plugin implicitly enabled local collection")
	}
	if !reflect.DeepEqual(collector.runner.(commandRunner).env, []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "SLURM_CONF=/etc/slurm/custom.conf"}) {
		t.Fatal("inherited process credentials or unsafe Slurm environment")
	}
	for _, cfg := range []Config{{SqueuePath: "squeue"}, {ScontrolPath: "/usr/bin/../bin/scontrol"}, {SlurmConf: "relative.conf"}, {CommandTimeout: "1h"}} {
		if _, err := cfg.WithDefaults(); err == nil {
			t.Fatal("unsafe local config accepted")
		}
	}
	blocked, _ := NewCollector("cluster", Config{CommandTimeout: "15ms"}, runFunc(func(ctx context.Context, _ string, _ ...string) ([]byte, error) { <-ctx.Done(); return nil, ctx.Err() }))
	started := time.Now()
	if _, err := blocked.Collect(context.Background(), time.Now()); err == nil || time.Since(started) > time.Second {
		t.Fatal("command timeout did not cancel")
	}
	path, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := (commandRunner{env: []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}}).Run(ctx, path, "10"); err == nil {
		t.Fatal("native command ignored context cancellation")
	}
	var output boundedBuffer
	if _, err := output.Write(make([]byte, maxCommandBytes+1)); err == nil {
		t.Fatal("unbounded command output accepted")
	}
}

func TestFailureReportsContainOnlySafeErrorCode(t *testing.T) {
	collector, _ := NewCollector("cluster", Config{}, runFunc(func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("private-token and command stderr")
	}))
	if _, err := collector.Collect(context.Background(), time.Now()); err == nil {
		t.Fatal("command failure treated as capacity zero")
	}
	report := ErrorReport("cluster", time.Unix(1000, 0))
	body, _ := json.Marshal(report)
	if report.Resources != nil || report.Jobs == nil || len(report.Jobs) != 0 || report.Health != "error" || report.Error == nil || *report.Error != "collection_failed" || bytes.Contains(body, []byte("private")) {
		t.Fatalf("unsafe failure snapshot %s", body)
	}
}

// Inside a container without directory NSS, squeue names nobody; the slave's lookup supplies the
// account name, and unknown UIDs stay numeric.
func TestCollectorNamesNumericJobOwners(t *testing.T) {
	named, unnamed, unknown := queueJob(1), queueJob(2), queueJob(3)
	unnamed["user_name"], unnamed["user_id"] = "", 10001
	unknown["user_name"], unknown["user_id"] = "", 10005
	queue := queueBody(named, unnamed, unknown)
	collector, err := NewCollector("cluster1", Config{}, runFunc(func(_ context.Context, command string, _ ...string) ([]byte, error) {
		if strings.HasSuffix(command, "scontrol") {
			return []byte(healthyNode), nil
		}
		return queue, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	collector.SetUsernames(func(uid int64) (string, bool) { return "bob", uid == 10001 })
	report, err := collector.Collect(context.Background(), time.Unix(1060, 0))
	if err != nil {
		t.Fatal(err)
	}
	var users []string
	for _, j := range report.Jobs {
		users = append(users, j.User)
	}
	if !reflect.DeepEqual(users, []string{"alice", "bob", "10005"}) {
		t.Fatalf("job owners = %v", users)
	}
}
