// Package slave replicates the v2 master snapshot into a local LDAP directory, trust files and triggers.
package slave

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	ldap "github.com/go-ldap/ldap/v3"

	"github.com/TING-HiuYu/SlurmPortal/internal/replication"
)

var ErrExists = errors.New("entry already exists")

// Directory is the minimal LDAP surface the reconciler needs; tests substitute an in-memory fake.
type Directory interface {
	AddGroup(ctx context.Context, g replication.Group) error
	UpdateGroup(ctx context.Context, g replication.Group) error
	Delete(ctx context.Context, dn string) error
	AddUser(ctx context.Context, u replication.User, setPassword bool) error
	UpdateUser(ctx context.Context, u replication.User) error
	SetPassword(ctx context.Context, dn, hash string) error
	SetCertificate(ctx context.Context, dn string, der []byte) error
	ClearCertificate(ctx context.Context, dn string) error
	AddMember(ctx context.Context, g replication.Group, u replication.User) error
	RemoveMember(ctx context.Context, g replication.Group, u replication.User) error
}

// LDAPConfig is how the slave reaches its cluster's OpenLDAP.
type LDAPConfig struct {
	URL      string
	BindDN   string
	Password string
	StartTLS bool
	// ServerName overrides TLS verification name; certificates are always verified.
	ServerName string
	Timeout    time.Duration
}

// LDAP applies directory changes to one OpenLDAP session.
type LDAP struct {
	cfg  LDAPConfig
	conn *ldap.Conn
}

// DialLDAP opens and binds one connection for a reconcile run.
func DialLDAP(cfg LDAPConfig) (*LDAP, error) {
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: cfg.ServerName}
	conn, err := ldap.DialURL(cfg.URL, ldap.DialWithTLSConfig(tlsCfg))
	if err != nil {
		return nil, err
	}
	conn.SetTimeout(cfg.Timeout)
	if cfg.StartTLS {
		if err := conn.StartTLS(tlsCfg); err != nil {
			conn.Close()
			return nil, err
		}
	}
	if err := conn.Bind(cfg.BindDN, cfg.Password); err != nil {
		conn.Close()
		return nil, err
	}
	return &LDAP{cfg: cfg, conn: conn}, nil
}

// Close ends the LDAP session.
func (l *LDAP) Close() error { return l.conn.Close() }

func ignore(err error, codes ...uint16) error {
	for _, c := range codes {
		if ldap.IsErrorWithCode(err, c) {
			return nil
		}
	}
	return err
}

func values(v string) []string {
	if v == "" {
		return nil
	}
	return []string{v}
}

// AddGroup creates a group entry.
func (l *LDAP) AddGroup(_ context.Context, g replication.Group) error {
	req := ldap.NewAddRequest(g.DN, nil)
	description := g.Remark
	if description == "" {
		description = g.Name
	}
	switch {
	case g.Type == "ou":
		req.Attribute("objectClass", []string{"organizationalUnit", "top"})
	case strings.EqualFold(g.Class, "posixGroup"):
		req.Attribute("objectClass", []string{"posixGroup", "top"})
		req.Attribute("gidNumber", []string{strconv.FormatInt(g.GIDNumber, 10)})
	default:
		req.Attribute("objectClass", []string{"groupOfUniqueNames", "top"})
		// groupOfUniqueNames requires a member; the bind (admin) DN keeps it valid while empty.
		req.Attribute("uniqueMember", []string{l.cfg.BindDN})
	}
	req.Attribute(g.Type, []string{g.Name})
	req.Attribute("description", []string{description})
	if err := l.conn.Add(req); err != nil {
		if ldap.IsErrorWithCode(err, ldap.LDAPResultEntryAlreadyExists) {
			return ErrExists
		}
		return err
	}
	return nil
}

// UpdateGroup rewrites a group entry's attributes.
func (l *LDAP) UpdateGroup(_ context.Context, g replication.Group) error {
	req := ldap.NewModifyRequest(g.DN, nil)
	description := g.Remark
	if description == "" {
		description = g.Name
	}
	req.Replace("description", []string{description})
	if strings.EqualFold(g.Class, "posixGroup") {
		req.Replace("gidNumber", []string{strconv.FormatInt(g.GIDNumber, 10)})
	}
	return l.conn.Modify(req)
}

// Delete removes an entry; a missing entry is not an error.
func (l *LDAP) Delete(_ context.Context, dn string) error {
	return ignore(l.conn.Del(ldap.NewDelRequest(dn, nil)), ldap.LDAPResultNoSuchObject)
}

func userAttributes(u replication.User) map[string][]string {
	sn := u.Nickname
	if sn == "" {
		sn = u.Username
	}
	attrs := map[string][]string{
		"cn": {u.Username}, "sn": {sn}, "uid": {u.Username},
		"displayName": values(u.Nickname), "givenName": values(u.GivenName), "mail": values(u.Mail),
		"employeeNumber": values(u.JobNumber), "mobile": values(u.Mobile), "postalAddress": values(u.PostalAddress),
		"departmentNumber": values(u.Position), "description": values(u.Introduction), "businessCategory": values(u.Departments),
	}
	if u.UIDNumber != 0 {
		attrs["uidNumber"] = []string{strconv.FormatInt(u.UIDNumber, 10)}
		attrs["gidNumber"] = []string{strconv.FormatInt(u.GIDNumber, 10)}
		attrs["homeDirectory"] = []string{u.HomeDirectory}
		attrs["loginShell"] = values(u.LoginShell)
	}
	return attrs
}

// AddUser creates a user entry; setPassword also writes userPassword.
func (l *LDAP) AddUser(_ context.Context, u replication.User, setPassword bool) error {
	req := ldap.NewAddRequest(u.DN, nil)
	classes := []string{"inetOrgPerson"}
	if u.UIDNumber != 0 {
		classes = append(classes, "posixAccount")
	}
	req.Attribute("objectClass", classes)
	for k, v := range userAttributes(u) {
		if len(v) > 0 {
			req.Attribute(k, v)
		}
	}
	if setPassword && u.PasswordHash != "" {
		req.Attribute("userPassword", []string{u.PasswordHash})
	}
	if err := l.conn.Add(req); err != nil {
		if ldap.IsErrorWithCode(err, ldap.LDAPResultEntryAlreadyExists) {
			return ErrExists
		}
		return err
	}
	return nil
}

// UpdateUser rewrites a user entry's attributes.
func (l *LDAP) UpdateUser(_ context.Context, u replication.User) error {
	req := ldap.NewModifyRequest(u.DN, nil)
	for k, v := range userAttributes(u) {
		if len(v) == 0 {
			req.Replace(k, []string{})
		} else {
			req.Replace(k, v)
		}
	}
	if u.UIDNumber != 0 {
		found, err := l.conn.Search(ldap.NewSearchRequest(u.DN, ldap.ScopeBaseObject, ldap.NeverDerefAliases, 1, 0, false, "(objectClass=*)", []string{"objectClass"}, nil))
		if err != nil {
			return err
		}
		if len(found.Entries) != 1 {
			return fmt.Errorf("user %s not found for POSIX update", u.DN)
		}
		posix := false
		for _, c := range found.Entries[0].GetAttributeValues("objectClass") {
			posix = posix || strings.EqualFold(c, "posixAccount")
		}
		if !posix {
			req.Add("objectClass", []string{"posixAccount"})
		}
	}
	return l.conn.Modify(req)
}

// SetPassword replaces userPassword with hash.
func (l *LDAP) SetPassword(_ context.Context, dn, hash string) error {
	req := ldap.NewModifyRequest(dn, nil)
	req.Replace("userPassword", []string{hash})
	return l.conn.Modify(req)
}

// SetCertificate writes the user's X.509 certificate (DER) to userCertificate;binary.
func (l *LDAP) SetCertificate(_ context.Context, dn string, der []byte) error {
	add := ldap.NewModifyRequest(dn, nil)
	add.Add("objectClass", []string{"pkiUser"})
	if err := ignore(l.conn.Modify(add), ldap.LDAPResultAttributeOrValueExists); err != nil {
		return err
	}
	req := ldap.NewModifyRequest(dn, nil)
	req.Replace("userCertificate;binary", []string{string(der)})
	return l.conn.Modify(req)
}

// ClearCertificate removes the user's certificate.
func (l *LDAP) ClearCertificate(_ context.Context, dn string) error {
	req := ldap.NewModifyRequest(dn, nil)
	req.Delete("userCertificate;binary", nil)
	return ignore(l.conn.Modify(req), ldap.LDAPResultNoSuchAttribute)
}

func member(g replication.Group, u replication.User) (string, string) {
	if strings.EqualFold(g.Class, "posixGroup") {
		return "memberUid", u.Username
	}
	return "uniqueMember", u.DN
}

// AddMember adds u to g (memberUid for POSIX groups, uniqueMember otherwise).
func (l *LDAP) AddMember(_ context.Context, g replication.Group, u replication.User) error {
	attr, v := member(g, u)
	req := ldap.NewModifyRequest(g.DN, nil)
	req.Add(attr, []string{v})
	return ignore(l.conn.Modify(req), ldap.LDAPResultAttributeOrValueExists)
}

// RemoveMember removes u from g.
func (l *LDAP) RemoveMember(_ context.Context, g replication.Group, u replication.User) error {
	attr, v := member(g, u)
	req := ldap.NewModifyRequest(g.DN, nil)
	req.Delete(attr, []string{v})
	return ignore(l.conn.Modify(req), ldap.LDAPResultNoSuchObject, ldap.LDAPResultNoSuchAttribute)
}
