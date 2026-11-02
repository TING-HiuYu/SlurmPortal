//go:build !js

// apidoc writes docs/dev/api-reference.md: every HTTP route of the master with its method, path,
// permission and owning package, read from the real router with all plugins enabled.
//
//	go run ./tools/apidoc            # rewrite the file
//	go run ./tools/apidoc -check     # fail if the file is out of date (CI)
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/TING-HiuYu/SlurmPortal/internal/app"
	"github.com/TING-HiuYu/SlurmPortal/internal/config"
	"github.com/TING-HiuYu/SlurmPortal/internal/httpx"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	_ "github.com/TING-HiuYu/SlurmPortal/internal/plugins/all"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

// zhOut is where the Simplified Chinese version of the page lives for the project site.
const zhOut = "site/i18n/zh-Hans/docusaurus-plugin-content-docs/current/dev/api-reference.md"

func main() {
	check := flag.Bool("check", false, "fail if the files differ from what would be written")
	flag.Parse()
	routes, err := loadRoutes()
	if err != nil {
		fmt.Fprintln(os.Stderr, "apidoc:", err)
		os.Exit(1)
	}
	var missing []string
	for _, r := range routes {
		if _, ok := descriptions[r.Method+" "+r.Path]; !ok {
			missing = append(missing, r.Method+" "+r.Path)
		}
	}
	if len(missing) > 0 {
		fmt.Fprintln(os.Stderr, "apidoc: add a description to tools/apidoc/descriptions.go for:\n  "+strings.Join(missing, "\n  "))
		os.Exit(1)
	}
	stale := false
	for path, body := range map[string][]byte{"docs/dev/api-reference.md": render(routes, en), zhOut: render(routes, zh)} {
		if *check {
			if old, _ := os.ReadFile(path); !bytes.Equal(old, body) {
				fmt.Fprintf(os.Stderr, "apidoc: %s is out of date; run go run ./tools/apidoc\n", path)
				stale = true
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "apidoc:", err)
			os.Exit(1)
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "apidoc:", err)
			os.Exit(1)
		}
		fmt.Println("wrote", path, len(routes), "routes")
	}
	if stale {
		os.Exit(1)
	}
}

// loadRoutes starts the master in memory with every compiled plugin enabled and returns its routes.
func loadRoutes() ([]httpx.Route, error) {
	db, err := sql.Open("sqlite", "file:apidoc?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	for k, v := range map[string]string{
		"SYSTEM_ROLE": "master", "JWT_KEYS": "k:" + key, "JWT_ACTIVE_KID": "k", "PKI_MASTER_KEY": key,
		"LDAP_BASE_DN": "dc=example,dc=org", "LDAP_USER_DN": "ou=people,dc=example,dc=org",
		"PLUGINS_ENABLED": strings.Join(routePlugins(), ","),
		"PLUGINS_CONFIG": `{"cloudflare-access": {"team-domain": "example.cloudflareaccess.com",
			"applications": [{"origin": "https://portal.example.org", "path-prefix": "/", "aud": "apidoc"}]},
			"current-status": {"account-id": "0123456789abcdef0123456789abcdef", "bucket": "apidoc"}}`,
		"PLUGIN_CURRENT_STATUS_R2_ACCESS_KEY_ID": "apidocapidocapidoc", "PLUGIN_CURRENT_STATUS_R2_SECRET_ACCESS_KEY": strings.Repeat("s", 40),
		"PLUGIN_WEB_ACCESS_TICKET_KEY": key,
	} {
		os.Setenv(k, v)
	}
	cfg, err := config.Load("")
	if err != nil {
		return nil, err
	}
	a, err := app.New(context.Background(), cfg, store.SQL{DB: db})
	if err != nil {
		return nil, err
	}
	return a.Router.Routes(), nil
}

// routePlugins is every compiled plugin except the telemetry exporters, which add no routes.
func routePlugins() []string {
	var ids []string
	for _, id := range plugin.Compiled() {
		if !strings.HasPrefix(id, "telemetry-") {
			ids = append(ids, id)
		}
	}
	return ids
}

// owner is the package that registered the route, taken from its handler's function name.
func owner(r httpx.Route) string {
	name := runtime.FuncForPC(reflect.ValueOf(r.Handler).Pointer()).Name()
	name = strings.TrimPrefix(name, "github.com/TING-HiuYu/SlurmPortal/")
	if i := strings.Index(name, "."); i > 0 {
		name = name[:i]
	}
	return name
}
