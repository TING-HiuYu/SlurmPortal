// Package replication defines the master→slave snapshot (protocol v2). The snapshot carries exactly what
// slaves need to drive LDAP, trust files and local triggers; it contains no secrets beyond LDAP-ready
// one-way password hashes and public CA material.
package replication

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

const Version = 2

// User is an account as the slave writes it to LDAP.
type User struct {
	ID            int64  `json:"id"`
	Username      string `json:"username"`
	DN            string `json:"dn"`
	Nickname      string `json:"nickname"`
	GivenName     string `json:"givenName"`
	Mail          string `json:"mail"`
	JobNumber     string `json:"jobNumber"`
	Mobile        string `json:"mobile"`
	UIDNumber     int64  `json:"uidNumber"`
	GIDNumber     int64  `json:"gidNumber"`
	HomeDirectory string `json:"homeDirectory"`
	LoginShell    string `json:"loginShell"`
	PostalAddress string `json:"postalAddress"`
	Position      string `json:"position"`
	Introduction  string `json:"introduction"`
	Departments   string `json:"departments"`
	PasswordHash  string `json:"passwordHash"`
}

// Group is a POSIX or organisational group as written to LDAP.
type Group struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	DN        string `json:"dn"`
	Type      string `json:"type"`
	Class     string `json:"class"`
	GIDNumber int64  `json:"gidNumber"`
	ParentID  int64  `json:"parentId"`
	Remark    string `json:"remark"`
}

// Membership places a user in a group.
type Membership struct {
	GroupID int64 `json:"groupId"`
	UserID  int64 `json:"userId"`
}

// CA is a certificate authority the clusters should trust, with its current CRL.
type CA struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	Kind           string `json:"kind"`
	CertificatePEM string `json:"certificatePem"`
	CRLPEM         string `json:"crlPem"`
	CRLNumber      int64  `json:"crlNumber"`
	Enabled        bool   `json:"enabled"`
}

// Revocation is a revoked certificate serial, published in the SSH KRL.
type Revocation struct {
	CAID      int64  `json:"caId"`
	SerialHex string `json:"serialHex"`
}

// UserCertificate is a user's current certificate, written to LDAP userCertificate.
type UserCertificate struct {
	UserID         int64  `json:"userId"`
	CAID           int64  `json:"caId"`
	SerialHex      string `json:"serialHex"`
	CertificatePEM string `json:"certificatePem"`
}

// Snapshot holds only active users; disabled or deleted accounts are absent and so removed from LDAP.
type Snapshot struct {
	Version          int               `json:"version"`
	BaseDN           string            `json:"baseDn"`
	Users            []User            `json:"users"`
	Groups           []Group           `json:"groups"`
	Memberships      []Membership      `json:"memberships"`
	CAs              []CA              `json:"cas"`
	SSHRevocations   []Revocation      `json:"sshRevocations"`
	UserCertificates []UserCertificate `json:"userCertificates"`
}

// Hash is the SHA-256 of the snapshot's JSON. It is published in DNS so slaves only download a
// snapshot that changed.
func (s Snapshot) Hash() (string, error) {
	if s.Version != Version {
		return "", fmt.Errorf("unsupported snapshot version %d", s.Version)
	}
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Load reads until two consecutive complete reads agree, since D1 offers no read transaction.
func Load(ctx context.Context, db store.Querier, baseDN string) (Snapshot, string, error) {
	var prev string
	for range 3 {
		s, err := loadOnce(ctx, db, baseDN)
		if err != nil {
			return Snapshot{}, "", err
		}
		h, err := s.Hash()
		if err != nil {
			return Snapshot{}, "", err
		}
		if h == prev {
			return s, h, nil
		}
		prev = h
	}
	return Snapshot{}, "", errors.New("snapshot changed during read")
}

func each(ctx context.Context, db store.Querier, q string, scan func(store.Rows) error, args ...any) error {
	rows, err := db.Query(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func loadOnce(ctx context.Context, db store.Querier, baseDN string) (Snapshot, error) {
	s := Snapshot{Version: Version, BaseDN: baseDN, Users: []User{}, Groups: []Group{}, Memberships: []Membership{},
		CAs: []CA{}, SSHRevocations: []Revocation{}, UserCertificates: []UserCertificate{}}
	err := each(ctx, db, `SELECT u.id, u.username, COALESCE(u.user_dn,''), COALESCE(u.nickname,''), COALESCE(u.given_name,''), COALESCE(u.mail,''),
		COALESCE(u.job_number,''), COALESCE(u.mobile,''), COALESCE(u.uid_number,0), COALESCE(u.gid_number,0), COALESCE(u.home_directory,''),
		COALESCE(u.login_shell,''), COALESCE(u.postal_address,''), COALESCE(u.position,''), COALESCE(u.introduction,''), COALESCE(c.ldap_hash,'')
		FROM users u LEFT JOIN v2_user_credentials c ON c.user_id = u.id
		WHERE u.deleted_at IS NULL AND u.status = 1 AND COALESCE(u.user_dn,'') <> '' ORDER BY u.id`, func(r store.Rows) error {
		var u User
		err := r.Scan(&u.ID, &u.Username, &u.DN, &u.Nickname, &u.GivenName, &u.Mail, &u.JobNumber, &u.Mobile, &u.UIDNumber, &u.GIDNumber,
			&u.HomeDirectory, &u.LoginShell, &u.PostalAddress, &u.Position, &u.Introduction, &u.PasswordHash)
		s.Users = append(s.Users, u)
		return err
	})
	if err != nil {
		return s, err
	}
	err = each(ctx, db, `SELECT id, COALESCE(group_name,''), group_dn, COALESCE(group_type,''), COALESCE(group_class,''), COALESCE(gid_number,0),
		COALESCE(parent_id,0), COALESCE(remark,'') FROM groups WHERE deleted_at IS NULL AND group_dn <> '' ORDER BY id`, func(r store.Rows) error {
		var g Group
		err := r.Scan(&g.ID, &g.Name, &g.DN, &g.Type, &g.Class, &g.GIDNumber, &g.ParentID, &g.Remark)
		s.Groups = append(s.Groups, g)
		return err
	})
	if err != nil {
		return s, err
	}
	active := map[int64]*User{}
	for i := range s.Users {
		active[s.Users[i].ID] = &s.Users[i]
	}
	names := map[int64]string{}
	for _, g := range s.Groups {
		names[g.ID] = g.Name
	}
	departments := map[int64][]string{}
	err = each(ctx, db, "SELECT group_id, user_id FROM group_users ORDER BY group_id, user_id", func(r store.Rows) error {
		var m Membership
		if err := r.Scan(&m.GroupID, &m.UserID); err != nil {
			return err
		}
		if _, ok := names[m.GroupID]; ok && active[m.UserID] != nil {
			s.Memberships = append(s.Memberships, m)
			departments[m.UserID] = append(departments[m.UserID], names[m.GroupID])
		}
		return nil
	})
	if err != nil {
		return s, err
	}
	for id, ds := range departments {
		sort.Strings(ds)
		active[id].Departments = strings.Join(ds, ",")
	}
	err = each(ctx, db, `SELECT id, name, COALESCE(kind,'x509'), certificate_pem, COALESCE(crl_pem,''), COALESCE(crl_number,0),
		CASE WHEN enabled = 1 OR enabled = 'true' THEN 1 ELSE 0 END FROM certificate_authorities WHERE deleted_at IS NULL ORDER BY id`, func(r store.Rows) error {
		var ca CA
		var enabled int
		err := r.Scan(&ca.ID, &ca.Name, &ca.Kind, &ca.CertificatePEM, &ca.CRLPEM, &ca.CRLNumber, &enabled)
		ca.Enabled = enabled == 1
		s.CAs = append(s.CAs, ca)
		return err
	})
	if err != nil {
		return s, err
	}
	err = each(ctx, db, `SELECT r.certificate_authority_id, r.serial_hex FROM revoked_certificates r
		JOIN certificate_authorities ca ON ca.id = r.certificate_authority_id
		WHERE r.deleted_at IS NULL AND ca.kind = 'ssh' ORDER BY r.certificate_authority_id, r.id`, func(r store.Rows) error {
		var v Revocation
		err := r.Scan(&v.CAID, &v.SerialHex)
		s.SSHRevocations = append(s.SSHRevocations, v)
		return err
	})
	if err != nil {
		return s, err
	}
	err = each(ctx, db, `SELECT uc.user_id, uc.certificate_authority_id, uc.serial_hex, uc.certificate_pem FROM user_certificates uc
		JOIN certificate_authorities ca ON ca.id = uc.certificate_authority_id
		WHERE uc.deleted_at IS NULL AND COALESCE(ca.kind,'x509') = 'x509' ORDER BY uc.user_id`, func(r store.Rows) error {
		var c UserCertificate
		if err := r.Scan(&c.UserID, &c.CAID, &c.SerialHex, &c.CertificatePEM); err != nil {
			return err
		}
		if active[c.UserID] != nil {
			s.UserCertificates = append(s.UserCertificates, c)
		}
		return nil
	})
	return s, err
}
