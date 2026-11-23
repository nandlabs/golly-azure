package azrbac

import (
	"context"
	"errors"
	"testing"
)

func TestLiveCheck_RBACResource(t *testing.T) {
	t.Parallel()
	fc := newFakeCaller() // empty cache: only the live path can grant
	fc.perms[vm] = []Permission{{
		Actions:     []string{"Microsoft.Compute/virtualMachines/*"},
		NotActions:  []string{"Microsoft.Compute/virtualMachines/delete"},
		DataActions: []string{"Microsoft.Compute/virtualMachines/login/action"},
	}}
	fc.perms[rg] = []Permission{{Actions: []string{"*/read"}}}
	e := newEval(t, fc, rg, WithExpectedMember(alice))
	p := &objPrincipal{oid: alice}
	ctx := context.Background()

	if !e.Can(p, "Microsoft.Compute/virtualMachines/start/action", RBACResource{Ctx: ctx, Scope: vm}) {
		t.Error("live allow: want true")
	}
	if e.Can(p, "Microsoft.Compute/virtualMachines/delete", &RBACResource{Ctx: ctx, Scope: vm}) {
		t.Error("live NotActions: want false")
	}
	if !e.Can(p, DataAction("Microsoft.Compute/virtualMachines/login/action"), RBACResource{Scope: vm}) {
		t.Error("live data action (nil ctx): want true")
	}
	// Empty scope falls back to the evaluator's base scope (normalized).
	fc.perms[e.Scope()] = fc.perms[rg]
	if !e.Can(p, "Microsoft.Web/sites/read", RBACResource{Ctx: ctx}) {
		t.Error("live at base scope: want true")
	}
	if got := fc.permScopes[len(fc.permScopes)-1]; got != e.Scope() {
		t.Errorf("live base scope = %q", got)
	}
	// Cache is not consulted on the live path.
	if e.Can(p, "Microsoft.Compute/virtualMachines/start/action", nil) {
		t.Error("cache should be empty")
	}
	// Principal gating applies to the live path.
	if e.Can(&objPrincipal{oid: bob}, "Microsoft.Compute/virtualMachines/start/action", RBACResource{Ctx: ctx, Scope: vm}) {
		t.Error("unexpected member on live path: want false")
	}
	// Transport errors fail closed.
	fc.set(func(f *fakeCaller) { f.permErr = errors.New("boom") })
	if e.Can(p, "Microsoft.Compute/virtualMachines/start/action", RBACResource{Ctx: ctx, Scope: vm}) {
		t.Error("live error: want false")
	}
	// A nil *RBACResource is treated as "no envelope" (cache path).
	var nilRes *RBACResource
	if e.Can(p, "Microsoft.Compute/virtualMachines/start/action", nilRes) {
		t.Error("nil envelope: want cache path (false)")
	}
}
