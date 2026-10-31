package identity_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/app"
	"github.com/TING-HiuYu/SlurmPortal/internal/audit"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/authz"
	"github.com/TING-HiuYu/SlurmPortal/internal/config"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
	"github.com/TING-HiuYu/SlurmPortal/internal/testutil"
)

type harness struct {
	t      *testing.T
	a      *app.App
	db     store.Store
	tokens map[string]string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db := testutil.DB(t)
	ops := testutil.Role(t, db, "ops", 2)
	_ = ops
	for _, r := range []authz.Rule{
		{Role: "ops", Object: "user", Action: "read"}, {Role: "ops", Object: "user", Action: "create"},
		{Role: "ops", Object: "user", Action: "update"}, {Role: "ops", Object: "user", Action: "delete"},
		{Role: "ops", Object: "group", Action: "read"}, {Role: "ops", Object: "group", Action: "create"},
		{Role: "ops", Object: "group", Action: "update"}, {Role: "ops", Object: "group", Action: "delete"},
		{Role: "ops", Object: "role", Action: "read"}, {Role: "ops", Object: "role", Action: "create"},
		{Role: "ops", Object: "role", Action: "update"},
	} {
		if _, err := db.Exec(context.Background(), "INSERT INTO v2_role_permissions(role, object, action) VALUES (?, ?, ?)", r.Role, r.Object, r.Action); err != nil {
			t.Fatal(err)
		}
	}
	h := &harness{t: t, db: db, tokens: map[string]string{}}
	ids := map[string]int64{
		"root":  testutil.User(t, db, "root", 1, "admin"),
		"ops":   testutil.User(t, db, "ops", 1, "ops"),
		"plain": testutil.User(t, db, "plain", 1, "user"),
	}
	cfg := &config.Config{
		Role:      "master",
		HTTP:      config.HTTP{RatePerSecond: 1e6, RateBurst: 1e6},
		Auth:      config.Auth{Issuer: "i", Audience: "a", TokenTTL: time.Hour},
		Audit:     config.Audit{Enabled: true},
		Directory: config.Directory{BaseDN: "dc=test", UserDN: "ou=people,dc=test", DefaultShell: "/bin/bash"},
		JWTKeys:   authn.Keyring{ActiveKID: "k", Keys: map[string][]byte{"k": []byte("0123456789abcdef0123456789abcdef")}},
	}
	a, err := app.New(context.Background(), cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	h.a = a
	for name, id := range ids {
		h.tokens[name] = h.token(id)
	}
	return h
}

func (h *harness) token(id int64) string {
	tok, _, _, err := h.a.JWT.Issue(id, authn.SourcePassword)
	if err != nil {
		h.t.Fatal(err)
	}
	return tok
}

type resp struct {
	Code int
	Data json.RawMessage
	Err  struct {
		Code string `json:"code"`
	}
}

func (h *harness) do(who, method, path string, body any) resp {
	h.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		if s, ok := body.(string); ok {
			b = []byte(s)
		}
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	req.RemoteAddr = "127.0.0.1:1"
	req.Header.Set("Content-Type", "application/json")
	if tok := h.tokens[who]; tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	h.a.Handler.ServeHTTP(rec, req)
	var env struct {
		Data  json.RawMessage `json:"data"`
		Error json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	r := resp{Code: rec.Code, Data: env.Data}
	_ = json.Unmarshal(env.Error, &r.Err)
	return r
}

func (h *harness) must(r resp, code int) resp {
	h.t.Helper()
	if r.Code != code {
		h.t.Fatalf("status %d (%s), want %d", r.Code, r.Err.Code, code)
	}
	return r
}

func decode[T any](t *testing.T, raw json.RawMessage) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return v
}

type group struct {
	ID        int64  `json:"id"`
	GIDNumber int64  `json:"gidNumber"`
	HomeRoot  string `json:"homeRoot"`
	DN        string `json:"groupDn"`
}

type user struct {
	ID            int64   `json:"id"`
	Username      string  `json:"username"`
	UIDNumber     int64   `json:"uidNumber"`
	GIDNumber     int64   `json:"gidNumber"`
	HomeDirectory string  `json:"homeDirectory"`
	LoginShell    string  `json:"loginShell"`
	UserDN        string  `json:"userDn"`
	RoleIDs       []int64 `json:"roleIds"`
	GroupIDs      []int64 `json:"groupIds"`
	Status        int     `json:"status"`
}

func TestGroupLifecycle(t *testing.T) {
	h := newHarness(t)
	h.must(h.do("ops", "POST", "/api/v2/groups", map[string]any{"groupName": "sshusers", "groupType": "cn", "groupClass": "posixGroup"}), 403)
	ssh := decode[group](t, h.must(h.do("root", "POST", "/api/v2/groups", map[string]any{"groupName": "sshusers", "groupType": "cn", "groupClass": "posixGroup"}), 201).Data)
	if ssh.GIDNumber < 10000 || ssh.DN != "cn=sshusers,dc=test" {
		t.Fatalf("posix group = %+v", ssh)
	}
	h.must(h.do("root", "POST", "/api/v2/groups", map[string]any{"groupName": "sshusers", "groupType": "cn", "groupClass": "posixGroup"}), 409)
	h.must(h.do("root", "POST", "/api/v2/groups", map[string]any{"groupName": "child", "groupType": "cn", "parentId": ssh.ID}), 400)

	ou := decode[group](t, h.must(h.do("ops", "POST", "/api/v2/groups", map[string]any{"groupName": "R&D, Lab", "groupType": "ou"}), 201).Data)
	if ou.DN != `ou=R&D\, Lab,dc=test` {
		t.Fatalf("DN not escaped: %q", ou.DN)
	}
	team := decode[group](t, h.must(h.do("ops", "POST", "/api/v2/groups", map[string]any{"groupName": "team", "groupType": "cn", "groupClass": "groupOfUniqueNames", "parentId": ou.ID}), 201).Data)
	if team.DN != `cn=team,ou=R&D\, Lab,dc=test` {
		t.Fatalf("child DN = %q", team.DN)
	}
	h.must(h.do("ops", "DELETE", "/api/v2/groups/"+strconv.FormatInt(ou.ID, 10), nil), 409)
	h.must(h.do("ops", "PATCH", "/api/v2/groups/"+strconv.FormatInt(team.ID, 10), map[string]any{"remark": "updated"}), 200)
	h.must(h.do("ops", "PATCH", "/api/v2/groups/"+strconv.FormatInt(team.ID, 10), map[string]any{"groupName": "rename"}), 400)
	h.must(h.do("ops", "DELETE", "/api/v2/groups/"+strconv.FormatInt(team.ID, 10), nil), 204)
	h.must(h.do("ops", "DELETE", "/api/v2/groups/"+strconv.FormatInt(ou.ID, 10), nil), 204)
	h.must(h.do("plain", "GET", "/api/v2/groups", nil), 403)
	h.must(h.do("", "GET", "/api/v2/groups", nil), 401)
}

func TestUserLifecycle(t *testing.T) {
	h := newHarness(t)
	ssh := decode[group](t, h.must(h.do("root", "POST", "/api/v2/groups", map[string]any{"groupName": "sshusers", "groupType": "cn", "groupClass": "posixGroup", "homeRoot": "/HOME/sshusers"}), 201).Data)
	bare := decode[group](t, h.must(h.do("root", "POST", "/api/v2/groups", map[string]any{"groupName": "bare", "groupType": "cn", "groupClass": "posixGroup"}), 201).Data)
	gpu := decode[group](t, h.must(h.do("root", "POST", "/api/v2/groups", map[string]any{"groupName": "gpu", "groupType": "cn", "groupClass": "posixGroup", "homeRoot": "/data/gpu", "loginShell": "/bin/zsh"}), 201).Data)
	h.must(h.do("root", "POST", "/api/v2/groups", map[string]any{"groupName": "ou-presets", "groupType": "ou", "homeRoot": "/x"}), 400)

	// Every account has a main group, and its home comes from that group's preset.
	h.must(h.do("ops", "POST", "/api/v2/users", map[string]any{"username": "nogroup"}), 400)
	h.must(h.do("ops", "POST", "/api/v2/users", map[string]any{"username": "nohome", "groupIds": []int64{bare.ID}}), 400)
	h.must(h.do("ops", "POST", "/api/v2/users", map[string]any{"username": "own-home", "groupIds": []int64{bare.ID}, "homeDirectory": "/srv/own-home"}), 201)
	g := decode[struct {
		User user `json:"user"`
	}](t, h.must(h.do("ops", "POST", "/api/v2/users", map[string]any{"username": "gina", "gidNumber": gpu.GIDNumber}), 201).Data).User
	if g.HomeDirectory != "/data/gpu/gina" || g.LoginShell != "/bin/zsh" {
		t.Fatalf("preset home/shell = %q %q", g.HomeDirectory, g.LoginShell)
	}
	bareID := "/api/v2/groups/" + strconv.FormatInt(bare.ID, 10)
	h.must(h.do("ops", "PATCH", bareID, map[string]any{"homeRoot": "/HOME/bare"}), 403)
	h.must(h.do("root", "PATCH", bareID, map[string]any{"homeRoot": "relative"}), 400)
	if got := decode[group](t, h.must(h.do("root", "PATCH", bareID, map[string]any{"homeRoot": "/HOME/bare/"}), 200).Data); got.HomeRoot != "/HOME/bare" {
		t.Fatalf("homeRoot = %q", got.HomeRoot)
	}
	h.must(h.do("ops", "POST", "/api/v2/users", map[string]any{"username": "nohome", "groupIds": []int64{bare.ID}}), 201)

	created := decode[struct {
		User            user   `json:"user"`
		InitialPassword string `json:"initialPassword"`
	}](t, h.must(h.do("ops", "POST", "/api/v2/users", map[string]any{"username": "alice", "mail": "alice@example.org", "gidNumber": ssh.GIDNumber}), 201).Data)
	u := created.User
	if created.InitialPassword == "" || u.UIDNumber < 10000 || u.GIDNumber != ssh.GIDNumber ||
		u.HomeDirectory != "/HOME/sshusers/alice" || u.LoginShell != "/bin/bash" || u.UserDN != "uid=alice,ou=people,dc=test" ||
		len(u.RoleIDs) != 1 || u.RoleIDs[0] != testutil.RoleID(t, h.db, "user") {
		t.Fatalf("created = %+v", created)
	}
	login := h.do("", "POST", "/api/v2/auth/login", map[string]string{"username": "alice", "password": created.InitialPassword})
	h.must(login, 200)

	second := decode[struct {
		User user `json:"user"`
	}](t, h.must(h.do("ops", "POST", "/api/v2/users", map[string]any{"username": "bob", "password": "bob-password-1", "gidNumber": ssh.GIDNumber}), 201).Data)
	if second.User.UIDNumber == u.UIDNumber {
		t.Fatal("UID reused")
	}

	for name, body := range map[string]any{
		"duplicate":     map[string]any{"username": "alice", "gidNumber": ssh.GIDNumber},
		"dup mail":      map[string]any{"username": "alice2", "mail": "alice@example.org", "gidNumber": ssh.GIDNumber},
		"bad username":  map[string]any{"username": "-bad"},
		"unknown field": `{"username":"carol","isAdmin":true}`,
		"weak password": map[string]any{"username": "carol", "password": "123"},
		"bad shell":     map[string]any{"username": "carol", "loginShell": "/bin/../sh"},
		"bad uid":       map[string]any{"username": "carol", "uidNumber": 5},
		"admin role":    map[string]any{"username": "carol", "roleIds": []int64{testutil.RoleID(t, h.db, "admin")}},
		"peer role":     map[string]any{"username": "carol", "roleIds": []int64{testutil.RoleID(t, h.db, "ops")}},
	} {
		r := h.do("ops", "POST", "/api/v2/users", body)
		want := map[string]int{"duplicate": 409, "dup mail": 409, "admin role": 403, "peer role": 403}[name]
		if want == 0 {
			want = 400
		}
		if r.Code != want {
			t.Errorf("%s: status %d, want %d", name, r.Code, want)
		}
	}

	rootID := testutil.RoleID(t, h.db, "admin")
	_ = rootID
	opsSelf := h.do("ops", "GET", "/api/v2/me", nil)
	me := decode[struct {
		User user `json:"user"`
	}](t, h.must(opsSelf, 200).Data)
	h.must(h.do("ops", "PATCH", "/api/v2/users/"+strconv.FormatInt(me.User.ID, 10), map[string]any{"roleIds": []int64{rootID}}), 403)
	h.must(h.do("ops", "PATCH", "/api/v2/users/1", map[string]any{"nickname": "x"}), 403)
	h.must(h.do("ops", "PATCH", "/api/v2/users/"+strconv.FormatInt(u.ID, 10), map[string]any{"nickname": "Alice", "loginShell": "/bin/zsh"}), 200)
	h.must(h.do("ops", "PATCH", "/api/v2/users/"+strconv.FormatInt(u.ID, 10), map[string]any{"groupIds": []int64{ssh.ID}}), 403)
	h.must(h.do("root", "PATCH", "/api/v2/users/"+strconv.FormatInt(u.ID, 10), map[string]any{"groupIds": []int64{ssh.ID}}), 200)
	// The main group (gidNumber) stays; editing cannot drop it.
	h.must(h.do("root", "PATCH", "/api/v2/users/"+strconv.FormatInt(u.ID, 10), map[string]any{"groupIds": []int64{}}), 400)

	h.tokens["alice"] = h.token(u.ID)
	h.must(h.do("alice", "PUT", "/api/v2/me/shell", map[string]any{"loginShell": "/usr/bin/fish"}), 204)
	h.must(h.do("alice", "PUT", "/api/v2/me/shell", map[string]any{"loginShell": "relative"}), 400)
	h.must(h.do("alice", "GET", "/api/v2/users", nil), 403)

	h.must(h.do("ops", "PUT", "/api/v2/users/"+strconv.FormatInt(me.User.ID, 10)+"/status", map[string]any{"status": 2}), 403)
	h.must(h.do("ops", "PUT", "/api/v2/users/"+strconv.FormatInt(u.ID, 10)+"/status", map[string]any{"status": 2}), 204)
	h.must(h.do("alice", "GET", "/api/v2/me", nil), 401)
	h.must(h.do("", "POST", "/api/v2/auth/login", map[string]string{"username": "alice", "password": created.InitialPassword}), 401)
	h.must(h.do("ops", "PUT", "/api/v2/users/"+strconv.FormatInt(u.ID, 10)+"/status", map[string]any{"status": 1}), 204)
	h.must(h.do("alice", "GET", "/api/v2/me", nil), 200)

	reset := decode[map[string]string](t, h.must(h.do("ops", "POST", "/api/v2/users/"+strconv.FormatInt(u.ID, 10)+"/password-reset", nil), 200).Data)
	h.must(h.do("", "POST", "/api/v2/auth/login", map[string]string{"username": "alice", "password": reset["password"]}), 200)

	page := decode[struct {
		Items []user `json:"items"`
		Total int    `json:"total"`
	}](t, h.must(h.do("ops", "GET", "/api/v2/users?q=ali&size=10", nil), 200).Data)
	if page.Total != 1 || page.Items[0].Username != "alice" {
		t.Fatalf("search page = %+v", page)
	}
	page = decode[struct {
		Items []user `json:"items"`
		Total int    `json:"total"`
	}](t, h.must(h.do("ops", "GET", "/api/v2/users?q=%25", nil), 200).Data)
	if page.Total != 0 {
		t.Fatal("LIKE wildcard not escaped")
	}
	h.must(h.do("ops", "GET", "/api/v2/users?size=1000", nil), 400)
	page = decode[struct {
		Items []user `json:"items"`
		Total int    `json:"total"`
	}](t, h.must(h.do("ops", "GET", "/api/v2/users?groupId="+strconv.FormatInt(ssh.ID, 10), nil), 200).Data)
	if page.Total != 1 {
		t.Fatalf("group filter total = %d", page.Total)
	}

	h.must(h.do("root", "DELETE", "/api/v2/groups/"+strconv.FormatInt(ssh.ID, 10), nil), 409)
	h.must(h.do("ops", "DELETE", "/api/v2/users/"+strconv.FormatInt(u.ID, 10), nil), 204)
	h.must(h.do("ops", "GET", "/api/v2/users/"+strconv.FormatInt(u.ID, 10), nil), 404)
	h.must(h.do("alice", "GET", "/api/v2/me", nil), 401)

	if n, err := audit.Verify(context.Background(), audit.SQLStore{DB: h.db}); err != nil || n < 10 {
		t.Fatalf("audit chain n=%d err=%v", n, err)
	}
}

func TestMembership(t *testing.T) {
	h := newHarness(t)
	ssh := decode[group](t, h.must(h.do("root", "POST", "/api/v2/groups", map[string]any{"groupName": "sshusers", "groupType": "cn", "groupClass": "posixGroup", "homeRoot": "/HOME/sshusers"}), 201).Data)
	extra := decode[group](t, h.must(h.do("root", "POST", "/api/v2/groups", map[string]any{"groupName": "gpu", "groupType": "cn", "groupClass": "posixGroup"}), 201).Data)
	team := decode[group](t, h.must(h.do("ops", "POST", "/api/v2/groups", map[string]any{"groupName": "team", "groupType": "cn"}), 201).Data)
	u := decode[struct {
		User user `json:"user"`
	}](t, h.must(h.do("ops", "POST", "/api/v2/users", map[string]any{"username": "dave", "gidNumber": ssh.GIDNumber}), 201).Data).User
	_ = ssh
	path := func(g group) string { return "/api/v2/groups/" + strconv.FormatInt(g.ID, 10) + "/members" }
	h.must(h.do("ops", "POST", path(extra), map[string]any{"add": []int64{u.ID}}), 403)
	h.must(h.do("root", "POST", path(extra), map[string]any{"add": []int64{u.ID}}), 204)
	h.must(h.do("ops", "POST", path(team), map[string]any{"add": []int64{u.ID}}), 204)
	h.must(h.do("ops", "POST", path(team), map[string]any{"add": []int64{9999}}), 400)
	h.must(h.do("ops", "POST", path(team), map[string]any{}), 400)
	got := decode[user](t, h.must(h.do("ops", "GET", "/api/v2/users/"+strconv.FormatInt(u.ID, 10), nil), 200).Data)
	if len(got.GroupIDs) != 2 {
		t.Fatalf("groups = %v", got.GroupIDs)
	}
	h.must(h.do("ops", "POST", path(team), map[string]any{"remove": []int64{u.ID}}), 204)
	gs := decode[[]struct {
		ID          int64 `json:"id"`
		MemberCount int64 `json:"memberCount"`
	}](t, h.must(h.do("ops", "GET", "/api/v2/groups", nil), 200).Data)
	for _, g := range gs {
		if g.ID == team.ID && g.MemberCount != 1 {
			t.Fatalf("team members = %d (bootstrap admin only)", g.MemberCount)
		}
	}
}

func TestRolesAndDelegation(t *testing.T) {
	h := newHarness(t)
	h.must(h.do("ops", "POST", "/api/v2/roles", map[string]any{"name": "Peer", "keyword": "peer", "sort": 2}), 403)
	h.must(h.do("ops", "POST", "/api/v2/roles", map[string]any{"name": "Bad", "keyword": "Bad Key", "sort": 5}), 400)
	viewer := decode[struct {
		ID int64 `json:"id"`
	}](t, h.must(h.do("ops", "POST", "/api/v2/roles", map[string]any{"name": "Viewer", "keyword": "viewer", "sort": 5}), 201).Data)
	h.must(h.do("ops", "POST", "/api/v2/roles", map[string]any{"name": "Viewer", "keyword": "viewer2", "sort": 5}), 409)
	perm := "/api/v2/roles/" + strconv.FormatInt(viewer.ID, 10) + "/permissions"
	h.must(h.do("ops", "PUT", perm, map[string]any{"permissions": []map[string]string{{"object": "role", "action": "delete"}}}), 403)
	h.must(h.do("ops", "PUT", perm, map[string]any{"permissions": []map[string]string{{"object": "*", "action": "*"}}}), 403)
	h.must(h.do("ops", "PUT", perm, map[string]any{"permissions": []map[string]string{{"object": "user", "action": "drop"}}}), 400)
	h.must(h.do("ops", "PUT", perm, map[string]any{"permissions": []map[string]string{{"object": "user", "action": "read"}}}), 204)
	ps := decode[[]map[string]string](t, h.must(h.do("ops", "GET", perm, nil), 200).Data)
	if len(ps) != 1 || ps[0]["object"] != "user" {
		t.Fatalf("permissions = %v", ps)
	}

	vid := testutil.User(t, h.db, "vera", 1, "viewer")
	h.tokens["vera"] = h.token(vid)
	h.must(h.do("vera", "GET", "/api/v2/users", nil), 200)
	h.must(h.do("vera", "POST", "/api/v2/users", map[string]any{"username": "x"}), 403)

	opsRole := testutil.RoleID(t, h.db, "ops")
	adminRole := testutil.RoleID(t, h.db, "admin")
	h.must(h.do("ops", "PATCH", "/api/v2/roles/"+strconv.FormatInt(opsRole, 10), map[string]any{"sort": 1}), 403)
	h.must(h.do("root", "PATCH", "/api/v2/roles/"+strconv.FormatInt(adminRole, 10), map[string]any{"status": 2}), 403)
	h.must(h.do("ops", "PATCH", "/api/v2/roles/"+strconv.FormatInt(viewer.ID, 10), map[string]any{"sort": 2}), 403)
	h.must(h.do("ops", "PATCH", "/api/v2/roles/"+strconv.FormatInt(viewer.ID, 10), map[string]any{"remark": "read only"}), 200)
	h.must(h.do("ops", "DELETE", "/api/v2/roles/"+strconv.FormatInt(viewer.ID, 10), nil), 403)
	h.must(h.do("root", "DELETE", "/api/v2/roles/"+strconv.FormatInt(viewer.ID, 10), nil), 409)
	h.must(h.do("root", "DELETE", "/api/v2/users/"+strconv.FormatInt(vid, 10), nil), 204)
	h.must(h.do("root", "DELETE", "/api/v2/roles/"+strconv.FormatInt(viewer.ID, 10), nil), 204)
	h.must(h.do("vera", "GET", "/api/v2/users", nil), 401)

	roles := decode[[]map[string]any](t, h.must(h.do("ops", "GET", "/api/v2/roles", nil), 200).Data)
	for _, r := range roles {
		if strings.EqualFold(r["keyword"].(string), "viewer") {
			t.Fatal("deleted role still listed")
		}
	}
}
