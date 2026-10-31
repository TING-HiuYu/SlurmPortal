package authz

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

const (
	ActionRead   = "read"
	ActionCreate = "create"
	ActionUpdate = "update"
	ActionDelete = "delete"
	ActionSync   = "sync"
	ActionAll    = "*"
)

var actions = []string{ActionRead, ActionCreate, ActionUpdate, ActionDelete, ActionSync, ActionAll}

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Validate checks that role, object and action are well-formed names ("*" is allowed as object).
func (r Rule) Validate() error {
	if !namePattern.MatchString(r.Role) || (r.Object != "*" && !namePattern.MatchString(r.Object)) {
		return fmt.Errorf("invalid rule %+v", r)
	}
	if !slices.Contains(actions, r.Action) {
		return fmt.Errorf("invalid action %q", r.Action)
	}
	return nil
}

// LoadRules reads every role permission from v2_role_permissions.
func LoadRules(ctx context.Context, q store.Querier) ([]Rule, error) {
	rows, err := q.Query(ctx, "SELECT role, object, action FROM v2_role_permissions ORDER BY role, object, action")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		var r Rule
		if err := rows.Scan(&r.Role, &r.Object, &r.Action); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetRoleRules atomically replaces every rule of one role.
func SetRoleRules(ctx context.Context, s store.Store, role string, rules []Rule) error {
	stmts := []store.Statement{{Query: "DELETE FROM v2_role_permissions WHERE role = ?", Args: []any{role}}}
	for _, r := range rules {
		if r.Role != role {
			return fmt.Errorf("rule for role %q in update of %q", r.Role, role)
		}
		if err := r.Validate(); err != nil {
			return err
		}
		stmts = append(stmts, store.Statement{
			Query: "INSERT OR IGNORE INTO v2_role_permissions(role, object, action) VALUES (?, ?, ?)",
			Args:  []any{r.Role, r.Object, r.Action},
		})
	}
	_, err := s.Batch(ctx, stmts)
	return err
}

// Legacy paths that become Authenticated self-service or Public routes in v2, or are removed.
var legacyNoPolicy = []string{
	"/base/", "/user/info", "/user/changePwd", "/user/my-shell", "/menu/access/tree",
	"/pki/my-certificate", "/oauth/callback", "/oauth/connectors/public",
	"/user/syncDingTalk", "/user/syncFeiShu", "/user/syncWeCom",
	"/group/syncDingTalk", "/group/syncFeiShu", "/group/syncWeCom",
}

var legacyObjectAlias = map[string]string{
	"log":              "operation-log",
	"fieldrelation":    "field-relation",
	"resource-monitor": "resource",
	"resource-status":  "resource",
	"cluster-status":   "resource",
	"current-status":   "resource",
	"plugins":          "plugin",
	"operation-audit":  "operation-log",
	"sync":             "sync-certificate",
}

// legacyVerb classifies the last path segment of legacy write endpoints; unknown ones are not imported.
var legacyVerb = map[string]string{
	"add": ActionCreate, "generate": ActionCreate, "generate-ssh": ActionCreate, "import": ActionCreate,
	"import-ssh": ActionCreate, "issue": ActionCreate, "connectors": ActionCreate, "temporary-ssh-certificate": ActionCreate,
	"update": ActionUpdate, "changeUserStatus": ActionUpdate, "resetPassword": ActionUpdate, "updatemenus": ActionUpdate,
	"updateapis": ActionUpdate, "assign-user": ActionUpdate, "rotate": ActionUpdate, "adduser": ActionUpdate,
	"removeuser": ActionUpdate, "config": ActionUpdate,
	"delete": ActionDelete, "revoke": ActionDelete, "clean": ActionDelete,
}

// LegacyRule maps one legacy casbin_rule (keyword, path without /api, HTTP method).
// ok=false means the rule has no v2 equivalent and must be reported, not silently granted.
func LegacyRule(role, path, method string) (Rule, bool) {
	for _, p := range legacyNoPolicy {
		if strings.HasPrefix(path, p) {
			return Rule{}, false
		}
	}
	seg, _, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	if alias, ok := legacyObjectAlias[seg]; ok {
		seg = alias
	}
	var action string
	switch strings.ToUpper(method) {
	case "GET", "HEAD":
		action = ActionRead
	case "*":
		action = ActionAll
	default:
		last := path[strings.LastIndex(path, "/")+1:]
		if strings.HasPrefix(last, "sync") {
			action = ActionSync
		} else if action = legacyVerb[last]; action == "" {
			return Rule{}, false
		}
	}
	r := Rule{Role: strings.ToLower(role), Object: strings.ToLower(seg), Action: action}
	return r, r.Validate() == nil
}

const legacyImportMarker = "legacy-casbin-import"

// ImportLegacy seeds v2 rules from casbin_rule exactly once; it returns unmapped legacy entries for review.
func ImportLegacy(ctx context.Context, s store.Store) (imported []Rule, skipped []string, err error) {
	err = store.QueryRow(ctx, s, "SELECT name FROM v2_schema_migrations WHERE name = ?", []any{legacyImportMarker}, new(string))
	if err == nil {
		return nil, nil, nil
	}
	if !errors.Is(err, store.ErrNoRows) {
		return nil, nil, err
	}
	var legacy int
	if err := store.QueryRow(ctx, s, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'casbin_rule'", nil, &legacy); err != nil {
		return nil, nil, err
	}
	if legacy > 0 {
		rows, err := s.Query(ctx, "SELECT v0, v1, v2 FROM casbin_rule WHERE ptype = 'p'")
		if err != nil {
			return nil, nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var role, path, method string
			if err := rows.Scan(&role, &path, &method); err != nil {
				return nil, nil, err
			}
			if r, ok := LegacyRule(role, path, method); ok {
				imported = append(imported, r)
			} else {
				skipped = append(skipped, role+" "+method+" "+path)
			}
		}
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
	}
	imported = collapse(imported)
	stmts := make([]store.Statement, 0, len(imported)+1)
	for _, r := range imported {
		stmts = append(stmts, store.Statement{
			Query: "INSERT OR IGNORE INTO v2_role_permissions(role, object, action) VALUES (?, ?, ?)",
			Args:  []any{r.Role, r.Object, r.Action},
		})
	}
	stmts = append(stmts, store.Statement{Query: "INSERT INTO v2_schema_migrations(name) VALUES (?)", Args: []any{legacyImportMarker}})
	if _, err := s.Batch(ctx, stmts); err != nil {
		return nil, nil, err
	}
	return imported, skipped, nil
}

// collapse sorts and dedupes; it never merges actions, so imports cannot widen privileges.
func collapse(rules []Rule) []Rule {
	slices.SortFunc(rules, func(a, b Rule) int {
		return strings.Compare(a.Role+"\x00"+a.Object+"\x00"+a.Action, b.Role+"\x00"+b.Object+"\x00"+b.Action)
	})
	return slices.Compact(rules)
}
