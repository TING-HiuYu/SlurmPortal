package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

func (g *Gateway) configurationError() string {
	if g.cfg.MasterOrigin == "" {
		return ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.registrationError == "config_mismatch" {
		return "config_mismatch"
	}
	if g.lastRegistered.IsZero() || time.Since(g.lastRegistered) > 180*time.Second {
		return "registration_unavailable"
	}
	return ""
}

// Capabilities are the web-access features this gateway build serves; master records them at registration.
var Capabilities = []string{"web-terminal", "web-files", "file-transfer"}

func (g *Gateway) registrationLoop() {
	defer g.wg.Done()
	g.register()
	timer := time.NewTicker(time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-g.ctx.Done():
			return
		case <-timer.C:
			g.register()
		}
	}
}
func (g *Gateway) register() {
	plugins := Capabilities
	caKeys, e := g.cfg.trustedUserCAs()
	if e != nil {
		g.setRegistrationFailure("user_ca_bundle_unavailable")
		return
	}
	publicCAKeys := make([]string, 0, len(caKeys))
	for _, key := range caKeys {
		publicCAKeys = append(publicCAKeys, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))))
	}
	raw, e := json.Marshal(map[string]any{"version": 1, "gateway_id": g.cfg.GatewayID, "instance_id": g.cfg.InstanceID, "boot_id": g.bootID, "cluster_id": g.cfg.ClusterID, "plugins": plugins, "trusted_user_ca_keys": publicCAKeys})
	if e != nil {
		return
	}
	ctx, cancel := context.WithTimeout(g.ctx, 15*time.Second)
	defer cancel()
	req, e := http.NewRequestWithContext(ctx, "POST", g.cfg.MasterOrigin+"/sync/v2/web-access/register", bytes.NewReader(raw))
	if e != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	response, e := g.masterClient.Do(req)
	if e != nil {
		g.setRegistrationFailure("registration_failed")
		return
	}
	defer func() { _ = response.Body.Close() }()
	var result struct {
		Code int `json:"code"`
		Data struct {
			Clusters []struct {
				ID           string `json:"id"`
				Enabled      bool   `json:"enabled"`
				GatewayID    string `json:"gateway_id"`
				Address      string `json:"address"`
				SSHHost      string `json:"ssh_host"`
				SSHPort      int    `json:"ssh_port"`
				SSHLocalPort int    `json:"ssh_local_port"`
			} `json:"clusters"`
			VerificationKeys []struct {
				Kid string `json:"kid"`
				Kty string `json:"kty"`
				Crv string `json:"crv"`
				X   string `json:"x"`
			} `json:"verification_keys"`
		} `json:"data"`
	}
	if response.StatusCode != 200 || json.NewDecoder(io.LimitReader(response.Body, 262144)).Decode(&result) != nil || result.Code != 0 {
		g.setRegistrationFailure("registration_failed")
		return
	}
	if len(result.Data.Clusters) > 64 || len(result.Data.VerificationKeys) > 8 {
		g.setRegistrationFailure("config_mismatch")
		return
	}
	source := false
	matched := map[string]bool{}
	for _, v := range result.Data.Clusters {
		local, exists := g.cfg.cluster(v.ID)
		if !exists {
			continue
		}
		host := v.Address
		port := v.SSHPort
		if v.ID == g.cfg.ClusterID {
			host = v.SSHHost
			if v.SSHLocalPort > 0 {
				port = v.SSHLocalPort
			}
			source = v.GatewayID == g.cfg.GatewayID
		}
		if !v.Enabled || host == "" || local.SSHAddress != net.JoinHostPort(host, strconv.Itoa(port)) {
			g.setRegistrationFailure("config_mismatch")
			return
		}
		matched[v.ID] = true
	}
	if !source || len(matched) != len(g.cfg.Clusters) {
		g.setRegistrationFailure("config_mismatch")
		return
	}
	if len(result.Data.VerificationKeys) > 0 {
		for _, k := range result.Data.VerificationKeys {
			key, exists := g.cfg.parsedKeys[k.Kid]
			if !exists || k.Kty != "OKP" || k.Crv != "Ed25519" || base64.RawURLEncoding.EncodeToString(key) != k.X {
				g.setRegistrationFailure("config_mismatch")
				return
			}
		}
	}
	g.mu.Lock()
	g.lastRegistered = time.Now()
	g.registrationError = ""
	g.mu.Unlock()
}
func (g *Gateway) setRegistrationFailure(code string) {
	g.mu.Lock()
	g.registrationError = code
	g.mu.Unlock()
}
