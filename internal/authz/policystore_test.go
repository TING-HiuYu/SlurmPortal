package authz

import (
	"context"
	"database/sql"
	"slices"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/TING-HiuYu/SlurmPortal/internal/store"
	"github.com/TING-HiuYu/SlurmPortal/migrations"
)

func TestLegacyRule(t *testing.T) {
	cases := []struct {
		path, method string
		want         Rule
		ok           bool
	}{
		{"/user/list", "GET", Rule{"admin", "user", ActionRead}, true},
		{"/user/add", "POST", Rule{"admin", "user", ActionCreate}, true},
		{"/user/changeUserStatus", "POST", Rule{"admin", "user", ActionUpdate}, true},
		{"/user/delete", "POST", Rule{"admin", "user", ActionDelete}, true},
		{"/user/syncOpenLdapUsers", "POST", Rule{"admin", "user", ActionSync}, true},
		{"/group/adduser", "POST", Rule{"admin", "group", ActionUpdate}, true},
		{"/log/operation/clean", "DELETE", Rule{"admin", "operation-log", ActionDelete}, true},
		{"/fieldrelation/list", "GET", Rule{"admin", "field-relation", ActionRead}, true},
		{"/resource-monitor/stats", "GET", Rule{"admin", "resource", ActionRead}, true},
		{"/user/info", "GET", Rule{}, false},
		{"/user/changePwd", "POST", Rule{}, false},
		{"/base/login", "POST", Rule{}, false},
		{"/user/syncDingTalkUsers", "POST", Rule{}, false},
		{"/user/mystery", "POST", Rule{}, false},
	}
	for _, tc := range cases {
		got, ok := LegacyRule("ADMIN", tc.path, tc.method)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("%s %s = %+v %v, want %+v %v", tc.method, tc.path, got, ok, tc.want, tc.ok)
		}
	}
}

func openDB(t *testing.T, legacy string) store.Store {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if legacy != "" {
		if _, err := db.Exec(legacy); err != nil {
			t.Fatal(err)
		}
	}
	s := store.SQL{DB: db}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := store.Migrate(context.Background(), s, migrations.FS); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestImportLegacyOnce(t *testing.T) {
	ctx := context.Background()
	s := openDB(t, `CREATE TABLE casbin_rule (ptype TEXT, v0 TEXT, v1 TEXT, v2 TEXT);
		INSERT INTO casbin_rule VALUES
		('p','admin','/user/list','GET'),('p','admin','/user/info','GET'),
		('p','admin','/user/add','POST'),('p','admin','/user/add','POST'),
		('p','reader','/user/list','GET'),('p','reader','/user/weird','POST'),
		('g','x','y','');`)
	imported, skipped, err := ImportLegacy(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	want := []Rule{{"admin", "user", ActionCreate}, {"admin", "user", ActionRead}, {"reader", "user", ActionRead}}
	if !slices.Equal(imported, want) || len(skipped) != 2 {
		t.Fatalf("imported=%v skipped=%v", imported, skipped)
	}
	if again, _, err := ImportLegacy(ctx, s); err != nil || again != nil {
		t.Fatalf("second import ran: %v %v", again, err)
	}
	loaded, err := LoadRules(ctx, s)
	if err != nil || !slices.Equal(loaded, append([]Rule{{"admin", "*", "*"}}, want...)) {
		t.Fatalf("loaded=%v err=%v", loaded, err)
	}
}

func TestImportLegacyFreshInstall(t *testing.T) {
	s := openDB(t, "")
	imported, skipped, err := ImportLegacy(context.Background(), s)
	if err != nil || imported != nil || skipped != nil {
		t.Fatalf("%v %v %v", imported, skipped, err)
	}
}

func TestSetRoleRules(t *testing.T) {
	ctx := context.Background()
	s := openDB(t, "")
	if err := SetRoleRules(ctx, s, "ops", []Rule{{"ops", "user", ActionRead}, {"ops", "group", ActionRead}}); err != nil {
		t.Fatal(err)
	}
	if err := SetRoleRules(ctx, s, "ops", []Rule{{"ops", "user", ActionRead}}); err != nil {
		t.Fatal(err)
	}
	if err := SetRoleRules(ctx, s, "ops", []Rule{{"other", "user", ActionRead}}); err == nil {
		t.Fatal("cross-role rule accepted")
	}
	if err := SetRoleRules(ctx, s, "ops", []Rule{{"ops", "user", "drop"}}); err == nil {
		t.Fatal("invalid action accepted")
	}
	rules, _ := LoadRules(ctx, s)
	if !slices.Equal(rules, []Rule{{"admin", "*", "*"}, {"ops", "user", ActionRead}}) {
		t.Fatalf("rules = %v", rules)
	}
}
