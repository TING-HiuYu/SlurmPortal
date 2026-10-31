package joblifecycle

import (
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/lifecycle/jobnotify"
	"github.com/TING-HiuYu/SlurmPortal/internal/lifecycle/resources"
)

const lifecycleSLUID = "s123456789ABCD"

func wrappedTest(n int64) map[string]any {
	return map[string]any{"set": true, "infinite": false, "number": n}
}

func jobTestRow(id uint32, sluid, state string, start, end int64, gpus int) map[string]any {
	return map[string]any{
		"job_id": id, "step_id": map[string]any{"job_id": id, "sluid": sluid},
		"job_state": []string{state}, "user_id": 10019, "name": "isolated lifecycle fixture",
		"start_time": wrappedTest(start), "end_time": wrappedTest(end),
		"suspend_time": wrappedTest(0), "pre_sus_time": wrappedTest(0),
		"exit_code":    map[string]any{"status": []string{"SUCCESS"}, "return_code": 0},
		"array_job_id": wrappedTest(0), "het_job_id": wrappedTest(0),
		"cpus": wrappedTest(4), "node_count": wrappedTest(1), "memory_per_node": wrappedTest(2048),
		"nodes": "fixture-node", "tres_alloc_str": "cpu=4,mem=2G,node=1,gres/gpu=" + strconv.Itoa(gpus),
	}
}

func jobsTestBody(rows ...map[string]any) []byte {
	body, _ := json.Marshal(map[string]any{"jobs": rows, "errors": []any{}, "warnings": []any{}})
	return body
}

func TestLifecycleStrictCallbackShape(t *testing.T) {
	valid := `{"job_id":7,"sluid":"` + lifecycleSLUID + `"}`
	if callback, err := decodeCallback([]byte(valid)); err != nil || callback.JobID != 7 || callback.SLUID != lifecycleSLUID {
		t.Fatalf("valid identity rejected: %+v %v", callback, err)
	}
	for _, body := range []string{
		`{"job_id":7,"job_id":8,"sluid":"` + lifecycleSLUID + `"}`,
		`{"Job_ID":7,"sluid":"` + lifecycleSLUID + `"}`,
		`{"job_id":7,"SLUID":"` + lifecycleSLUID + `"}`,
		`{"job_id":7,"sluid":"` + lifecycleSLUID + `","extra":true}`,
		valid + `{}`,
		`{"job_id":0,"sluid":"` + lifecycleSLUID + `"}`,
		`{"job_id":7,"sluid":"bad"}`,
	} {
		if _, err := decodeCallback([]byte(body)); err == nil {
			t.Errorf("ambiguous callback was accepted: %s", body)
		}
	}
}

func TestLifecycleNumberAndGPUIndexParsing(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  int64
		ok    bool
	}{{`12`, 12, true}, {`{"set":true,"infinite":false,"number":15}`, 15, true}, {`{"set":false,"number":15}`, 0, false}, {`{"set":true,"infinite":true,"number":15}`, 0, false}, {`1.5`, 0, false}, {`"12"`, 0, false}, {`null`, 0, false}} {
		n, ok := number(json.RawMessage(tc.input))
		if ok != tc.ok || (ok && n != tc.want) {
			t.Errorf("number(%s) = %d,%v; want %d,%v", tc.input, n, ok, tc.want, tc.ok)
		}
	}
	indices, err := expandIndices("0,2-4,7")
	if err != nil || !reflect.DeepEqual(indices, []int{0, 2, 3, 4, 7}) {
		t.Fatalf("indices %+v %v", indices, err)
	}
	for _, value := range []string{"-1", "4-2", "1--2", "4096", "0-4096", "a", "0-"} {
		if _, err := expandIndices(value); err == nil {
			t.Errorf("invalid allocation index accepted: %q", value)
		}
	}
	for tres, want := range map[string]int64{"cpu=4,gres/gpu=2": 2, "gres/gpu:a100=2,gres/gpu:h100=1": 3, "gres/gpu:a100=2,gres/gpu=2": 2, "cpu=4": 0, "gres/gpu=-1": 0, "gres/gpu=99999": 0} {
		if got := gpuCount(tres); got != want {
			t.Errorf("GPU count %q = %d, want %d", tres, got, want)
		}
	}
}

func TestLifecycleSelectJobMatchesExactExecutionAndRejectsAmbiguity(t *testing.T) {
	callback := jobnotify.Callback{JobID: 7, SLUID: lifecycleSLUID}
	row := jobTestRow(7, lifecycleSLUID, "RUNNING", 1000, 0, 0)
	if _, err := selectJob(jobsTestBody(row), callback); err != nil {
		t.Fatal(err)
	}
	other := jobTestRow(7, "s123456789ABCE", "RUNNING", 1000, 0, 0)
	if _, err := selectJob(jobsTestBody(other), callback); !errors.Is(err, jobnotify.ErrSuperseded) {
		t.Fatalf("new execution wasn't rejected: %v", err)
	}
	for _, body := range [][]byte{jobsTestBody(row, row), jobsTestBody(jobTestRow(8, lifecycleSLUID, "RUNNING", 1000, 0, 0)), []byte(`{"jobs":[],"errors":[{"error":"unavailable"}]}`), []byte(`{"jobs":`)} {
		if _, err := selectJob(body, callback); !errors.Is(err, jobnotify.ErrRetry) {
			t.Fatalf("unverifiable response wasn't retried: %s %v", body, err)
		}
	}
}

func TestLifecycleSummaryUsesAllocationMemoryAndLastAttemptDuration(t *testing.T) {
	row := jobTestRow(7, lifecycleSLUID, "FAILED", 1000, 1020, 1)
	report, err := jobnotify.NormalizeLifecycle("fixture", jobnotify.Callback{JobID: 7, SLUID: lifecycleSLUID}, jobsTestBody(row), time.Unix(1021, 0), 256<<10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobnotify.Normalize("fixture", jobnotify.Callback{JobID: 7, SLUID: lifecycleSLUID}, jobsTestBody(row), time.Unix(1021, 0), 256<<10); !errors.Is(err, jobnotify.ErrFiltered) {
		t.Fatalf("short-job email policy changed: %v", err)
	}
	usage := resources.GPUUsage{ObservedGPUSeconds: 10, UtilizationPercentSeconds: 800, MemoryUtilizationPercentSeconds: 500}
	summary := summaryFor(report, usage)
	if summary.Job.CPUs != 4 || summary.Job.GPUs != 1 || summary.Job.MemoryMiB != 2048 || summary.Duration.ActiveSeconds != 20 || summary.Duration.Scope != "last_attempt" || summary.Result.State != "FAILED" || !summary.EndedAt.Equal(time.Unix(1020, 0)) || summary.Usage.GPU.AllocatedGPUSeconds != 20 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	if summary.EventID != "fixture/"+lifecycleSLUID+"/resources" {
		t.Fatalf("unstable summary identity: %s", summary.EventID)
	}
}

func TestLifecycleMemoryUnitsAreValidated(t *testing.T) {
	for value, want := range map[string]int64{"2048": 2048, "1024K": 1, "512M": 512, "1.5G": 1536, "1T": 1048576} {
		got, err := parseMemoryMiB(value)
		if err != nil || got != want {
			t.Errorf("memory %q = %d,%v; want %d", value, got, err, want)
		}
	}
	for _, value := range []string{"", "-1G", "NaN", "+Inf", "1P", "1e99T"} {
		if _, err := parseMemoryMiB(value); err == nil {
			t.Errorf("invalid memory accepted: %q", value)
		}
	}
}
