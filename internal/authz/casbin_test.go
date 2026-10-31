package authz

import (
	"context"
	"testing"

	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
)

func TestCasbin(t *testing.T) {
	c, err := NewCasbin([]Rule{
		{Role: "admin", Object: "user", Action: "*"},
		{Role: "resource_readonly", Object: "resource", Action: "read"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	check := func(roles []string, obj, act string, want bool) {
		t.Helper()
		got, err := c.Allow(ctx, &authn.Principal{Roles: roles}, obj, act)
		if err != nil || got != want {
			t.Fatalf("%v %s %s = %v (%v), want %v", roles, obj, act, got, err, want)
		}
	}
	check([]string{"admin"}, "user", "delete", true)
	check([]string{"resource_readonly"}, "resource", "read", true)
	check([]string{"resource_readonly"}, "resource", "write", false)
	check([]string{"resource_readonly", "admin"}, "user", "write", true)
	check(nil, "user", "read", false)
	if ok, _ := c.Allow(ctx, nil, "user", "read"); ok {
		t.Fatal("nil principal allowed")
	}

	if err := c.Replace(nil); err != nil {
		t.Fatal(err)
	}
	check([]string{"admin"}, "user", "delete", false)
}

func TestPermissionValidate(t *testing.T) {
	if (Permission{}).Validate() == nil || Policy("", "x").Validate() == nil || Policy("x", "").Validate() == nil {
		t.Fatal("invalid permission accepted")
	}
	if Public().Validate() != nil || Authenticated().Validate() != nil || Policy("a", "b").Validate() != nil {
		t.Fatal("valid permission rejected")
	}
}
