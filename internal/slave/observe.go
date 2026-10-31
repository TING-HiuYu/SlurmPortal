package slave

import (
	"bufio"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/TING-HiuYu/SlurmPortal/internal/replication"
	"github.com/go-ldap/ldap/v3"
)

// Entry is one directory object with lower-cased attribute names (";binary" options stripped).
type Entry struct {
	DN    string
	Attrs map[string][]string
}

func (e Entry) first(name string) string {
	if v := e.Attrs[name]; len(v) > 0 {
		return v[0]
	}
	return ""
}

func (e Entry) has(class string) bool {
	for _, c := range e.Attrs["objectclass"] {
		if strings.EqualFold(c, class) {
			return true
		}
	}
	return false
}

func normalizeAttr(name string) string {
	name, _, _ = strings.Cut(strings.ToLower(name), ";")
	return name
}

// SearchEntries reads every user and group entry under baseDN (read-only).
func (l *LDAP) SearchEntries(baseDN string) ([]Entry, error) {
	req := ldap.NewSearchRequest(baseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		"(|(objectClass=inetOrgPerson)(objectClass=posixGroup)(objectClass=groupOfUniqueNames)(objectClass=organizationalUnit))", []string{"*"}, nil)
	res, err := l.conn.SearchWithPaging(req, 500)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(res.Entries))
	for _, e := range res.Entries {
		x := Entry{DN: e.DN, Attrs: map[string][]string{}}
		for _, a := range e.Attributes {
			n := normalizeAttr(a.Name)
			if n == "usercertificate" {
				for _, b := range a.ByteValues {
					x.Attrs[n] = append(x.Attrs[n], string(b))
				}
				continue
			}
			x.Attrs[n] = append(x.Attrs[n], a.Values...)
		}
		out = append(out, x)
	}
	return out, nil
}

// ParseLDIF reads slapcat/ldapsearch output: folded lines, "attr:: base64" values, comments.
func ParseLDIF(r io.Reader) ([]Entry, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	var lines []string
	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")
		if strings.HasPrefix(line, " ") && len(lines) > 0 && lines[len(lines)-1] != "" {
			lines[len(lines)-1] += line[1:]
			continue
		}
		lines = append(lines, line)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	var out []Entry
	var cur *Entry
	flush := func() {
		if cur != nil && cur.DN != "" {
			out = append(out, *cur)
		}
		cur = nil
	}
	for i, line := range lines {
		switch {
		case line == "":
			flush()
			continue
		case strings.HasPrefix(line, "#"), strings.HasPrefix(strings.ToLower(line), "version:"):
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("ldif line %d: missing ':'", i+1)
		}
		if strings.HasPrefix(value, ":") {
			b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value[1:]))
			if err != nil {
				return nil, fmt.Errorf("ldif line %d: %w", i+1, err)
			}
			value = string(b)
		} else {
			value = strings.TrimPrefix(value, " ")
		}
		n := normalizeAttr(name)
		if n == "dn" {
			flush()
			cur = &Entry{DN: value, Attrs: map[string][]string{}}
			continue
		}
		if cur == nil {
			return nil, fmt.Errorf("ldif line %d: attribute before dn", i+1)
		}
		cur.Attrs[n] = append(cur.Attrs[n], value)
	}
	flush()
	return out, nil
}

func parseInt(v string) int64 {
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}

// Observed converts directory entries into the snapshot shape Reconcile diffs, mirroring how
// ldap.go writes them, so an unchanged directory plans to zero operations.
func Observed(entries []Entry, baseDN, adminDN string) replication.Snapshot {
	s := replication.Snapshot{Version: replication.Version, BaseDN: baseDN, Users: []replication.User{}, Groups: []replication.Group{}}
	sort.Slice(entries, func(i, j int) bool { return strings.ToLower(entries[i].DN) < strings.ToLower(entries[j].DN) })
	byDN, byName := map[string]int64{}, map[string]int64{}
	var id int64
	for _, e := range entries {
		if !e.has("inetOrgPerson") || strings.EqualFold(e.DN, adminDN) {
			continue
		}
		id++
		u := replication.User{ID: id, DN: e.DN, Username: e.first("uid"), Nickname: e.first("displayname"), GivenName: e.first("givenname"),
			Mail: e.first("mail"), JobNumber: e.first("employeenumber"), Mobile: e.first("mobile"), PostalAddress: e.first("postaladdress"),
			Position: e.first("departmentnumber"), Introduction: e.first("description"), Departments: e.first("businesscategory"),
			PasswordHash: e.first("userpassword")}
		if e.has("posixAccount") {
			u.UIDNumber, u.GIDNumber = parseInt(e.first("uidnumber")), parseInt(e.first("gidnumber"))
			u.HomeDirectory, u.LoginShell = e.first("homedirectory"), e.first("loginshell")
		}
		s.Users = append(s.Users, u)
		byDN[strings.ToLower(e.DN)], byName[u.Username] = id, id
		if der := e.first("usercertificate"); der != "" {
			c := replication.UserCertificate{UserID: id, CertificatePEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte(der)}))}
			if cert, err := x509.ParseCertificate([]byte(der)); err == nil {
				c.SerialHex = hex.EncodeToString(cert.SerialNumber.Bytes())
			}
			s.UserCertificates = append(s.UserCertificates, c)
		}
	}
	for _, e := range entries {
		if strings.EqualFold(e.DN, baseDN) || e.has("inetOrgPerson") {
			continue
		}
		rdn, _, _ := strings.Cut(e.DN, ",")
		typ, name, _ := strings.Cut(rdn, "=")
		g := replication.Group{DN: e.DN, Type: strings.ToLower(typ), Name: name, Remark: e.first("description")}
		switch {
		case e.has("organizationalUnit"):
			g.Type = "ou"
		case e.has("posixGroup"):
			g.Class, g.GIDNumber = "posixGroup", parseInt(e.first("gidnumber"))
		case e.has("groupOfUniqueNames"):
			g.Class = "groupOfUniqueNames"
		default:
			continue
		}
		id++
		g.ID = id
		s.Groups = append(s.Groups, g)
		for _, m := range e.Attrs["memberuid"] {
			if uid, ok := byName[m]; ok {
				s.Memberships = append(s.Memberships, replication.Membership{GroupID: id, UserID: uid})
			}
		}
		for _, m := range e.Attrs["uniquemember"] {
			if uid, ok := byDN[strings.ToLower(m)]; ok {
				s.Memberships = append(s.Memberships, replication.Membership{GroupID: id, UserID: uid})
			}
		}
	}
	return s
}
