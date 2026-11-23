package azrbac

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"oss.nandlabs.io/golly/authz"
)

const (
	sub      = "/subscriptions/00000000-1111-2222-3333-444444444444"
	rg       = sub + "/resourceGroups/rg-app"
	otherRG  = sub + "/resourceGroups/rg-other"
	vm       = rg + "/providers/Microsoft.Compute/virtualMachines/vm1"
	mgScope  = "/providers/Microsoft.Management/managementGroups/root-mg"
	alice    = "aaaaaaaa-0000-0000-0000-000000000001"
	bob      = "bbbbbbbb-0000-0000-0000-000000000002"
	devGroup = "99999999-0000-0000-0000-00000000000d"

	readerID      = sub + "/providers/Microsoft.Authorization/roleDefinitions/acdd72a7-3385-48ef-bd42-f606fba81ae7"
	contributorID = sub + "/providers/Microsoft.Authorization/roleDefinitions/b24988ac-6180-42a0-ab88-20f7382dd24c"
	blobReaderID  = sub + "/providers/Microsoft.Authorization/roleDefinitions/2a2b9908-6ea1-4ae2-8e65-a410df84e7d1"
	vmOperatorID  = sub + "/providers/Microsoft.Authorization/roleDefinitions/custom-vm-operator"
)

var roleDefs = map[string]RoleDefinition{
	readerID: {ID: readerID, RoleName: "Reader", Permissions: []Permission{{Actions: []string{"*/read"}}}},
	contributorID: {ID: contributorID, RoleName: "Contributor", Permissions: []Permission{{
		Actions:    []string{"*"},
		NotActions: []string{"Microsoft.Authorization/*/Delete", "Microsoft.Authorization/*/Write", "Microsoft.Authorization/elevateAccess/Action"},
	}}},
	blobReaderID: {ID: blobReaderID, RoleName: "Storage Blob Data Reader", Permissions: []Permission{{
		Actions:     []string{"Microsoft.Storage/storageAccounts/blobServices/containers/read"},
		DataActions: []string{"Microsoft.Storage/storageAccounts/blobServices/containers/blobs/read"},
	}}},
	vmOperatorID: {ID: vmOperatorID, RoleName: "VM Operator", Permissions: []Permission{{
		Actions: []string{"Microsoft.Compute/virtualMachines/*"},
	}, {
		Actions:    []string{"Microsoft.Network/*"},
		NotActions: []string{"Microsoft.Network/*/delete"},
	}}},
}

type objPrincipal struct {
	oid    string
	groups []string
}

func (p *objPrincipal) ObjectID() string       { return p.oid }
func (p *objPrincipal) Groups() []string       { return p.groups }
func (p *objPrincipal) Roles() []string        { return nil }
func (p *objPrincipal) Capabilities() []string { return nil }

type idPrincipal struct{ id string }

func (p idPrincipal) ID() string             { return p.id }
func (p idPrincipal) Roles() []string        { return nil }
func (p idPrincipal) Capabilities() []string { return nil }

type memberPrincipal struct{ m string }

func (p memberPrincipal) Member() string         { return p.m }
func (p memberPrincipal) Roles() []string        { return nil }
func (p memberPrincipal) Capabilities() []string { return nil }

// fakeCaller is an in-memory RBACCaller.
type fakeCaller struct {
	mu          sync.Mutex
	assignments []RoleAssignment
	denies      []DenyAssignment
	defs        map[string]RoleDefinition
	perms       map[string][]Permission // scope -> caller permissions

	listErr, defErr, denyErr, permErr error
	listCalls, defCalls, denyCalls    int
	permScopes                        []string
}

func newFakeCaller(assignments ...RoleAssignment) *fakeCaller {
	return &fakeCaller{assignments: assignments, defs: roleDefs, perms: map[string][]Permission{}}
}

func (f *fakeCaller) ListRoleAssignments(_ context.Context, scope string) ([]RoleAssignment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]RoleAssignment(nil), f.assignments...), nil
}

func (f *fakeCaller) GetRoleDefinition(_ context.Context, id string) (RoleDefinition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.defCalls++
	if f.defErr != nil {
		return RoleDefinition{}, f.defErr
	}
	d, ok := f.defs[id]
	if !ok {
		return RoleDefinition{}, errors.New("RoleDefinitionDoesNotExist")
	}
	return d, nil
}

func (f *fakeCaller) ListDenyAssignments(context.Context, string) ([]DenyAssignment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.denyCalls++
	if f.denyErr != nil {
		return nil, f.denyErr
	}
	return append([]DenyAssignment(nil), f.denies...), nil
}

func (f *fakeCaller) ListPermissions(_ context.Context, scope string) ([]Permission, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.permScopes = append(f.permScopes, scope)
	if f.permErr != nil {
		return nil, f.permErr
	}
	return f.perms[scope], nil
}

func (f *fakeCaller) set(fn func(f *fakeCaller)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeCaller) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls
}

func assign(principal, scope, def string) RoleAssignment {
	return RoleAssignment{ID: scope + "/ra/" + principal, Scope: scope, PrincipalID: principal, RoleDefinitionID: def}
}

func newEval(t *testing.T, fc *fakeCaller, scope string, opts ...Option) *Evaluator {
	t.Helper()
	e := NewCachedEvaluatorFromCaller(fc, scope, 0, opts...)
	t.Cleanup(func() { _ = e.Close() })
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	return e
}

func TestCan_RoleAssignmentAtOrAboveScope(t *testing.T) {
	t.Parallel()
	fc := newFakeCaller(
		assign(alice, sub, readerID),        // inherited from subscription
		assign(alice, rg, vmOperatorID),     // at the base scope
		assign(bob, mgScope, contributorID), // management group, non-prefix id
	)
	e := newEval(t, fc, rg)
	a := &objPrincipal{oid: alice}
	b := &objPrincipal{oid: bob}

	cases := []struct {
		p      authz.Principal
		action string
		res    any
		want   bool
	}{
		{a, "Microsoft.Compute/virtualMachines/read", nil, true},
		{a, "Microsoft.Compute/virtualMachines/start/action", nil, true},
		{a, "microsoft.compute/VIRTUALMACHINES/deallocate/action", nil, true}, // case-insensitive
		{a, "Microsoft.Storage/storageAccounts/write", nil, false},
		{a, "Microsoft.Network/virtualNetworks/write", vm, true},
		{a, "Microsoft.Network/virtualNetworks/delete", vm, false}, // NotActions within block
		{a, "Microsoft.Storage/storageAccounts/read", nil, true},   // Reader */read
		{b, "Microsoft.Storage/storageAccounts/write", vm, true},   // MG inherited into base
		{b, "Microsoft.Authorization/roleAssignments/write", nil, false},
		{b, "Microsoft.Authorization/elevateAccess/Action", nil, false},
		{&objPrincipal{oid: "stranger"}, "Microsoft.Compute/virtualMachines/read", nil, false},
	}
	for i, c := range cases {
		if got := e.Can(c.p, c.action, c.res); got != c.want {
			t.Errorf("case %d (%s on %v): got %v want %v", i, c.action, c.res, got, c.want)
		}
	}
}

func TestCan_TargetOutsideBaseScope(t *testing.T) {
	t.Parallel()
	fc := newFakeCaller(
		assign(alice, sub, readerID),
		assign(alice, rg, vmOperatorID),
		assign(bob, mgScope, contributorID),
	)
	e := newEval(t, fc, rg)
	a := &objPrincipal{oid: alice}
	// Subscription-level Reader still applies to a sibling RG...
	if !e.Can(a, "Microsoft.Compute/virtualMachines/read", otherRG) {
		t.Error("sub-level reader on sibling RG: want true")
	}
	// ...but the RG-level VM operator role does not.
	if e.Can(a, "Microsoft.Compute/virtualMachines/start/action", otherRG) {
		t.Error("rg-level role on sibling RG: want false")
	}
	// MG assignments can't be placed textually outside the base scope:
	// conservative deny.
	if e.Can(&objPrincipal{oid: bob}, "Microsoft.Storage/storageAccounts/write", otherRG) {
		t.Error("mg role outside base: want false (conservative)")
	}
	// Non-scope strings fall back to the base scope.
	if !e.Can(a, "Microsoft.Compute/virtualMachines/start/action", "not-a-scope") {
		t.Error("non-scope string: want base-scope evaluation")
	}
	if !e.Can(a, "Microsoft.Compute/virtualMachines/start/action", strings.ToUpper(rg)+"/") {
		t.Error("scope compare should be case-insensitive and ignore trailing slash")
	}
}

func TestCan_RootScopeAssignment(t *testing.T) {
	t.Parallel()
	fc := newFakeCaller(assign(alice, "/", readerID))
	e := newEval(t, fc, rg)
	if !e.Can(&objPrincipal{oid: alice}, "Microsoft.Web/sites/read", otherRG) {
		t.Error("root-scope assignment applies everywhere")
	}
}

func TestCan_GroupAssignments(t *testing.T) {
	t.Parallel()
	fc := newFakeCaller(assign(devGroup, rg, contributorID))
	e := newEval(t, fc, rg)
	member := &objPrincipal{oid: alice, groups: []string{"unrelated", strings.ToUpper(devGroup), ""}}
	if !e.Can(member, "Microsoft.Web/sites/write", nil) {
		t.Error("group member: want true")
	}
	if e.Can(&objPrincipal{oid: alice}, "Microsoft.Web/sites/write", nil) {
		t.Error("non-member: want false")
	}
	// Custom group resolver.
	e2 := newEval(t, fc, rg, WithPrincipalGroups(func(authz.Principal) []string { return []string{devGroup} }))
	if !e2.Can(idPrincipal{id: bob}, "Microsoft.Web/sites/write", nil) {
		t.Error("custom group resolver: want true")
	}
	// Evaluate takes ids directly.
	if !e.Evaluate(bob, []string{devGroup}, "", "Microsoft.Web/sites/write") {
		t.Error("Evaluate with group: want true")
	}
	if e.Evaluate(bob, nil, vm, "Microsoft.Web/sites/write") || e.Evaluate("", nil, "", "x") || e.Evaluate(bob, nil, "", "") {
		t.Error("Evaluate negatives")
	}
	if e.Evaluate(bob, []string{devGroup}, "", "data:") {
		t.Error("empty data action")
	}
}

func TestCan_DataActions(t *testing.T) {
	t.Parallel()
	fc := newFakeCaller(assign(alice, rg, blobReaderID), assign(bob, rg, contributorID))
	blobRead := "Microsoft.Storage/storageAccounts/blobServices/containers/blobs/read"
	e := newEval(t, fc, rg, WithDataActions(blobRead))
	a, b := &objPrincipal{oid: alice}, &objPrincipal{oid: bob}
	if !e.Can(a, DataAction(blobRead), nil) {
		t.Error("data action via prefix: want true")
	}
	if !e.Can(a, blobRead, nil) {
		t.Error("data action via WithDataActions: want true")
	}
	if !e.Can(a, "Microsoft.Storage/storageAccounts/blobServices/containers/read", nil) {
		t.Error("control action of data role: want true")
	}
	// Contributor's Actions "*" does NOT grant data actions.
	if e.Can(b, DataAction(blobRead), nil) || e.Can(b, blobRead, nil) {
		t.Error("contributor must not get data actions")
	}
	if e.Can(a, "data:", nil) {
		t.Error("empty data action")
	}
}

func TestCan_DenyAssignments(t *testing.T) {
	t.Parallel()
	fc := newFakeCaller(assign(alice, rg, contributorID), assign(bob, rg, contributorID))
	fc.denies = []DenyAssignment{
		{ID: "d1", Scope: sub, Principals: []string{EveryonePrincipalID}, ExcludePrincipals: []string{strings.ToUpper(bob)},
			Permissions: []Permission{{Actions: []string{"Microsoft.Compute/*/delete"}}}},
		{ID: "d2", Scope: rg, Principals: []string{alice}, DoNotApplyToChildScopes: true,
			Permissions: []Permission{{Actions: []string{"Microsoft.Web/*"}, NotActions: []string{"*/read"}}}},
		{ID: "d3", Scope: sub, Principals: []string{devGroup},
			Permissions: []Permission{{Actions: []string{"Microsoft.Sql/*"}}}},
	}
	e := newEval(t, fc, rg)
	a, b := &objPrincipal{oid: alice}, &objPrincipal{oid: bob}

	if e.Can(a, "Microsoft.Compute/virtualMachines/delete", vm) {
		t.Error("everyone deny: alice must be denied")
	}
	if !e.Can(b, "Microsoft.Compute/virtualMachines/delete", vm) {
		t.Error("excluded principal: bob must be allowed")
	}
	if e.Can(a, "Microsoft.Web/sites/write", nil) {
		t.Error("deny at exact scope: want false")
	}
	if !e.Can(a, "Microsoft.Web/sites/read", nil) {
		t.Error("deny NotActions: read still allowed")
	}
	if !e.Can(a, "Microsoft.Web/sites/write", vm) {
		t.Error("DoNotApplyToChildScopes: child scope allowed")
	}
	if !e.Can(a, "Microsoft.Sql/servers/write", nil) {
		t.Error("group deny doesn't apply to non-member")
	}
	if e.Can(&objPrincipal{oid: alice, groups: []string{devGroup}}, "Microsoft.Sql/servers/write", nil) {
		t.Error("group deny applies to member")
	}
	// Deny outside base scope uses textual containment.
	fc.set(func(f *fakeCaller) {
		f.assignments = []RoleAssignment{assign(alice, sub, contributorID)}
		f.denies = []DenyAssignment{{ID: "d4", Scope: sub, Principals: []string{alice},
			Permissions: []Permission{{Actions: []string{"Microsoft.KeyVault/*"}}}}}
	})
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.Can(a, "Microsoft.KeyVault/vaults/write", otherRG) {
		t.Error("sub-level deny on sibling RG: want false")
	}
	if !e.Can(a, "Microsoft.Web/sites/write", otherRG) {
		t.Error("sub-level contributor on sibling RG: want true")
	}

	// WithoutDenyAssignments never lists them.
	e2 := newEval(t, fc, rg, WithoutDenyAssignments())
	before := fc.denyCalls
	_ = e2.Refresh(context.Background())
	if fc.denyCalls != before {
		t.Error("deny assignments listed despite WithoutDenyAssignments")
	}
	if !e2.Can(a, "Microsoft.KeyVault/vaults/write", nil) {
		t.Error("without denies: want true")
	}
}

func TestCan_ConditionalAssignmentsIgnored(t *testing.T) {
	t.Parallel()
	ra := assign(alice, rg, blobReaderID)
	ra.Condition = "@Resource[Microsoft.Storage/storageAccounts/blobServices/containers:name] StringEquals 'logs'"
	fc := newFakeCaller(ra, RoleAssignment{PrincipalID: "", RoleDefinitionID: readerID}, RoleAssignment{PrincipalID: bob})
	e := newEval(t, fc, rg)
	if e.Can(&objPrincipal{oid: alice}, DataAction("Microsoft.Storage/storageAccounts/blobServices/containers/blobs/read"), nil) {
		t.Error("conditional assignment must fail closed")
	}
}

func TestCan_PrincipalExtraction(t *testing.T) {
	t.Parallel()
	fc := newFakeCaller(assign(alice, rg, readerID))
	e := newEval(t, fc, rg)
	for name, p := range map[string]authz.Principal{
		"ID":     idPrincipal{id: alice},
		"Member": memberPrincipal{m: strings.ToUpper(alice)},
	} {
		if !e.Can(p, "x/read", nil) {
			t.Errorf("%s accessor: want true", name)
		}
	}
	if e.Can(&authz.BasicPrincipal{}, "x/read", nil) || e.Can(nil, "x/read", nil) {
		t.Error("principal without id: want false")
	}
	if e.Can(idPrincipal{id: alice}, "", nil) {
		t.Error("empty action: want false")
	}
	custom := newEval(t, fc, rg, WithPrincipalMember(func(authz.Principal) string { return alice }))
	if !custom.Can(&authz.BasicPrincipal{}, "x/read", nil) {
		t.Error("custom extractor: want true")
	}
	pinned := newEval(t, fc, rg, WithExpectedMember(strings.ToUpper(alice)))
	if !pinned.Can(idPrincipal{id: alice}, "x/read", nil) {
		t.Error("expected member: want true")
	}
	if pinned.Can(idPrincipal{id: bob}, "x/read", nil) {
		t.Error("other member: want false")
	}
	var nilEval *Evaluator
	if nilEval.Can(idPrincipal{id: alice}, "x/read", nil) || nilEval.Evaluate(alice, nil, "", "x") || nilEval.Close() != nil {
		t.Error("nil evaluator")
	}
	if nilEval.Refresh(context.Background()) == nil {
		t.Error("nil evaluator Refresh should error")
	}
}

func TestRefresh_ErrorsPreserveSnapshot(t *testing.T) {
	t.Parallel()
	fc := newFakeCaller(assign(alice, rg, readerID))
	e := newEval(t, fc, rg)
	p := &objPrincipal{oid: alice}
	boom := errors.New("boom")
	for name, mut := range map[string]func(f *fakeCaller){
		"list": func(f *fakeCaller) { f.listErr = boom },
		"deny": func(f *fakeCaller) { f.denyErr = boom },
		"def": func(f *fakeCaller) {
			f.assignments = append(f.assignments, assign(alice, rg, "/missing/role"))
		},
	} {
		fc.set(func(f *fakeCaller) {
			f.listErr, f.denyErr = nil, nil
			f.assignments = []RoleAssignment{assign(alice, rg, readerID)}
		})
		fc.set(mut)
		if err := e.Refresh(context.Background()); err == nil {
			t.Errorf("%s: expected error", name)
		}
		if !e.Can(p, "x/read", nil) {
			t.Errorf("%s: snapshot should be preserved", name)
		}
	}
}

func TestRefresh_NoSnapshotDeniesAndValidation(t *testing.T) {
	t.Parallel()
	fc := newFakeCaller(assign(alice, rg, readerID))
	e := NewCachedEvaluatorFromCaller(fc, rg, 0)
	defer func() { _ = e.Close() }()
	if e.Can(&objPrincipal{oid: alice}, "x/read", nil) {
		t.Error("before first Refresh: want false")
	}
	if e.Scope() != strings.ToLower(rg) {
		t.Errorf("Scope = %q", e.Scope())
	}
	if err := NewCachedEvaluatorFromCaller(fc, "", 0).Refresh(context.Background()); err == nil {
		t.Error("empty scope: want error")
	}
	if err := NewCachedEvaluatorFromCaller(nil, rg, 0).Refresh(context.Background()); err == nil {
		t.Error("nil caller: want error")
	}
	//nolint:staticcheck // nil ctx is tolerated deliberately
	if err := e.Refresh(nil); err != nil {
		t.Errorf("nil ctx: %v", err)
	}
}

func TestRefresh_RoleDefinitionCache(t *testing.T) {
	t.Parallel()
	now := time.Now()
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	fc := newFakeCaller(assign(alice, rg, readerID), assign(bob, rg, readerID), assign(bob, sub, readerID))
	e := newEval(t, fc, rg, WithClock(clock), WithRoleDefinitionTTL(time.Minute))
	if fc.defCalls != 1 {
		t.Fatalf("role def fetched %d times, want 1 (deduped)", fc.defCalls)
	}
	_ = e.Refresh(context.Background())
	if fc.defCalls != 1 {
		t.Fatalf("role def should be cached across refreshes; calls=%d", fc.defCalls)
	}
	mu.Lock()
	now = now.Add(2 * time.Minute)
	mu.Unlock()
	_ = e.Refresh(context.Background())
	if fc.defCalls != 2 {
		t.Fatalf("role def should refetch after TTL; calls=%d", fc.defCalls)
	}
}

func TestMaxStaleness(t *testing.T) {
	t.Parallel()
	now := time.Now()
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	fc := newFakeCaller(assign(alice, rg, readerID))
	e := newEval(t, fc, rg, WithClock(clock), WithMaxStaleness(time.Minute))
	p := &objPrincipal{oid: alice}
	if !e.Can(p, "x/read", nil) {
		t.Fatal("fresh: want true")
	}
	mu.Lock()
	now = now.Add(2 * time.Minute)
	mu.Unlock()
	if e.Can(p, "x/read", nil) {
		t.Fatal("stale: want false (fail closed)")
	}
	_ = e.Refresh(context.Background())
	if !e.Can(p, "x/read", nil) {
		t.Fatal("refreshed: want true")
	}
}

func TestBackgroundLoop(t *testing.T) {
	t.Parallel()
	fc := newFakeCaller(assign(alice, rg, readerID))
	e := NewCachedEvaluatorFromCaller(fc, rg, 20*time.Millisecond)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && fc.calls() < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	if fc.calls() < 2 {
		t.Fatalf("background loop refreshes: %d", fc.calls())
	}
	if !e.Can(&objPrincipal{oid: alice}, "x/read", nil) {
		t.Error("after background refresh: want true")
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal("second Close")
	}
	n := fc.calls()
	time.Sleep(60 * time.Millisecond)
	if fc.calls() != n {
		t.Error("loop kept running after Close")
	}
}

func TestRBACPolicyComposes(t *testing.T) {
	t.Parallel()
	fc := newFakeCaller(assign(alice, rg, readerID))
	e := newEval(t, fc, rg)
	rbac := authz.NewRBAC()
	rbac.AddRole("ops", "Microsoft.Compute/virtualMachines/restart/action")
	pol := authz.Any(e, rbac)
	p := &rolePrincipal{objPrincipal: objPrincipal{oid: alice}, roles: []string{"ops"}}
	if !pol.Can(p, "Microsoft.Compute/virtualMachines/read", nil) || !pol.Can(p, "Microsoft.Compute/virtualMachines/restart/action", nil) {
		t.Error("composition")
	}
	var _ authz.Policy = e
}

type rolePrincipal struct {
	objPrincipal
	roles []string
}

func (r *rolePrincipal) Roles() []string { return r.roles }
