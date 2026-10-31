package plugin

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
)

var (
	clusterIDPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	fingerprintPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// Cluster is one slave site; its client certificate fingerprint is its only identity.
type Cluster struct {
	ID          string `yaml:"id" json:"id"`
	Name        string `yaml:"name" json:"name"`
	Fingerprint string `yaml:"fingerprint" json:"fingerprint"`
}

// Clusters is the configured list of slave sites.
type Clusters []Cluster

// Validate requires unique IDs and fingerprints.
func (cs Clusters) Validate() error {
	ids, fps := map[string]bool{}, map[string]bool{}
	for i := range cs {
		c := &cs[i]
		c.Fingerprint = strings.ToLower(c.Fingerprint)
		if c.Name == "" {
			c.Name = c.ID
		}
		if !clusterIDPattern.MatchString(c.ID) || !fingerprintPattern.MatchString(c.Fingerprint) || len([]rune(c.Name)) > 200 || ids[c.ID] || fps[c.Fingerprint] {
			return fmt.Errorf("cluster %q: id must match %s, fingerprint must be unique sha256 hex", c.ID, clusterIDPattern)
		}
		ids[c.ID], fps[c.Fingerprint] = true, true
	}
	return nil
}

// FromPeer resolves the cluster of an authenticated peer (TokenID carries the certificate fingerprint).
func (cs Clusters) FromPeer(p *authn.Principal) (Cluster, bool) {
	if p == nil || p.Source != authn.SourcePeer {
		return Cluster{}, false
	}
	for _, c := range cs {
		if c.Fingerprint == strings.ToLower(p.TokenID) {
			return c, true
		}
	}
	return Cluster{}, false
}

// Get returns the cluster with the given ID.
func (cs Clusters) Get(id string) (Cluster, bool) {
	for _, c := range cs {
		if c.ID == id {
			return c, true
		}
	}
	return Cluster{}, false
}
