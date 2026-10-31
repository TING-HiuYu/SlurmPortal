package slave

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/TING-HiuYu/SlurmPortal/internal/replication"
)

// fakeDir is an in-memory LDAP stand-in that records operations and can fail on demand.
type fakeDir struct {
	mu      sync.Mutex
	entries map[string]map[string][]string
	ops     []string
	failOn  string
	closed  int
	// observed is what SearchEntries reports as the pre-existing directory.
	observed []Entry
}

func (f *fakeDir) SearchEntries(string) ([]Entry, error) { return f.observed, nil }

func newFakeDir() *fakeDir { return &fakeDir{entries: map[string]map[string][]string{}} }

func (f *fakeDir) Close() error { f.closed++; return nil }

func (f *fakeDir) op(name, dn string) error {
	f.ops = append(f.ops, name+" "+dn)
	if f.failOn != "" && strings.HasPrefix(name+" "+dn, f.failOn) {
		f.failOn = ""
		return errors.New("injected failure")
	}
	return nil
}

func key(dn string) string { return strings.ToLower(dn) }

func (f *fakeDir) AddGroup(_ context.Context, g replication.Group) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("addgroup", g.DN); err != nil {
		return err
	}
	if _, ok := f.entries[key(g.DN)]; ok {
		return ErrExists
	}
	attrs := map[string][]string{"type": {g.Type}, "class": {g.Class}, "gid": {fmt.Sprint(g.GIDNumber)}, "description": {g.Remark}}
	if g.Type != "ou" && !strings.EqualFold(g.Class, "posixGroup") {
		attrs["uniqueMember"] = []string{"cn=admin"}
	}
	f.entries[key(g.DN)] = attrs
	return nil
}

func (f *fakeDir) UpdateGroup(_ context.Context, g replication.Group) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("updategroup", g.DN); err != nil {
		return err
	}
	e, ok := f.entries[key(g.DN)]
	if !ok {
		return fmt.Errorf("no such group %s", g.DN)
	}
	e["description"], e["gid"] = []string{g.Remark}, []string{fmt.Sprint(g.GIDNumber)}
	return nil
}

func (f *fakeDir) Delete(_ context.Context, dn string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("delete", dn); err != nil {
		return err
	}
	for k := range f.entries {
		if k != key(dn) && strings.HasSuffix(k, ","+key(dn)) {
			return fmt.Errorf("non-leaf delete of %s", dn)
		}
	}
	delete(f.entries, key(dn))
	return nil
}

func userAttrs(u replication.User) map[string][]string {
	return map[string][]string{"uid": {u.Username}, "uidNumber": {fmt.Sprint(u.UIDNumber)}, "gidNumber": {fmt.Sprint(u.GIDNumber)},
		"home": {u.HomeDirectory}, "shell": {u.LoginShell}, "mail": {u.Mail}, "dept": {u.Departments}}
}

func (f *fakeDir) AddUser(_ context.Context, u replication.User, setPassword bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("adduser", u.DN); err != nil {
		return err
	}
	if _, ok := f.entries[key(u.DN)]; ok {
		return ErrExists
	}
	a := userAttrs(u)
	if setPassword && u.PasswordHash != "" {
		a["userPassword"] = []string{u.PasswordHash}
	}
	f.entries[key(u.DN)] = a
	return nil
}

func (f *fakeDir) UpdateUser(_ context.Context, u replication.User) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("updateuser", u.DN); err != nil {
		return err
	}
	e, ok := f.entries[key(u.DN)]
	if !ok {
		return fmt.Errorf("no such user %s", u.DN)
	}
	for k, v := range userAttrs(u) {
		e[k] = v
	}
	return nil
}

func (f *fakeDir) SetPassword(_ context.Context, dn, hash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("setpassword", dn); err != nil {
		return err
	}
	f.entries[key(dn)]["userPassword"] = []string{hash}
	return nil
}

func (f *fakeDir) SetCertificate(_ context.Context, dn string, der []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("setcert", dn); err != nil {
		return err
	}
	f.entries[key(dn)]["cert"] = []string{string(der)}
	return nil
}

func (f *fakeDir) ClearCertificate(_ context.Context, dn string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("clearcert", dn); err != nil {
		return err
	}
	delete(f.entries[key(dn)], "cert")
	return nil
}

func (f *fakeDir) member(g replication.Group, u replication.User, add bool) error {
	attr, v := member(g, u)
	e, ok := f.entries[key(g.DN)]
	if !ok {
		return fmt.Errorf("no such group %s", g.DN)
	}
	vals := e[attr]
	idx := -1
	for i, x := range vals {
		if strings.EqualFold(x, v) {
			idx = i
		}
	}
	switch {
	case add && idx < 0:
		e[attr] = append(vals, v)
	case !add && idx >= 0:
		e[attr] = append(vals[:idx], vals[idx+1:]...)
	}
	return nil
}

func (f *fakeDir) AddMember(_ context.Context, g replication.Group, u replication.User) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("addmember", g.DN); err != nil {
		return err
	}
	return f.member(g, u, true)
}

func (f *fakeDir) RemoveMember(_ context.Context, g replication.Group, u replication.User) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.op("removemember", g.DN); err != nil {
		return err
	}
	return f.member(g, u, false)
}

func (f *fakeDir) attr(dn, a string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.entries[key(dn)]
	if e == nil {
		return nil
	}
	v := append([]string(nil), e[a]...)
	sort.Strings(v)
	return v
}

func (f *fakeDir) has(dn string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.entries[key(dn)]
	return ok
}
