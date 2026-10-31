package jobnotify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// Normalize only accepts the same execution in a stable terminal state. Its
// input is one complete GET /slurm/v0.0.45/job/{numeric_id} response. Raw retains
// the actual selected job, wrappers, and controller response metadata.
func Normalize(cluster string, callback Callback, body []byte, observed time.Time, maxBytes int) (Event, error) {
	return normalize(cluster, callback, body, observed, maxBytes, MinActiveSeconds)
}

// NormalizeLifecycle verifies all stable completions, including short jobs.
// Notification thresholds belong to the job-done-email plugin, not the bus.
func NormalizeLifecycle(cluster string, callback Callback, body []byte, observed time.Time, maxBytes int) (Event, error) {
	return normalize(cluster, callback, body, observed, maxBytes, -1)
}

func normalize(cluster string, callback Callback, body []byte, observed time.Time, maxBytes int, minimum int64) (Event, error) {
	var response struct {
		Jobs     []json.RawMessage `json:"jobs"`
		Errors   []json.RawMessage `json:"errors"`
		Meta     json.RawMessage   `json:"meta"`
		Warnings json.RawMessage   `json:"warnings"`
	}
	if !clusterPattern.MatchString(cluster) || callback.Validate() != nil || json.Unmarshal(body, &response) != nil || len(response.Errors) > 0 {
		return Event{}, ErrRetry
	}
	callback.SLUID = canonicalSLUID(callback.SLUID)
	var selected map[string]json.RawMessage
	var selectedRaw json.RawMessage
	for _, raw := range response.Jobs {
		var row map[string]json.RawMessage
		if json.Unmarshal(raw, &row) != nil {
			return Event{}, ErrRetry
		}
		id, ok := number(row["job_id"])
		if !ok || id != int64(callback.JobID) {
			continue
		}
		if selected != nil {
			return Event{}, ErrRetry
		} // Never choose an ambiguous array record.
		selected, selectedRaw = row, raw
	}
	if selected == nil {
		return Event{}, ErrRetry
	}
	var step struct {
		SLUID string `json:"sluid"`
	}
	if json.Unmarshal(selected["step_id"], &step) != nil || step.SLUID == "" {
		return Event{}, ErrRetry
	}
	if canonicalSLUID(step.SLUID) != callback.SLUID {
		return Event{}, ErrSuperseded
	}
	var states []string
	if json.Unmarshal(selected["job_state"], &states) != nil || len(states) == 0 {
		return Event{}, ErrRetry
	}
	terminal := map[string]bool{"COMPLETED": true, "CANCELLED": true, "FAILED": true, "TIMEOUT": true, "NODE_FAIL": true, "PREEMPTED": true, "BOOT_FAIL": true, "DEADLINE": true, "OUT_OF_MEMORY": true}
	state := ""
	for _, flag := range states {
		switch flag {
		case "PENDING", "RUNNING", "SUSPENDED":
			return Event{}, ErrSuperseded
		case "COMPLETING", "CONFIGURING", "REQUEUE", "REQUEUE_HOLD", "REQUEUE_FED", "RESIZING", "SPECIAL_EXIT", "STAGE_OUT":
			return Event{}, ErrRetry
		}
		if terminal[flag] {
			if state != "" {
				return Event{}, ErrRetry
			}
			state = flag
		} else {
			// An unrecognized transitional/federation flag is not proof that
			// this execution has finally ended. Fail closed until understood.
			return Event{}, ErrRetry
		}
	}
	if state == "" {
		return Event{}, ErrRetry
	}
	start, startOK := number(selected["start_time"])
	end, endOK := number(selected["end_time"])
	suspend, suspendOK := number(selected["suspend_time"])
	pre, preOK := number(selected["pre_sus_time"])
	uid, uidOK := number(selected["user_id"])
	if !startOK || !endOK || !suspendOK || !preOK || !uidOK || uid <= 0 || uid > 4294967295 {
		return Event{}, ErrRetry
	}
	runtime, runtimeOK := activeRuntimeSeconds(start, end, suspend, pre)
	if !runtimeOK {
		return Event{}, ErrRetry
	}
	if runtime <= minimum {
		return Event{}, ErrFiltered
	}
	var name string
	_ = json.Unmarshal(selected["name"], &name)
	event := Event{Version: 1, EventID: cluster + "/" + callback.SLUID + "/ended", ClusterID: cluster,
		Job:        Job{JobID: callback.JobID, SLUID: callback.SLUID, UID: uint32(uid), Name: name},
		Result:     Result{State: state, ExitCode: selected["exit_code"]},
		Duration:   Duration{ActiveSeconds: runtime, Scope: "last_attempt", ExcludesSuspend: true},
		ObservedAt: observed.UTC()}
	if id, ok := number(selected["array_job_id"]); ok && id > 0 {
		event.Job.Array = map[string]any{"job_id": jsonValue(selected["array_job_id"]), "task_id": jsonValue(selected["array_task_id"])}
	}
	if id, ok := number(selected["het_job_id"]); ok && id > 0 {
		event.Job.Heterogeneous = map[string]any{"job_id": jsonValue(selected["het_job_id"]), "offset": jsonValue(selected["het_job_offset"])}
	}
	projectJobMetadata(&event.Job, selected)
	raw, err := json.Marshal(map[string]json.RawMessage{"job": selectedRaw, "meta": nullable(response.Meta), "warnings": nullable(response.Warnings)})
	if err != nil {
		return Event{}, ErrRetry
	}
	event.Raw = Raw{Controller: raw, Complete: true}
	event, _, err = encodeBoundedEvent(event, maxBytes)
	return event, err
}

func encodeBoundedEvent(event Event, maxBytes int) (Event, []byte, error) {
	encoded, err := json.Marshal(event)
	if err != nil {
		return Event{}, nil, ErrRetry
	}
	if maxBytes > 0 && len(encoded) > maxBytes && len(event.Raw.InitialController) > 0 {
		event.Raw.InitialController = nil
		event.Raw.Complete = false
		event.Raw.OmittedReason = "initial snapshot exceeds notification size limit"
		encoded, _ = json.Marshal(event)
	}
	if maxBytes > 0 && len(encoded) > maxBytes {
		event.Raw.Controller = json.RawMessage(`null`)
		event.Raw.Complete = false
		event.Raw.OmittedReason = "controller metadata exceeds notification size limit"
		encoded, _ = json.Marshal(event)
		if len(encoded) > maxBytes {
			trimJobMetadata(&event.Job, func() bool {
				encoded, _ = json.Marshal(event)
				return len(encoded) <= maxBytes
			})
			encoded, _ = json.Marshal(event)
			if len(encoded) > maxBytes {
				// At very small configured budgets even the omission-name list
				// may not fit. Preserve the existing core notification and an
				// explicit incomplete marker rather than dropping it for names.
				event.Job.MetadataOmitted = nil
				complete := false
				event.Job.MetadataComplete = &complete
				event.Raw.OmittedReason = "metadata size limit"
				encoded, _ = json.Marshal(event)
			}
			if len(encoded) > maxBytes {
				return Event{}, nil, fmt.Errorf("%w: normalized event too large", ErrFiltered)
			}
		}
	}
	return event, encoded, nil
}

func nullable(v json.RawMessage) json.RawMessage {
	if len(v) == 0 {
		return json.RawMessage(`null`)
	}
	return v
}
func jsonValue(v json.RawMessage) any { var x any; _ = json.Unmarshal(v, &x); return x }

// Slurm NO_VAL numbers are {set,infinite,number}; never coerce unset or
// infinity to zero. Timestamps and some IDs are plain numbers in v0.0.45.
func number(raw json.RawMessage) (int64, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return 0, false
	}
	if raw[0] != '{' {
		return jsonInteger(raw)
	}
	var wrapped struct {
		Set      bool            `json:"set"`
		Infinite bool            `json:"infinite"`
		Number   json.RawMessage `json:"number"`
	}
	if json.Unmarshal(raw, &wrapped) != nil || !wrapped.Set || wrapped.Infinite {
		return 0, false
	}
	return jsonInteger(wrapped.Number)
}

func jsonInteger(raw json.RawMessage) (int64, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] == '"' {
		return 0, false
	}
	var n json.Number
	if json.Unmarshal(raw, &n) != nil {
		return 0, false
	}
	i, err := strconv.ParseInt(n.String(), 10, 64)
	return i, err == nil
}
