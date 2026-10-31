package joblifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/lifecycle/jobnotify"
	"github.com/TING-HiuYu/SlurmPortal/internal/lifecycle/resources"
)

func number(v json.RawMessage) (int64, bool) {
	return jobnotify.SlurmNumber(v)
}
func text(v json.RawMessage) string { var s string; _ = json.Unmarshal(v, &s); return s }

func (r *Runtime) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(r.cfg.SlurmURL, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	response, err := r.reader.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != 200 {
		return nil, errors.New("slurm observer unavailable")
	}
	b, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil || len(b) > 4<<20 {
		return nil, errors.New("slurm observer response exceeds budget")
	}
	return b, nil
}
func selectJob(body []byte, callback jobnotify.Callback) (map[string]json.RawMessage, error) {
	var d struct {
		Jobs   []map[string]json.RawMessage `json:"jobs"`
		Errors []json.RawMessage            `json:"errors"`
	}
	if json.Unmarshal(body, &d) != nil || len(d.Errors) > 0 {
		return nil, jobnotify.ErrRetry
	}
	var row map[string]json.RawMessage
	for _, j := range d.Jobs {
		if n, ok := number(j["job_id"]); ok && n == int64(callback.JobID) {
			if row != nil {
				return nil, jobnotify.ErrRetry
			}
			row = j
		}
	}
	if row == nil {
		return nil, jobnotify.ErrRetry
	}
	var step struct {
		SLUID string `json:"sluid"`
	}
	if json.Unmarshal(row["step_id"], &step) != nil || jobnotify.CanonicalSLUID(step.SLUID) != jobnotify.CanonicalSLUID(callback.SLUID) {
		return nil, jobnotify.ErrSuperseded
	}
	return row, nil
}

func expandIndices(s string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(s, ",") {
		if part == "" {
			continue
		}
		lo, hi := part, part
		if p := strings.Split(part, "-"); len(p) == 2 {
			lo, hi = p[0], p[1]
		} else if len(p) > 2 {
			return nil, errors.New("invalid GPU index range")
		}
		a, e := strconv.Atoi(lo)
		b, f := strconv.Atoi(hi)
		if e != nil || f != nil || a < 0 || b < a || b > 4095 || len(out)+b-a+1 > 4096 {
			return nil, errors.New("invalid GPU index range")
		}
		for i := a; i <= b; i++ {
			out = append(out, i)
		}
	}
	return out, nil
}

func (r *Runtime) allocation(ctx context.Context, callback jobnotify.Callback, row map[string]json.RawMessage) (resources.Allocation, error) {
	start, ok := number(row["start_time"])
	if !ok || start <= 0 {
		return resources.Allocation{}, jobnotify.ErrRetry
	}
	a := resources.Allocation{Key: r.key(callback), StartedAt: time.Unix(start, 0).UTC(), GPUCount: gpuCount(text(row["tres_alloc_str"]))}
	if a.GPUCount == 0 {
		return a, nil
	}
	body, err := r.get(ctx, "/slurm/"+jobnotify.SlurmAPIVersion+"/resources/"+strconv.FormatUint(uint64(callback.JobID), 10))
	if err != nil {
		return a, err
	}
	var layout struct {
		Nodes []struct {
			Node string `json:"node"`
			GRES []struct {
				Name  string `json:"name"`
				Index string `json:"index"`
			} `json:"gres"`
		} `json:"nodes"`
		Errors []json.RawMessage `json:"errors"`
	}
	if json.Unmarshal(body, &layout) != nil || len(layout.Errors) > 0 || len(layout.Nodes) > 4096 {
		return a, jobnotify.ErrRetry
	}
	names := make([]string, 0, len(layout.Nodes))
	for _, n := range layout.Nodes {
		names = append(names, n.Node)
	}
	samples, err := r.backend.Read(ctx, names)
	if err != nil {
		return a, err
	}
	byNode := map[string]map[int]string{}
	for _, n := range samples {
		if n.Stale || (n.Error != nil && *n.Error != "") {
			continue
		}
		if _, exists := byNode[n.Hostname]; exists {
			return a, errors.New("ambiguous clustat node identity")
		}
		byNode[n.Hostname] = map[int]string{}
		uuids := map[string]bool{}
		for _, d := range n.Devices {
			if d.UUID != nil && *d.UUID != "" {
				if _, exists := byNode[n.Hostname][d.Index]; exists || uuids[*d.UUID] {
					return a, errors.New("ambiguous clustat GPU identity")
				}
				uuids[*d.UUID] = true
				byNode[n.Hostname][d.Index] = *d.UUID
			}
		}
	}
	for _, n := range layout.Nodes {
		for _, g := range n.GRES {
			if g.Name != "gpu" {
				continue
			}
			indices, e := expandIndices(g.Index)
			if e != nil {
				return a, e
			}
			for _, index := range indices {
				uuid := r.cfg.GPUUUIDMapping[n.Node][strconv.Itoa(index)]
				if uuid == "" && r.cfg.GRESIndicesAreNVML {
					uuid = byNode[n.Node][index]
				}
				if uuid == "" {
					return a, errors.New("GPU allocation UUID mapping is unavailable")
				}
				a.Devices = append(a.Devices, resources.Device{Node: n.Node, UUID: uuid})
			}
		}
	}
	if int64(len(a.Devices)) != a.GPUCount {
		return a, errors.New("GPU allocation device count mismatch")
	}
	return a, nil
}

func gpuCount(tres string) int64 {
	var total int64
	for _, part := range strings.Split(tres, ",") {
		k, v, ok := strings.Cut(part, "=")
		if ok && (k == "gres/gpu" || strings.HasPrefix(k, "gres/gpu:")) {
			n, e := strconv.ParseInt(v, 10, 64)
			if e == nil && n >= 0 && n <= 4096 {
				if k == "gres/gpu" {
					return n
				}
				total += n
			}
		}
	}
	return total
}
func summaryFor(report jobnotify.Event, usage resources.GPUUsage) resources.Summary {
	s := resources.Summary{Version: 1, EventID: report.ClusterID + "/" + report.Job.SLUID + "/resources", ClusterID: report.ClusterID, Usage: resources.Usage{GPU: usage}}
	s.Job = resources.Job{JobID: report.Job.JobID, SLUID: report.Job.SLUID, UID: report.Job.UID, Name: report.Job.Name}
	if report.Job.CPUs != nil {
		s.Job.CPUs = *report.Job.CPUs
	}
	if report.Job.AllocatedTRES != nil {
		s.Job.GPUs = gpuCount(*report.Job.AllocatedTRES)
	}
	if report.Job.MemoryPerNodeMiB != nil {
		s.Job.MemoryMiB = *report.Job.MemoryPerNodeMiB
		if report.Job.NodeCount != nil {
			s.Job.MemoryMiB *= *report.Job.NodeCount
		}
	} else if report.Job.MemoryPerCPUMiB != nil {
		s.Job.MemoryMiB = *report.Job.MemoryPerCPUMiB * s.Job.CPUs
	}
	// TRES memory is allocation-wide and preferred when present.
	if report.Job.AllocatedTRES != nil {
		for _, p := range strings.Split(*report.Job.AllocatedTRES, ",") {
			if strings.HasPrefix(p, "mem=") {
				if n, e := parseMemoryMiB(strings.TrimPrefix(p, "mem=")); e == nil {
					s.Job.MemoryMiB = n
				}
			}
		}
	}
	if report.Job.Nodes != nil {
		s.Job.Nodes = []string{*report.Job.Nodes}
	} else {
		s.Job.Nodes = []string{}
	}
	s.Result.State = report.Result.State
	s.Duration.ActiveSeconds = report.Duration.ActiveSeconds
	s.Duration.Scope = "last_attempt"
	if report.Job.EndTime != nil {
		s.EndedAt = time.Unix(*report.Job.EndTime, 0).UTC()
	} else {
		s.EndedAt = report.ObservedAt
	}
	if s.Usage.GPU.AllocatedGPUSeconds == 0 {
		s.Usage.GPU.AllocatedGPUSeconds = float64(s.Job.GPUs) * float64(s.Duration.ActiveSeconds)
	}
	return s
}
func parseMemoryMiB(value string) (int64, error) {
	if value == "" {
		return 0, errors.New("missing allocation memory")
	}
	factor := float64(1)
	last := value[len(value)-1]
	if last < '0' || last > '9' {
		value = value[:len(value)-1]
		switch last {
		case 'K', 'k':
			factor = 1.0 / 1024
		case 'M', 'm':
		case 'G', 'g':
			factor = 1024
		case 'T', 't':
			factor = 1024 * 1024
		default:
			return 0, fmt.Errorf("unsupported memory unit")
		}
	}
	n, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n*factor > 1<<50 {
		return 0, errors.New("invalid allocation memory")
	}
	return int64(n * factor), nil
}
