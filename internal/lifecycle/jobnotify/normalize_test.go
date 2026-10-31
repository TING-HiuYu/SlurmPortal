package jobnotify

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const testSLUID = "s123456789ABCD"

func fixtureJob(id uint32, sluid string, states ...string) map[string]any {
	return map[string]any{"job_id": id, "step_id": map[string]any{"job_id": id, "sluid": sluid}, "job_state": states, "user_id": 10019, "name": "test <job>", "start_time": wrapped(1000), "end_time": wrapped(1061), "suspend_time": wrapped(0), "pre_sus_time": wrapped(0), "exit_code": map[string]any{"status": []string{"SUCCESS"}, "return_code": 0}, "array_job_id": wrapped(0), "het_job_id": wrapped(0), "unknown_future_field": map[string]any{"set": false, "number": 0}}
}
func wrapped(n int64) map[string]any {
	return map[string]any{"set": true, "infinite": false, "number": n}
}
func responseBytes(jobs ...map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{"jobs": jobs, "errors": []any{}, "warnings": []any{}, "meta": map[string]any{"plugin": map[string]any{"data_parser": "v0.0.45"}}})
	return b
}

func TestNormalize2605(t *testing.T) {
	for _, state := range []string{"COMPLETED", "FAILED", "CANCELLED", "TIMEOUT", "OUT_OF_MEMORY", "NODE_FAIL", "BOOT_FAIL", "PREEMPTED", "DEADLINE"} {
		t.Run(state, func(t *testing.T) {
			event, err := Normalize("cluster1", Callback{7, testSLUID}, responseBytes(fixtureJob(7, testSLUID, state)), time.Now(), 256<<10)
			if err != nil || event.Job.UID != 10019 || event.Duration.ActiveSeconds != 61 || event.Result.State != state {
				t.Fatalf("bad finalization: %+v %v", event, err)
			}
			if !strings.Contains(string(event.Raw.Controller), "unknown_future_field") || !event.Raw.Complete {
				t.Fatal("raw fields lost")
			}
		})
	}
}

func TestNormalizeEligibilityAndIdentity(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
		want   error
	}{
		{"below60", func(j map[string]any) { j["end_time"] = wrapped(1059) }, ErrFiltered},
		{"exactly60", func(j map[string]any) { j["end_time"] = wrapped(1060) }, ErrFiltered},
		{"suspendExcluded", func(j map[string]any) { j["suspend_time"] = wrapped(1060); j["pre_sus_time"] = wrapped(0) }, ErrFiltered},
		{"pausedExactly60", func(j map[string]any) {
			j["end_time"] = wrapped(2000)
			j["suspend_time"] = wrapped(1950)
			j["pre_sus_time"] = wrapped(10)
		}, ErrFiltered},
		{"requeuedLastAttemptOnly60", func(j map[string]any) {
			j["end_time"] = wrapped(1060)
			j["restart_cnt"] = 4
			j["total_run_time"] = 10000
		}, ErrFiltered},
		{"unsetSuspend", func(j map[string]any) { j["suspend_time"] = map[string]any{"set": false, "number": 0} }, ErrRetry},
		{"unknownFlag", func(j map[string]any) { j["job_state"] = []string{"FAILED", "REVOKED"} }, ErrRetry},
		{"completing", func(j map[string]any) { j["job_state"] = []string{"FAILED", "COMPLETING"} }, ErrRetry},
		{"requeued", func(j map[string]any) { j["job_state"] = []string{"PENDING", "COMPLETING"} }, ErrSuperseded},
		{"newExecution", func(j map[string]any) { j["step_id"] = map[string]any{"sluid": "s123456789abce"} }, ErrSuperseded},
		{"wrongJobID", func(j map[string]any) { j["job_id"] = 8 }, ErrRetry},
		{"missingUID", func(j map[string]any) { delete(j, "user_id") }, ErrRetry},
		{"pendingSyntheticStart", func(j map[string]any) { j["start_time"] = wrapped(1061) }, ErrFiltered},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			j := fixtureJob(7, testSLUID, "FAILED")
			tc.mutate(j)
			_, err := Normalize("cluster1", Callback{7, testSLUID}, responseBytes(j), time.Now(), 256<<10)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
		})
	}
	j := fixtureJob(7, testSLUID, "COMPLETED")
	j["end_time"] = wrapped(1061)
	if event, err := Normalize("cluster1", Callback{7, testSLUID}, responseBytes(j), time.Now(), 256<<10); err != nil || event.Duration.ActiveSeconds != 61 {
		t.Fatalf("61 seconds rejected: %v", err)
	}
	j["end_time"], j["suspend_time"], j["pre_sus_time"] = wrapped(2000), wrapped(1950), wrapped(11)
	if event, err := Normalize("cluster1", Callback{7, testSLUID}, responseBytes(j), time.Now(), 256<<10); err != nil || event.Duration.ActiveSeconds != 61 {
		t.Fatalf("61 active seconds after excluding suspension rejected: %v", err)
	}
}

func TestNormalizeSuspensionArrayAndSize(t *testing.T) {
	j := fixtureJob(7, testSLUID, "CANCELLED")
	j["end_time"] = wrapped(2000)
	j["suspend_time"] = wrapped(1900)
	j["pre_sus_time"] = wrapped(250)
	j["array_job_id"] = wrapped(6)
	j["array_task_id"] = wrapped(3)
	j["het_job_id"] = wrapped(4)
	j["het_job_offset"] = wrapped(2)
	event, err := Normalize("cluster1", Callback{7, testSLUID}, responseBytes(fixtureJob(99, "s123456789abcf", "FAILED"), j), time.Now(), 256<<10)
	if err != nil || event.Duration.ActiveSeconds != 350 || event.Job.Array == nil || event.Job.Heterogeneous == nil {
		t.Fatalf("bad pause/array mapping: %+v %v", event, err)
	}
	j["large"] = strings.Repeat("x", 3000)
	event, err = Normalize("cluster1", Callback{7, testSLUID}, responseBytes(j), time.Now(), 2048)
	if err != nil || event.Raw.Complete || event.Raw.OmittedReason == "" {
		t.Fatalf("oversize not explicit: %+v %v", event, err)
	}
}
