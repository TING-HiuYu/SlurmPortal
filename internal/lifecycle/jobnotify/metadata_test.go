package jobnotify

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func detailedJob(state string) map[string]any {
	j := fixtureJob(7, testSLUID, state)
	for key, value := range map[string]any{
		"tres_req_str":    "cpu=30,mem=62G,node=1,billing=30,gres/gpu=1",
		"tres_alloc_str":  "cpu=30,mem=62G,node=1,billing=30,gres/gpu=1",
		"standard_output": "/HOME/example-user/logs/task.%j.out",
		"stdout_expanded": "/HOME/example-user/logs/task.7.out",
		"standard_error":  "", "stderr_expanded": "",
		"standard_input": "/dev/null", "stdin_expanded": "/dev/null",
		"command":                   "/HOME/example-user/run <task>.sbatch",
		"submit_line":               "sbatch /HOME/example-user/run <task>.sbatch",
		"current_working_directory": "/HOME/example-user",
		"batch_host":                "node01", "nodes": "cgn[01-02,04]",
		"partition": "gpu", "account": "example-user", "qos": "normal",
		"user_name": "example-user", "group_name": "sshusers", "group_id": 10000,
		"cpus": wrapped(30), "node_count": wrapped(1), "tasks": wrapped(1), "cpus_per_task": wrapped(1),
		"memory_per_node": wrapped(63488), "memory_per_cpu": map[string]any{"set": false, "infinite": false, "number": 0},
		"priority": wrapped(1), "restart_cnt": 0,
		"cpus_per_tres": "gres/gpu:30", "memory_per_tres": "gres/gpu:63488", "tres_per_node": "gres/gpu:1",
		"time_limit":  map[string]any{"set": false, "infinite": true, "number": 0},
		"submit_time": wrapped(1792886390), "start_time": wrapped(1792886400), "end_time": wrapped(1792886461),
	} {
		j[key] = value
	}
	return j
}

func jobJSON(t *testing.T, job Job) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := json.Unmarshal(encoded, &values); err != nil {
		t.Fatal(err)
	}
	return values
}

func TestJobMetadataAcrossTerminalStates(t *testing.T) {
	for _, state := range []string{"COMPLETED", "FAILED", "CANCELLED", "TIMEOUT", "OUT_OF_MEMORY", "NODE_FAIL", "BOOT_FAIL", "PREEMPTED", "DEADLINE"} {
		t.Run(state, func(t *testing.T) {
			event, err := Normalize("cluster1", Callback{7, testSLUID}, responseBytes(detailedJob(state)), time.Now(), 256<<10)
			if err != nil {
				t.Fatal(err)
			}
			fields := jobJSON(t, event.Job)
			for key, want := range map[string]any{
				"requested_tres":  "cpu=30,mem=62G,node=1,billing=30,gres/gpu=1",
				"allocated_tres":  "cpu=30,mem=62G,node=1,billing=30,gres/gpu=1",
				"standard_output": "/HOME/example-user/logs/task.%j.out", "stdout_expanded": "/HOME/example-user/logs/task.7.out",
				"standard_error": "", "stderr_expanded": "", "standard_input": "/dev/null", "stdin_expanded": "/dev/null",
				"command": "/HOME/example-user/run <task>.sbatch", "submit_line": "sbatch /HOME/example-user/run <task>.sbatch",
				"working_directory": "/HOME/example-user", "batch_host": "node01", "nodes": "cgn[01-02,04]",
				"partition": "gpu", "account": "example-user", "qos": "normal", "user_name": "example-user", "group_name": "sshusers",
				"cpus": float64(30), "node_count": float64(1), "tasks": float64(1), "cpus_per_task": float64(1),
				"memory_per_node_mib": float64(63488), "priority": float64(1), "restart_count": float64(0), "group_id": float64(10000),
				"cpus_per_tres": "gres/gpu:30", "memory_per_tres": "gres/gpu:63488", "tres_per_node": "gres/gpu:1",
				"time_limit": "UNLIMITED", "submit_time": float64(1792886390), "start_time": float64(1792886400), "end_time": float64(1792886461),
				"submitted_at": "2026-10-24T23:59:50Z", "started_at": "2026-10-25T00:00:00Z", "ended_at": "2026-10-25T00:01:01Z",
				"metadata_complete": true,
			} {
				if got, present := fields[key]; !present || got != want {
					t.Errorf("%s: got %#v (present=%v), want %#v", key, got, present, want)
				}
			}
			if _, exists := fields["memory_per_cpu_mib"]; exists {
				t.Error("unset memory per CPU invented a value")
			}
			if !event.Raw.Complete || !bytes.Contains(event.Raw.Controller, []byte(`"tres_req_str"`)) {
				t.Error("projection changed raw retention")
			}
		})
	}
}

func TestJobMetadataUnknownAndMalformedValues(t *testing.T) {
	j := detailedJob("FAILED")
	delete(j, "qos")
	j["account"] = nil
	j["cpus"] = map[string]any{"set": false, "infinite": false, "number": 0}
	j["memory_per_node"] = wrapped(0)
	j["node_count"] = wrapped(-1)
	j["tasks"] = wrapped(1 << 53)
	j["priority"] = "1"
	j["batch_host"] = []string{"node01"}
	j["submit_time"] = wrapped(253402300800) // ISO year 10000 is not supported.
	j["time_limit"] = wrapped(90)
	event, err := Normalize("cluster1", Callback{7, testSLUID}, responseBytes(j), time.Now(), 256<<10)
	if err != nil {
		t.Fatal(err)
	}
	fields := jobJSON(t, event.Job)
	for _, key := range []string{"qos", "account", "cpus", "node_count", "tasks", "priority", "batch_host", "submitted_at"} {
		if _, exists := fields[key]; exists {
			t.Errorf("unavailable or invalid %s should be absent", key)
		}
	}
	if fields["memory_per_node_mib"] != float64(0) || fields["standard_error"] != "" || fields["time_limit"] != "90 min" {
		t.Error("known zero, explicit empty string or finite time limit lost")
	}
	if fields["metadata_complete"] != false {
		t.Error("invalid projection must be explicitly marked")
	}
	for _, name := range []string{"node_count", "tasks", "priority", "batch_host", "submitted_at"} {
		if !strings.Contains(strings.Join(event.Job.MetadataOmitted, ","), "job."+name) {
			t.Errorf("missing omission path for %s", name)
		}
	}
	if !bytes.Contains(event.Raw.Controller, []byte(`"batch_host":["node01"]`)) {
		t.Error("malformed optional source should still be available in raw")
	}
}

func TestJobMetadataSurvivesRawSizeLimit(t *testing.T) {
	j := detailedJob("TIMEOUT")
	j["large_unknown_field"] = strings.Repeat("x", 300<<10)
	event, err := Normalize("cluster1", Callback{7, testSLUID}, responseBytes(j), time.Now(), 256<<10)
	if err != nil {
		t.Fatal(err)
	}
	if string(event.Raw.Controller) != "null" || event.Raw.Complete || event.Raw.OmittedReason == "" {
		t.Fatal("oversized raw must be explicitly omitted")
	}
	if event.Job.RequestedTRES == nil || event.Job.AllocatedTRES == nil || event.Job.StdoutExpanded == nil || event.Job.BatchHost == nil || event.Job.Command == nil {
		t.Fatal("key job details must survive independently of raw")
	}
	if event.Job.MetadataComplete == nil || !*event.Job.MetadataComplete {
		t.Fatal("raw and summary completeness have different meanings")
	}
}

func TestJobMetadataStringAndAggregateBudgets(t *testing.T) {
	j := detailedJob("COMPLETED")
	j["command"] = strings.Repeat("界", 2731) // 8193 UTF-8 bytes; must not become a partial path.
	j["submit_line"] = strings.Repeat("s", 8192)
	j["standard_input"] = strings.Repeat("i", 8192)
	j["stdin_expanded"] = strings.Repeat("I", 8192)
	j["standard_error"] = strings.Repeat("e", 8192)
	j["stderr_expanded"] = strings.Repeat("E", 8192)
	event, err := Normalize("cluster1", Callback{7, testSLUID}, responseBytes(j), time.Now(), 256<<10)
	if err != nil {
		t.Fatal(err)
	}
	if event.Job.Command != nil || event.Job.MetadataComplete == nil || *event.Job.MetadataComplete {
		t.Fatal("oversized command should be omitted and marked, never truncated")
	}
	if event.Job.SubmitLine == nil || len(*event.Job.SubmitLine) != 8192 || event.Job.RequestedTRES == nil {
		t.Fatal("higher-priority fields should be retained intact")
	}
	encoded, _ := json.Marshal(event.Job)
	// Core identity is not part of the summary budget.
	var projected map[string]json.RawMessage
	_ = json.Unmarshal(encoded, &projected)
	for _, key := range []string{"job_id", "sluid", "uid", "name", "array", "heterogeneous"} {
		delete(projected, key)
	}
	encoded, _ = json.Marshal(projected)
	if len(encoded) > 32<<10 || len(event.Job.MetadataOmitted) < 2 {
		t.Fatalf("summary budget/omission markers failed: %d bytes, %v", len(encoded), event.Job.MetadataOmitted)
	}
}

func TestJobMetadataSmallWireBudgetDoesNotDropCore(t *testing.T) {
	j := detailedJob("CANCELLED")
	j["name"] = strings.Repeat("n", 1200)
	for _, key := range []string{"tres_req_str", "tres_alloc_str", "stdout_expanded", "standard_output", "batch_host", "command", "current_working_directory", "submit_line"} {
		j[key] = strings.Repeat("x", 8000)
	}
	event, err := Normalize("cluster1", Callback{7, testSLUID}, responseBytes(j), time.Now(), 2048)
	if err != nil {
		t.Fatalf("optional metadata must not suppress an otherwise fitting core event: %v", err)
	}
	_, encoded, err := encodeBoundedEvent(event, 2048)
	if err != nil || len(encoded) > 2048 || event.Job.Name != j["name"] || event.Result.State != "CANCELLED" {
		t.Fatalf("core identity/state not retained: %d bytes, %v", len(encoded), err)
	}
	if event.Job.MetadataComplete == nil || *event.Job.MetadataComplete || len(event.Job.MetadataOmitted) != 0 || event.Raw.OmittedReason != "metadata size limit" {
		t.Fatal("when omission names cannot fit, preserve the incomplete marker and short reason")
	}
	_, again, err := encodeBoundedEvent(event, 2048)
	if err != nil || !bytes.Equal(encoded, again) {
		t.Fatal("bounded wire encoding must be idempotent")
	}
}
