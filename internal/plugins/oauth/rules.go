package oauth

import (
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
)

// Match kinds of a sign-in rule.
const (
	MatchAny      = "any"      // every verified account
	MatchEmail    = "email"    // verified primary e-mail, glob patterns such as *@example.com
	MatchUsername = "username" // the provider login, glob patterns
	MatchOrg      = "org"      // membership of one of the listed GitHub organisations (needs read:org)
)

// Rule filters who may sign in through a connector and places a new account: MainGroupID is its
// required main POSIX group (whose presets give home and shell), GroupIDs further non-POSIX groups.
// Rules are evaluated in order; the first match wins, and an identity no rule matches is refused.
type Rule struct {
	Name        string   `json:"name"`
	Match       string   `json:"match"`
	Values      []string `json:"values"`
	MainGroupID int64    `json:"mainGroupId"`
	GroupIDs    []int64  `json:"groupIds"`
}

func ruleLabel(r Rule, i int) string {
	if r.Name != "" {
		return strconv.Quote(r.Name)
	}
	return "#" + strconv.Itoa(i+1)
}

const (
	maxRules      = 20
	maxRuleValues = 50
)

// defaultRules is what a connector without stored rules behaves like: anyone, into the legacy group
// if set; saving it requires a main group.
func defaultRules(groupID int64) []Rule {
	r := Rule{Name: "所有用户", Match: MatchAny, Values: []string{}, GroupIDs: []int64{}}
	if groupID > 0 {
		r.GroupIDs = []int64{groupID}
	}
	return []Rule{r}
}

func validateRules(rules []Rule) ([]Rule, error) {
	if len(rules) == 0 || len(rules) > maxRules {
		return nil, apperr.BadRequest("a connector needs 1-20 rules")
	}
	out := make([]Rule, 0, len(rules))
	for _, r := range rules {
		r.Name = strings.TrimSpace(r.Name)
		if len([]rune(r.Name)) > 60 {
			return nil, apperr.BadRequest("rule name is too long")
		}
		vals := []string{}
		for _, v := range r.Values {
			v = strings.ToLower(strings.TrimSpace(v))
			if v == "" {
				continue
			}
			if len(v) > 200 || strings.ContainsAny(v, "\r\n\x00") {
				return nil, apperr.BadRequest("invalid rule value")
			}
			if _, err := path.Match(v, ""); err != nil {
				return nil, apperr.BadRequest("invalid rule pattern " + v)
			}
			vals = append(vals, v)
		}
		switch r.Match {
		case MatchAny:
			vals = []string{}
		case MatchEmail, MatchUsername, MatchOrg:
			if len(vals) == 0 {
				return nil, apperr.BadRequest("rule " + r.Match + " needs at least one value")
			}
		default:
			return nil, apperr.BadRequest("rule match must be any, email, username or org")
		}
		if len(vals) > maxRuleValues || len(r.GroupIDs) > 20 {
			return nil, apperr.BadRequest("too many rule values or groups")
		}
		if r.MainGroupID < 0 {
			return nil, apperr.BadRequest("invalid main group id")
		}
		groups := []int64{}
		for _, g := range r.GroupIDs {
			if g <= 0 {
				return nil, apperr.BadRequest("invalid group id")
			}
			if g != r.MainGroupID && !slices.Contains(groups, g) {
				groups = append(groups, g)
			}
		}
		r.Values, r.GroupIDs = vals, groups
		out = append(out, r)
	}
	return out, nil
}

func needsOrgs(rules []Rule) bool {
	return slices.ContainsFunc(rules, func(r Rule) bool { return r.Match == MatchOrg })
}

func globAny(patterns []string, v string) bool {
	v = strings.ToLower(v)
	for _, p := range patterns {
		if ok, _ := path.Match(p, v); ok {
			return true
		}
	}
	return false
}

// matchRule returns the first rule the profile satisfies.
func matchRule(rules []Rule, p Profile) (Rule, bool) {
	for _, r := range rules {
		var ok bool
		switch r.Match {
		case MatchAny:
			ok = true
		case MatchEmail:
			ok = globAny(r.Values, p.Email)
		case MatchUsername:
			ok = globAny(r.Values, p.Login)
		case MatchOrg:
			ok = slices.ContainsFunc(p.Orgs, func(o string) bool { return slices.Contains(r.Values, strings.ToLower(o)) })
		}
		if ok {
			return r, true
		}
	}
	return Rule{}, false
}
