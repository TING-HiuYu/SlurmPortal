package plugin

import (
	"context"
	"testing"

	"github.com/TING-HiuYu/SlurmPortal/internal/telemetry"
)

type fake struct {
	id    string
	roles []Role
}

func (f fake) ID() string                      { return f.id }
func (f fake) Roles() []Role                   { return f.roles }
func (fake) Init(context.Context, *Host) error { return nil }
func (fake) Close(context.Context) error       { return nil }
func (fake) Provider(context.Context, Role, func(any) error) (telemetry.Provider, error) {
	return telemetry.Noop{}, nil
}

func TestRegistry(t *testing.T) {
	Register(fake{id: "test-master", roles: []Role{RoleMaster}})
	RegisterTelemetry(fake{id: "test-otel", roles: []Role{RoleMaster, RoleSlave}})

	sel, err := Select(RoleMaster, []string{"test-master", "test-otel"})
	if err != nil || len(sel.Plugins) != 1 || len(sel.Telemetry) != 1 {
		t.Fatalf("sel=%+v err=%v", sel, err)
	}
	for name, ids := range map[string][]string{
		"not compiled": {"missing"},
		"wrong role":   {"test-master", "test-otel"},
		"duplicate":    {"test-otel", "test-otel"},
	} {
		role := RoleMaster
		if name == "wrong role" {
			role = RoleSlave
		}
		if _, err := Select(role, ids); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("duplicate ID across kinds accepted")
			}
		}()
		Register(fake{id: "test-otel"})
	}()
}
