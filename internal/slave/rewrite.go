package slave

import (
	"fmt"
	"strings"

	ldap "github.com/go-ldap/ldap/v3"

	"github.com/TING-HiuYu/SlurmPortal/internal/replication"
)

// RewriteDNs maps master's directory suffix onto this slave's suffix; top-level cn= groups and their
// subtrees may be relocated under localGroupDN. Numeric identities are never changed.
func RewriteDNs(src replication.Snapshot, sourceBase, localBase, localGroupDN string) (replication.Snapshot, error) {
	from, err := ldap.ParseDN(sourceBase)
	if err != nil || len(from.RDNs) == 0 {
		return replication.Snapshot{}, fmt.Errorf("invalid source base DN")
	}
	to, err := ldap.ParseDN(localBase)
	if err != nil || len(to.RDNs) == 0 {
		return replication.Snapshot{}, fmt.Errorf("invalid local base DN")
	}
	if localGroupDN != "" {
		gb, err := ldap.ParseDN(localGroupDN)
		if err != nil || !to.AncestorOfFold(gb) {
			return replication.Snapshot{}, fmt.Errorf("local group DN must be below local base DN")
		}
	}
	rewrite := func(value string, group bool) (string, error) {
		if value == "" {
			return "", nil
		}
		parsed, err := ldap.ParseDN(value)
		if err != nil || (!from.EqualFold(parsed) && !from.AncestorOfFold(parsed)) {
			return "", fmt.Errorf("DN %q is outside source base", value)
		}
		if !strings.HasSuffix(strings.ToLower(value), strings.ToLower(sourceBase)) {
			return "", fmt.Errorf("DN %q uses a noncanonical source suffix", value)
		}
		prefix := value[:len(value)-len(sourceBase)]
		if prefix == "" {
			return localBase, nil
		}
		if !strings.HasSuffix(prefix, ",") {
			return "", fmt.Errorf("DN %q has an invalid suffix boundary", value)
		}
		dest := localBase
		if group && localGroupDN != "" {
			root := parsed.RDNs[len(parsed.RDNs)-len(from.RDNs)-1]
			if len(root.Attributes) == 1 && strings.EqualFold(root.Attributes[0].Type, "cn") {
				dest = localGroupDN
			}
		}
		return prefix + dest, nil
	}
	out := src
	out.BaseDN = localBase
	out.Users = append([]replication.User(nil), src.Users...)
	out.Groups = append([]replication.Group(nil), src.Groups...)
	seen := map[string]string{}
	for i := range out.Users {
		before := out.Users[i].DN
		after, err := rewrite(before, false)
		if err != nil {
			return replication.Snapshot{}, err
		}
		if prev, dup := seen[strings.ToLower(after)]; dup && !strings.EqualFold(prev, before) {
			return replication.Snapshot{}, fmt.Errorf("two entries map to %s", after)
		}
		seen[strings.ToLower(after)] = before
		out.Users[i].DN = after
	}
	for i := range out.Groups {
		before := out.Groups[i].DN
		after, err := rewrite(before, true)
		if err != nil {
			return replication.Snapshot{}, err
		}
		if prev, dup := seen[strings.ToLower(after)]; dup && !strings.EqualFold(prev, before) {
			return replication.Snapshot{}, fmt.Errorf("two entries map to %s", after)
		}
		seen[strings.ToLower(after)] = before
		out.Groups[i].DN = after
	}
	return out, nil
}
