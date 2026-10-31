package slave

import (
	"context"
	"encoding/pem"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/TING-HiuYu/SlurmPortal/internal/replication"
)

const posixContainer = "go-ldap-admin-posix-groups"

// projectPOSIX publishes a companion posixGroup for every GID-bearing non-POSIX group (OUs and
// groupOfUniqueNames cannot also be posixGroup) and propagates subgroup membership to GID-bearing
// ancestors. DN layout is identical to v1 so existing directories do not churn.
func projectPOSIX(src replication.Snapshot, baseDN string) replication.Snapshot {
	out := src
	out.Groups = append([]replication.Group(nil), src.Groups...)
	out.Memberships = append([]replication.Membership(nil), src.Memberships...)
	byID := make(map[int64]replication.Group, len(src.Groups))
	for _, g := range src.Groups {
		byID[g.ID] = g
	}
	containerDN := "ou=" + posixContainer + "," + baseDN
	added := false
	for _, g := range src.Groups {
		if g.GIDNumber == 0 || strings.EqualFold(g.Class, "posixGroup") {
			continue
		}
		if !added {
			out.Groups = append(out.Groups, replication.Group{Name: posixContainer, Type: "ou", DN: containerDN})
			added = true
		}
		name := "managed-" + strconv.FormatInt(g.ID, 10)
		out.Groups = append(out.Groups, replication.Group{ID: g.ID, Name: name, Remark: g.Name, Type: "cn", Class: "posixGroup",
			GIDNumber: g.GIDNumber, DN: "cn=" + name + "," + containerDN})
	}
	for _, m := range src.Memberships {
		seen := map[int64]bool{}
		for pid := byID[m.GroupID].ParentID; pid != 0 && !seen[pid]; {
			seen[pid] = true
			parent, ok := byID[pid]
			if !ok {
				break
			}
			if parent.GIDNumber != 0 {
				out.Memberships = append(out.Memberships, replication.Membership{GroupID: pid, UserID: m.UserID})
			}
			pid = parent.ParentID
		}
	}
	return out
}

type edge struct {
	g replication.Group
	u replication.User
}

type view struct {
	groups map[string]replication.Group
	users  map[string]replication.User
	edges  map[string]edge
	certs  map[int64]replication.UserCertificate
}

func newView(s replication.Snapshot, baseDN, adminDN string) view {
	v := view{groups: map[string]replication.Group{}, users: map[string]replication.User{}, edges: map[string]edge{}, certs: map[int64]replication.UserCertificate{}}
	byID := map[int64][]replication.Group{}
	for _, g := range s.Groups {
		if g.DN == "" || strings.EqualFold(g.DN, baseDN) {
			continue
		}
		v.groups[strings.ToLower(g.DN)] = g
		if g.Type != "ou" {
			byID[g.ID] = append(byID[g.ID], g)
		}
	}
	usersByID := map[int64]replication.User{}
	for _, u := range s.Users {
		if u.DN == "" || strings.EqualFold(u.DN, adminDN) {
			continue
		}
		v.users[strings.ToLower(u.DN)] = u
		usersByID[u.ID] = u
	}
	for _, m := range s.Memberships {
		u, ok := usersByID[m.UserID]
		if !ok {
			continue
		}
		for _, g := range byID[m.GroupID] {
			v.edges[strings.ToLower(g.DN)+"\x00"+strings.ToLower(u.DN)] = edge{g, u}
		}
	}
	for _, c := range s.UserCertificates {
		v.certs[c.UserID] = c
	}
	return v
}

func validate(old, next view) error {
	for dn, u := range next.users {
		posix := u.UIDNumber != 0 || u.GIDNumber != 0 || u.HomeDirectory != "" || u.LoginShell != ""
		if posix && (u.UIDNumber == 0 || u.GIDNumber == 0 || !strings.HasPrefix(u.HomeDirectory, "/")) {
			return fmt.Errorf("incomplete POSIX identity for %s", dn)
		}
	}
	for dn, g := range next.groups {
		if g.Class != "" && !strings.EqualFold(g.Class, "posixGroup") && !strings.EqualFold(g.Class, "groupOfUniqueNames") {
			return fmt.Errorf("group %s has unsupported objectClass", dn)
		}
		if strings.EqualFold(g.Class, "posixGroup") && (g.Type != "cn" || g.GIDNumber == 0) {
			return fmt.Errorf("posixGroup %s requires cn and gidNumber", dn)
		}
		if prev, ok := old.groups[dn]; ok && !strings.EqualFold(prev.Class, g.Class) {
			return fmt.Errorf("group %s changes objectClass; explicit migration required", dn)
		}
	}
	return nil
}

func sameUser(a, b replication.User) bool {
	a.PasswordHash, b.PasswordHash = "", ""
	a.ID, b.ID = 0, 0
	return a == b
}

func certDER(c replication.UserCertificate) ([]byte, error) {
	b, rest := pem.Decode([]byte(c.CertificatePEM))
	if b == nil || b.Type != "CERTIFICATE" || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, fmt.Errorf("invalid certificate PEM for user %d", c.UserID)
	}
	return b.Bytes, nil
}

func depth(dn string) int { return strings.Count(dn, ",") }

// Reconcile drives LDAP from old to next. Every step tolerates already-applied state, so a failed
// run is retried from the same old snapshot until it fully succeeds.
func Reconcile(ctx context.Context, dir Directory, old, next replication.Snapshot, baseDN, adminDN string) error {
	o := newView(projectPOSIX(old, baseDN), baseDN, adminDN)
	n := newView(projectPOSIX(next, baseDN), baseDN, adminDN)
	if err := validate(o, n); err != nil {
		return err
	}
	step := func(err error, what, dn string) error {
		if err != nil {
			return fmt.Errorf("%s %s: %w", what, dn, err)
		}
		return ctx.Err()
	}

	for _, k := range sortedKeys(o.edges) {
		if _, keep := n.edges[k]; !keep {
			e := o.edges[k]
			if err := step(dir.RemoveMember(ctx, e.g, e.u), "remove member", e.g.DN); err != nil {
				return err
			}
		}
	}
	for _, k := range sortedKeys(o.users) {
		if _, keep := n.users[k]; !keep {
			if err := step(dir.Delete(ctx, o.users[k].DN), "delete user", o.users[k].DN); err != nil {
				return err
			}
		}
	}
	var gone []string
	for k := range o.groups {
		if _, keep := n.groups[k]; !keep {
			gone = append(gone, k)
		}
	}
	sort.Slice(gone, func(i, j int) bool {
		return depth(gone[i]) > depth(gone[j]) || depth(gone[i]) == depth(gone[j]) && gone[i] < gone[j]
	})
	for _, k := range gone {
		if err := step(dir.Delete(ctx, o.groups[k].DN), "delete group", o.groups[k].DN); err != nil {
			return err
		}
	}

	keys := sortedKeys(n.groups)
	sort.SliceStable(keys, func(i, j int) bool { return depth(keys[i]) < depth(keys[j]) })
	for _, k := range keys {
		g := n.groups[k]
		prev, existed := o.groups[k]
		var err error
		switch {
		case !existed:
			if err = dir.AddGroup(ctx, g); err == ErrExists {
				err = dir.UpdateGroup(ctx, g)
			}
		case prev.Remark != g.Remark || prev.Name != g.Name || prev.GIDNumber != g.GIDNumber:
			err = dir.UpdateGroup(ctx, g)
		}
		if err := step(err, "apply group", g.DN); err != nil {
			return err
		}
	}

	for _, k := range sortedKeys(n.users) {
		u := n.users[k]
		prev, existed := o.users[k]
		var err error
		switch {
		case !existed:
			// joining an existing directory must not overwrite passwords changed locally before the first snapshot
			if err = dir.AddUser(ctx, u, true); err == ErrExists {
				err = dir.UpdateUser(ctx, u)
			}
		default:
			if !sameUser(prev, u) {
				err = dir.UpdateUser(ctx, u)
			}
			if err == nil && u.PasswordHash != "" && u.PasswordHash != prev.PasswordHash {
				err = dir.SetPassword(ctx, u.DN, u.PasswordHash)
			}
		}
		if err := step(err, "apply user", u.DN); err != nil {
			return err
		}
		cur, has := n.certs[u.ID]
		before, had := o.certs[prev.ID]
		if !existed {
			had = false
		}
		switch {
		case has && (!had || before.SerialHex != cur.SerialHex || before.CertificatePEM != cur.CertificatePEM):
			der, err := certDER(cur)
			if err != nil {
				return err
			}
			err = dir.SetCertificate(ctx, u.DN, der)
			if err := step(err, "set certificate", u.DN); err != nil {
				return err
			}
		case !has && had:
			if err := step(dir.ClearCertificate(ctx, u.DN), "clear certificate", u.DN); err != nil {
				return err
			}
		}
	}

	for _, k := range sortedKeys(n.edges) {
		if _, had := o.edges[k]; !had {
			e := n.edges[k]
			if err := step(dir.AddMember(ctx, e.g, e.u), "add member", e.g.DN); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
