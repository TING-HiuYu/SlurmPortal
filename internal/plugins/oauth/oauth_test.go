package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/TING-HiuYu/SlurmPortal/internal/app"
	"github.com/TING-HiuYu/SlurmPortal/internal/audit"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/config"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
	"github.com/TING-HiuYu/SlurmPortal/internal/testutil"
)

func init() { plugin.Register(Plugin{}) }

func fakeGitHub(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			_ = r.ParseForm()
			if r.PostForm.Get("code") != "good" || r.PostForm.Get("client_secret") != "s3cret" {
				_, _ = w.Write([]byte(`{"error":"bad_verification_code"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"gho_x"}`))
		case "/user":
			if r.Header.Get("Authorization") != "Bearer gho_x" {
				w.WriteHeader(401)
				return
			}
			_, _ = w.Write([]byte(`{"id":42,"login":"octo cat","name":"Octo","avatar_url":"https://a/x.png"}`))
		case "/user/orgs":
			_, _ = w.Write([]byte(`[{"login":"Example-Org"},{"login":"other-org"}]`))
		case "/user/emails":
			_, _ = w.Write([]byte(`[{"email":"other@example.com","primary":false,"verified":true},{"email":"Octo@Example.com","primary":true,"verified":true}]`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	oldA, oldT, oldAPI := githubAuthorize, githubToken, githubAPI
	githubAuthorize, githubToken, githubAPI = srv.URL+"/authorize", srv.URL+"/token", srv.URL
	t.Cleanup(func() { githubAuthorize, githubToken, githubAPI = oldA, oldT, oldAPI })
}

type env struct {
	t   *testing.T
	app *app.App
	db  store.Store
}

func (e env) do(method, path string, userID int64, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:5000"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if userID > 0 {
		tok, _, _, err := e.app.JWT.Issue(userID, authn.SourcePassword)
		if err != nil {
			e.t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	e.app.Handler.ServeHTTP(rec, req)
	return rec
}

func setup(t *testing.T) env {
	fakeGitHub(t)
	db := testutil.DB(t)
	testutil.User(t, db, "admin", 1, "admin")
	testutil.User(t, db, "bob", 1, "user")
	ctx := context.Background()
	cfg := &config.Config{
		Role:         "master",
		HTTP:         config.HTTP{RatePerSecond: 1000, RateBurst: 1000, CORSOrigins: []string{"https://ui.example"}},
		Auth:         config.Auth{Issuer: "i", Audience: "a", TokenTTL: 3600e9},
		Audit:        config.Audit{Enabled: true},
		Plugins:      config.Plugins{Enabled: []string{ID}},
		PKIMasterKey: base64.StdEncoding.EncodeToString(make([]byte, 32)),
		JWTKeys:      authn.Keyring{ActiveKID: "k", Keys: map[string][]byte{"k": []byte("0123456789abcdef0123456789abcdef")}},
	}
	a, err := app.New(ctx, cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	// Only the row's kind/enabled matter to the connector flow.
	if _, err := db.Exec(ctx, "INSERT INTO certificate_authorities(id, name, kind, certificate_pem, enabled) VALUES (7, 'ssh', 'ssh', 'x', 1), (8, 'tls', 'x509', 'x', 1)"); err != nil {
		t.Fatal(err)
	}
	// The main group new accounts land in; its presets give them home and shell.
	if _, err := db.Exec(ctx, "INSERT INTO `groups` (id, group_name, group_type, group_class, group_dn, gid_number, home_root, login_shell) VALUES (60, 'sshusers', 'cn', 'posixGroup', 'cn=sshusers,dc=x', 20000, '/HOME/sshusers', '/bin/bash')"); err != nil {
		t.Fatal(err)
	}
	return env{t, a, db}
}

const connectorJSON = `{"name":"GitHub","provider":"github","caId":7,"enabled":true,"groupId":60,"defaultRoleIds":[%s],
	"clientId":"cid","redirectUri":"https://api.example/api/v2/oauth/callback/github","clientSecret":"s3cret"}`

func connector(roles string) string { return strings.Replace(connectorJSON, "%s", roles, 1) }

func callbackResult(t *testing.T, rec *httptest.ResponseRecorder) (result, string) {
	t.Helper()
	body := rec.Body.String()
	i, j := strings.Index(body, "var d="), strings.Index(body, ",o=")
	if i < 0 || j < i {
		t.Fatalf("callback page %d: %s", rec.Code, body)
	}
	var res result
	if err := json.Unmarshal([]byte(body[i+len("var d="):j]), &res); err != nil {
		t.Fatalf("decode %q: %v", body[i:j], err)
	}
	return res, body[j+3 : j+3+strings.Index(body[j+3:], ";")]
}

func (e env) login(id string) (result, string) {
	e.t.Helper()
	rec := e.do("GET", "/api/v2/oauth/authorize/"+id, 0, "", nil)
	if rec.Code != http.StatusFound {
		e.t.Fatalf("authorize %d: %s", rec.Code, rec.Body.String())
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	state := loc.Query().Get("state")
	if loc.Query().Get("client_id") != "cid" || state == "" {
		e.t.Fatalf("bad authorize redirect %s", loc)
	}
	ck := rec.Result().Cookies()[0]
	if ck.Name != cookieName || !ck.HttpOnly || !ck.Secure || ck.Value != state {
		e.t.Fatalf("bad state cookie %+v", ck)
	}
	return callbackResult(e.t, e.do("GET", "/api/v2/oauth/callback/github?code=good&state="+state, 0, "", ck))
}

func TestConnectorAdmin(t *testing.T) {
	e := setup(t)
	if rec := e.do("POST", "/api/v2/oauth/connectors", 2, connector(""), nil); rec.Code != 403 {
		t.Fatalf("non-admin create: %d", rec.Code)
	}
	for _, bad := range []string{
		strings.Replace(connector(""), `"caId":7`, `"caId":8`, 1),
		strings.Replace(connector(""), "api.example/api/v2/oauth/callback/github", "evil.example/cb", 1),
		strings.Replace(connector(""), "s3cret", "env:lower", 1),
		connector("999"),
	} {
		if rec := e.do("POST", "/api/v2/oauth/connectors", 1, bad, nil); rec.Code != 400 {
			t.Fatalf("invalid connector accepted (%d): %s", rec.Code, rec.Body.String())
		}
	}
	rec := e.do("POST", "/api/v2/oauth/connectors", 1, connector(""), nil)
	if rec.Code != 201 || strings.Contains(rec.Body.String(), "s3cret") || !strings.Contains(rec.Body.String(), `"clientSecretSet":true`) {
		t.Fatalf("create %d: %s", rec.Code, rec.Body.String())
	}
	var stored string
	_ = store.QueryRow(context.Background(), e.db, "SELECT config FROM oauth_connectors WHERE id = 1", nil, &stored)
	if strings.Contains(stored, "s3cret") || !strings.Contains(stored, `"clientSecret":"sb1:`) {
		t.Fatalf("secret not sealed at rest: %s", stored)
	}
	// Updating without clientSecret keeps the sealed secret; env references are kept verbatim.
	upd := strings.Replace(strings.Replace(connector(""), `,"clientSecret":"s3cret"`, "", 1), `"GitHub"`, `"GH"`, 1)
	if rec := e.do("PUT", "/api/v2/oauth/connectors/1", 1, upd, nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"clientSecretSet":true`) {
		t.Fatalf("update %d: %s", rec.Code, rec.Body.String())
	}
	if rec := e.do("GET", "/api/v2/oauth/connectors/public", 0, "", nil); !strings.Contains(rec.Body.String(), `"name":"GH"`) || strings.Contains(rec.Body.String(), "clientId") {
		t.Fatalf("public list: %s", rec.Body.String())
	}
	if rec := e.do("DELETE", "/api/v2/oauth/connectors/1", 1, "", nil); rec.Code != 204 {
		t.Fatalf("delete %d", rec.Code)
	}
	if rec := e.do("GET", "/api/v2/oauth/authorize/1", 0, "", nil); rec.Code != 404 {
		t.Fatalf("deleted connector authorize: %d", rec.Code)
	}
}

func TestLoginProvisionsThenLinks(t *testing.T) {
	e := setup(t)
	if rec := e.do("POST", "/api/v2/oauth/connectors", 1, connector(strconv.FormatInt(testutil.RoleID(t, e.db, "user"), 10)), nil); rec.Code != 201 {
		t.Fatalf("create %d: %s", rec.Code, rec.Body.String())
	}
	res, origin := e.login("1")
	if res.Error != nil || res.Token == "" || res.InitialPassword == "" || origin != `"https://ui.example"` {
		t.Fatalf("first login: %+v origin %s", res, origin)
	}
	var username, mail, source, sub, home, shell string
	var gid int64
	_ = store.QueryRow(context.Background(), e.db, "SELECT username, mail, source, source_user_id, gid_number, home_directory, login_shell FROM users WHERE id = 3", nil, &username, &mail, &source, &sub, &gid, &home, &shell)
	if username != "octo-cat" || mail != "octo@example.com" || source != "oauth-github-1" || sub != "42" {
		t.Fatalf("provisioned %s %s %s %s", username, mail, source, sub)
	}
	if gid != 20000 || home != "/HOME/sshusers/octo-cat" || shell != "/bin/bash" {
		t.Fatalf("main group presets: gid %d home %q shell %q", gid, home, shell)
	}
	req := httptest.NewRequest("GET", "/api/v2/me", nil)
	req.Header.Set("Authorization", "Bearer "+res.Token)
	rec := httptest.NewRecorder()
	e.app.Handler.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"octo-cat"`) {
		t.Fatalf("token unusable: %d %s", rec.Code, rec.Body.String())
	}

	again, _ := e.login("1")
	if again.Error != nil || again.InitialPassword != "" {
		t.Fatalf("second login should link without a new password: %+v", again)
	}
	var n int
	_ = store.QueryRow(context.Background(), e.db, "SELECT COUNT(*) FROM users", nil, &n)
	if n != 3 {
		t.Fatalf("users = %d", n)
	}

	// A user disabled after provisioning cannot sign in.
	_, _ = e.db.Exec(context.Background(), "UPDATE users SET status = 2 WHERE id = 3")
	if res, _ := e.login("1"); res.Error == nil || res.Error.Code != "account_disabled" || res.Token != "" {
		t.Fatalf("disabled login: %+v", res)
	}

	var recs []audit.Record
	_ = audit.SQLStore{DB: e.db}.Scan(context.Background(), func(r audit.Record) error {
		if strings.HasPrefix(r.Action, "GET /api/v2/oauth/callback") {
			recs = append(recs, r)
		}
		return nil
	})
	if len(recs) != 3 || recs[0].Target != "user:3" || recs[0].Outcome != audit.OutcomeSuccess || recs[2].Outcome != audit.OutcomeDenied {
		t.Fatalf("callback audit: %+v", recs)
	}
}

func TestCallbackRejectsForgedOrReplayedState(t *testing.T) {
	e := setup(t)
	e.do("POST", "/api/v2/oauth/connectors", 1, connector(""), nil)
	rec := e.do("GET", "/api/v2/oauth/authorize/1", 0, "", nil)
	loc, _ := url.Parse(rec.Header().Get("Location"))
	state, ck := loc.Query().Get("state"), rec.Result().Cookies()[0]
	cb := "/api/v2/oauth/callback/github?code=good&state=" + state

	if res, _ := callbackResult(t, e.do("GET", cb, 0, "", nil)); res.Error == nil || res.Error.Code != "invalid_state" {
		t.Fatalf("missing cookie (login CSRF) accepted: %+v", res)
	}
	if res, _ := callbackResult(t, e.do("GET", "/api/v2/oauth/callback/other?code=good&state="+state, 0, "", ck)); res.Error == nil {
		t.Fatalf("provider mismatch accepted: %+v", res)
	}
	// The mismatched attempt consumed the state: one browser round trip, one use.
	if res, _ := callbackResult(t, e.do("GET", cb, 0, "", ck)); res.Error == nil || res.Error.Code != "invalid_state" {
		t.Fatalf("replayed state accepted: %+v", res)
	}
	rec = e.do("GET", cb, 0, "", ck)
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'nonce-") || strings.Contains(csp, "unsafe-inline") {
		t.Fatalf("callback CSP %q", csp)
	}
}

func TestConnectorRules(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if _, err := e.db.Exec(ctx, "INSERT INTO `groups` (id, group_name, group_type, group_class, group_dn, gid_number, home_root, login_shell) VALUES (50, 'gh-members', 'cn', 'groupOfUniqueNames', 'cn=gh-members,dc=x', 0, '', ''), (51, 'lab-a', 'cn', 'posixGroup', 'cn=lab-a,dc=x', 20001, '', ''), (52, 'lab-b', 'cn', 'posixGroup', 'cn=lab-b,dc=x', 20002, '/data/lab-b', '/bin/zsh')"); err != nil {
		t.Fatal(err)
	}
	g := struct{ Data struct{ ID int64 } }{}
	g.Data.ID = 50

	// A new connector gets one match-anyone rule; its main group is required (here the legacy groupId).
	if rec := e.do("POST", "/api/v2/oauth/connectors", 1, strings.Replace(connector(""), `"groupId":60,`, "", 1), nil); rec.Code != 400 || !strings.Contains(rec.Body.String(), "needs a main group") {
		t.Fatalf("connector without a main group: %d %s", rec.Code, rec.Body.String())
	}
	rec := e.do("POST", "/api/v2/oauth/connectors", 1, connector(""), nil)
	if rec.Code != 201 || !strings.Contains(rec.Body.String(), `"rules":[{"name":"所有用户","match":"any","values":[],"mainGroupId":60,"groupIds":[]}]`) {
		t.Fatalf("default rule %d %s", rec.Code, rec.Body.String())
	}
	withRules := func(rules string) string {
		return strings.Replace(connector(""), `"enabled":true`, `"enabled":true,"rules":`+rules, 1)
	}
	for _, bad := range []string{`[{"match":"any","groupIds":[51,52]}]`, `[]`, `[{"match":"email","values":[]}]`, `[{"match":"regex","values":["x"]}]`, `[{"match":"email","values":["[x"]}]`, `[{"match":"any","groupIds":[0]}]`,
		`[{"match":"any"}]`, `[{"match":"any","mainGroupId":50}]`, `[{"match":"any","mainGroupId":51,"groupIds":[52]}]`, `[{"match":"any","mainGroupId":999}]`} {
		if rec := e.do("PUT", "/api/v2/oauth/connectors/1", 1, withRules(bad), nil); rec.Code != 400 {
			t.Fatalf("rules %s accepted: %d", bad, rec.Code)
		}
	}
	// No rule matches: refused, and no account is created.
	if rec := e.do("PUT", "/api/v2/oauth/connectors/1", 1, withRules(`[{"match":"email","values":["*@corp.example"],"mainGroupId":51}]`), nil); rec.Code != 200 {
		t.Fatalf("put %d %s", rec.Code, rec.Body.String())
	}
	if res, _ := e.login("1"); res.Error == nil || res.Error.Code != "not_permitted" {
		t.Fatalf("unmatched login: %+v", res)
	}
	var n int
	_ = store.QueryRow(ctx, e.db, "SELECT COUNT(*) FROM users", nil, &n)
	if n != 2 {
		t.Fatalf("account created for refused identity: %d", n)
	}
	// Org rule: the authorize URL asks for read:org and the first matching rule places the account.
	rules := `[{"name":"org","match":"org","values":["example-org"],"mainGroupId":52,"groupIds":[` + strconv.FormatInt(g.Data.ID, 10) + `]},{"match":"any","mainGroupId":51}]`
	if rec := e.do("PUT", "/api/v2/oauth/connectors/1", 1, withRules(rules), nil); rec.Code != 200 {
		t.Fatalf("put %d %s", rec.Code, rec.Body.String())
	}
	auth := e.do("GET", "/api/v2/oauth/authorize/1", 0, "", nil)
	if loc, _ := url.Parse(auth.Header().Get("Location")); !strings.Contains(loc.Query().Get("scope"), "read:org") {
		t.Fatalf("scope %s", loc)
	}
	if res, _ := e.login("1"); res.Error != nil {
		t.Fatalf("org login: %+v", res)
	}
	var member int
	_ = store.QueryRow(ctx, e.db, "SELECT COUNT(*) FROM group_users WHERE group_id = ? AND user_id = 3", []any{g.Data.ID}, &member)
	if member != 1 {
		t.Fatalf("matched rule did not place the account in its group")
	}
	var gid int64
	var home, shell string
	_ = store.QueryRow(ctx, e.db, "SELECT gid_number, home_directory, login_shell FROM users WHERE id = 3", nil, &gid, &home, &shell)
	if gid != 20002 || home != "/data/lab-b/octo-cat" || shell != "/bin/zsh" {
		t.Fatalf("main group presets: gid %d home %q shell %q", gid, home, shell)
	}
}
