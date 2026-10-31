package slave

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/TING-HiuYu/SlurmPortal/internal/replication"
)

var triggerEvents = map[string]bool{
	"user.created": true, "user.updated": true, "user.deleted": true,
	"group.created": true, "group.updated": true, "group.deleted": true,
	"membership.added": true, "membership.removed": true,
	"ca.created": true, "ca.updated": true, "ca.deleted": true,
}

// TriggerRule runs a local command for selected directory events (e.g. create the Slurm account of a
// new user).
type TriggerRule struct {
	ID              string   `yaml:"id"`
	Enabled         bool     `yaml:"enabled"`
	Events          []string `yaml:"events"`
	Command         []string `yaml:"command"`
	TimeoutSeconds  int      `yaml:"timeoutSeconds"`
	CurrentUserOnly bool     `yaml:"currentUserOnly"`
}

func isUserEvent(e string) bool {
	return strings.HasPrefix(e, "user.") || strings.HasPrefix(e, "membership.")
}

// LoadTriggers reads the local trigger file (v1 format). It must be a 0600 regular file; shells are refused.
func LoadTriggers(path string) ([]TriggerRule, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("read trigger file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("trigger file %s must be a regular file with permissions 0600 or stricter", path)
	}
	if info.Size() > 256<<10 {
		return nil, errors.New("trigger file is too large")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var file struct {
		Version  int           `yaml:"version"`
		Triggers []TriggerRule `yaml:"triggers"`
	}
	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("parse trigger file: %w", err)
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("trigger file must contain exactly one YAML document")
	}
	if file.Version != 1 {
		return nil, fmt.Errorf("unsupported trigger file version %d", file.Version)
	}
	seen := map[string]bool{}
	var rules []TriggerRule
	for _, r := range file.Triggers {
		if r.ID == "" || seen[r.ID] || strings.ContainsAny(r.ID, "/\\\x00") {
			return nil, fmt.Errorf("trigger IDs must be unique, nonempty and path-safe: %q", r.ID)
		}
		seen[r.ID] = true
		if !r.Enabled {
			continue
		}
		if len(r.Events) == 0 || len(r.Command) < 2 || len(r.Command) > 16 || r.TimeoutSeconds < 1 || r.TimeoutSeconds > 300 {
			return nil, fmt.Errorf("trigger %s has invalid events, command or timeout", r.ID)
		}
		for _, e := range r.Events {
			if !triggerEvents[e] {
				return nil, fmt.Errorf("trigger %s has unsupported event %q", r.ID, e)
			}
			if r.CurrentUserOnly && !isUserEvent(e) {
				return nil, fmt.Errorf("trigger %s currentUserOnly supports only user and membership events", r.ID)
			}
		}
		if !filepath.IsAbs(r.Command[0]) {
			return nil, fmt.Errorf("trigger %s executable must be an absolute path", r.ID)
		}
		switch strings.ToLower(filepath.Base(r.Command[0])) {
		case "sh", "bash", "dash", "zsh", "fish", "ksh", "csh", "tcsh", "env":
			return nil, fmt.Errorf("trigger %s cannot execute a shell", r.ID)
		}
		n := 0
		for _, a := range r.Command {
			if len(a) > 16384 || strings.ContainsRune(a, 0) {
				return nil, fmt.Errorf("trigger %s has an invalid argument", r.ID)
			}
			if strings.Contains(a, "%DATA%") {
				if a != "%DATA%" {
					return nil, fmt.Errorf("trigger %s requires %%DATA%% as a whole argument", r.ID)
				}
				n++
			}
		}
		if n != 1 {
			return nil, fmt.Errorf("trigger %s requires exactly one %%DATA%% argument", r.ID)
		}
		rules = append(rules, r)
	}
	return rules, nil
}

type publicUser struct {
	Username      string `json:"username"`
	DN            string `json:"dn"`
	UIDNumber     int64  `json:"uidNumber"`
	GIDNumber     int64  `json:"gidNumber"`
	HomeDirectory string `json:"homeDirectory"`
	LoginShell    string `json:"loginShell"`
	Mail          string `json:"mail"`
}

func pubUser(u replication.User) publicUser {
	return publicUser{u.Username, u.DN, u.UIDNumber, u.GIDNumber, u.HomeDirectory, u.LoginShell, u.Mail}
}

type publicGroup struct {
	Name      string `json:"name"`
	DN        string `json:"dn"`
	GIDNumber int64  `json:"gidNumber"`
	Class     string `json:"class"`
}

func pubGroup(g replication.Group) publicGroup {
	return publicGroup{g.Name, g.DN, g.GIDNumber, g.Class}
}

type event struct {
	Name string
	Data map[string]any
}

func ids[T any](a, b map[int64]T) []int64 {
	set := map[int64]bool{}
	for k := range a {
		set[k] = true
	}
	for k := range b {
		set[k] = true
	}
	out := make([]int64, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func diff[T any](name string, old, next map[int64]T, data func(T) any, events *[]event) {
	for _, id := range ids(old, next) {
		b, had := old[id]
		a, has := next[id]
		switch {
		case !had && has:
			*events = append(*events, event{name + ".created", map[string]any{name: data(a)}})
		case had && !has:
			*events = append(*events, event{name + ".deleted", map[string]any{name: data(b)}})
		case !reflect.DeepEqual(b, a):
			d := map[string]any{name: data(a)}
			if name != "ca" {
				d["previous"] = data(b)
			}
			*events = append(*events, event{name + ".updated", d})
		}
	}
}

func diffEvents(old, next replication.Snapshot) []event {
	var out []event
	ou, nu := map[int64]publicUser{}, map[int64]publicUser{}
	for _, u := range old.Users {
		ou[u.ID] = pubUser(u)
	}
	for _, u := range next.Users {
		nu[u.ID] = pubUser(u)
	}
	diff("user", ou, nu, func(u publicUser) any { return u }, &out)
	og, ng := map[int64]publicGroup{}, map[int64]publicGroup{}
	for _, g := range old.Groups {
		og[g.ID] = pubGroup(g)
	}
	for _, g := range next.Groups {
		ng[g.ID] = pubGroup(g)
	}
	diff("group", og, ng, func(g publicGroup) any { return g }, &out)

	key := func(m replication.Membership) string { return fmt.Sprintf("%020d:%020d", m.GroupID, m.UserID) }
	oe, ne := map[string]replication.Membership{}, map[string]replication.Membership{}
	for _, m := range old.Memberships {
		oe[key(m)] = m
	}
	for _, m := range next.Memberships {
		ne[key(m)] = m
	}
	var keys []string
	for k := range oe {
		if _, ok := ne[k]; !ok {
			keys = append(keys, k)
		}
	}
	for k := range ne {
		if _, ok := oe[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		m, added := ne[k]
		name := "membership.added"
		if !added {
			m, name = oe[k], "membership.removed"
		}
		u, ok := nu[m.UserID]
		if !ok {
			u = ou[m.UserID]
		}
		g, ok := ng[m.GroupID]
		if !ok {
			g = og[m.GroupID]
		}
		if u.Username != "" {
			out = append(out, event{name, map[string]any{"user": u, "group": g}})
		}
	}

	type caData struct {
		ID        int64  `json:"id"`
		Name      string `json:"name"`
		Kind      string `json:"kind"`
		Enabled   bool   `json:"enabled"`
		CRLNumber int64  `json:"crlNumber"`
	}
	oc, nc := map[int64]replication.CA{}, map[int64]replication.CA{}
	for _, c := range old.CAs {
		oc[c.ID] = c
	}
	for _, c := range next.CAs {
		nc[c.ID] = c
	}
	diff("ca", oc, nc, func(c replication.CA) any { return caData{c.ID, c.Name, c.Kind, c.Enabled, c.CRLNumber} }, &out)
	return out
}

// Queue is a crash-safe, ordered on-disk delivery queue; delivery is at least once.
type Queue struct {
	Dir   string
	Rules []TriggerRule
	Now   func() time.Time
}

type delivery struct {
	Rule    string   `json:"rule"`
	Event   string   `json:"event"`
	Payload string   `json:"payload"`
	Command []string `json:"command"`
	Timeout int      `json:"timeout"`
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Enqueue writes one queue file per (event, matching rule) for the difference between two snapshots.
func (q *Queue) Enqueue(old, next replication.Snapshot) error {
	if len(q.Rules) == 0 {
		return nil
	}
	if err := os.MkdirAll(q.Dir, 0o700); err != nil {
		return err
	}
	base := q.Now().UnixNano()
	n := 0
	for _, e := range diffEvents(old, next) {
		payload, err := json.Marshal(map[string]any{"event": e.Name, "data": e.Data})
		if err != nil {
			return err
		}
		for _, r := range q.Rules {
			if !slices.Contains(r.Events, e.Name) {
				continue
			}
			b, err := json.Marshal(delivery{r.ID, e.Name, string(payload), r.Command, r.TimeoutSeconds})
			if err != nil {
				return err
			}
			name := fmt.Sprintf("%020d-%06d.json", base, n)
			n++
			if err := writeAtomic(filepath.Join(q.Dir, name), b); err != nil {
				return err
			}
		}
	}
	return nil
}

// Pending counts deliveries not yet run.
func (q *Queue) Pending() (int, error) {
	names, err := q.list()
	return len(names), err
}

func (q *Queue) list() ([]string, error) {
	entries, err := os.ReadDir(q.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".json") && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// Dispatch runs queued deliveries in order, stopping at the first failure so ordering is kept.
// current is the latest applied snapshot, used to skip obsolete deliveries of currentUserOnly rules.
func (q *Queue) Dispatch(ctx context.Context, current replication.Snapshot) error {
	names, err := q.list()
	if err != nil {
		return err
	}
	guarded := map[string]bool{}
	for _, r := range q.Rules {
		guarded[r.ID] = r.CurrentUserOnly
	}
	for _, name := range names {
		path := filepath.Join(q.Dir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var d delivery
		if err := json.Unmarshal(raw, &d); err != nil || len(d.Command) < 2 || d.Timeout < 1 || d.Timeout > 300 {
			return fmt.Errorf("corrupt trigger delivery %s", name)
		}
		if guarded[d.Rule] && isUserEvent(d.Event) && !stillCurrent(d, current) {
			if err := os.Remove(path); err != nil {
				return err
			}
			continue
		}
		argv := slices.Clone(d.Command)
		for i := range argv {
			if argv[i] == "%DATA%" {
				argv[i] = d.Payload
			}
		}
		cctx, cancel := context.WithTimeout(ctx, time.Duration(d.Timeout)*time.Second)
		cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
		err = cmd.Run()
		cancel()
		if err != nil {
			return fmt.Errorf("trigger %s delivery %s failed: %w", d.Rule, strings.TrimSuffix(name, ".json"), err)
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}

// stillCurrent drops deliveries describing a user state that has since been superseded.
func stillCurrent(d delivery, current replication.Snapshot) bool {
	var p struct {
		Data struct {
			User publicUser `json:"user"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(d.Payload), &p) != nil || p.Data.User.Username == "" {
		return false
	}
	var now *publicUser
	for _, u := range current.Users {
		if u.Username == p.Data.User.Username {
			pu := pubUser(u)
			now = &pu
			break
		}
	}
	if d.Event == "user.deleted" {
		return now == nil
	}
	return now != nil && *now == p.Data.User
}
