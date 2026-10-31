package resourcemonitor

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/httpx"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

const windowDays = 7

var (
	errFilter = apperr.BadRequest("invalid statistics filter")
	errPOSIX  = apperr.New(http.StatusForbidden, "forbidden", "POSIX identity required")
)

type Sums struct {
	JobCount                           int64   `json:"jobCount"`
	DurationSeconds                    int64   `json:"durationSeconds"`
	CPUCoreHours                       float64 `json:"cpuCoreHours"`
	MemoryGiBHours                     float64 `json:"memoryGibHours"`
	GPUHours                           float64 `json:"gpuHours"`
	AllocatedGPUSeconds                float64 `json:"allocatedGpuSeconds"`
	GPUObservedSeconds                 float64 `json:"gpuObservedSeconds"`
	GPUUtilizationPercentSeconds       float64 `json:"gpuUtilizationPercentSeconds"`
	GPUMemoryUtilizationPercentSeconds float64 `json:"gpuMemoryUtilizationPercentSeconds"`
	CompleteJobCount                   int64   `json:"completeJobCount"`
	PartialJobCount                    int64   `json:"partialJobCount"`
}

func (a *Sums) add(b Sums) {
	a.JobCount += b.JobCount
	a.DurationSeconds += b.DurationSeconds
	a.CPUCoreHours += b.CPUCoreHours
	a.MemoryGiBHours += b.MemoryGiBHours
	a.GPUHours += b.GPUHours
	a.AllocatedGPUSeconds += b.AllocatedGPUSeconds
	a.GPUObservedSeconds += b.GPUObservedSeconds
	a.GPUUtilizationPercentSeconds += b.GPUUtilizationPercentSeconds
	a.GPUMemoryUtilizationPercentSeconds += b.GPUMemoryUtilizationPercentSeconds
	a.CompleteJobCount += b.CompleteJobCount
	a.PartialJobCount += b.PartialJobCount
}

func (a *Sums) means() (util, mem *float64) {
	if a.GPUObservedSeconds > 0 {
		u, m := a.GPUUtilizationPercentSeconds/a.GPUObservedSeconds, a.GPUMemoryUtilizationPercentSeconds/a.GPUObservedSeconds
		return &u, &m
	}
	return nil, nil
}

type StateRow struct {
	Date  string `json:"date"`
	State string `json:"state"`
	Sums
}

type Day struct {
	Date                        string     `json:"date"`
	States                      []StateRow `json:"states"`
	GPUUtilizationPercent       *float64   `json:"gpuUtilizationPercent"`
	GPUMemoryUtilizationPercent *float64   `json:"gpuMemoryUtilizationPercent"`
	AllocatedGPUSeconds         float64    `json:"allocatedGpuSeconds"`
	ObservedGPUSeconds          float64    `json:"observedGpuSeconds"`
	CompleteJobCount            int64      `json:"completeJobCount"`
	PartialJobCount             int64      `json:"partialJobCount"`
}

type User struct {
	UID      int64  `json:"uid"`
	Username string `json:"username"`
	Sums
	GPUUtilizationPercent       *float64 `json:"gpuUtilizationPercent"`
	GPUMemoryUtilizationPercent *float64 `json:"gpuMemoryUtilizationPercent"`
}

// Statistics is daily GPU usage over the last 7 days, split by job state.
type Statistics struct {
	Timezone        string   `json:"timezone"`
	Days            []Day    `json:"days"`
	AvailableStates []string `json:"availableStates"`
	SelectedStates  []string `json:"selectedStates"`
}

// GlobalStatistics adds a per-user breakdown for the whole site.
type GlobalStatistics struct {
	Statistics
	Users []User `json:"users"`
}

type row struct {
	StateRow
	UID      int64
	Username *string
}

func selectedStates(r *http.Request) ([]string, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, errFilter
	}
	for k, v := range q {
		if k != "states" || len(v) > 1 {
			return nil, errFilter
		}
	}
	v, ok := q["states"]
	if !ok {
		return slices.Clone(States), nil
	}
	out := []string{}
	if v[0] == "" {
		return out, nil
	}
	for _, s := range strings.Split(v[0], ",") {
		s = normalizeState(s)
		if !slices.Contains(States, s) {
			return nil, errFilter
		}
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out, nil
}

func (s *service) load(ctx context.Context, now time.Time, uid int64) ([]row, error) {
	global := uid == 0
	cols, where, group, args := "", "", "", []any{day(now.Add(-(windowDays - 1) * 24 * time.Hour)), day(now)}
	if global {
		cols = `s.uid, (SELECT u.username FROM users u WHERE u.uid_number = s.uid AND u.deleted_at IS NULL ORDER BY (u.status = 1) DESC, u.id LIMIT 1),`
		group = ", s.uid"
	} else {
		where = " AND s.uid = ?"
		args = append(args, uid)
	}
	rows, err := s.db.Query(ctx, `SELECT s.ended_day, s.state, `+cols+`
		COUNT(*), SUM(s.active_seconds), SUM(s.cpus * s.active_seconds / 3600.0), SUM(s.memory_mib * s.active_seconds / 3686400.0),
		SUM(s.gpus * s.active_seconds / 3600.0), SUM(s.allocated_gpu_seconds),
		SUM(CASE WHEN s.gpus > 0 THEN s.observed_gpu_seconds ELSE 0 END),
		SUM(CASE WHEN s.gpus > 0 THEN s.gpu_utilization_percent_seconds ELSE 0 END),
		SUM(CASE WHEN s.gpus > 0 THEN s.gpu_memory_utilization_percent_seconds ELSE 0 END),
		SUM(CASE WHEN s.complete = 1 THEN 1 ELSE 0 END), SUM(CASE WHEN s.complete = 0 THEN 1 ELSE 0 END)
		FROM v2_resource_summaries s WHERE s.ended_day >= ? AND s.ended_day <= ?`+where+`
		GROUP BY s.ended_day, s.state`+group+` ORDER BY s.ended_day, s.state`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []row
	for rows.Next() {
		var x row
		dest := []any{&x.Date, &x.State}
		if global {
			dest = append(dest, &x.UID, &x.Username)
		}
		m := &x.Sums
		dest = append(dest, &m.JobCount, &m.DurationSeconds, &m.CPUCoreHours, &m.MemoryGiBHours, &m.GPUHours, &m.AllocatedGPUSeconds,
			&m.GPUObservedSeconds, &m.GPUUtilizationPercentSeconds, &m.GPUMemoryUtilizationPercentSeconds, &m.CompleteJobCount, &m.PartialJobCount)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func statistics(rows []row, now time.Time, states []string) Statistics {
	st := Statistics{Timezone: "Asia/Shanghai", Days: make([]Day, windowDays), AvailableStates: slices.Clone(States), SelectedStates: states}
	merged := map[[2]string]*StateRow{}
	for _, x := range rows {
		if !slices.Contains(states, x.State) {
			continue
		}
		k := [2]string{x.Date, x.State}
		if merged[k] == nil {
			merged[k] = &StateRow{Date: x.Date, State: x.State}
		}
		merged[k].add(x.Sums)
	}
	for i := range st.Days {
		d := &st.Days[i]
		d.Date, d.States = day(now.Add(-time.Duration(windowDays-1-i)*24*time.Hour)), []StateRow{}
		var total Sums
		for _, state := range States {
			if m := merged[[2]string{d.Date, state}]; m != nil {
				d.States = append(d.States, *m)
				total.add(m.Sums)
			}
		}
		d.GPUUtilizationPercent, d.GPUMemoryUtilizationPercent = total.means()
		d.AllocatedGPUSeconds, d.ObservedGPUSeconds = total.AllocatedGPUSeconds, total.GPUObservedSeconds
		d.CompleteJobCount, d.PartialJobCount = total.CompleteJobCount, total.PartialJobCount
	}
	return st
}

func users(rows []row, states []string) []User {
	byUID := map[int64]*User{}
	for _, x := range rows {
		if !slices.Contains(states, x.State) {
			continue
		}
		u := byUID[x.UID]
		if u == nil {
			u = &User{UID: x.UID}
			if x.Username != nil {
				u.Username = *x.Username
			}
			byUID[x.UID] = u
		}
		u.add(x.Sums)
	}
	out := make([]User, 0, len(byUID))
	for _, u := range byUID {
		u.GPUUtilizationPercent, u.GPUMemoryUtilizationPercent = u.means()
		out = append(out, *u)
	}
	slices.SortFunc(out, func(a, b User) int { return cmp.Compare(a.UID, b.UID) })
	return out
}

func (s *service) stats(w http.ResponseWriter, r *http.Request) error {
	states, err := selectedStates(r)
	if err != nil {
		return err
	}
	now := s.now()
	rows, err := s.load(r.Context(), now, 0)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, GlobalStatistics{statistics(rows, now, states), users(rows, states)})
	return nil
}

func (s *service) myStats(w http.ResponseWriter, r *http.Request) error {
	states, err := selectedStates(r)
	if err != nil {
		return err
	}
	p := authn.FromContext(r.Context())
	if p == nil || p.UserID < 1 {
		return apperr.ErrUnauthenticated
	}
	var uid int64
	err = store.QueryRow(r.Context(), s.db, "SELECT COALESCE(uid_number, 0) FROM users WHERE id = ? AND deleted_at IS NULL", []any{p.UserID}, &uid)
	if errors.Is(err, store.ErrNoRows) || err == nil && (uid < 1 || uid > 4294967295) {
		return errPOSIX
	}
	if err != nil {
		return err
	}
	now := s.now()
	rows, err := s.load(r.Context(), now, uid)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, statistics(rows, now, states))
	return nil
}
