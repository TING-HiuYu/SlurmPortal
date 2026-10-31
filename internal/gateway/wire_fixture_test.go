package gateway

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// This fixture uses the producer's real Transfer JSON tags. The master's JS
// contract tests consume the generated JSON instead of maintaining a second,
// independently assembled event shape. Regenerate only by explicit opt-in:
//
//	WEB_ACCESS_FIXTURE_OUTPUT=/absolute/path/to/webaccessgateway/testdata/completion-wire.json \
//	  go test ./webaccessgateway -run '^TestCompletionWireFixture$'
func TestCompletionWireFixture(t *testing.T) {
	fixture := completionWireFixture()
	raw, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	for _, hidden := range []string{"internal-session-must-not-leak", "internal-callback-hash-must-not-leak", "internal-runner-directory-must-not-leak"} {
		if bytes.Contains(raw, []byte(hidden)) {
			t.Fatalf("internal transfer value was serialized: %s", hidden)
		}
	}
	for index, transfer := range fixture.Events {
		// Exercise Marshal on the production type independently of the outer
		// test envelope, matching the actual completion outbox producer.
		event, err := json.Marshal(transfer)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err = json.Unmarshal(event, &fields); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"UserID", "userID", "session_id", "remote_directory", "callback_hash", "callback_expires", "delivered"} {
			if _, found := fields[field]; found {
				t.Fatalf("event %d exposed internal field %s", index, field)
			}
		}
		if string(fields["user_id"]) != "7" {
			t.Fatalf("event %d user_id is not the numeric database ID: %s", index, fields["user_id"])
		}
		if index > 0 && string(fields["average_mb_per_second"]) != "null" {
			t.Fatalf("unknown event %d speed must remain null: %s", index, event)
		}
		if index == 2 {
			if string(fields["bytes_sent"]) != "null" || string(fields["error"]) != `"runner_unsupported"` {
				t.Fatalf("startup failure lost its unknown statistics or stable error: %s", event)
			}
		} else if _, found := fields["error"]; found {
			t.Fatalf("successful/partial event unexpectedly has an error: %s", event)
		}
	}
	if output := os.Getenv("WEB_ACCESS_FIXTURE_OUTPUT"); output != "" {
		if err = os.MkdirAll(filepath.Dir(output), 0755); err == nil {
			err = os.WriteFile(output, raw, 0644)
		}
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	tracked, err := os.ReadFile(filepath.Join("testdata", "completion-wire.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, tracked) {
		t.Fatal("completion wire fixture is stale; regenerate with WEB_ACCESS_FIXTURE_OUTPUT")
	}
}

func completionWireFixture() struct {
	Binding struct {
		GatewayID string `json:"gateway_id"`
		ClusterID string `json:"cluster_id"`
	} `json:"binding"`
	NowMS  int64      `json:"now_ms"`
	Events []Transfer `json:"events"`
} {
	var fixture struct {
		Binding struct {
			GatewayID string `json:"gateway_id"`
			ClusterID string `json:"cluster_id"`
		} `json:"binding"`
		NowMS  int64      `json:"now_ms"`
		Events []Transfer `json:"events"`
	}
	fixture.Binding.GatewayID = "fixture-gateway"
	fixture.Binding.ClusterID = "source"
	fixture.NowMS = time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC).UnixMilli()
	base := Transfer{Version: 1, GatewayID: fixture.Binding.GatewayID, ClusterID: fixture.Binding.ClusterID, UserID: "7", NumericUserID: 7, Username: "user", DestinationClusterID: "destination", SourcePaths: []string{"/home/user/data", "/home/user/batch one.bin"}, DestinationPath: "/home/user/incoming", SessionID: "internal-session-must-not-leak", RemoteDirectory: "internal-runner-directory-must-not-leak", CallbackHash: "internal-callback-hash-must-not-leak", CallbackExpires: time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC), Delivered: true}
	completed := base
	completed.EventID = "00000000-0000-4000-8000-000000000101"
	completed.TransferID = completed.EventID
	completed.Status = "completed"
	completed.ExitCode = completionWirePointer(0)
	completed.StartedAt = time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)
	completed.FinishedAt = completionWirePointer(completed.StartedAt.Add(time.Minute))
	completed.BytesSent = completionWirePointer(int64(120000000))
	completed.BytesReceived = completionWirePointer(int64(0))
	completed.TotalBytes = completionWirePointer(int64(120000000))
	completed.ElapsedSeconds = completionWirePointer(float64(60))
	completed.AverageMBPerSecond = completionWirePointer(float64(2))
	partial := base
	partial.EventID = "00000000-0000-4000-8000-000000000102"
	partial.TransferID = partial.EventID
	partial.Status = "partial"
	partial.ExitCode = completionWirePointer(23)
	partial.StartedAt = time.Date(2026, 10, 27, 8, 2, 0, 0, time.UTC)
	partial.FinishedAt = completionWirePointer(partial.StartedAt.Add(30 * time.Second))
	partial.BytesSent = completionWirePointer(int64(6000000))
	partial.TotalBytes = completionWirePointer(int64(12000000))
	partial.ElapsedSeconds = completionWirePointer(float64(30))
	failed := base
	failed.EventID = "00000000-0000-4000-8000-000000000103"
	failed.TransferID = failed.EventID
	failed.Status = "failed"
	failed.ExitCode = completionWirePointer(125)
	failed.StartedAt = time.Date(2026, 10, 27, 8, 3, 0, 123456789, time.UTC)
	failed.FinishedAt = completionWirePointer(failed.StartedAt.Add(125 * time.Millisecond))
	failed.ElapsedSeconds = completionWirePointer(0.125)
	failed.Error = completionWirePointer("runner_unsupported")
	fixture.Events = []Transfer{completed, partial, failed}
	return fixture
}

func completionWirePointer[T any](value T) *T { return &value }
