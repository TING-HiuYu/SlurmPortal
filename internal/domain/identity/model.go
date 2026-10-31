package identity

import (
	"errors"
	"fmt"
	"net/mail"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	MinPOSIXID = 10000
	MaxPOSIXID = 2147483647

	ClassPOSIX  = "posixGroup"
	ClassUnique = "groupOfUniqueNames"
	TypeCN      = "cn"
	TypeOU      = "ou"
)

var (
	usernamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,49}$`)
	shellPattern    = regexp.MustCompile(`^/[A-Za-z0-9_./-]+$`)
	keywordPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,19}$`)
)

// User is an account as the API returns it.
type User struct {
	ID            int64   `json:"id"`
	Username      string  `json:"username"`
	Nickname      string  `json:"nickname"`
	GivenName     string  `json:"givenName"`
	Mail          string  `json:"mail"`
	JobNumber     string  `json:"jobNumber"`
	Mobile        string  `json:"mobile"`
	UIDNumber     int64   `json:"uidNumber"`
	GIDNumber     int64   `json:"gidNumber"`
	HomeDirectory string  `json:"homeDirectory"`
	LoginShell    string  `json:"loginShell"`
	Avatar        string  `json:"avatar"`
	PostalAddress string  `json:"postalAddress"`
	Position      string  `json:"position"`
	Introduction  string  `json:"introduction"`
	Status        int     `json:"status"`
	Source        string  `json:"source"`
	Creator       string  `json:"creator"`
	UserDN        string  `json:"userDn"`
	GroupIDs      []int64 `json:"groupIds"`
	RoleIDs       []int64 `json:"roleIds"`
	CreatedAt     string  `json:"createdAt"`
	UpdatedAt     string  `json:"updatedAt"`
}

// Group is a group as the API returns it.
type Group struct {
	ID         int64  `json:"id"`
	Name       string `json:"groupName"`
	Remark     string `json:"remark"`
	Type       string `json:"groupType"`
	Class      string `json:"groupClass"`
	GIDNumber  int64  `json:"gidNumber"`
	ParentID   int64  `json:"parentId"`
	DN         string `json:"groupDn"`
	Source     string `json:"source"`
	Creator    string `json:"creator"`
	CreatedAt  string `json:"createdAt"`
	MemberSize int64  `json:"memberCount"`
	// Presets of accounts whose main group this POSIX group is: home is HomeRoot/<username>.
	HomeRoot   string `json:"homeRoot"`
	LoginShell string `json:"loginShell"`
}

// IsPOSIX reports whether the group is a posixGroup (it has a GID and memberUid entries).
func (g Group) IsPOSIX() bool { return strings.EqualFold(g.Class, ClassPOSIX) }

// Role is a named set of permissions. Sort is its rank: a lower value outranks a higher one.
type Role struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Keyword string `json:"keyword"`
	Remark  string `json:"remark"`
	Status  int    `json:"status"`
	Sort    int    `json:"sort"`
	Creator string `json:"creator"`
}

func validText(field, v string, max int) error {
	if !utf8.ValidString(v) || utf8.RuneCountInString(v) > max || strings.ContainsAny(v, "\x00\r\n") {
		return fmt.Errorf("%s is invalid or longer than %d characters", field, max)
	}
	return nil
}

// ValidUsername checks that u is a safe POSIX login name.
func ValidUsername(u string) error {
	if !usernamePattern.MatchString(u) {
		return errors.New("username must start with a letter or digit and contain only letters, digits, '.', '_' or '-' (max 50)")
	}
	return nil
}

// ValidShell checks that s is a clean absolute path.
func ValidShell(s string) error {
	if len(s) > 64 || s == "/" || !shellPattern.MatchString(s) || path.Clean(s) != s {
		return errors.New("login shell must be a clean absolute path")
	}
	return nil
}

func validHome(h string) error {
	if len(h) > 255 || len(h) < 2 || h[0] != '/' || path.Clean(h) != h || strings.ContainsAny(h, "\x00\r\n:") {
		return errors.New("home directory must be a clean absolute path")
	}
	return nil
}

func validMail(m string) error {
	if m == "" {
		return nil
	}
	a, err := mail.ParseAddress(m)
	if err != nil || a.Address != m || len(m) > 100 {
		return errors.New("mail is not a valid address")
	}
	return nil
}

func validPOSIX(id int64, field string) error {
	if id != 0 && (id < MinPOSIXID || id > MaxPOSIXID) {
		return fmt.Errorf("%s must be within [%d, %d]", field, MinPOSIXID, MaxPOSIXID)
	}
	return nil
}

// EscapeDNValue escapes an attribute value per RFC 4514 so names cannot inject DN components.
func EscapeDNValue(v string) string {
	var sb strings.Builder
	for i, r := range v {
		switch {
		case strings.ContainsRune(`,+"\<>;=`, r),
			i == 0 && (r == ' ' || r == '#'),
			i == len(v)-1 && r == ' ':
			sb.WriteByte('\\')
			sb.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&sb, "\\%02x", r)
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}
