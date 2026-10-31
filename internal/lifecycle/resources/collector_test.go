package resources

import (
	"encoding/json"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

func sample(uuid string, index int, util, used, total float64) DeviceSample {
	return DeviceSample{UUID: &uuid, Index: index, Util: util, MemoryUsedMiB: used, MemoryTotalMiB: total}
}
func node(name string, devices ...DeviceSample) NodeSample {
	return NodeSample{Hostname: name, TimestampMS: 1, Devices: devices}
}
func epoch() time.Time { return time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC) }
func near(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-6 {
		t.Fatalf("got %v want %v", got, want)
	}
}
func reason(u GPUUsage, want string) bool {
	for _, r := range u.MissingReasons {
		if r == want {
			return true
		}
	}
	return false
}
func startCollector(t *testing.T, devices []Device, gpus int64) *Collector {
	t.Helper()
	c := NewCollector(100, time.Second)
	if err := c.Start(Allocation{Key: "job", StartedAt: epoch(), Devices: devices, GPUCount: gpus}); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCollectorWeightsUnevenIntervalsAndUsesUUIDAcrossReordering(t *testing.T) {
	c := startCollector(t, []Device{{"node-a", "GPU-A"}, {"node-a", "GPU-B"}}, 2)
	c.Observe(epoch().Add(time.Second), []NodeSample{node("node-a", sample("GPU-A", 0, 0, 2, 4), sample("GPU-B", 1, 100, 2, 4))})
	// Array order and numerical indices both change. UUID identity must win.
	c.Observe(epoch().Add(3*time.Second), []NodeSample{node("node-a", sample("GPU-B", 0, 100, 1, 4), sample("GPU-A", 1, 80, 1, 4))})
	u := c.Finish("job", epoch().Add(3*time.Second), 3)
	near(t, u.ObservedGPUSeconds, 6)
	near(t, u.AllocatedGPUSeconds, 6)
	near(t, *u.MeanUtilizationPercent, 460.0/6)
	near(t, *u.MeanMemoryUtilizationPercent, 200.0/6)
	if !u.Complete || c.Count() != 0 {
		t.Fatalf("complete execution leaked %+v", u)
	}
}

func TestCollectorGapMissingNodeNullUUIDAndStaleAreNotZeroSamples(t *testing.T) {
	tests := []struct {
		name  string
		nodes []NodeSample
	}{
		{"missing-node", []NodeSample{}},
		{"missing-device", []NodeSample{node("node-a", sample("GPU-OTHER", 0, 99, 1, 4))}},
		{"null-uuid", []NodeSample{node("node-a", DeviceSample{Index: 0, Util: 99, MemoryUsedMiB: 1, MemoryTotalMiB: 4})}},
		{"stale", []NodeSample{{Hostname: "node-a", Stale: true, Devices: []DeviceSample{sample("GPU-A", 0, 99, 1, 4)}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := startCollector(t, []Device{{"node-a", "GPU-A"}}, 1)
			c.Observe(epoch().Add(time.Second), tc.nodes)
			u := c.Finish("job", epoch().Add(time.Second), 1)
			if u.MeanUtilizationPercent != nil || u.Complete || u.ObservedGPUSeconds != 0 || !reason(u, "device_unavailable") {
				t.Fatalf("missing samples turned into values %+v", u)
			}
		})
	}
	c := startCollector(t, []Device{{"node-a", "GPU-A"}}, 1)
	c.Observe(epoch().Add(time.Second), []NodeSample{node("node-a", sample("GPU-A", 0, 50, 1, 4))})
	c.Observe(epoch().Add(6*time.Second), []NodeSample{node("node-a", sample("GPU-A", 0, 0, 1, 4))})
	c.Observe(epoch().Add(7*time.Second), []NodeSample{node("node-a", sample("GPU-A", 0, 100, 3, 4))})
	u := c.Finish("job", epoch().Add(7*time.Second), 7)
	near(t, u.ObservedGPUSeconds, 2)
	near(t, *u.MeanUtilizationPercent, 75)
	if u.Complete || !reason(u, "sampling_gap") || !reason(u, "incomplete_sampling_coverage") {
		t.Fatalf("gap not reflected %+v", u)
	}
}

func TestCollectorTrimsLateEndAndBackdatedSuspend(t *testing.T) {
	c := startCollector(t, []Device{{"node-a", "GPU-A"}}, 1)
	for i, util := range []float64{10, 20, 100} {
		c.Observe(epoch().Add(time.Duration(i+1)*time.Second), []NodeSample{node("node-a", sample("GPU-A", 0, util, 1, 4))})
	}
	u := c.Finish("job", epoch().Add(2*time.Second), 2)
	near(t, *u.MeanUtilizationPercent, 15)
	near(t, u.ObservedGPUSeconds, 2)
	c = startCollector(t, []Device{{"node-a", "GPU-A"}}, 1)
	c.Observe(epoch().Add(time.Second), []NodeSample{node("node-a", sample("GPU-A", 0, 10, 1, 4))})
	c.Observe(epoch().Add(3*time.Second), []NodeSample{node("node-a", sample("GPU-A", 0, 100, 1, 4))})
	c.Pause("job", true, epoch().Add(time.Second))
	c.Observe(epoch().Add(4*time.Second), []NodeSample{node("node-a", sample("GPU-A", 0, 100, 1, 4))})
	c.Pause("job", false, epoch().Add(4*time.Second))
	c.Observe(epoch().Add(5*time.Second), []NodeSample{node("node-a", sample("GPU-A", 0, 50, 1, 4))})
	u = c.Finish("job", epoch().Add(5*time.Second), 2)
	near(t, u.ObservedGPUSeconds, 2)
	near(t, *u.MeanUtilizationPercent, 30)
	if !u.Complete {
		t.Fatalf("valid pause coverage incomplete %+v", u)
	}
}

func TestRepeatedPauseDoesNotMoveSamplingBaselineAndRemoteIntervalApplies(t *testing.T) {
	c := startCollector(t, []Device{{"node-a", "GPU-A"}}, 1)
	c.Pause("job", false, epoch().Add(time.Second))
	c.Observe(epoch().Add(time.Second), []NodeSample{node("node-a", sample("GPU-A", 0, 50, 1, 4))})
	if err := c.SetInterval(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	c.Observe(epoch().Add(31*time.Second), []NodeSample{node("node-a", sample("GPU-A", 0, 50, 1, 4))})
	u := c.Finish("job", epoch().Add(31*time.Second), 31)
	near(t, u.ObservedGPUSeconds, 31)
	if u.Complete == false {
		t.Fatalf("negotiated sample period was ignored %+v", u)
	}
	if c.SetInterval(0) == nil {
		t.Fatal("zero period accepted")
	}
}

func TestCollectorRejectsNonfiniteAndAmbiguousDevices(t *testing.T) {
	for _, bad := range []DeviceSample{sample("GPU-A", 0, math.NaN(), 1, 4), sample("GPU-A", 0, 50, 1, math.NaN()), sample("GPU-A", 0, 50, math.Inf(1), 4), sample("GPU-A", 0, 50, 1, math.Inf(1)), sample("GPU-A", 0, 101, 1, 4)} {
		c := startCollector(t, []Device{{"node-a", "GPU-A"}}, 1)
		c.Observe(epoch().Add(time.Second), []NodeSample{node("node-a", bad)})
		u := c.Finish("job", epoch().Add(time.Second), 1)
		if u.MeanUtilizationPercent != nil || !reason(u, "invalid_sample") {
			t.Fatalf("nonfinite sample accepted %+v", u)
		}
		if _, err := json.Marshal(u); err != nil {
			t.Fatalf("summary contains nonfinite values: %v", err)
		}
	}
	c := startCollector(t, []Device{{"node-a", "GPU-A"}}, 1)
	c.Observe(epoch().Add(time.Second), []NodeSample{node("node-a", sample("GPU-A", 0, 50, 1, 4), sample("GPU-A", 1, 99, 1, 4))})
	u := c.Finish("job", epoch().Add(time.Second), 1)
	if u.ObservedGPUSeconds != 0 || !reason(u, "device_unavailable") {
		t.Fatalf("duplicate identity was arbitrarily selected %+v", u)
	}
	c = startCollector(t, []Device{{"node-a", "GPU-A"}}, 1)
	c.Observe(epoch().Add(time.Second), []NodeSample{node("node-a", sample("GPU-A", 0, 50, 1e308, 1e308))})
	u = c.Finish("job", epoch().Add(time.Second), 1)
	near(t, *u.MeanMemoryUtilizationPercent, 100)
}

func TestCollectorZeroGPUCapacityIdentityAndUnrecoverableOldEnd(t *testing.T) {
	c := NewCollector(1, time.Second)
	a := Allocation{Key: "cpu-only", StartedAt: epoch(), GPUCount: 0}
	if err := c.Start(a); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(a); err != nil {
		t.Fatal("idempotent registration failed", err)
	}
	if err := c.Start(Allocation{Key: "another", StartedAt: epoch()}); err == nil {
		t.Fatal("capacity not enforced")
	}
	if len(c.Nodes()) != 0 {
		t.Fatal("CPU job introduced a GPU query")
	}
	c.Observe(epoch().Add(time.Second), nil)
	u := c.Finish("cpu-only", epoch().Add(time.Second), 1)
	if !u.Complete || u.MeanUtilizationPercent != nil || u.AllocatedGPUSeconds != 0 {
		t.Fatalf("CPU-only stats %+v", u)
	}
	bad := Allocation{Key: "duplicate-uuid", StartedAt: epoch(), GPUCount: 2, Devices: []Device{{"a", "GPU-A"}, {"b", "GPU-A"}}}
	if c.Start(bad) == nil {
		t.Fatal("global UUID duplicates accepted")
	}
	a = Allocation{Key: "long", StartedAt: epoch(), GPUCount: 1, Devices: []Device{{"a", "GPU-A"}}}
	if err := c.Start(a); err != nil {
		t.Fatal(err)
	}
	changed := a
	changed.GPUCount = 2
	if c.Start(changed) == nil {
		t.Fatal("execution allocation silently changed")
	}
	for i := 1; i <= 200; i++ {
		c.Observe(epoch().Add(time.Duration(i)*time.Second), []NodeSample{node("a", sample("GPU-A", 0, 50, 1, 4))})
	}
	u = c.Finish("long", epoch().Add(10*time.Second), 10)
	if u.MeanUtilizationPercent != nil || !reason(u, "end_timestamp_older_than_tail_buffer") {
		t.Fatalf("unverifiable old samples reported as valid %+v", u)
	}
	if c.Finish("unknown", epoch(), 0).Complete {
		t.Fatal("unknown start marked complete")
	}
}

func TestCollectorConcurrentObservationAndLifecycle(t *testing.T) {
	c := NewCollector(100, time.Second)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := strings.Repeat("a", i+1)
			if err := c.Start(Allocation{Key: key, StartedAt: epoch(), GPUCount: 1, Devices: []Device{{"node-a", "GPU-A"}}}); err != nil {
				t.Error(err)
			}
			c.Observe(epoch().Add(time.Second), []NodeSample{node("node-a", sample("GPU-A", 0, 50, 1, 4))})
			c.Nodes()
			c.Count()
			c.Finish(key, epoch().Add(time.Second), 1)
		}(i)
	}
	wg.Wait()
	c.Clear()
	if c.Count() != 0 {
		t.Fatal("clear leaked watches")
	}
}
