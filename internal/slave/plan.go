package slave

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/TING-HiuYu/SlurmPortal/internal/replication"
)

// Op is one LDAP write a reconcile would perform.
type Op struct {
	Kind string `json:"kind"`
	DN   string `json:"dn"`
	// Fields lists changed user attributes for update-user.
	Fields []string `json:"fields,omitempty"`
	// Login marks writes that can change who may log in or as which POSIX identity.
	Login bool `json:"login"`
}

// recorder is a Directory that records instead of writing.
type recorder struct {
	old map[string]replication.User
	ops []Op
}

func (r *recorder) add(op Op) error { r.ops = append(r.ops, op); return nil }

func (r *recorder) AddGroup(_ context.Context, g replication.Group) error {
	return r.add(Op{Kind: "add-group", DN: g.DN, Login: g.GIDNumber != 0})
}
func (r *recorder) UpdateGroup(_ context.Context, g replication.Group) error {
	return r.add(Op{Kind: "update-group", DN: g.DN, Login: g.GIDNumber != 0})
}
func (r *recorder) Delete(_ context.Context, dn string) error {
	return r.add(Op{Kind: "delete", DN: dn, Login: true})
}
func (r *recorder) AddUser(_ context.Context, u replication.User, _ bool) error {
	return r.add(Op{Kind: "add-user", DN: u.DN, Login: true})
}
func (r *recorder) UpdateUser(_ context.Context, u replication.User) error {
	prev := r.old[strings.ToLower(u.DN)]
	var fields []string
	login := false
	for _, f := range []struct {
		name     string
		a, b     any
		identity bool
	}{
		{"username", prev.Username, u.Username, true}, {"uidNumber", prev.UIDNumber, u.UIDNumber, true}, {"gidNumber", prev.GIDNumber, u.GIDNumber, true},
		{"homeDirectory", prev.HomeDirectory, u.HomeDirectory, true}, {"loginShell", prev.LoginShell, u.LoginShell, true},
		{"nickname", prev.Nickname, u.Nickname, false}, {"givenName", prev.GivenName, u.GivenName, false}, {"mail", prev.Mail, u.Mail, false},
		{"jobNumber", prev.JobNumber, u.JobNumber, false}, {"mobile", prev.Mobile, u.Mobile, false}, {"postalAddress", prev.PostalAddress, u.PostalAddress, false},
		{"position", prev.Position, u.Position, false}, {"introduction", prev.Introduction, u.Introduction, false}, {"departments", prev.Departments, u.Departments, false},
	} {
		if f.a != f.b {
			fields = append(fields, f.name)
			login = login || f.identity
		}
	}
	return r.add(Op{Kind: "update-user", DN: u.DN, Fields: fields, Login: login})
}
func (r *recorder) SetPassword(_ context.Context, dn, _ string) error {
	return r.add(Op{Kind: "set-password", DN: dn})
}
func (r *recorder) SetCertificate(_ context.Context, dn string, _ []byte) error {
	return r.add(Op{Kind: "set-certificate", DN: dn})
}
func (r *recorder) ClearCertificate(_ context.Context, dn string) error {
	return r.add(Op{Kind: "clear-certificate", DN: dn})
}
func (r *recorder) AddMember(_ context.Context, g replication.Group, u replication.User) error {
	return r.add(Op{Kind: "add-member", DN: g.DN + " <- " + u.DN, Login: g.GIDNumber != 0})
}
func (r *recorder) RemoveMember(_ context.Context, g replication.Group, u replication.User) error {
	return r.add(Op{Kind: "remove-member", DN: g.DN + " <- " + u.DN, Login: g.GIDNumber != 0})
}

// Plan returns the LDAP writes that would take the directory from old to next, without writing.
func Plan(ctx context.Context, old, next replication.Snapshot, baseDN, adminDN string) ([]Op, error) {
	rec := &recorder{old: map[string]replication.User{}}
	for _, u := range old.Users {
		rec.old[strings.ToLower(u.DN)] = u
	}
	err := Reconcile(ctx, rec, old, next, baseDN, adminDN)
	return rec.ops, err
}

// Limits bound what one sync may do without an operator explicitly allowing it.
type Limits struct {
	// MaxUserDeletions is the most users one sync may delete (default 5).
	MaxUserDeletions int `yaml:"max-user-deletions"`
	// AllowCARemoval permits a sync that stops trusting an SSH user CA currently trusted.
	AllowCARemoval bool `yaml:"allow-ca-removal"`
}

// Guard refuses a sync that deletes many users or drops a trusted SSH CA: either would lock users
// out of the cluster, and both are far more likely to be a misconfigured master than intent.
func Guard(old, next replication.Snapshot, l Limits) error {
	if l.MaxUserDeletions == 0 {
		l.MaxUserDeletions = 5
	}
	keep := map[string]bool{}
	for _, u := range next.Users {
		keep[strings.ToLower(u.DN)] = true
	}
	var deleted []string
	for _, u := range old.Users {
		if !keep[strings.ToLower(u.DN)] {
			deleted = append(deleted, u.Username)
		}
	}
	if len(deleted) > l.MaxUserDeletions {
		return fmt.Errorf("refusing sync: it would delete %d users (limit %d), e.g. %s", len(deleted), l.MaxUserDeletions, strings.Join(deleted[:min(5, len(deleted))], ", "))
	}
	if l.AllowCARemoval {
		return nil
	}
	trusted := func(s replication.Snapshot) []string {
		var keys []string
		for _, ca := range s.CAs {
			if ca.Enabled && ca.Kind == "ssh" {
				keys = append(keys, strings.Join(strings.Fields(ca.CertificatePEM)[:min(2, len(strings.Fields(ca.CertificatePEM)))], " "))
			}
		}
		return keys
	}
	now := trusted(next)
	for _, k := range trusted(old) {
		if !slices.Contains(now, k) {
			return fmt.Errorf("refusing sync: SSH user CA %q would no longer be trusted (set allow-ca-removal to permit)", k[:min(40, len(k))])
		}
	}
	return nil
}

// TrustedCAs reads the SSH user CA keys currently published in shareDir (absent file: none).
func TrustedCAs(shareDir string) ([]replication.CA, error) {
	raw, err := os.ReadFile(filepath.Join(shareDir, "current", "ssh", "trusted-user-ca-keys.pub"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []replication.CA
	for _, line := range strings.Split(string(raw), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && !strings.HasPrefix(f[0], "#") {
			out = append(out, replication.CA{Kind: "ssh", Enabled: true, CertificatePEM: f[0] + " " + f[1]})
		}
	}
	return out, nil
}

// Baseline turns an observed directory into the starting point of a first sync: only entries master
// manages (DNs present in next) are kept, so entries created locally (or OUs master never models) are
// never deleted, and existing password hashes are left alone (as v1 did) by adopting next's hash.
func Baseline(observed, next replication.Snapshot) replication.Snapshot {
	users, groups := map[string]replication.User{}, map[string]replication.Group{}
	for _, u := range next.Users {
		users[strings.ToLower(u.DN)] = u
	}
	for _, g := range next.Groups {
		groups[strings.ToLower(g.DN)] = g
	}
	// Observed entries carry IDs made up while reading LDAP; adopt master's IDs for the entries kept,
	// so ID-keyed diffs (trigger events) see an unchanged entry as unchanged, not deleted and re-created.
	userIDs, groupIDs := map[int64]int64{}, map[int64]int64{}
	out := observed
	out.Users, out.Groups, out.Memberships, out.UserCertificates = nil, nil, nil, nil
	for _, u := range observed.Users {
		if want, ok := users[strings.ToLower(u.DN)]; ok {
			userIDs[u.ID] = want.ID
			u.ID, u.PasswordHash = want.ID, want.PasswordHash
			out.Users = append(out.Users, u)
		}
	}
	for _, g := range observed.Groups {
		want, ok := groups[strings.ToLower(g.DN)]
		if !ok {
			continue
		}
		groupIDs[g.ID] = want.ID
		g.ID = want.ID
		// LDAP holds only the description AddGroup derived (remark, else name); adopt master's
		// fields when they produce the same description, so equal entries diff to nothing.
		if desc := cmp.Or(want.Remark, want.Name); g.Remark == desc {
			g.Remark, g.Name = want.Remark, want.Name
		}
		out.Groups = append(out.Groups, g)
	}
	for _, m := range observed.Memberships {
		gid, gok := groupIDs[m.GroupID]
		uid, uok := userIDs[m.UserID]
		if gok && uok {
			out.Memberships = append(out.Memberships, replication.Membership{GroupID: gid, UserID: uid})
		}
	}
	for _, c := range observed.UserCertificates {
		if uid, ok := userIDs[c.UserID]; ok {
			c.UserID = uid
			out.UserCertificates = append(out.UserCertificates, c)
		}
	}
	return out
}
