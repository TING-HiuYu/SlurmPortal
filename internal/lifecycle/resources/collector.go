package resources

import (
	"errors"
	"math"
	"sort"
	"sync"
	"time"
)

type totals struct{ seconds, gpu, memory float64 }

func (a totals) add(b totals) totals {
	return totals{a.seconds + b.seconds, a.gpu + b.gpu, a.memory + b.memory}
}
func (a totals) scale(f float64) totals { return totals{a.seconds * f, a.gpu * f, a.memory * f} }

type segment struct {
	start, end time.Time
	sum        totals
}
type watch struct {
	allocation     Allocation
	last           time.Time
	committed      totals
	committedUntil time.Time
	tail           []segment
	reasons        map[string]bool
	paused         bool
}

// Collector keeps cumulative counters plus a bounded tail. The tail allows the
// final Slurm end timestamp to trim samples received after the execution ended.
type Collector struct {
	mu       sync.Mutex
	jobs     map[string]*watch
	maxJobs  int
	interval time.Duration
}

// NewCollector tracks at most maxJobs jobs, sampling every interval.
func NewCollector(maxJobs int, interval time.Duration) *Collector {
	if maxJobs < 1 {
		maxJobs = 1000
	}
	if interval <= 0 {
		interval = time.Second
	}
	return &Collector{jobs: map[string]*watch{}, maxJobs: maxJobs, interval: interval}
}

func (c *Collector) SetInterval(interval time.Duration) error {
	if interval <= 0 || interval > 5*time.Minute {
		return errors.New("invalid resource sample interval")
	}
	c.mu.Lock()
	c.interval = interval
	c.mu.Unlock()
	return nil
}

func (c *Collector) Remove(key string) {
	c.mu.Lock()
	delete(c.jobs, key)
	c.mu.Unlock()
}

func (c *Collector) Clear() {
	c.mu.Lock()
	c.jobs = map[string]*watch{}
	c.mu.Unlock()
}

// Start begins tracking a job's allocation.
func (c *Collector) Start(a Allocation) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if a.Key == "" || len(a.Key) > 512 || a.StartedAt.IsZero() || a.GPUCount < 0 || a.GPUCount > 4096 || len(a.Devices) > 4096 || int64(len(a.Devices)) > a.GPUCount {
		return errors.New("invalid resource allocation")
	}
	seen := map[Device]bool{}
	seenUUID := map[string]bool{}
	for _, d := range a.Devices {
		if d.Node == "" || len(d.Node) > 255 || d.UUID == "" || len(d.UUID) > 256 || seen[d] || seenUUID[d.UUID] {
			return errors.New("invalid GPU UUID binding")
		}
		seen[d] = true
		seenUUID[d.UUID] = true
	}
	if old := c.jobs[a.Key]; old != nil {
		if !old.allocation.StartedAt.Equal(a.StartedAt) || old.allocation.GPUCount != a.GPUCount || len(old.allocation.Devices) != len(a.Devices) {
			return errors.New("resource allocation identity conflict")
		}
		for _, device := range old.allocation.Devices {
			if !seen[device] {
				return errors.New("resource allocation identity conflict")
			}
		}
		return nil
	}
	if len(c.jobs) >= c.maxJobs {
		return errors.New("resource monitor capacity reached")
	}
	a.Devices = append([]Device(nil), a.Devices...)
	c.jobs[a.Key] = &watch{allocation: a, last: a.StartedAt, reasons: map[string]bool{}}
	return nil
}
func (c *Collector) Pause(key string, paused bool, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if w := c.jobs[key]; w != nil {
		if w.paused == paused {
			return
		}
		if at.IsZero() || at.Before(w.allocation.StartedAt) {
			w.reasons["invalid_pause_transition"] = true
			return
		}
		if paused {
			// A delayed Slurm suspend timestamp may precede our last query. Trim
			// the bounded tail so that the suspended interval is not counted twice.
			w.trimAfter(at)
		}
		w.paused = paused
		w.last = at
	}
}

func (w *watch) trimAfter(at time.Time) {
	if !w.committedUntil.IsZero() && at.Before(w.committedUntil) {
		w.committed = totals{}
		w.committedUntil = time.Time{}
		w.reasons["transition_older_than_tail_buffer"] = true
	}
	out := w.tail[:0]
	for _, s := range w.tail {
		if !at.After(s.start) {
			break
		}
		if at.Before(s.end) {
			s.sum = s.sum.scale(at.Sub(s.start).Seconds() / s.end.Sub(s.start).Seconds())
			s.end = at
		}
		out = append(out, s)
	}
	w.tail = out
}
func (c *Collector) Nodes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	set := map[string]bool{}
	for _, w := range c.jobs {
		for _, d := range w.allocation.Devices {
			set[d.Node] = true
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
func (c *Collector) Count() int   { c.mu.Lock(); defer c.mu.Unlock(); return len(c.jobs) }
func validPercent(n float64) bool { return !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 && n <= 100 }

// Observe adds one round of node samples to every tracked job on those nodes.
func (c *Collector) Observe(at time.Time, nodes []NodeSample) {
	c.mu.Lock()
	defer c.mu.Unlock()
	devices := map[Device]DeviceSample{}
	invalid := map[string]bool{}
	nodeSeen := map[string]bool{}
	uuidNode := map[string]string{}
	for _, n := range nodes {
		if nodeSeen[n.Hostname] || n.Hostname == "" {
			invalid[n.Hostname] = true
		}
		nodeSeen[n.Hostname] = true
		if n.Stale || (n.Error != nil && *n.Error != "") {
			invalid[n.Hostname] = true
			continue
		}
		for _, d := range n.Devices {
			if d.UUID == nil || *d.UUID == "" {
				continue
			}
			key := Device{n.Hostname, *d.UUID}
			if node, exists := uuidNode[*d.UUID]; exists && node != n.Hostname {
				invalid[node], invalid[n.Hostname] = true, true
			}
			uuidNode[*d.UUID] = n.Hostname
			if _, exists := devices[key]; exists {
				invalid[n.Hostname] = true
			}
			devices[key] = d
		}
	}
	for _, w := range c.jobs {
		start := w.last
		if !at.After(start) {
			continue
		}
		w.last = at
		if w.paused || w.allocation.GPUCount == 0 {
			continue
		}
		// First observation cannot stand in for an arbitrarily long initial gap.
		if at.Sub(start) > 3*c.interval {
			w.reasons["sampling_gap"] = true
			continue
		}
		var sum totals
		for _, ref := range w.allocation.Devices {
			d, ok := devices[ref]
			if !ok || invalid[ref.Node] {
				w.reasons["device_unavailable"] = true
				continue
			}
			if !validPercent(d.Util) || d.MemoryTotalMiB <= 0 || d.MemoryUsedMiB < 0 || d.MemoryUsedMiB > d.MemoryTotalMiB || math.IsNaN(d.MemoryUsedMiB) || math.IsInf(d.MemoryUsedMiB, 0) || math.IsNaN(d.MemoryTotalMiB) || math.IsInf(d.MemoryTotalMiB, 0) {
				w.reasons["invalid_sample"] = true
				continue
			}
			seconds := at.Sub(start).Seconds()
			sum = sum.add(totals{seconds, d.Util * seconds, 100 * (d.MemoryUsedMiB / d.MemoryTotalMiB) * seconds})
		}
		if int64(len(w.allocation.Devices)) != w.allocation.GPUCount {
			w.reasons["gpu_mapping_incomplete"] = true
		}
		w.tail = append(w.tail, segment{start, at, sum})
		if len(w.tail) > 128 {
			s := w.tail[0]
			w.committed = w.committed.add(s.sum)
			w.committedUntil = s.end
			copy(w.tail, w.tail[1:])
			w.tail = w.tail[:len(w.tail)-1]
		}
	}
}

// Finish stops tracking a job and returns its GPU usage up to the end time.
func (c *Collector) Finish(key string, ended time.Time, activeSeconds int64) GPUUsage {
	return c.summarize(key, ended, activeSeconds, true)
}

// Preview prepares a summary without consuming the watch. The host can first
// admit the immutable report into its bounded queue, then Remove the watch.
func (c *Collector) Preview(key string, ended time.Time, activeSeconds int64) GPUUsage {
	return c.summarize(key, ended, activeSeconds, false)
}

func (c *Collector) summarize(key string, ended time.Time, activeSeconds int64, remove bool) GPUUsage {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.jobs[key]
	if w == nil {
		return GPUUsage{Complete: false, MissingReasons: []string{"start_event_or_allocation_missing"}}
	}
	if remove {
		delete(c.jobs, key)
	}
	u := GPUUsage{AllocatedGPUSeconds: float64(w.allocation.GPUCount) * float64(activeSeconds), MissingReasons: []string{}}
	if int64(len(w.allocation.Devices)) != w.allocation.GPUCount {
		w.reasons["gpu_mapping_incomplete"] = true
	}
	if activeSeconds < 0 {
		u.AllocatedGPUSeconds = 0
		w.reasons["invalid_active_duration"] = true
	}
	if ended.IsZero() || ended.Before(w.allocation.StartedAt) {
		w.reasons["invalid_execution_window"] = true
		ended = w.allocation.StartedAt
	}
	sum := w.committed
	if !w.committedUntil.IsZero() && ended.Before(w.committedUntil) {
		w.reasons["end_timestamp_older_than_tail_buffer"] = true
		// We cannot reconstruct the old window. Report no unverifiable mean
		// rather than including another execution's post-end samples.
		sum = totals{}
	}
	for _, s := range w.tail {
		if !ended.After(s.start) {
			break
		}
		if !ended.Before(s.end) {
			sum = sum.add(s.sum)
		} else {
			sum = sum.add(s.sum.scale(ended.Sub(s.start).Seconds() / s.end.Sub(s.start).Seconds()))
		}
	}
	if sum.seconds > u.AllocatedGPUSeconds {
		sum = sum.scale(u.AllocatedGPUSeconds / sum.seconds)
		w.reasons["active_window_shorter_than_samples"] = true
	}
	u.ObservedGPUSeconds = sum.seconds
	u.UtilizationPercentSeconds = sum.gpu
	u.MemoryUtilizationPercentSeconds = sum.memory
	if sum.seconds > 0 {
		a, b := sum.gpu/sum.seconds, sum.memory/sum.seconds
		u.MeanUtilizationPercent = &a
		u.MeanMemoryUtilizationPercent = &b
	}
	for reason := range w.reasons {
		u.MissingReasons = append(u.MissingReasons, reason)
	}
	if w.allocation.GPUCount > 0 && u.ObservedGPUSeconds+1e-6 < u.AllocatedGPUSeconds {
		u.MissingReasons = append(u.MissingReasons, "incomplete_sampling_coverage")
	}
	sort.Strings(u.MissingReasons)
	u.Complete = len(u.MissingReasons) == 0
	return u
}
